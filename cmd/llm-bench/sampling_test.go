package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseCaptureSample(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		want    captureSampleSpec
		wantErr string
	}{
		{
			name: "n-only",
			spec: "n=20",
			want: captureSampleSpec{N: 20},
		},
		{
			name: "stratify-and-filters",
			spec: "n=50,stratify=token-length:turn-count,recency=last-30d,has-tool-calls=true",
			want: captureSampleSpec{
				N:            50,
				Stratify:     []string{"token-length", "turn-count"},
				Recency:      "last-30d",
				HasToolCalls: trueBool(),
			},
		},
		{
			name:    "empty-rejected",
			spec:    "",
			wantErr: "empty",
		},
		{
			name:    "unknown-key",
			spec:    "n=10,wat=1",
			wantErr: "unknown key wat",
		},
		{
			name:    "bad-n",
			spec:    "n=zero",
			wantErr: "n=",
		},
		{
			name:    "bad-stratify-dimension",
			spec:    "n=10,stratify=wat",
			wantErr: "unknown stratify dimension wat",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCaptureSample(tc.spec)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v; want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v; want %+v", got, tc.want)
			}
		})
	}
}

func trueBool() *bool {
	b := true
	return &b
}

func TestTokenLengthBucket(t *testing.T) {
	cases := []struct {
		runes int
		want  string
	}{
		{0, "small"},
		{3500, "small"},
		{4000, "small"}, // ~1k tokens at 4 bytes/token
		{4001, "medium"},
		{32000, "medium"},
		{32001, "large"},
	}
	for _, tc := range cases {
		if got := tokenLengthBucket(tc.runes); got != tc.want {
			t.Errorf("tokenLengthBucket(%d)=%q; want %q", tc.runes, got, tc.want)
		}
	}
}

func TestTurnCountBucket(t *testing.T) {
	if got := turnCountBucket(2); got != "short" {
		t.Errorf("turnCountBucket(2)=%q; want short", got)
	}
	if got := turnCountBucket(5); got != "medium" {
		t.Errorf("turnCountBucket(5)=%q; want medium", got)
	}
	if got := turnCountBucket(20); got != "long" {
		t.Errorf("turnCountBucket(20)=%q; want long", got)
	}
}

func TestRecencyBucket(t *testing.T) {
	now := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		captured time.Time
		want     string
	}{
		{now.Add(-3 * 24 * time.Hour), "last-7d"},
		{now.Add(-15 * 24 * time.Hour), "last-30d"},
		{now.Add(-60 * 24 * time.Hour), "older"},
	}
	for _, tc := range cases {
		if got := recencyBucket(tc.captured, now); got != tc.want {
			t.Errorf("recencyBucket=%q; want %q", got, tc.want)
		}
	}
}

func TestStratifiedSampleDeterministicWithSeed(t *testing.T) {
	traces := makeStratificationFixture(60)
	spec, _ := parseCaptureSample("n=20,stratify=turn-count")
	now := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)

	got1, manifest1, _ := stratifiedSample(traces, spec, 42, now)
	got2, manifest2, _ := stratifiedSample(traces, spec, 42, now)
	if !reflect.DeepEqual(got1, got2) {
		t.Fatalf("seed=42 produced different selections")
	}
	if !reflect.DeepEqual(manifest1.SampledIDs, manifest2.SampledIDs) {
		t.Fatalf("manifest IDs differ across runs with same seed")
	}
	if manifest1.Seed != 42 {
		t.Fatalf("manifest.Seed=%d; want 42", manifest1.Seed)
	}
}

func TestStratifiedSampleRedistributesSlack(t *testing.T) {
	// Cells: short=2, medium=50. n=20, 2 cells → 10 each, short cell
	// only has 2 → slack of 8 must go to medium, yielding 2+18=20 total.
	traces := makeTwoCellFixture(2, 50)
	spec, _ := parseCaptureSample("n=20,stratify=turn-count")
	now := time.Now()
	got, manifest, _ := stratifiedSample(traces, spec, 1, now)
	if len(got) != 20 {
		t.Fatalf("got %d sampled; want 20 (slack redistribution)", len(got))
	}
	if manifest.CellCounts["turn-count=short"] != 2 {
		t.Fatalf("short cell sampled %d; want 2 (all)", manifest.CellCounts["turn-count=short"])
	}
	if manifest.CellCounts["turn-count=medium"] != 18 {
		t.Fatalf("medium cell sampled %d; want 18 (10 + 8 slack)", manifest.CellCounts["turn-count=medium"])
	}
}

func TestStratifiedSampleRespectsHardFilters(t *testing.T) {
	// Half the traces have a final-answer-criteria; spec filters for them.
	traces := makeHalfWithCriteria(40)
	spec, _ := parseCaptureSample("n=10,stratify=turn-count,has-final-answer-criteria=true")
	got, _, _ := stratifiedSample(traces, spec, 7, time.Now())
	for _, tr := range got {
		if strings.TrimSpace(tr.Golden.FinalAnswerCriteria) == "" {
			t.Fatalf("sampled trace %q has no final-answer-criteria but filter required it", tr.ID)
		}
	}
}

func TestStratifiedSampleDoesNotOversampleWhenMoreCellsThanN(t *testing.T) {
	traces := makeStratificationFixture(3) // short, medium, long cells
	spec, _ := parseCaptureSample("n=2,stratify=turn-count")
	got, manifest, _ := stratifiedSample(traces, spec, 9, time.Now())
	if len(got) != 2 {
		t.Fatalf("got %d sampled; want exactly 2 when cells > n", len(got))
	}
	var counted int
	for _, n := range manifest.CellCounts {
		counted += n
	}
	if counted != 2 {
		t.Fatalf("manifest counted %d sampled traces; want exactly 2", counted)
	}
}

// makeStratificationFixture returns N traces with rotating turn-counts
// (1, 5, 12) so the turn-count bucket cycles through short/medium/long.
func makeStratificationFixture(n int) []Trace {
	out := make([]Trace, n)
	for i := range out {
		turns := []int{1, 5, 12}[i%3]
		out[i] = Trace{
			ID:    fmt.Sprintf("trace-%03d", i),
			Turns: makeTurns(turns),
		}
	}
	return out
}

// makeTwoCellFixture returns short turn-count traces followed by
// medium turn-count traces, in that order.
func makeTwoCellFixture(short, medium int) []Trace {
	out := make([]Trace, 0, short+medium)
	for i := 0; i < short; i++ {
		out = append(out, Trace{ID: fmt.Sprintf("s-%03d", i), Turns: makeTurns(1)})
	}
	for i := 0; i < medium; i++ {
		out = append(out, Trace{ID: fmt.Sprintf("m-%03d", i), Turns: makeTurns(5)})
	}
	return out
}

// makeHalfWithCriteria returns N traces, the first half with a
// non-empty FinalAnswerCriteria, the second half empty.
func makeHalfWithCriteria(n int) []Trace {
	out := make([]Trace, n)
	for i := range out {
		out[i] = Trace{ID: fmt.Sprintf("c-%03d", i), Turns: makeTurns(5)}
		if i < n/2 {
			out[i].Golden = Golden{FinalAnswerCriteria: "non-empty"}
		}
	}
	return out
}

func makeTurns(n int) []Turn {
	out := make([]Turn, n)
	for i := range out {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out[i] = Turn{Role: role, Content: "hi"}
	}
	return out
}
