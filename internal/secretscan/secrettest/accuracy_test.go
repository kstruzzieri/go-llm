package secrettest

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

// literalCase builds a case whose rendered value is exactly value, so a test
// pins the scanner's behavior on a value it composed from the generators.
func literalCase(id string, class Class, kind string, value string, carriers ...Carrier) Case {
	return Case{
		ID: id, Class: class, Kind: kind, Carriers: carriers, Template: "{{VALUE}}",
		Generator: "literal", Params: map[string]string{"text": value},
	}
}

// openaiCase renders sk- plus 40 alnum bytes inside prose.
func openaiCase(id string, carriers ...Carrier) Case {
	return Case{
		ID: id, Class: ExpectedSensitive, Kind: "openai_token", Carriers: carriers,
		Template: "found {{VALUE}} in the logs\n", Generator: "provider",
		Params: map[string]string{"prefix_a": "sk", "prefix_b": "-", "len": "40", "charset": "alnum"},
	}
}

func evaluate(t *testing.T, cases ...Case) []Outcome {
	t.Helper()
	outs, err := Evaluate(context.Background(), cases)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return outs
}

func TestEvaluateCarriers(t *testing.T) {
	t.Parallel()
	type cell struct {
		carrier Carrier
		hook    string
	}
	observation := cell{Observation, "input"}
	output := cell{ToolArgs, "output"}
	toolCall := cell{ToolArgs, "tool_call"}
	for _, tt := range []struct {
		name     string
		carriers []Carrier
		want     []cell
	}{
		{"both carriers", []Carrier{Observation, ToolArgs}, []cell{observation, output, toolCall}},
		{"declared order does not reorder", []Carrier{ToolArgs, Observation}, []cell{observation, output, toolCall}},
		{"observation only", []Carrier{Observation}, []cell{observation}},
		{"tool-args only", []Carrier{ToolArgs}, []cell{output, toolCall}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			outs := evaluate(t, openaiCase("unit-openai", tt.carriers...))
			if len(outs) != len(tt.want) {
				t.Fatalf("got %d outcomes, want %d: %+v", len(outs), len(tt.want), outs)
			}
			for i, o := range outs {
				if got := (cell{o.Carrier, o.Hook}); got != tt.want[i] {
					t.Errorf("outcome %d = %v, want %v", i, got, tt.want[i])
				}
				if o.CaseID != "unit-openai" || o.Class != ExpectedSensitive || o.Kind != "openai_token" || o.CoverageGap {
					t.Errorf("outcome %d does not echo the case: %+v", i, o)
				}
				if !o.Any || !slices.Equal(o.Kinds, []string{"openai_token"}) {
					t.Errorf("outcome %d (%s): Any=%v Kinds=%v, want true [openai_token]", i, o.Hook, o.Any, o.Kinds)
				}
			}
		})
	}

	// Prose with no secret fires nowhere, so Any is false and Kinds stays empty.
	benign := Case{
		ID: "unit-benign", Class: Benign, Carriers: []Carrier{Observation, ToolArgs}, Template: "{{VALUE}}",
		Generator: "literal", Params: map[string]string{"text": "the build finished in 12 seconds"},
	}
	for _, o := range evaluate(t, benign) {
		if o.Any || len(o.Kinds) != 0 {
			t.Errorf("benign %s/%s: Any=%v Kinds=%v, want false and none", o.Carrier, o.Hook, o.Any, o.Kinds)
		}
	}
}

// TestToolArgsEncoding: the carrier rewrites only what JSON requires. HTML
// characters stay literal, because escaped forms are cases of their own.
func TestToolArgsEncoding(t *testing.T) {
	t.Parallel()
	got, err := toolArgs("a<b>&\"c\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"content":"a<b>&\"c\"\n"}`; string(got) != want {
		t.Errorf("toolArgs = %s, want %s", got, want)
	}
}

func TestEvaluateStopsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outs, err := Evaluate(ctx, []Case{openaiCase("unit-cancel", Observation)})
	if !errors.Is(err, context.Canceled) || outs != nil {
		t.Errorf("Evaluate(canceled) = %v, %v; want nil, context.Canceled", outs, err)
	}
}

// TestEvaluateToolArgsFieldIsNeutral: a high-entropy value that no rule names
// must not fire just because of the JSON field that carries it. A field named
// like a secret key (token, password, ...) would turn every such value into a
// false positive on the tool-args hooks.
func TestEvaluateToolArgsFieldIsNeutral(t *testing.T) {
	t.Parallel()
	c := Case{
		ID: "unit-neutral", Class: Benign, Carriers: []Carrier{Observation, ToolArgs}, Template: "{{VALUE}}",
		Generator: "provider", Params: map[string]string{"len": "40", "charset": "alnum"},
	}
	value, err := c.Render()
	if err != nil || len(value) != 40 {
		t.Fatalf("Render = %q, %v; want 40 bytes", value, err)
	}
	outs := evaluate(t, c)
	if len(outs) != 3 {
		t.Fatalf("got %d outcomes, want 3", len(outs))
	}
	for _, o := range outs {
		if o.Any {
			t.Errorf("%s/%s: a bare high-entropy value fired %v", o.Carrier, o.Hook, o.Kinds)
		}
	}
}

// TestAnyVersusKindAttribution: "any finding" is separate from kind
// attribution because Scan merges overlapping spans by precedence (private key
// over provider token) and keeps source order for disjoint ones.
func TestAnyVersusKindAttribution(t *testing.T) {
	t.Parallel()
	pem, err := Generate("pem", map[string]string{"label": "PRIVATE KEY", "complete": "true", "lines": "2", "seed": "attr-pem"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := Generate("provider", map[string]string{"prefix_a": "sk", "prefix_b": "-", "len": "40", "charset": "alnum", "seed": "attr-token"})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(pem, "\n") // BEGIN, two body lines, END
	if len(lines) != 4 {
		t.Fatalf("PEM has %d lines, want 4", len(lines))
	}
	for _, tt := range []struct {
		name  string
		value string
		want  []string
	}{
		{"token inside the envelope is absorbed", strings.Join(slices.Insert(slices.Clone(lines), 2, token), "\n"), []string{"private_key"}},
		{"token after the envelope", pem + "\n" + token + "\n", []string{"private_key", "openai_token"}},
		{"token before the envelope", token + "\n" + pem, []string{"openai_token", "private_key"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			outs := evaluate(t, literalCase("attr", ExpectedSensitive, "private_key", tt.value, Observation, ToolArgs))
			if len(outs) != 3 {
				t.Fatalf("got %d outcomes, want 3", len(outs))
			}
			for _, o := range outs {
				if !o.Any {
					t.Errorf("%s/%s: Any = false, want true", o.Carrier, o.Hook)
				}
				if !slices.Equal(o.Kinds, tt.want) {
					t.Errorf("%s/%s: Kinds = %v, want %v", o.Carrier, o.Hook, o.Kinds, tt.want)
				}
			}
		})
	}
}

// TestMandatoryCasesDetected is spec §5.6 item 1. The table holds 55
// mandatory cases, and all 55 fire today on every carrier and hook they
// declare. validate lets a mandatory case predict a miss (the evidence report
// then records item 1 as FAIL), so a scanner change that stops detecting one
// turns this red until wantDetected is lowered on purpose.
func TestMandatoryCasesDetected(t *testing.T) {
	t.Parallel()
	const wantMandatory, wantDetected = 55, 55
	cases, err := Cases()
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	outs := evaluate(t, cases...)
	mandatory := map[string][]Carrier{}
	for _, c := range cases {
		if c.Mandatory {
			mandatory[c.ID] = c.Carriers
		}
	}
	if len(mandatory) != wantMandatory {
		t.Errorf("the table holds %d mandatory cases, want %d", len(mandatory), wantMandatory)
	}
	hooks := map[string][]string{}
	missed := map[string]bool{}
	for _, o := range outs {
		carriers, ok := mandatory[o.CaseID]
		if !ok {
			continue
		}
		if !slices.Contains(carriers, o.Carrier) {
			t.Errorf("case %s: outcome on undeclared carrier %s", o.CaseID, o.Carrier)
		}
		hooks[o.CaseID] = append(hooks[o.CaseID], o.Hook)
		if !o.Any {
			missed[o.CaseID] = true
		}
	}
	if detected := len(mandatory) - len(missed); detected != wantDetected {
		t.Errorf("%d mandatory cases fire on every carrier and hook, want %d; missed: %v", detected, wantDetected, slices.Sorted(maps.Keys(missed)))
	}
	for id, carriers := range mandatory {
		var want []string
		if slices.Contains(carriers, Observation) {
			want = append(want, "input")
		}
		if slices.Contains(carriers, ToolArgs) {
			want = append(want, "output", "tool_call")
		}
		if !slices.Equal(hooks[id], want) {
			t.Errorf("case %s: hooks = %v, want %v", id, hooks[id], want)
		}
	}
}

func TestConfusionExcludesUnclassified(t *testing.T) {
	t.Parallel()
	var unclassified []Outcome
	for _, fired := range []bool{true, false} {
		for _, h := range []struct {
			carrier Carrier
			hook    string
		}{{Observation, "input"}, {ToolArgs, "output"}, {ToolArgs, "tool_call"}} {
			unclassified = append(unclassified, Outcome{CaseID: "u", Class: Unclassified, Kind: "openai_token", Carrier: h.carrier, Hook: h.hook, Any: fired})
		}
	}
	if rows := Confusions(unclassified); len(rows) != 0 {
		t.Errorf("unclassified outcomes produced rows: %+v", rows)
	}

	// Beside a counted outcome they still add nothing to its row.
	tp := Outcome{CaseID: "tp", Class: ExpectedSensitive, Kind: "openai_token", Carrier: Observation, Hook: "input", Any: true}
	got := Confusions(append(slices.Clone(unclassified), tp))
	want := []Confusion{{Kind: "openai_token", Carrier: Observation, Hook: "input", TP: 1}}
	if !slices.Equal(got, want) {
		t.Errorf("Confusions = %+v, want %+v", got, want)
	}
}

func TestConfusions(t *testing.T) {
	t.Parallel()
	o := func(id string, class Class, kind string, carrier Carrier, hook string, fired bool) Outcome {
		return Outcome{CaseID: id, Class: class, Kind: kind, Carrier: carrier, Hook: hook, Any: fired}
	}
	gap := o("gap", ExpectedSensitive, "", Observation, "input", false)
	gap.CoverageGap = true
	outs := []Outcome{
		o("tp", ExpectedSensitive, "openai_token", Observation, "input", true),
		o("fn", ExpectedSensitive, "openai_token", Observation, "input", false),
		o("fp", Benign, "openai_token", Observation, "input", true),
		o("tn", Benign, "openai_token", Observation, "input", false),
		// Outside the matrix: lookalikes, fixtures, a coverage gap.
		o("look", BenignLookalike, "openai_token", Observation, "input", true),
		o("fix", SyntheticFixture, "openai_token", Observation, "input", true),
		gap,
		// Tool-args splits by hook; rows sort by kind, carrier, hook.
		o("out", ExpectedSensitive, "openai_token", ToolArgs, "output", true),
		o("call", ExpectedSensitive, "openai_token", ToolArgs, "tool_call", false),
		o("call2", ExpectedSensitive, "openai_token", ToolArgs, "tool_call", false),
		o("bear", Benign, "bearer_token", ToolArgs, "tool_call", false),
		o("none", Benign, "", Observation, "input", false),
	}
	want := []Confusion{
		{Kind: "", Carrier: Observation, Hook: "input", TN: 1},
		{Kind: "bearer_token", Carrier: ToolArgs, Hook: "tool_call", TN: 1},
		{Kind: "openai_token", Carrier: Observation, Hook: "input", TP: 1, FN: 1, FP: 1, TN: 1},
		{Kind: "openai_token", Carrier: ToolArgs, Hook: "output", TP: 1},
		{Kind: "openai_token", Carrier: ToolArgs, Hook: "tool_call", FN: 2},
	}
	if got := Confusions(outs); !slices.Equal(got, want) {
		t.Errorf("Confusions =\n%+v\nwant\n%+v", got, want)
	}
}

func TestTables(t *testing.T) {
	t.Parallel()
	outs := []Outcome{
		{CaseID: "s", Class: ExpectedSensitive},
		{CaseID: "l1", Class: BenignLookalike},
		{CaseID: "f1", Class: SyntheticFixture, CoverageGap: true},
		{CaseID: "b", Class: Benign},
		{CaseID: "u", Class: Unclassified},
		{CaseID: "l2", Class: BenignLookalike},
		{CaseID: "f2", Class: SyntheticFixture},
	}
	ids := func(os []Outcome) []string {
		var out []string
		for _, o := range os {
			out = append(out, o.CaseID)
		}
		return out
	}
	lookalike, fixture := Tables(outs)
	if got, want := ids(lookalike), []string{"l1", "l2"}; !slices.Equal(got, want) {
		t.Errorf("lookalike = %v, want %v", got, want)
	}
	if got, want := ids(fixture), []string{"f1", "f2"}; !slices.Equal(got, want) {
		t.Errorf("fixture = %v, want %v", got, want)
	}
}

// TestOutcomeScoreFollowsCaseScore pins that an Outcome scores exactly as the
// Case it came from does, including the coverage-gap exclusion (R6).
func TestOutcomeScoreFollowsCaseScore(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		class Class
		gap   bool
		fired bool
		want  string
	}{
		{ExpectedSensitive, false, true, "TP"},
		{ExpectedSensitive, false, false, "FN"},
		{ExpectedSensitive, true, false, ""},
		{Benign, false, true, "FP"},
		{Benign, false, false, "TN"},
		{BenignLookalike, false, true, ""},
		{SyntheticFixture, false, false, ""},
		{Unclassified, false, true, ""},
	} {
		o := Outcome{Class: tt.class, CoverageGap: tt.gap, Any: tt.fired}
		if got := o.Score(); got != tt.want {
			t.Errorf("Outcome{%s gap=%v fired=%v}.Score() = %q, want %q", tt.class, tt.gap, tt.fired, got, tt.want)
		}
	}
}

// TestEvaluateCarriesCoverageGap: a coverage gap stays outside the matrix end to
// end. Evaluate must copy the flag onto every hook's Outcome; otherwise a gap
// that fires nothing scores FN (R6).
func TestEvaluateCarriesCoverageGap(t *testing.T) {
	t.Parallel()
	// Same non-firing value; only the flag differs.
	gap := literalCase("unit-gap", ExpectedSensitive, "", "the build finished", Observation, ToolArgs)
	gap.CoverageGap = true
	counted := literalCase("unit-counted", ExpectedSensitive, "openai_token", "the build finished", Observation, ToolArgs)
	outs := evaluate(t, gap, counted)
	if len(outs) != 6 {
		t.Fatalf("got %d outcomes, want 6", len(outs))
	}
	for _, o := range outs[:3] {
		if !o.CoverageGap || o.Score() != "" {
			t.Errorf("gap %s/%s: CoverageGap=%v Score=%q, want true and empty", o.Carrier, o.Hook, o.CoverageGap, o.Score())
		}
	}
	for _, o := range outs[3:] {
		if o.CoverageGap || o.Score() != "FN" {
			t.Errorf("counted %s/%s: CoverageGap=%v Score=%q, want false and FN", o.Carrier, o.Hook, o.CoverageGap, o.Score())
		}
	}
	if rows := Confusions(outs[:3]); len(rows) != 0 {
		t.Errorf("gap outcomes produced rows: %+v", rows)
	}
	want := []Confusion{
		{Kind: "openai_token", Carrier: Observation, Hook: "input", FN: 1},
		{Kind: "openai_token", Carrier: ToolArgs, Hook: "output", FN: 1},
		{Kind: "openai_token", Carrier: ToolArgs, Hook: "tool_call", FN: 1},
	}
	if got := Confusions(outs); !slices.Equal(got, want) {
		t.Errorf("Confusions = %+v, want %+v", got, want)
	}

	// The committed gap rows, which fire nothing the matrix could count.
	cases, err := Cases()
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	gaps := map[string]bool{}
	for _, c := range cases {
		if c.CoverageGap {
			gaps[c.ID] = true
		}
	}
	if len(gaps) == 0 {
		t.Fatal("the table holds no coverage-gap case")
	}
	var gapOuts []Outcome
	for _, o := range evaluate(t, cases...) {
		if gaps[o.CaseID] {
			gapOuts = append(gapOuts, o)
			if !o.CoverageGap {
				t.Errorf("case %s %s/%s: CoverageGap not carried", o.CaseID, o.Carrier, o.Hook)
			}
		}
	}
	if len(gapOuts) < len(gaps) {
		t.Fatalf("%d gap outcomes for %d gap cases", len(gapOuts), len(gaps))
	}
	if rows := Confusions(gapOuts); len(rows) != 0 {
		t.Errorf("committed gap outcomes produced rows: %+v", rows)
	}
}

func TestAttribution(t *testing.T) {
	t.Parallel()
	o := func(id string, class Class, kind string, carrier Carrier, hook string, kinds ...string) Outcome {
		return Outcome{CaseID: id, Class: class, Kind: kind, Carrier: carrier, Hook: hook, Any: len(kinds) > 0, Kinds: kinds}
	}
	gap := o("gap", ExpectedSensitive, "github_token", Observation, "input", "github_token")
	gap.CoverageGap = true
	outs := []Outcome{
		// One kind's row: fired under its own kind, under another, and among two.
		o("own", ExpectedSensitive, "github_token", Observation, "input", "github_token"),
		o("other", ExpectedSensitive, "github_token", Observation, "input", "secret_assignment"),
		o("among", ExpectedSensitive, "github_token", Observation, "input", "private_key", "github_token"),
		o("silent", ExpectedSensitive, "github_token", Observation, "input"),
		// Outside the count even though they fire under the row's kind.
		gap,
		o("benign", Benign, "github_token", Observation, "input", "github_token"),
		o("look", BenignLookalike, "github_token", Observation, "input", "github_token"),
		o("fix", SyntheticFixture, "github_token", Observation, "input", "github_token"),
		o("uncl", Unclassified, "github_token", Observation, "input", "github_token"),
		// Rows split by carrier and hook and sort by kind, carrier, hook.
		o("call", ExpectedSensitive, "openai_token", ToolArgs, "tool_call", "openai_token"),
		o("emit", ExpectedSensitive, "openai_token", ToolArgs, "output", "openai_token"),
		o("seen", ExpectedSensitive, "openai_token", Observation, "input", "secret_assignment"),
		o("out", ExpectedSensitive, "bearer_token", ToolArgs, "output", "secret_assignment"),
		// A kind that never fires has no row.
		o("quiet", ExpectedSensitive, "npm_token", Observation, "input"),
	}
	want := []AttributionRow{
		{Kind: "bearer_token", Carrier: ToolArgs, Hook: "output", Fired: 1, Attributed: 0},
		{Kind: "github_token", Carrier: Observation, Hook: "input", Fired: 3, Attributed: 2},
		{Kind: "openai_token", Carrier: Observation, Hook: "input", Fired: 1, Attributed: 0},
		{Kind: "openai_token", Carrier: ToolArgs, Hook: "output", Fired: 1, Attributed: 1},
		{Kind: "openai_token", Carrier: ToolArgs, Hook: "tool_call", Fired: 1, Attributed: 1},
	}
	if got := Attribution(outs); !slices.Equal(got, want) {
		t.Errorf("Attribution =\n%+v\nwant\n%+v", got, want)
	}
}

// TestAttributionOnTable pins the committed table: every expected-sensitive
// case that fires does so under its own kind, on every carrier and hook. The
// two literals are the TP counts of the private_key and secret_assignment
// rows.
func TestAttributionOnTable(t *testing.T) {
	t.Parallel()
	cases, err := Cases()
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	rows := Attribution(evaluate(t, cases...))
	if len(rows) == 0 {
		t.Fatal("no attribution rows")
	}
	for _, r := range rows {
		if r.Attributed != r.Fired {
			t.Errorf("%s %s/%s: %d of %d fired outcomes attributed to the case's kind", r.Kind, r.Carrier, r.Hook, r.Attributed, r.Fired)
		}
	}
	for _, want := range []AttributionRow{
		{Kind: "private_key", Carrier: Observation, Hook: "input", Fired: 8, Attributed: 8},
		{Kind: "secret_assignment", Carrier: ToolArgs, Hook: "tool_call", Fired: 18, Attributed: 18},
	} {
		if !slices.Contains(rows, want) {
			t.Errorf("rows lack %+v", want)
		}
	}
}
