package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

// formatFIMReport renders inline-completion latency per model, distinct from
// chat-replay latency. The sub-second interactive regime is a different
// measurement than chat seconds, so the two are never mixed.
func formatFIMReport(models []string, results []FIMResult) string {
	byModel := make(map[string][]FIMResult, len(models))
	for _, r := range results {
		byModel[r.Model] = append(byModel[r.Model], r)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# llm-bench — FIM / inline-completion latency\n\n")
	fmt.Fprintf(&b, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintln(&b, "> Interactive regime: prefix/suffix completion with num_predict capped. This is kept separate from chat latency — the sub-second completion target is a different regime than chat seconds, so do NOT compare these numbers to chat-replay latency.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## FIM latency by model")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Model | LatencyMs (p50 / p90 / mean, successful-only) | Gen tokens (mean) | n | Failures/total |")
	fmt.Fprintln(&b, "|---|---|---|---|---|")
	for _, m := range models {
		rs := byModel[m]
		var latencies []int64
		var genSum int64
		failures := 0
		for _, r := range rs {
			if r.Err != nil {
				failures++
				continue
			}
			latencies = append(latencies, r.LatencyMs)
			genSum += int64(r.GenTokens)
		}
		scored := len(latencies)
		latCell := "n/a"
		genCell := "n/a"
		if scored > 0 {
			ps := int64Percentiles(latencies, 0.5, 0.9)
			latCell = fmt.Sprintf("%d / %d / %d", ps[0], ps[1], sumInt64(latencies)/int64(scored))
			genCell = fmt.Sprintf("%d", genSum/int64(scored))
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d/%d |\n",
			markdownCell(m), latCell, genCell, scored, failures, len(rs))
	}
	fmt.Fprintln(&b)
	return redactPaths(b.String())
}

// defaultFIMNumPredict bounds generation for an inline-completion measurement:
// FIM completions are short, and the interactive regime targets sub-second
// latency, so a small cap keeps the measurement honest.
const defaultFIMNumPredict = 64

// FIMCase is one inline-completion fixture: the code before the cursor (Prefix)
// and after it (Suffix). The model fills the middle. Distinct from chat Traces.
type FIMCase struct {
	ID     string `json:"id"`
	Prefix string `json:"prefix"`
	Suffix string `json:"suffix"`
}

// loadFIMCases reads each path as a JSON FIMCase. A case must have an ID and at
// least one of prefix/suffix (a completion with neither has no context).
func loadFIMCases(paths []string) ([]FIMCase, error) {
	cases := make([]FIMCase, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("fim: read %q: %w", path, err)
		}
		var c FIMCase
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("fim: parse %q: %w", path, err)
		}
		if strings.TrimSpace(c.ID) == "" {
			return nil, fmt.Errorf("fim: %q missing id", path)
		}
		if c.Prefix == "" && c.Suffix == "" {
			return nil, fmt.Errorf("fim: case %q has neither prefix nor suffix", c.ID)
		}
		cases = append(cases, c)
	}
	return cases, nil
}

// fimGenOutput is the provider-agnostic result of one FIM generation.
type fimGenOutput struct {
	Text      string
	GenTokens int
}

// fimGenerator is the transport seam for inline completion. The Ollama impl uses
// /api/generate (prompt+suffix); the OpenAI-compat candidate transport (#136)
// will add a llama.cpp impl (/v1/completions suffix or /infill) with no change
// to the runner/report layer.
type fimGenerator interface {
	GenerateFIM(ctx context.Context, model, prefix, suffix string, numPredict int) (fimGenOutput, error)
}

// ollamaFIMGenerator drives FIM through the Ollama /api/generate endpoint.
type ollamaFIMGenerator struct {
	client *ollama.Client
}

func (g ollamaFIMGenerator) GenerateFIM(ctx context.Context, model, prefix, suffix string, numPredict int) (fimGenOutput, error) {
	resp, err := g.client.Generate(ctx, ollama.GenerateRequest{
		// Strip the bench provider prefix so Ollama receives its native model
		// name (the report still keys on the user-facing Display selector).
		Model:   modelSelectorWithoutBenchProvider(model),
		Prompt:  prefix,
		Suffix:  suffix,
		Options: &ollama.ModelOptions{NumPredict: numPredict},
	})
	if err != nil {
		return fimGenOutput{}, err
	}
	return fimGenOutput{Text: resp.Response, GenTokens: resp.EvalCount}, nil
}

// FIMResult is one (model, case) inline-completion measurement.
type FIMResult struct {
	Model     string
	CaseID    string
	LatencyMs int64
	GenTokens int
	Err       error
}

// fimRunOptions configures runFIMLatency.
type fimRunOptions struct {
	Generator  fimGenerator
	Models     []string
	Cases      []FIMCase
	NumPredict int
	Warmup     bool             // when true, issue one discarded generate per model before timing
	Now        func() time.Time // injectable clock for deterministic tests; defaults to time.Now
}

// runFIMLatency measures inline-completion latency per (model, case). It warms
// each model with one discarded call (when Warmup) so the reported latency
// reflects the warm interactive regime, then times each case. Per-case errors
// are recorded as result rows so a single failure never aborts the run.
func runFIMLatency(ctx context.Context, opts fimRunOptions) []FIMResult {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	numPredict := opts.NumPredict
	if numPredict <= 0 {
		numPredict = defaultFIMNumPredict
	}

	results := make([]FIMResult, 0, len(opts.Models)*len(opts.Cases))
	for _, model := range opts.Models {
		if opts.Warmup && len(opts.Cases) > 0 {
			warm := opts.Cases[0]
			_, _ = opts.Generator.GenerateFIM(ctx, model, warm.Prefix, warm.Suffix, numPredict)
		}
		for _, c := range opts.Cases {
			start := now()
			out, err := opts.Generator.GenerateFIM(ctx, model, c.Prefix, c.Suffix, numPredict)
			latency := now().Sub(start).Milliseconds()
			res := FIMResult{Model: model, CaseID: c.ID, LatencyMs: latency}
			if err != nil {
				res.Err = err
			} else {
				res.GenTokens = out.GenTokens
			}
			results = append(results, res)
		}
	}
	return results
}
