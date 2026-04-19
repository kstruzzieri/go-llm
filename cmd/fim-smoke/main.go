package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/completion"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

func main() {
	model := flag.String("model", "", "Ollama model name (required, e.g. qwen2.5-coder:7b)")
	fixturePattern := flag.String("file", "", "Fixture file or glob (required). Use '<CURSOR>' marker in source.")
	ollamaURL := flag.String("ollama-url", "http://localhost:11434", "Ollama server URL")
	stream := flag.Bool("stream", false, "Use CompleteStream instead of Complete")
	check := flag.Bool("check", false, "Assert sidecar invariants and exit nonzero on failure")
	verbose := flag.Bool("verbose", false, "Print full budget trace and prompt metadata")
	timeout := flag.Duration("timeout", 60*time.Second, "Per-fixture timeout")
	flag.Parse()

	if *model == "" || *fixturePattern == "" {
		flag.Usage()
		os.Exit(2)
	}

	paths, err := filepath.Glob(*fixturePattern)
	if err != nil || len(paths) == 0 {
		fmt.Fprintf(os.Stderr, "no fixtures matched %q\n", *fixturePattern)
		os.Exit(2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p, stopTokens, err := buildProvider(ctx, *ollamaURL, *model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build provider: %v\n", err)
		os.Exit(1)
	}

	failures := 0
	for _, path := range paths {
		f, err := LoadFixture(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: load: %v\n", path, err)
			failures++
			continue
		}

		fctx, fcancel := context.WithTimeout(ctx, *timeout)
		resp, err := runFixture(fctx, p, f, *stream)
		fcancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: run: %v\n", path, err)
			failures++
			continue
		}

		printResult(path, resp, *verbose, *check)

		if *check {
			res := Check(f, resp, stopTokens)
			if !res.OK() {
				failures++
				for _, msg := range res.Failures {
					fmt.Fprintf(os.Stderr, "  FAIL: %s\n", msg)
				}
			} else {
				fmt.Println("  check: OK")
			}
		}
	}

	if failures > 0 {
		os.Exit(1)
	}
}

// buildProvider wires the full registry → profile → completion.Provider stack.
// Returns the provider and the resolved stop tokens (for leak checks).
func buildProvider(ctx context.Context, url, model string) (*completion.Provider, []string, error) {
	client := ollama.NewClient(ollama.WithBaseURL(url))
	if !client.IsAvailable(ctx) {
		return nil, nil, fmt.Errorf("ollama not reachable at %s", url)
	}

	pReg := provider.NewRegistry()
	op := provider.NewOllamaProvider(client)
	if err := pReg.Register(op); err != nil {
		return nil, nil, fmt.Errorf("register: %w", err)
	}
	if err := pReg.RefreshModels(ctx, op.Name()); err != nil {
		return nil, nil, fmt.Errorf("refresh models: %w", err)
	}

	mr, err := provider.NewModelRegistry(pReg, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("model registry: %w", err)
	}
	profile, err := mr.Lookup(ctx, provider.ModelKey{Provider: op.Name(), Model: model})
	if err != nil {
		return nil, nil, fmt.Errorf("lookup %q: %w", model, err)
	}
	if !profile.SupportsFIM() {
		return nil, nil, unsupportedFIMError(url, model, profile)
	}
	cfg, err := completion.ProviderConfigFromProfile(profile)
	if err != nil {
		return nil, nil, fmt.Errorf("config from profile: %w", err)
	}
	prov, err := completion.NewProvider(client, model, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("new provider: %w", err)
	}
	return prov, cfg.FIM.StopTokens, nil
}

func unsupportedFIMError(url, model string, profile *provider.ModelProfile) error {
	reason := "unknown reason"
	templatePreview := "<empty>"
	caps := "<none reported>"

	if profile != nil {
		if got := profile.FIMUnsupportedReason(); got != "" {
			reason = got
		}
		templatePreview = summarizeTemplate(profile.Template)
		if got := profileCapabilities(profile); len(got) > 0 {
			caps = strings.Join(got, ",")
		}
	}

	return fmt.Errorf(
		"model %q does not support native FIM (%s)\n"+
			"  detected template: %s\n"+
			"  detected capabilities: %s\n"+
			"  preflight commands:\n"+
			"    ollama show %s --modelfile\n"+
			"    curl -s %s/api/show -d '{\"model\":\"%s\"}'",
		model,
		reason,
		templatePreview,
		caps,
		model,
		strings.TrimRight(url, "/"),
		model,
	)
}

func summarizeTemplate(tmpl string) string {
	tmpl = strings.TrimSpace(tmpl)
	if tmpl == "" {
		return "<empty>"
	}
	parts := strings.Fields(tmpl)
	flat := strings.Join(parts, " ")
	const maxLen = 80
	if len(flat) <= maxLen {
		return flat
	}
	return flat[:maxLen-3] + "..."
}

func profileCapabilities(p *provider.ModelProfile) []string {
	if p == nil || p.Caps == 0 {
		return nil
	}
	parts := strings.Split(p.Caps.String(), "|")
	return parts
}

// runFixture runs a single fixture through the provider. When stream is
// true, the streamed tokens are concatenated into the response's Completion
// so the same printing/checking path handles both modes.
func runFixture(ctx context.Context, p *completion.Provider, f *Fixture, stream bool) (*completion.FIMResponse, error) {
	req := completion.FIMRequest{
		Prefix:   f.Prefix,
		Suffix:   f.Suffix,
		FilePath: f.FilePath,
		Trace:    true,
	}

	if !stream {
		start := time.Now()
		resp, err := p.Complete(ctx, req)
		if err != nil {
			return nil, err
		}
		if resp.LatencyMs == 0 {
			resp.LatencyMs = time.Since(start).Milliseconds()
		}
		return resp, nil
	}

	// Streaming mode: accumulate tokens, then return a synthesized response.
	// Complete is re-called after the stream only to obtain the trace; we
	// cannot pull trace off a stream handle. This is deliberately a smoke-
	// testing aid, not a perf-accurate measurement.
	var collected string
	start := time.Now()
	if err := p.CompleteStream(ctx, req, func(tok string) error {
		collected += tok
		return nil
	}); err != nil {
		return nil, err
	}
	latency := time.Since(start).Milliseconds()

	traceResp, err := p.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("trace pass after stream: %w", err)
	}
	return &completion.FIMResponse{
		Completion:      collected,
		Tokens:          traceResp.Tokens,
		LatencyMs:       latency,
		CursorContext:   traceResp.CursorContext,
		CompletionShape: traceResp.CompletionShape,
		BudgetTrace:     traceResp.BudgetTrace,
	}, nil
}

// printResult writes a compact, human-readable summary for one fixture.
func printResult(path string, r *completion.FIMResponse, verbose, check bool) {
	fmt.Printf("== %s\n", path)
	fmt.Printf("  latency=%dms tokens=%d context=%s shape=%s\n",
		r.LatencyMs, r.Tokens, r.CursorContext, r.CompletionShape)
	if verbose && r.BudgetTrace != nil {
		b, _ := json.MarshalIndent(r.BudgetTrace, "  ", "  ")
		fmt.Printf("  trace=%s\n", string(b))
	}
	if !check {
		fmt.Printf("  ---\n%s\n  ---\n", r.Completion)
	}
}
