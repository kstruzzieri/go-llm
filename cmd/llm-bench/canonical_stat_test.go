package main

// Cross-arch float canonicalization (#331 slice 3c, external PR review round
// 2 P1): darwin/arm64 FMA contraction produced last-ULP differences from
// linux/amd64 in the report's derived floats (delta_ci_low, LOGO band), so
// the committed-report byte-identity gate failed on the linux/amd64 required
// check. Every derived float statistic the assembly report emits is rounded
// to 12 decimal places (canonicalStat) before marshal, and the committed
// report.json pins the canonical bytes.

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
)

func TestCanonicalStat(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		// The three cross-arch divergences the review reproduced: both sides'
		// raw values collapse to one canonical form.
		{"ci low darwin", 0.17857142857142863, 0.178571428571},
		{"ci low linux", 0.17857142857142860, 0.178571428571},
		{"logo min darwin", 0.1544117647058824, 0.154411764706},
		{"logo min linux", 0.15441176470588236, 0.154411764706},
		{"logo max darwin", 0.2058823529411765, 0.205882352941},
		{"logo max linux", 0.20588235294117646, 0.205882352941},
		{"exact small values pass through", 0.5, 0.5},
		{"exact negative", -1, -1},
		{"zero", 0, 0},
		{"negative underflow never emits -0", -1e-13, 0},
		{"sign preserved", -0.10714285714285714, -0.107142857143},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canonicalStat(tc.in)
			if got != tc.want {
				t.Fatalf("canonicalStat(%v) = %v; want %v", tc.in, got, tc.want)
			}
			if math.Signbit(got) && got == 0 {
				t.Fatalf("canonicalStat(%v) emitted -0", tc.in)
			}
			if again := canonicalStat(got); again != got {
				t.Fatalf("canonicalStat not idempotent: %v -> %v -> %v", tc.in, got, again)
			}
		})
	}
}

// TestAssemblyReportDecisionUsesUnroundedTokenReduction proves display
// rounding cannot promote a sub-threshold raw median into a verdict.
func TestAssemblyReportDecisionUsesUnroundedTokenReduction(t *testing.T) {
	const flatTokens = 10_000_000_000_000
	const progressiveTokens = 8_000_000_000_001

	var arts []Artifact
	var labels []Label
	for i := 0; i < assemblyMinimumPairsPerModel; i++ {
		pair := fmt.Sprintf("rounding-%03d", i)
		flat := assemblyArtifact(pair, AssemblyFlat, "m", flatTokens, []string{"c"})
		progressive := assemblyArtifact(pair, AssemblyProgressive, "m", progressiveTokens, []string{"c"})
		arts = append(arts, flat, progressive)
		labels = append(labels, labelFor(flat, 0.5), labelFor(progressive, 0.5))
	}

	rep, err := computeAssemblyReport(arts, labels, 1, 100, nil, assemblyReportExtras{})
	if err != nil {
		t.Fatal(err)
	}
	model := rep.Models[0]
	if model.MedianTokenReduction != 0.2 {
		t.Fatalf("displayed median token reduction = %v, want 0.2", model.MedianTokenReduction)
	}
	if model.Decision != "inconclusive" {
		t.Fatalf("decision = %q, want inconclusive for raw reduction below 0.2", model.Decision)
	}
}

// TestAssemblyMixedDecisionUsesUnroundedCI proves the mixed report likewise
// keeps raw CI bounds for its decision while serializing canonical values.
func TestAssemblyMixedDecisionUsesUnroundedCI(t *testing.T) {
	arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, 0.5, 0.5000000000004, nil)
	rep := mustMixedReport(t, arts, labels)
	model := rep.LegacyMixedModels[0]
	if model.DeltaCILow != 0 {
		t.Fatalf("displayed mixed CI low = %v, want 0", model.DeltaCILow)
	}
	if model.Decision != "quality-improved" {
		t.Fatalf("mixed decision = %q, want quality-improved for raw positive CI", model.Decision)
	}
}

// TestAssemblyReportEmitsCanonicalFloats builds a report whose raw derived
// statistics are non-terminating decimals (means of 3, one-third token
// reduction) and asserts every emitted float is its own canonical form —
// dropping any canonicalStat call at a stat assignment site turns the
// corresponding field back into a 16-digit raw float and fails the walk.
func TestAssemblyReportEmitsCanonicalFloats(t *testing.T) {
	// Discriminating-fixture sanity: the raw stats this corpus produces MUST
	// differ from their canonical forms, or the walk below proves nothing.
	if raw := 2.0 / 3; canonicalStat(raw) == raw {
		t.Fatalf("fixture is blind: 2/3 is already canonical")
	}

	var arts []Artifact
	var labels []Label
	// 3a flat-progressive: deltas 1, 1, 0 (mean 2/3); token reduction 2/3.
	ids := []string{"c1", "c2"}
	for i, d := range []float64{1, 1, 0} {
		pair := fmt.Sprintf("fp-%d", i)
		f := assemblyArtifact(pair, AssemblyFlat, "m", 3000, ids)
		p := assemblyArtifact(pair, AssemblyProgressive, "m", 1000, ids)
		arts = append(arts, f, p)
		labels = append(labels, labelFor(f, 0), labelFor(p, d))
	}
	// Legacy-mixed: deltas 1, 1, 0 (pooled mean 2/3, degenerate CI 2/3) plus
	// one control pair.
	arts, labels = appendMixedPairs(arts, labels, "m", 0, 2, 0, 1, nil)
	arts, labels = appendMixedPairs(arts, labels, "m", 2, 1, 1, 1, nil)
	arts, labels = appendMixedPairs(arts, labels, "m", 3, 1, 0.5, 1, func(_ int, ae *AssemblyEval) {
		ae.Control = true
	})
	// Topline ceiling: labeled qualities 1, 1, 0 in one stratum (mean 2/3).
	for i, q := range []float64{1, 1, 0} {
		top := mixedArtifact(fmt.Sprintf("top-%d", i), AssemblyTopline, "m", nil)
		arts = append(arts, top)
		labels = append(labels, labelFor(top, q))
	}

	rep := mustMixedReport(t, arts, labels)
	if len(rep.Models) != 1 || len(rep.LegacyMixedModels) != 1 || len(rep.Topline) != 1 {
		t.Fatalf("fixture sections = %d/%d/%d; want 1/1/1",
			len(rep.Models), len(rep.LegacyMixedModels), len(rep.Topline))
	}
	// The stats genuinely landed in non-canonical territory before rounding.
	want := canonicalStat(2.0 / 3)
	if rep.Models[0].MeanDelta != want || rep.Models[0].MedianTokenReduction != want {
		t.Fatalf("3a mean/median = %v/%v; want the canonical 2/3 %v",
			rep.Models[0].MeanDelta, rep.Models[0].MedianTokenReduction, want)
	}
	if rep.LegacyMixedModels[0].MeanDelta != want || rep.LegacyMixedModels[0].DeltaCILow != want {
		t.Fatalf("mixed mean/ci-low = %v/%v; want the canonical 2/3 %v",
			rep.LegacyMixedModels[0].MeanDelta, rep.LegacyMixedModels[0].DeltaCILow, want)
	}
	if rep.Topline[0].MeanQuality != want {
		t.Fatalf("topline mean quality = %v; want the canonical 2/3 %v", rep.Topline[0].MeanQuality, want)
	}

	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				walk(path+"."+k, child)
			}
		case []any:
			for i, child := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		case float64:
			if c := canonicalStat(x); c != x {
				t.Errorf("%s = %v is not canonical (want %v)", path, x, c)
			}
		}
	}
	walk("report", decoded)
}
