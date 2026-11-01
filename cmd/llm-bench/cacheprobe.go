package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// The cache probe measures how stable the prompt prefix is across the requests
// of one scripted Golem session, and what the provider's own cache counters
// say about them. It is the before/after evidence for the prompt-caching
// phases, so it is scripted: the model's replies are fixed, only usage is taken
// from the real server.

//go:embed testdata/cacheprobe/*.txt
var probeFixtureFS embed.FS

// probeScenario selects how much context pressure the scripted session runs under.
type probeScenario string

const (
	// probeUncompacted runs under a 131072-token input ceiling, so the ~48 KB
	// of reads never compacts: only prompt stability is on trial.
	probeUncompacted probeScenario = "uncompacted"
	// probePressure runs under Golem's default 8192-token ceiling
	// (agent.DefaultInputCeiling), so the assembler compacts mid-session.
	probePressure probeScenario = "pressure"
)

const (
	probeReads     = 8 // read_file calls in the first turn (f1.txt..f8.txt)
	probeFollowUps = 3 // single-step turns carrying History, as Golem does
	// probeMaxSteps leaves room for the reads plus the final answer.
	probeMaxSteps = probeReads + 3
)

// probeRow is one model request of the scripted session.
type probeRow struct {
	Scenario probeScenario
	// Step is the 1-based request within its turn; Turn is the 1-based Run.
	Step int
	Turn int
	// PromptTokens includes the cached part (OpenAI semantics).
	PromptTokens int
	// Cached is the provider's cached prompt-token counter; CachedReported is
	// false when the server did not report one, so a measured zero stays
	// distinguishable from silence.
	Cached         int
	CachedReported bool
	// PrefixRatio is commonPrefixRatio of this request's messages against the
	// previous request's: 0 for the first request.
	PrefixRatio float64
}

// commonPrefixRatio is the length of the common byte prefix of prev and next
// divided by len(prev), or 0 when prev is empty. It is the share of the
// previous request a prefix cache could reuse, if the provider's cache key
// were these bytes.
func commonPrefixRatio(prev, next []byte) float64 {
	if len(prev) == 0 {
		return 0
	}
	n := 0
	for n < len(prev) && n < len(next) && prev[n] == next[n] {
		n++
	}
	return float64(n) / float64(len(prev))
}

// probeCeiling is the scenario's per-turn input ceiling.
func probeCeiling(sc probeScenario) (int, error) {
	switch sc {
	case probeUncompacted:
		return 131072, nil
	case probePressure:
		return 8192, nil
	}
	return 0, fmt.Errorf("cache probe: unknown scenario %q", sc)
}

// checkProbeCompaction rejects a run whose compaction behavior contradicts its
// scenario, because its table would be mislabeled: an uncompacted run must
// never shed, and a pressure run must shed at least once.
func checkProbeCompaction(sc probeScenario, compactions int) error {
	switch sc {
	case probeUncompacted:
		if compactions > 0 {
			return fmt.Errorf("cache probe: uncompacted scenario compacted in %d step(s); the fixtures exceed the input ceiling", compactions)
		}
	case probePressure:
		if compactions == 0 {
			return fmt.Errorf("cache probe: pressure scenario never compacted; the fixtures no longer exceed the input ceiling")
		}
	default:
		return fmt.Errorf("cache probe: unknown scenario %q", sc)
	}
	return nil
}

// loadProbeFixtures reads f1.txt..f8.txt from dir, or from the embedded copy
// when dir is empty. Reading them all up front fails a bad directory before
// any request is sent.
func loadProbeFixtures(dir string) (map[string]string, error) {
	var fsys fs.FS
	if dir == "" {
		sub, err := fs.Sub(probeFixtureFS, "testdata/cacheprobe")
		if err != nil {
			return nil, fmt.Errorf("cache probe: embedded fixtures: %w", err)
		}
		fsys = sub
	} else {
		fsys = os.DirFS(dir)
	}
	files := make(map[string]string, probeReads)
	for i := 1; i <= probeReads; i++ {
		name := fmt.Sprintf("f%d.txt", i)
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("cache probe: fixture %s: %w", name, err)
		}
		files[name] = string(b)
	}
	return files, nil
}

// probeReadTool is the stub read_file the scripted calls hit: it serves the
// fixtures by name and touches nothing else.
type probeReadTool struct{ files map[string]string }

func (probeReadTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "read_file",
		Description: "Read a file from the workspace and return its contents.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File to read."}},"required":["path"]}`),
	}
}

func (probeReadTool) Effect() agent.Effect { return agent.Effect{Class: agent.Read} }

func (t probeReadTool) Invoke(_ context.Context, args json.RawMessage) (agent.ToolResult, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ToolResult{}, fmt.Errorf("read_file arguments: %w", err)
	}
	content, ok := t.files[a.Path]
	if !ok {
		return agent.ToolResult{Content: "no such file: " + a.Path, IsError: true}, nil
	}
	return agent.ToolResult{Content: content}, nil
}

// probeCaller is the model seam the agent loop talks to. It forwards each
// request to the real caller for usage only, records the row, and returns the
// scripted reply: one read_file call per step up to reads, then the answer.
type probeCaller struct {
	real agent.ModelCaller
	sc   probeScenario

	rows []probeRow
	prev []byte // previous request's messages, as JSON

	turn, step, reads int
	answer            string
}

// begin starts the scripted turn: reads read_file calls, then answer.
func (c *probeCaller) begin(turn, reads int, answer string) {
	c.turn, c.step, c.reads, c.answer = turn, 0, reads, answer
}

func (c *probeCaller) Chat(ctx context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.step++
	// Messages only: tools and options serialize after them, so comparing
	// whole requests would under-read a stable history. This is
	// provider.ChatMessage JSON, a proxy for the outbound OpenAI bytes.
	body, err := json.Marshal(req.Messages)
	if err != nil {
		return agent.ModelResult{}, fmt.Errorf("step %d: encode messages: %w", c.step, err)
	}
	fwd := req
	fwd.Options.NumPredict = 1 // the reply is discarded; only usage is wanted
	res, err := c.real.Chat(ctx, fwd, nil)
	if err != nil {
		// runCacheProbe adds the "cache probe: turn N" prefix; only the step is new here.
		return agent.ModelResult{}, fmt.Errorf("step %d: %w", c.step, err)
	}
	u := res.Response.Usage
	c.rows = append(c.rows, probeRow{
		Scenario: c.sc, Step: c.step, Turn: c.turn,
		PromptTokens: u.PromptTokens, Cached: u.CachedPromptTokens, CachedReported: u.CachedPromptTokensReported,
		PrefixRatio: commonPrefixRatio(c.prev, body),
	})
	c.prev = body

	resp := provider.ChatResponse{Done: true, Usage: u}
	if c.step > c.reads {
		resp.Content = c.answer
		return agent.ModelResult{Response: resp}, nil
	}
	args, err := json.Marshal(map[string]string{"path": fmt.Sprintf("f%d.txt", c.step)})
	if err != nil {
		return agent.ModelResult{}, err
	}
	resp.ToolCalls = []provider.ToolCall{{
		ID: fmt.Sprintf("probe-call-%d", c.step), Type: "function",
		Function: provider.ToolCallFunction{Name: "read_file", Arguments: args},
	}}
	return agent.ModelResult{Response: resp}, nil
}

// runCacheProbe drives the scripted Golem session through the real agent loop:
// one turn of probeReads read_file calls and a final answer, then probeFollowUps
// single-step turns that carry History. Every request goes to real for its
// usage and gets a scripted reply, so the request bytes are what Golem's loop
// would send for the same flow; the run is scripted (the fence keys and the
// session id are random per invocation).
//
// Out of its reach: Golem's cross-turn compression (golem/session.go) and mixed
// assembly. A run whose compaction contradicts sc is an error. It also returns
// the run's fence rotations: each re-frames an earlier tool result, so the
// prompt cache misses from that request on.
func runCacheProbe(ctx context.Context, real agent.ModelCaller, fixtureDir string, sc probeScenario) ([]probeRow, int, error) {
	ceiling, err := probeCeiling(sc)
	if err != nil {
		return nil, 0, err
	}
	files, err := loadProbeFixtures(fixtureDir)
	if err != nil {
		return nil, 0, err
	}
	sessionID := "probe-" + rand.Text()
	pc := &probeCaller{real: real, sc: sc}
	orch := agent.New(pc, agent.ContextManager{})
	system := golem.SystemPrompt(false, false)

	var history []provider.ChatMessage
	compactions, rotations := 0, 0
	for turn := 1; turn <= 1+probeFollowUps; turn++ {
		goal := "Read f1.txt through f8.txt one at a time, then summarize them."
		reads := probeReads
		if turn > 1 {
			goal, reads = fmt.Sprintf("Follow-up question %d: which file did you read first?", turn-1), 0
		}
		answer := fmt.Sprintf("Probe answer %d.", turn)
		pc.begin(turn, reads, answer)
		res, err := orch.Run(ctx, agent.Request{
			Goal: goal, System: system, History: history, Tools: []agent.Tool{probeReadTool{files: files}},
			MaxSteps: probeMaxSteps, Budget: agent.Budget{InputCeiling: ceiling}, SessionID: sessionID,
		}, nil)
		if err != nil {
			return nil, 0, fmt.Errorf("cache probe: turn %d: %w", turn, err)
		}
		if res.StopReason != agent.Completed || pc.step != reads+1 {
			return nil, 0, fmt.Errorf("cache probe: turn %d stopped (%s) after %d request(s), want %d", turn, res.StopReason, pc.step, reads+1)
		}
		for _, s := range res.Steps {
			compactions += s.Pressure.Compactions
		}
		for _, e := range res.Events {
			if e.Kind == "fence_rotation" {
				rotations++
			}
		}
		history = append(history,
			provider.ChatMessage{Role: "user", Content: goal},
			provider.ChatMessage{Role: "assistant", Content: answer})
	}
	if err := checkProbeCompaction(sc, compactions); err != nil {
		return nil, 0, err
	}
	return pc.rows, rotations, nil
}

// writeProbeTable writes rows as a markdown table, then the run's fence
// rotation count. The Cached column is the ground truth; the prefix ratio is
// labeled a serialization proxy.
func writeProbeTable(w io.Writer, rows []probeRow, rotations int) error {
	scenario := ""
	if len(rows) > 0 {
		scenario = string(rows[0].Scenario)
	}
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "# Cache probe: scenario %s\n\n", scenario)
	b.WriteString("\"Cached\" is the provider's cached prompt-token counter and is the ground truth (\"n/a\": the server did not report one). " +
		"\"Prefix ratio (proxy)\" is a serialization proxy: the common byte prefix of consecutive requests' messages as provider.ChatMessage JSON, " +
		"divided by the previous request's length; it is not the outbound OpenAI body.\n\n")
	b.WriteString("| Turn | Step | Prompt tokens | Cached | Prefix ratio (proxy) |\n|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		cached := "n/a"
		if r.CachedReported {
			cached = fmt.Sprint(r.Cached)
		}
		_, _ = fmt.Fprintf(&b, "| %d | %d | %d | %s | %.3f |\n", r.Turn, r.Step, r.PromptTokens, cached, r.PrefixRatio)
	}
	_, _ = fmt.Fprintf(&b, "\nFence rotations: %d. A rotation re-frames an earlier tool result, so the prompt cache misses from that request on.\n", rotations)
	_, err := io.WriteString(w, b.String())
	return err
}

// providerChatCaller adapts a non-streaming openai-compat chat to the agent's
// ModelCaller. Prefix caching keys on the prompt, not the delivery mode, and a
// plain JSON response carries the usage block without stream_options.
type providerChatCaller struct {
	p     *openaicompat.Provider
	model string
}

func (c providerChatCaller) Chat(ctx context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	req.Model = c.model
	resp, err := c.p.Chat(ctx, req)
	if err != nil {
		return agent.ModelResult{}, err
	}
	return agent.ModelResult{Response: *resp}, nil
}

// newProbeCaller builds the probe's real ModelCaller for an openai-compat
// target through newCandidateTransport, so the API key and HTTP client are the
// ones every other candidate mode uses. The transport's own chat client is
// ollama-shaped and drops the cache counters, hence the provider is used
// directly.
func newProbeCaller(target ModelTarget, opts candidateTransportOptions) (agent.ModelCaller, error) {
	tr, err := newCandidateTransport(target, opts)
	if err != nil {
		return nil, err
	}
	c, ok := tr.chat.(openAICompatCandidateClient)
	if !ok {
		return nil, fmt.Errorf("cache probe: %q is not an %s target", target.Display, openAICompatTransport)
	}
	return providerChatCaller{p: c.provider, model: target.Model}, nil
}

// cacheProbeOptions are the resolved -cache-probe flags.
type cacheProbeOptions struct {
	BaseURL  string // -candidate-base-url
	APIKey   string // -candidate-api-key / $LLM_BENCH_CANDIDATE_API_KEY
	Models   string // -models: exactly one openai-compat selector
	Out      string // -cache-probe-out
	Scenario string // -cache-probe-scenario
	// FixtureDir, when set, replaces the embedded fixtures (tests only).
	FixtureDir string
	Timeout    time.Duration // -timeout
	// CacheAffinity is -cache-affinity; empty infers the mode from BaseURL.
	CacheAffinity string
}

// runCacheProbeMode validates the flags, checks opts.Out is writable, runs the
// probe against the one selected model, and writes the table to opts.Out. Every
// flag error and an unwritable opts.Out fail before any request is sent.
func runCacheProbeMode(ctx context.Context, opts cacheProbeOptions, stderr io.Writer) error {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return fmt.Errorf("-cache-probe requires -candidate-base-url")
	}
	if strings.TrimSpace(opts.Models) == "" {
		return fmt.Errorf("-cache-probe requires -models")
	}
	targets, err := parseModelTargets(opts.Models)
	if err != nil {
		return fmt.Errorf("-models: %w", err)
	}
	if len(targets) != 1 {
		return fmt.Errorf("-cache-probe takes exactly one -models selector, got %d", len(targets))
	}
	if normalizeModelSelector(targets[0].Provider) != openAICompatTransport {
		return fmt.Errorf("-cache-probe needs an %s model (-models %s/<model>): only that transport reports provider cache counters, got %q",
			openAICompatTransport, openAICompatTransport, targets[0].Display)
	}
	if strings.TrimSpace(opts.Out) == "" {
		return fmt.Errorf("-cache-probe requires -cache-probe-out")
	}
	sc := probeScenario(opts.Scenario)
	if sc != probeUncompacted && sc != probePressure {
		return fmt.Errorf("-cache-probe-scenario must be %q or %q, got %q", probeUncompacted, probePressure, opts.Scenario)
	}

	real, err := newProbeCaller(targets[0], candidateTransportOptions{
		openAICompatBaseURL: opts.BaseURL, openAICompatAPIKey: opts.APIKey, timeout: opts.Timeout,
		cacheAffinity: opts.CacheAffinity,
	})
	if err != nil {
		return err
	}
	// Open without truncating so a failed run preserves an earlier table.
	// Keep the handle open so a FIFO reader cannot see EOF before the write.
	probe, err := os.OpenFile(opts.Out, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open -cache-probe-out: %w", err)
	}
	defer func() { _ = probe.Close() }() // Failure paths retain their primary error; success checks Close below.
	info, err := probe.Stat()
	if err != nil {
		return fmt.Errorf("stat -cache-probe-out: %w", err)
	}

	rows, rotations, err := runCacheProbe(ctx, real, opts.FixtureDir, sc)
	if err != nil {
		return err
	}
	// Only regular files need truncation; devices and pipes do not support it.
	if info.Mode().IsRegular() {
		if err := probe.Truncate(0); err != nil {
			return fmt.Errorf("truncate -cache-probe-out: %w", err)
		}
	}
	if err := writeProbeTable(probe, rows, rotations); err != nil {
		return fmt.Errorf("write -cache-probe-out: %w", err)
	}
	if err := probe.Close(); err != nil {
		return fmt.Errorf("close -cache-probe-out: %w", err)
	}
	_, _ = fmt.Fprintf(stderr, "llm-bench: cache-probe: %d requests (%s) -> %s\n", len(rows), sc, opts.Out)
	return nil
}
