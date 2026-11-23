package main

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestPurposeLabelTable(t *testing.T) {
	rows := []struct {
		purpose, label string
		metadata       bool
	}{
		{"agent", "chat", false}, {"coding", "code delegation", false},
		{"planning", "planning", false}, {"summarize", "summaries", false},
		{"embedding", "embeddings", false}, {"route", "routing", false},
		{"rerank", "reranking", false}, {"verify", "verification", false},
		{"extract", "extraction", false}, {"approval", "approval checks", false},
		{"vision", "vision", false}, {"fim", "code completion", false},
		{"model-refresh", "model list", true}, {"capability-probe", "capabilities", true},
		{"slot-probe", "slot status", true}, {"discovery", "server discovery", true},
		{"health", "health checks", true}, {"warmth-poll", "warm-up status", true},
	}
	for _, r := range rows {
		label, md := purposeLabel(r.purpose)
		if label != r.label || md != r.metadata {
			t.Errorf("purposeLabel(%q) = (%q, %v), want (%q, %v)", r.purpose, label, md, r.label, r.metadata)
		}
	}
	if label, md := purposeLabel("chat"); label != "other: chat" || md {
		t.Errorf("purposeLabel(chat) = (%q, %v)", label, md)
	}
	// An id that is not bare is quoted and escaped, like a provider name.
	if label, _ := purposeLabel("x\x1b"); strings.Contains(label, "\x1b") || label != `other: "x\x1b"` {
		t.Errorf("purposeLabel(x ESC) = %q, want %q and no ESC", label, `other: "x\x1b"`)
	}
}

func TestDestinationName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"opencode", "opencode"},
		{"llama.cpp-2", "llama.cpp-2"},
		// Spaces show as \x20, so no space sits inside a quoted name.
		{"alpha and beta", `"alpha\x20and\x20beta"`},
		{`a\x20b`, `"a\\x20b"`},
		{`a"b\c`, `"a\"b\\c"`},
		{"n\u00e9v", `"n\u00e9v"`},
		{"a\u202eb", `"a\u202eb"`},
	}
	for _, c := range cases {
		// Fixtures must be reachable from config.
		admDest(t, c.in, "https://example.com/v1")
		if got := destinationName(c.in); got != c.want {
			t.Errorf("destinationName(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestRemoteStates(t *testing.T) {
	lo := admDest(t, "lo", "http://127.0.0.1:8080")
	flagged := admDest(t, "flagged", "https://flagged.example.com/v1")
	granted := admDest(t, "granted", "https://granted.example.com/v1")
	fresh := admDest(t, "fresh", "https://fresh.example.com/v1")
	m, err := provider.NewDestinationManifest(
		provider.DestinationEdge{Purpose: "agent", Destination: lo},
		provider.DestinationEdge{Purpose: "agent", Destination: flagged},
		provider.DestinationEdge{Purpose: "summarize", Destination: granted},
		provider.DestinationEdge{Purpose: "embedding", Destination: fresh},
	)
	if err != nil {
		t.Fatal(err)
	}
	flags := provider.NewDestinationPolicy(flagged)
	// Runtime shape: the baseline always includes the flag grants, so only
	// the switch order makes a flagged remote read "by flag".
	baseline := provider.NewDestinationPolicy(flagged, granted)
	for _, c := range []struct {
		mode  renderMode
		fresh remoteState
	}{{renderAsking, remoteNeedsOK}, {renderRefusing, remoteRefused}, {renderListing, remoteNotAllowed}} {
		got := remoteStates(m, flags, baseline, c.mode)
		if len(got) != 3 {
			t.Errorf("mode %d: %d entries, want the 3 remotes: %v", c.mode, len(got), got)
		}
		if _, ok := got[lo]; ok {
			t.Errorf("mode %d: local destination present", c.mode)
		}
		if got[flagged] != remoteAllowedByFlag || got[granted] != remoteAllowedEarlier || got[fresh] != c.fresh {
			t.Errorf("mode %d: got %v", c.mode, got)
		}
	}
}

// A flagged remote the baseline does not cover reads the mode's uncovered
// state, and the asking-mode "needs your OK" set equals uncoveredAll.
func TestRemoteStatesBaselineDecidesCoverage(t *testing.T) {
	flagged := admDest(t, "flagged", "https://flagged.example.com/v1")
	granted := admDest(t, "granted", "https://granted.example.com/v1")
	m, err := provider.NewDestinationManifest(
		provider.DestinationEdge{Purpose: "agent", Destination: flagged},
		provider.DestinationEdge{Purpose: "agent", Destination: granted},
	)
	if err != nil {
		t.Fatal(err)
	}
	flags := provider.NewDestinationPolicy(flagged)
	baseline := provider.NewDestinationPolicy(granted) // flags not a subset
	for _, c := range []struct {
		mode renderMode
		want remoteState
	}{{renderAsking, remoteNeedsOK}, {renderRefusing, remoteRefused}, {renderListing, remoteNotAllowed}} {
		got := remoteStates(m, flags, baseline, c.mode)
		if got[flagged] != c.want || got[granted] != remoteAllowedEarlier {
			t.Errorf("mode %d: got %v", c.mode, got)
		}
	}
	got := remoteStates(m, flags, baseline, renderAsking)
	uncovered := uncoveredAll(m.Destinations(), baseline)
	n := 0
	for d, st := range got {
		if st == remoteNeedsOK {
			n++
			if !slices.Contains(uncovered, d) {
				t.Errorf("%v needs OK but is covered", d)
			}
		}
	}
	if n != len(uncovered) {
		t.Errorf("needs-OK rows = %d, uncoveredAll = %d", n, len(uncovered))
	}
}

func TestRemoteStateText(t *testing.T) {
	want := []string{
		"needs your OK",
		"allowed by -allow-destination",
		"allowed earlier this session",
		"not allowed (cleared; the next goal asks again)",
		"not allowed (needs -allow-destination)",
	}
	got := []remoteState{remoteNeedsOK, remoteAllowedByFlag, remoteAllowedEarlier, remoteNotAllowed, remoteRefused}
	for i, s := range got {
		if s.text() != want[i] {
			t.Errorf("state %d text = %q, want %q", i, s.text(), want[i])
		}
	}
}

func TestUncoveredAll(t *testing.T) {
	a := admDest(t, "a", "https://a.example.com/v1")
	b := admDest(t, "b", "https://b.example.com/v1")
	c := admDest(t, "c", "https://c.example.com/v1")
	lo := admDest(t, "lo", "http://127.0.0.1:8080")
	got := uncoveredAll([]provider.Destination{c, lo, a, b}, provider.NewDestinationPolicy(a))
	if len(got) != 2 || got[0] != c || got[1] != b {
		t.Errorf("uncoveredAll = %v", got)
	}
}

func TestConsentQuestion(t *testing.T) {
	q := func(names ...string) string {
		var ds []provider.Destination
		for _, n := range names {
			ds = append(ds, admDest(t, n, "https://"+strings.ReplaceAll(n, " ", "-")+".example.com/v1"))
		}
		return consentQuestion(ds)
	}
	const pre, post = "Send data to ", " (remote) during this session? [y/N] "
	for _, c := range []struct{ got, names string }{
		{q("opencode"), "opencode"},
		{q("opencode", "groq"), "groq and opencode"},
		{q("e", "d", "c", "b", "a"), "a, b, c, d and e"},
		{q("alpha and beta", "gamma"), `"alpha\x20and\x20beta" and gamma`},
		{q("alpha and beta"), `"alpha\x20and\x20beta"`},
		{q("alpha", "beta"), "alpha and beta"},
		{q("beta", "alpha"), "alpha and beta"},
	} {
		if want := pre + c.names + post; c.got != want {
			t.Errorf("got %q, want %q", c.got, want)
		}
	}
	// One server named "alpha and beta" is not two servers alpha and beta.
	if q("alpha and beta") == q("alpha", "beta") {
		t.Error("one server named \"alpha and beta\" and the servers alpha and beta produce identical questions")
	}
	if strings.Contains(q("a"), "\x1b") {
		t.Error("question carries ESC")
	}
}

// testManifest builds a manifest from edges or fails the test.
func testManifest(t *testing.T, edges ...provider.DestinationEdge) *provider.DestinationManifest {
	t.Helper()
	m, err := provider.NewDestinationManifest(edges...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// renderString is renderManifest into a string.
func renderString(m *provider.DestinationManifest, keys map[string]string, states map[provider.Destination]remoteState, opt manifestOpts) string {
	var b strings.Builder
	_ = renderManifest(&b, m, keys, states, opt) // a strings.Builder never fails
	return b.String()
}

// plain80 is the color-off, 80-column style most renderer tests use.
var plain80 = manifestOpts{style: consoleStyle{false, 80}}

// dailyManifest is the daily config: a local llama.cpp server and a keyed
// remote that is a chat fallback and the summaries server.
func dailyManifest(t *testing.T) (*provider.DestinationManifest, provider.Destination) {
	t.Helper()
	local := admDest(t, "llamacpp", "http://localhost:8090")
	remote := admDest(t, "opencode", "https://opencode.ai/zen/go")
	return testManifest(t,
		provider.DestinationEdge{Purpose: "agent", Destination: local},
		provider.DestinationEdge{Purpose: provider.DestinationPurposeModelRefresh, Destination: local},
		provider.DestinationEdge{Purpose: provider.DestinationPurposeCapabilityProbe, Destination: local},
		provider.DestinationEdge{Purpose: provider.DestinationPurposeSlotProbe, Destination: local},
		provider.DestinationEdge{Purpose: "agent", Destination: remote, IsFallback: true},
		provider.DestinationEdge{Purpose: "summarize", Destination: remote},
		provider.DestinationEdge{Purpose: provider.DestinationPurposeModelRefresh, Destination: remote},
		provider.DestinationEdge{Purpose: provider.DestinationPurposeCapabilityProbe, Destination: remote},
	), remote
}

// dailyGolden is the daily config rendered at 80 columns, color off.
const dailyGolden = "\u2500\u2500 model servers \u2500\u2500 where this session's model requests can go " +
	"\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\n" +
	"On this machine (allowed automatically)\n" +
	"  llamacpp  http://localhost:8090\n" +
	"            used for  chat\n" +
	"            checks    capabilities, model list, slot status\n" +
	"Remote\n" +
	"  opencode  https://opencode.ai/zen/go  needs your OK\n" +
	"            used for  chat (fallback), summaries\n" +
	"            checks    capabilities, model list\n" +
	"            sends api_key from $OC_GO_API_KEY\n" +
	"\n"

func TestRenderManifestDailyConfig(t *testing.T) {
	m, remote := dailyManifest(t)
	got := renderString(m, map[string]string{"opencode": "sends api_key from $OC_GO_API_KEY"},
		map[provider.Destination]remoteState{remote: remoteNeedsOK}, plain80)
	if got != dailyGolden {
		t.Errorf("got:\n%s\nwant:\n%s\n(%q)", got, dailyGolden, got)
	}
	if !strings.HasSuffix(got, "$OC_GO_API_KEY\n\n") {
		t.Errorf("no trailing empty line after an asking render: %q", got)
	}
}

func TestRenderManifestLocalOnly(t *testing.T) {
	local := admDest(t, "llamacpp", "http://localhost:8090")
	got := renderString(testManifest(t, provider.DestinationEdge{Purpose: "agent", Destination: local}), nil, nil, plain80)
	if strings.Contains(got, "Remote") || strings.HasSuffix(got, "\n\n") {
		t.Errorf("local-only render:\n%q", got)
	}
	if !strings.Contains(got, "\nOn this machine (allowed automatically)\n  llamacpp  http://localhost:8090\n") {
		t.Errorf("local group missing:\n%s", got)
	}
}

func TestRenderManifestEmpty(t *testing.T) {
	if got := renderString(testManifest(t), nil, nil, plain80); got != "" {
		t.Errorf("empty manifest rendered %q", got)
	}
}

func TestRenderManifestStates(t *testing.T) {
	states := map[provider.Destination]remoteState{}
	var edges []provider.DestinationEdge
	for i, st := range []remoteState{remoteAllowedByFlag, remoteAllowedEarlier, remoteNotAllowed, remoteRefused} {
		d := admDest(t, "r"+strconv.Itoa(i), "https://r"+strconv.Itoa(i)+".example.com")
		states[d] = st
		edges = append(edges, provider.DestinationEdge{Purpose: "agent", Destination: d})
	}
	got := renderString(testManifest(t, edges...), nil, states, manifestOpts{style: consoleStyle{false, 120}})
	for d, st := range states {
		want := "  " + d.Provider() + "  " + d.BaseURL() + "  " + st.text() + "\n"
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.HasSuffix(got, "\n\n") || strings.Contains(got, remoteNeedsOK.text()) {
		t.Errorf("non-asking render has a trailing empty line or asks:\n%q", got)
	}
	// One heading for the whole remote group, however many remotes.
	if n := strings.Count(got, "Remote\n"); n != 1 {
		t.Errorf("Remote heading printed %d times, want 1:\n%s", n, got)
	}
	on := renderString(testManifest(t, edges...), nil, states, manifestOpts{style: consoleStyle{true, 120}})
	if n := strings.Count(on, "\x1b[1;33mRemote\x1b[0m\n"); n != 1 {
		t.Errorf("color Remote heading printed %d times, want 1:\n%q", n, on)
	}
}

// After /grants clear the note sits right under the rule, word-wrapped at the
// writer width with no indent and each line dim on its own, so no line of it,
// and no 80-column row a terminal could wrap one into, reads like a state.
func TestRenderManifestCleared(t *testing.T) {
	m, remote := dailyManifest(t)
	states := map[provider.Destination]remoteState{remote: remoteNotAllowed}
	for _, c := range []struct {
		width int
		note  []string
	}{
		{80, []string{
			"access cleared by /grants clear; the next admission asks about every remote",
			"-allow-destination does not cover",
		}},
		{40, []string{
			"access cleared by /grants clear; the",
			"next admission asks about every remote",
			"-allow-destination does not cover",
		}},
	} {
		got := renderString(m, nil, states, manifestOpts{style: consoleStyle{false, c.width}, cleared: true})
		// The rule is one line at both widths (the session subtitle is
		// dropped at 40), and the machine group follows the note.
		lines := strings.Split(got, "\n")
		end := slices.Index(lines, "On this machine (allowed automatically)")
		if end < 1 || !slices.Equal(lines[1:end], c.note) {
			t.Errorf("width %d: note under the rule = %q, want %q:\n%s", c.width, lines[1:max(1, end)], c.note, got)
		}
		for _, l := range lines[1:max(1, end)] {
			for _, row := range append([]string{l}, physicalRows(l, 80)...) {
				for _, bad := range []string{remoteAllowedByFlag.text(), "not allowed"} {
					if strings.Contains(row, bad) {
						t.Errorf("width %d: cleared note row %q contains %q", c.width, row, bad)
					}
				}
			}
		}
		on := renderString(m, nil, states, manifestOpts{style: consoleStyle{true, c.width}, cleared: true})
		if stripped := sgrPattern.ReplaceAllString(on, ""); stripped != got {
			t.Errorf("width %d: color output strips to\n%q\nwant\n%q", c.width, stripped, got)
		}
		for _, l := range c.note {
			if want := "\n\x1b[2m" + l + "\x1b[0m\n"; !strings.Contains(on, want) {
				t.Errorf("width %d: note line not dim on its own: %q in\n%q", c.width, want, on)
			}
		}
	}
	if strings.Contains(renderString(m, nil, states, plain80), "access cleared") {
		t.Error("cleared note shown without cleared")
	}
}

func TestRenderManifestModels(t *testing.T) {
	m, remote := dailyManifest(t)
	states := map[provider.Destination]remoteState{remote: remoteRefused}
	got := renderString(m, nil, states, manifestOpts{style: consoleStyle{false, 80}, operation: "models"})
	for _, want := range []string{
		"\u2500\u2500 servers golem models lists and checks \u2500",
		"            in route  chat (fallback), summaries\n",
		"            checks    capabilities, model list\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("models render missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "used for") {
		t.Errorf("models render says used for:\n%s", got)
	}
	for _, op := range []string{"index", "source"} {
		got := renderString(m, nil, states, manifestOpts{style: consoleStyle{false, 80}, operation: op})
		if want := "\u2500\u2500 where golem " + op + " sends model requests \u2500"; !strings.Contains(got, want) {
			t.Errorf("%s subtitle missing %q:\n%s", op, want, got)
		}
	}
}

// At the 40-column floor a subtitle too long for the rule moves to its own
// line under a title-only rule, so golem index and golem source stay
// distinguishable; one wider than the line (the 42-column session subtitle at
// 40) is dropped.
func TestRenderManifestNarrowSubtitle(t *testing.T) {
	m, remote := dailyManifest(t)
	states := map[provider.Destination]remoteState{remote: remoteRefused}
	for _, c := range []struct {
		op    string
		width int
		sub   string // "" when dropped
	}{
		{"index", 40, "where golem index sends model requests"},
		{"source", 40, "where golem source sends model requests"},
		{"", 42, "where this session's model requests can go"},
		{"", 40, ""},
	} {
		got := renderString(m, nil, states, manifestOpts{style: consoleStyle{false, c.width}, operation: c.op})
		want := "\u2500\u2500 model servers " + strings.Repeat("\u2500", c.width-17) + "\n"
		if c.sub != "" {
			want += c.sub + "\n"
		}
		want += "On this machine (allowed automatically)\n"
		if !strings.HasPrefix(got, want) {
			t.Errorf("op %q at %d columns:\n%s\nwant it to start:\n%s", c.op, c.width, got, want)
		}
	}
}

func TestRenderManifestUnknownPurposesDistinct(t *testing.T) {
	local := admDest(t, "llamacpp", "http://localhost:8090")
	m := testManifest(t,
		provider.DestinationEdge{Purpose: "agent", Destination: local},
		provider.DestinationEdge{Purpose: "chat", Destination: local},
	)
	if got := renderString(m, nil, nil, plain80); !strings.Contains(got, "            used for  chat, other: chat\n") {
		t.Errorf("purposes merged:\n%s", got)
	}

	// A primary purpose named "custom (fallback)" is quoted, so the fallback
	// mark appended outside the quotes cannot make it read as a fallback
	// "custom" on the same remote.
	r := admDest(t, "r", "https://r.example.com")
	used := func(e provider.DestinationEdge) string {
		got := renderString(testManifest(t, e), nil, map[provider.Destination]remoteState{r: remoteNeedsOK}, plain80)
		for _, l := range strings.Split(got, "\n") {
			if strings.HasPrefix(l, "     used for  ") {
				return l
			}
		}
		t.Fatalf("no used for row:\n%s", got)
		return ""
	}
	primary := used(provider.DestinationEdge{Purpose: "custom (fallback)", Destination: r})
	fallback := used(provider.DestinationEdge{Purpose: "custom", Destination: r, IsFallback: true})
	if want := `     used for  other: "custom\x20(fallback)"`; primary != want {
		t.Errorf("primary %q: used for = %q, want %q", "custom (fallback)", primary, want)
	}
	if want := "     used for  other: custom (fallback)"; fallback != want {
		t.Errorf("fallback %q: used for = %q, want %q", "custom", fallback, want)
	}
	if primary == fallback {
		t.Errorf("primary %q and fallback %q both read %q", "custom (fallback)", "custom", primary)
	}
}

// A provider name padded with spaces cannot draw a state on a physical line
// of its own: every space in a quoted name shows as \x20, so no logical line,
// and no 80-column row a terminal soft-wraps one into, carries "allowed by
// -allow-destination" except the row that really has that state.
func TestRenderManifestNameCannotForgeState(t *testing.T) {
	label := remoteAllowedByFlag.text()
	pad := strings.Repeat(" ", 60)
	hostile, err := provider.NewDestination("x"+pad+label+pad+"y", "https://hostile.example.com")
	if err != nil {
		t.Fatalf("NewDestination refuses the padded name, so this case proves nothing: %v", err)
	}
	flagged := admDest(t, "flagged", "https://flagged.example.com")
	m := testManifest(t,
		provider.DestinationEdge{Purpose: "agent", Destination: hostile},
		provider.DestinationEdge{Purpose: "agent", Destination: flagged},
	)
	got := renderString(m, nil, map[provider.Destination]remoteState{hostile: remoteNeedsOK, flagged: remoteAllowedByFlag}, plain80) +
		consentQuestion([]provider.Destination{hostile})
	flagRow := fmt.Sprintf("  %-16s  https://flagged.example.com  %s", "flagged", label)
	n := 0
	for _, l := range strings.Split(got, "\n") {
		if l == flagRow {
			n++
			continue
		}
		for _, row := range append([]string{l}, physicalRows(l, 80)...) {
			if strings.Contains(row, label) {
				t.Errorf("%q outside the flagged row: %q\nin:\n%s", label, row, got)
			}
		}
	}
	if n != 1 {
		t.Errorf("the flagged row %q appears %d times, want 1:\n%s", flagRow, n, got)
	}
}

// physicalRows splits one logical line into the rows a terminal width columns
// wide soft-wraps it into.
func physicalRows(l string, width int) []string {
	var rows []string
	var b strings.Builder
	cols := 0
	for _, r := range l {
		if c := runeCols(r); cols+c > width {
			rows = append(rows, b.String())
			b.Reset()
			cols = 0
		}
		b.WriteRune(r)
		cols += runeCols(r)
	}
	return append(rows, b.String())
}

func TestRenderManifestHostileFields(t *testing.T) {
	// NewDestination accepts a hostname carrying U+202E, so the hostile URL
	// arrives through the real constructor.
	d := admDest(t, "alpha and beta", "https://a\u202eb.example.com")
	m := testManifest(t,
		provider.DestinationEdge{Purpose: "agent", Destination: d},
		provider.DestinationEdge{Purpose: "evil\x1b[31m\npurpose", Destination: d},
	)
	got := renderString(m, map[string]string{"alpha and beta": "sends api_key from $A\nB"},
		map[provider.Destination]remoteState{d: remoteNeedsOK}, plain80)
	if strings.Contains(got, "\u202e") || strings.Contains(got, "\x1b") {
		t.Errorf("raw bidi or ESC in output: %q", got)
	}
	// The quoted name (22 columns with its spaces escaped) is wider than the
	// name column, so it sits alone above its URL.
	for _, want := range []string{
		"\n  \"alpha\\x20and\\x20beta\"\n                    https://a\\u202eb.example.com  needs your OK\n",
		"\n                    used for  chat, other: \"evil\\x1b[31m\\npurpose\"\n",
		"\n                    sends api_key from $A\\nB\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// rule, heading, name, URL, uses, key line, then the empty line.
	if n := strings.Count(got, "\n"); n != 7 {
		t.Errorf("rendered %d newlines, want 7:\n%s", n, got)
	}

	// Blank-rendering graphic runes in a host (an ideographic space, a
	// no-break space, a Hangul filler) would draw a fake state on the row.
	// The URL is an identity, so every non-ASCII rune shows as its escape and
	// the only state on screen is a real one: the flagged row's.
	flagged := admDest(t, "flagged", "https://flagged.example.com")
	for _, r := range []rune{'\u3000', '\u00a0', '\u3164'} {
		blank := string(r)
		hostile, err := provider.NewDestination("cloud", "https://evil.example"+blank+blank+"allowed"+blank+"by"+blank+"-allow-destination")
		if err != nil {
			t.Fatalf("NewDestination with %U: %v", r, err)
		}
		got := renderString(testManifest(t,
			provider.DestinationEdge{Purpose: "agent", Destination: hostile},
			provider.DestinationEdge{Purpose: "agent", Destination: flagged},
		), nil, map[provider.Destination]remoteState{hostile: remoteNeedsOK, flagged: remoteAllowedByFlag},
			manifestOpts{style: consoleStyle{false, 120}})
		esc := fmt.Sprintf(`\u%04x`, r)
		header := "\n  cloud    https://evil.example" + esc + esc + "allowed" + esc + "by" + esc + "-allow-destination  needs your OK\n"
		if !strings.Contains(got, header) {
			t.Errorf("%U: header missing %q in:\n%s", r, header, got)
		}
		if strings.ContainsRune(got, r) {
			t.Errorf("%U: raw rune on screen:\n%q", r, got)
		}
		if n := strings.Count(got, "  "+remoteAllowedByFlag.text()); n != 1 ||
			!strings.Contains(got, "\n  flagged  https://flagged.example.com  "+remoteAllowedByFlag.text()+"\n") {
			t.Errorf("%U: %q appears %d times, want once, on the flagged row:\n%s", r, remoteAllowedByFlag.text(), n, got)
		}
	}
}

func TestRenderManifestNarrowAndWide(t *testing.T) {
	local := admDest(t, "llamacpp", "http://localhost:8090")
	remote := admDest(t, "opencode", "https://opencode.ai/zen/go")
	longURL := "https://" + strings.Repeat("w", 108) + ".com"
	wide := admDest(t, "wide", longURL)
	longName := admDest(t, "abcdefghijklmnopqrst", "https://long.example.com")
	var edges []provider.DestinationEdge
	for _, p := range []string{"agent", "planning", "summarize", "embedding", "route", "verify"} {
		edges = append(edges, provider.DestinationEdge{Purpose: p, Destination: local})
	}
	edges = append(edges,
		provider.DestinationEdge{Purpose: "agent", Destination: remote, IsFallback: true},
		provider.DestinationEdge{Purpose: "agent", Destination: wide, IsFallback: true},
		provider.DestinationEdge{Purpose: "agent", Destination: longName, IsFallback: true},
	)
	m := testManifest(t, edges...)
	states := map[provider.Destination]remoteState{remote: remoteNeedsOK, wide: remoteRefused, longName: remoteAllowedByFlag}
	const value = 20 // 2 + nameCol (capped at 16) + 2
	indent := strings.Repeat(" ", value)
	// 60 and 61 straddle the opencode header plus its state (61 columns).
	for _, width := range []int{30, 40, 60, 61, 80, 120} {
		got := renderString(m, nil, states, manifestOpts{style: consoleStyle{false, width}})
		if !strings.Contains(got, "  "+longURL+"\n") {
			t.Errorf("width %d: long URL broken:\n%s", width, got)
		}
		if !strings.Contains(got, "  "+longURL+"\n"+indent+remoteRefused.text()+"\n") {
			t.Errorf("width %d: state of the over-wide header not on its own line:\n%s", width, got)
		}
		head := "  opencode          https://opencode.ai/zen/go"
		inline := head + "  " + remoteNeedsOK.text()
		fits := displayCols(inline) <= width
		if fits && !strings.Contains(got, inline+"\n") || !fits && !strings.Contains(got, head+"\n"+indent+remoteNeedsOK.text()+"\n") {
			t.Errorf("width %d: state placement (fits=%v):\n%s", width, fits, got)
		}
		if !strings.Contains(got, "\n  abcdefghijklmnopqrst\n"+indent+"https://long.example.com") {
			t.Errorf("width %d: long name did not move the URL to its own line:\n%s", width, got)
		}
		// The long name's state follows its URL line, or takes its own line
		// at the value column when the URL line cannot hold it.
		urlLine := indent + "https://long.example.com"
		flagText := remoteAllowedByFlag.text()
		if fits := displayCols(urlLine+"  "+flagText) <= width; fits && !strings.Contains(got, "\n"+urlLine+"  "+flagText+"\n") ||
			!fits && !strings.Contains(got, "\n"+urlLine+"\n"+indent+flagText+"\n") {
			t.Errorf("width %d: long-name state placement (fits=%v):\n%s", width, fits, got)
		}
		lines := strings.Split(got, "\n")
		conts := 0
		for i, l := range lines {
			if !strings.HasPrefix(l, indent+"used for  ") {
				continue
			}
			for _, c := range lines[i+1:] {
				if !strings.HasPrefix(c, indent+"          ") {
					break
				}
				if c[value+10] == ' ' {
					t.Errorf("width %d: continuation not aligned under the first item: %q", width, c)
				}
				conts++
			}
		}
		if width <= 80 && conts == 0 {
			t.Errorf("width %d: uses row never wrapped:\n%s", width, got)
		}
		if width == 120 && conts != 0 {
			t.Errorf("width 120: uses row wrapped:\n%s", got)
		}
	}

	// destinationName always yields ASCII (non-ASCII names are quoted with
	// escapes), so this pins only that such names stay column-aligned; wide
	// text on screen is a URL, covered by TestRenderManifestWideURLStatePlacement.
	cjk := admDest(t, "\u4e2d\u6587", "https://cjk.example.com")
	mark := admDest(t, "e\u0301", "https://mark.example.com")
	m = testManifest(t,
		provider.DestinationEdge{Purpose: "agent", Destination: cjk},
		provider.DestinationEdge{Purpose: "agent", Destination: mark},
	)
	got := renderString(m, nil, map[provider.Destination]remoteState{cjk: remoteRefused, mark: remoteRefused}, plain80)
	want := 2 + displayCols(destinationName(cjk.Provider())) + 2
	for _, url := range []string{"https://cjk.example.com", "https://mark.example.com"} {
		found := false
		for _, l := range strings.Split(got, "\n") {
			if i := strings.Index(l, url); i >= 0 {
				found = true
				if c := displayCols(l[:i]); c != want {
					t.Errorf("%s starts at column %d, want %d:\n%s", url, c, want, got)
				}
			}
		}
		if !found {
			t.Errorf("%s missing:\n%s", url, got)
		}
	}
}

// An IDN host survives canonicalization raw, and the row shows it escaped
// (the URL is an identity). The state placement must measure the escaped
// text, which is what is printed: at the width where the header and its state
// exactly fill the line, the state stays inline; one column narrower it moves
// to its own line. Measuring the raw URL (8 columns of wide runes, against
// 24 escaped) would keep it inline there and overflow.
func TestRenderManifestWideURLStatePlacement(t *testing.T) {
	d := admDest(t, "wide2", "https://\u4f8b\u4f8b\u4f8b\u4f8b.jp")
	m := testManifest(t, provider.DestinationEdge{Purpose: "agent", Destination: d})
	states := map[provider.Destination]remoteState{d: remoteNeedsOK}
	head := `  wide2  https://\u4f8b\u4f8b\u4f8b\u4f8b.jp`
	text := remoteNeedsOK.text()
	cols := len(head) + 2 + len(text)
	if raw := displayCols("  wide2  "+d.BaseURL()) + 2 + len(text); raw >= cols {
		t.Fatalf("fixture does not separate raw (%d) from escaped (%d) columns", raw, cols)
	}
	indent := strings.Repeat(" ", 2+len("wide2")+2)

	got := renderString(m, nil, states, manifestOpts{style: consoleStyle{false, cols}})
	if !strings.Contains(got, "\n"+head+"  "+text+"\n") {
		t.Errorf("width %d: state should stay inline:\n%s", cols, got)
	}
	got = renderString(m, nil, states, manifestOpts{style: consoleStyle{false, cols - 1}})
	if !strings.Contains(got, "\n"+head+"\n"+indent+text+"\n") {
		t.Errorf("width %d: state should move to its own line:\n%s", cols-1, got)
	}
}

// sgrPattern matches one SGR escape sequence.
var sgrPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestRenderManifestColor(t *testing.T) {
	m, remote := dailyManifest(t)
	keys := map[string]string{"opencode": "sends api_key from $OC_GO_API_KEY"}
	for _, st := range []remoteState{remoteNeedsOK, remoteAllowedByFlag, remoteRefused} {
		states := map[provider.Destination]remoteState{remote: st}
		on := renderString(m, keys, states, manifestOpts{style: consoleStyle{true, 80}, cleared: true})
		off := renderString(m, keys, states, manifestOpts{style: consoleStyle{false, 80}, cleared: true})
		if !strings.Contains(on, "\x1b[1;33mRemote\x1b[0m") {
			t.Errorf("Remote heading not yellow bold: %q", on)
		}
		if stripped := sgrPattern.ReplaceAllString(on, ""); stripped != off {
			t.Errorf("state %d: stripped color output differs:\n%q\nwant\n%q", st, stripped, off)
		}
	}
	on := renderString(m, keys, map[provider.Destination]remoteState{remote: remoteNeedsOK}, manifestOpts{style: consoleStyle{true, 80}})
	for _, want := range []string{
		"\x1b[32mOn this machine\x1b[0m \x1b[2m(allowed automatically)\x1b[0m\n",
		"  \x1b[33mneeds your OK\x1b[0m\n",
		"            \x1b[36mused for\x1b[0m  chat\n",
		"            \x1b[36mchecks\x1b[0m    capabilities",
		"            \x1b[2msends api_key from $OC_GO_API_KEY\x1b[0m\n",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("color render missing %q:\n%q", want, on)
		}
	}
	// Asking and not-allowed states are yellow; allowed states are dim.
	for st, sgr := range map[remoteState]string{
		remoteNeedsOK:        sgrYellow,
		remoteRefused:        sgrYellow,
		remoteNotAllowed:     sgrYellow,
		remoteAllowedByFlag:  sgrDim,
		remoteAllowedEarlier: sgrDim,
	} {
		got := renderString(m, keys, map[provider.Destination]remoteState{remote: st}, manifestOpts{style: consoleStyle{true, 80}})
		if want := "\x1b[" + sgr + "m" + st.text() + "\x1b[0m\n"; !strings.Contains(got, want) {
			t.Errorf("state %q: missing %q in:\n%q", st.text(), want, got)
		}
	}
}

// A destination reached only by metadata purposes gets a checks row and no
// uses row.
func TestRenderManifestMetadataOnly(t *testing.T) {
	local := admDest(t, "llamacpp", "http://localhost:8090")
	meta := admDest(t, "meta", "https://meta.example.com")
	m := testManifest(t,
		provider.DestinationEdge{Purpose: "agent", Destination: local},
		provider.DestinationEdge{Purpose: provider.DestinationPurposeModelRefresh, Destination: meta},
		provider.DestinationEdge{Purpose: provider.DestinationPurposeCapabilityProbe, Destination: meta},
	)
	got := renderString(m, nil, map[provider.Destination]remoteState{meta: remoteRefused}, plain80)
	want := "Remote\n" +
		"  meta      https://meta.example.com  not allowed (needs -allow-destination)\n" +
		"            checks    capabilities, model list\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("metadata-only block:\n%s\nwant it to end with:\n%s", got, want)
	}
}

func TestRenderManifestMissingStatePanics(t *testing.T) {
	m, _ := dailyManifest(t)
	defer func() {
		if recover() == nil {
			t.Error("a remote without a state rendered instead of panicking")
		}
	}()
	renderString(m, nil, map[provider.Destination]remoteState{}, plain80)
}

func TestRenderManifestKeySourceLines(t *testing.T) {
	ref := admDest(t, "ref", "https://one.example.com")
	m := testManifest(t,
		provider.DestinationEdge{Purpose: "agent", Destination: admDest(t, "lit", "http://127.0.0.1:8090")},
		provider.DestinationEdge{Purpose: "chat", Destination: admDest(t, "none", "http://127.0.0.1:8091")},
		provider.DestinationEdge{Purpose: "summarize", Destination: ref},
	)
	got := renderString(m, map[string]string{
		"ref": "sends api_key from $CANARY_VAR",
		"lit": "sends a literal api_key",
	}, map[provider.Destination]remoteState{ref: remoteNeedsOK}, plain80)
	want := "\u2500\u2500 model servers \u2500\u2500 where this session's model requests can go " + strings.Repeat("\u2500", 17) + "\n" +
		"On this machine (allowed automatically)\n" +
		"  lit   http://127.0.0.1:8090\n" +
		"        used for  chat\n" +
		"        sends a literal api_key\n" +
		"  none  http://127.0.0.1:8091\n" +
		"        used for  other: chat\n" +
		"Remote\n" +
		"  ref   https://one.example.com  needs your OK\n" +
		"        used for  summaries\n" +
		"        sends api_key from $CANARY_VAR\n" +
		"\n"
	if got != want {
		t.Errorf("rendered block:\n%s\nwant:\n%s", got, want)
	}
}
