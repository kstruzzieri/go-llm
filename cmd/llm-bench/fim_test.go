package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFIMCases_RoundTripsPrefixSuffix(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c1.json", `{"id":"c1","prefix":"func add(a, b int) int {\n\treturn ","suffix":"\n}"}`)
	cases, err := loadFIMCases([]string{p})
	if err != nil {
		t.Fatalf("loadFIMCases: %v", err)
	}
	if len(cases) != 1 || cases[0].ID != "c1" || !strings.Contains(cases[0].Prefix, "return ") || cases[0].Suffix != "\n}" {
		t.Fatalf("loaded case = %+v; want prefix/suffix round-trip", cases[0])
	}
}

func TestLoadFIMCases_RejectsMissingPrefixAndSuffix(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "bad.json", `{"id":"bad"}`)
	if _, err := loadFIMCases([]string{p}); err == nil {
		t.Fatalf("loadFIMCases accepted a case with neither prefix nor suffix; want error")
	}
}

// fakeFIMGenerator records calls and advances a shared clock by a fixed step per
// call so runFIMLatency produces deterministic latencies. Single-goroutine only:
// runFIMLatency drives the generator sequentially, so no synchronization is
// needed (and the unlocked clock read in the Now closure stays race-free).
type fakeFIMGenerator struct {
	clock     *time.Time
	step      time.Duration
	calls     []string // "model|prefix|suffix|numPredict"
	failModel string   // GenerateFIM returns an error when model == failModel
	genTokens int
}

func (f *fakeFIMGenerator) GenerateFIM(_ context.Context, model, prefix, suffix string, numPredict int) (fimGenOutput, error) {
	f.calls = append(f.calls, fmt.Sprintf("%s|%s|%s|%d", model, prefix, suffix, numPredict))
	*f.clock = f.clock.Add(f.step)
	if model == f.failModel {
		return fimGenOutput{}, errors.New("boom")
	}
	return fimGenOutput{Text: "completion", GenTokens: f.genTokens}, nil
}

func TestRunFIMLatency_TimesEachCaseDeterministically(t *testing.T) {
	clock := time.Unix(0, 0)
	gen := &fakeFIMGenerator{clock: &clock, step: 25 * time.Millisecond, genTokens: 7}
	cases := []FIMCase{{ID: "c1", Prefix: "a", Suffix: "b"}, {ID: "c2", Prefix: "c", Suffix: "d"}}
	results := runFIMLatency(context.Background(), fimRunOptions{
		Generator:  gen,
		Models:     []string{"m"},
		Cases:      cases,
		NumPredict: 64,
		Warmup:     false,
		Now:        func() time.Time { return clock },
	})
	if len(results) != 2 {
		t.Fatalf("results = %d; want 2 (one per case)", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("unexpected error: %v", r.Err)
		}
		if r.LatencyMs != 25 {
			t.Errorf("LatencyMs = %d; want 25 (clock step)", r.LatencyMs)
		}
		if r.GenTokens != 7 {
			t.Errorf("GenTokens = %d; want 7", r.GenTokens)
		}
	}
}

func TestRunFIMLatency_WarmupIssuesOneDiscardedCallPerModel(t *testing.T) {
	clock := time.Unix(0, 0)
	gen := &fakeFIMGenerator{clock: &clock, step: time.Millisecond}
	cases := []FIMCase{{ID: "c1", Prefix: "a", Suffix: "b"}}
	results := runFIMLatency(context.Background(), fimRunOptions{
		Generator: gen, Models: []string{"m"}, Cases: cases, NumPredict: 64,
		Warmup: true, Now: func() time.Time { return clock },
	})
	// 1 warm-up + 1 timed case = 2 generator calls, but only 1 result row.
	if len(gen.calls) != 2 {
		t.Errorf("generator calls = %d; want 2 (1 warm-up + 1 case)", len(gen.calls))
	}
	if len(results) != 1 {
		t.Errorf("results = %d; want 1 (warm-up is discarded)", len(results))
	}
}

func TestRunFIMLatency_ForwardsNumPredictAndDefaults(t *testing.T) {
	clock := time.Unix(0, 0)
	cases := []FIMCase{{ID: "c1", Prefix: "a", Suffix: "b"}}

	explicit := &fakeFIMGenerator{clock: &clock, step: time.Millisecond}
	runFIMLatency(context.Background(), fimRunOptions{
		Generator: explicit, Models: []string{"m"}, Cases: cases,
		NumPredict: 32, Warmup: false, Now: func() time.Time { return clock },
	})
	if len(explicit.calls) != 1 || !strings.HasSuffix(explicit.calls[0], "|32") {
		t.Fatalf("explicit numPredict not forwarded; calls=%v", explicit.calls)
	}

	defaulted := &fakeFIMGenerator{clock: &clock, step: time.Millisecond}
	runFIMLatency(context.Background(), fimRunOptions{
		Generator: defaulted, Models: []string{"m"}, Cases: cases,
		NumPredict: 0, Warmup: false, Now: func() time.Time { return clock },
	})
	if len(defaulted.calls) != 1 || !strings.HasSuffix(defaulted.calls[0], "|64") {
		t.Fatalf("numPredict<=0 should default to 64; calls=%v", defaulted.calls)
	}
}

func TestRunFIMLatency_PerCaseErrorRecordedAndRunContinues(t *testing.T) {
	clock := time.Unix(0, 0)
	gen := &fakeFIMGenerator{clock: &clock, step: time.Millisecond, failModel: "m"}
	results := runFIMLatency(context.Background(), fimRunOptions{
		Generator: gen, Models: []string{"m"}, Cases: []FIMCase{{ID: "c1", Prefix: "a", Suffix: "b"}},
		NumPredict: 64, Warmup: false, Now: func() time.Time { return clock },
	})
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("expected one errored result row; got %+v", results)
	}
}

func TestFormatFIMReport_ShowsLatencySeparateFromChat(t *testing.T) {
	results := []FIMResult{
		{Model: "m", CaseID: "c1", LatencyMs: 20, GenTokens: 5},
		{Model: "m", CaseID: "c2", LatencyMs: 40, GenTokens: 7},
	}
	out := formatFIMReport([]string{"m"}, results)
	for _, want := range []string{
		"FIM",
		"separate from chat",
		"| m | 20 / 40 / 30 | 6 | 2 | 0/2 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("FIM report missing %q:\n%s", want, out)
		}
	}
}

func TestFormatFIMReport_CountsFailuresAndExcludesFromLatency(t *testing.T) {
	results := []FIMResult{
		{Model: "m", CaseID: "c1", LatencyMs: 30, GenTokens: 4},
		{Model: "m", CaseID: "c2", Err: errors.New("boom")},
	}
	out := formatFIMReport([]string{"m"}, results)
	// One success (latency 30), one failure → latency reflects only the success.
	if !strings.Contains(out, "| m | 30 / 30 / 30 | 4 | 1 | 1/2 |") {
		t.Errorf("FIM report should exclude failures from latency and count them:\n%s", out)
	}
}
