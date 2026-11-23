package main

import (
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
	if label, _ := purposeLabel("x\x1b"); strings.Contains(label, "\x1b") {
		t.Errorf("label carries ESC: %q", label)
	}
}

func TestDestinationName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"opencode", "opencode"},
		{"llama.cpp-2", "llama.cpp-2"},
		{"alpha and beta", `"alpha and beta"`},
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
	baseline := provider.NewDestinationPolicy(granted)
	for _, c := range []struct {
		mode  renderMode
		fresh remoteState
	}{{renderAsking, remoteNeedsOK}, {renderRefusing, remoteRefused}, {renderListing, remoteNotAllowed}} {
		got := remoteStates(m, flags, baseline, c.mode)
		if _, ok := got[lo]; ok {
			t.Errorf("mode %d: local destination present", c.mode)
		}
		if got[flagged] != remoteAllowedByFlag || got[granted] != remoteAllowedEarlier || got[fresh] != c.fresh {
			t.Errorf("mode %d: got %v", c.mode, got)
		}
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
	got := uncoveredAll([]provider.Destination{c, a, b}, provider.NewDestinationPolicy(a))
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
		{q("alpha and beta", "gamma"), `"alpha and beta" and gamma`},
		{q("beta", "alpha"), "alpha and beta"},
	} {
		if want := pre + c.names + post; c.got != want {
			t.Errorf("got %q, want %q", c.got, want)
		}
	}
	if q("alpha and beta", "gamma") == q("alpha", "beta") {
		t.Error("ambiguous names produce identical questions")
	}
	if strings.Contains(q("a"), "\x1b") {
		t.Error("question carries ESC")
	}
}
