package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Legacy byte-identity goldens (#687 acceptance 1). They were generated ONCE,
// at the base commit and before any #687 production change, with
// LLM_BENCH_UPDATE_LEGACY_GOLDEN=1. Never regenerate them on a branch that
// changes llm-bench: a golden rewritten from the code under test cannot fail.
const legacyGoldenUpdateEnv = "LLM_BENCH_UPDATE_LEGACY_GOLDEN"

// goldenTurnDelay is the scripted server delay on the success exchanges of the
// replay cases, so measured per-turn latency is provably nonzero. Loopback
// round trips otherwise measure 0 whole milliseconds almost every run, which
// would make the latency assertions below compare 0 with 0.
const goldenTurnDelay = 5 * time.Millisecond

func compareLegacyGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "legacy_golden", name)
	if os.Getenv(legacyGoldenUpdateEnv) == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("legacy output %s changed; #687 requires byte identity\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// legacyGoldenResult is a Result with its run-dependent parts pinned: the
// endpoint-derived provider name and wall-clock latencies (lengths kept).
// Requests pins what the candidate SENT, so a legacy change to the request
// side (headers, temperature, raw tool-argument bytes) is as visible as a
// change to the Result.
type legacyGoldenResult struct {
	Case              string
	Model             string
	TraceID           string
	CandidateProvider string
	Score             Score
	Err               string
	Transcript        []Turn
	Requests          []legacyGoldenRequest
}

// legacyGoldenRequest is one request the fake server received. Nothing here
// depends on the httptest URL or port: the path is the request path, and the
// body is hashed (the chat body carries no endpoint).
type legacyGoldenRequest struct {
	Path           string
	Authorization  string
	AcceptEncoding string
	Correlation    string
	BodyBytes      int
	BodySHA256     string
}

func TestLegacyGolden_RunOneOpenAICompat(t *testing.T) {
	cases := []struct {
		name   string
		trace  string
		script []compatExchange
	}{
		{"plain-answer", fixtureSmokeTrace, []compatExchange{
			{Delay: goldenTurnDelay, Body: compatText("smoke-ok", `{"prompt_tokens":11,"completion_tokens":3}`)},
		}},
		{"tool-route", fixtureToolRouteTrace, []compatExchange{
			{Delay: goldenTurnDelay, Body: compatToolCall("rag_search", `{"query":"retry budget"}`, `{"prompt_tokens":20,"completion_tokens":5}`)},
			{Delay: goldenTurnDelay, Body: compatText("RetryBudget defaults to 3 in provider/router.go.", `{"prompt_tokens":30,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":2}}`)},
		}},
		{"tool-route-bypassed", fixtureToolRouteTrace, []compatExchange{
			{Body: compatText("It is in provider/router.go.", "")},
		}},
		{"prefilled-answer", fixturePrefilledTrace, []compatExchange{
			{Delay: goldenTurnDelay, Body: compatText("The drift detector averages over a 35 minute window.", `{"prompt_tokens":40,"completion_tokens":8}`)},
		}},
		{"prefilled-tool-call-no-content", fixturePrefilledTrace, []compatExchange{
			{Body: compatToolCall("retrieve", `{"query":"drift"}`, "")},
		}},
		{"plain-chat-tool-call", fixtureSmokeTrace, []compatExchange{
			{Body: compatToolCall("lookup", `{"q":"x"}`, "")},
		}},
		{"argument-decode-error", fixtureToolRouteTrace, []compatExchange{
			{Body: compatToolCall("rag_search", "not json", `{"prompt_tokens":17,"completion_tokens":4}`)},
		}},
		{"empty-reply", fixtureSmokeTrace, []compatExchange{
			{Body: compatText("", "")},
		}},
		{"http-500", fixtureSmokeTrace, []compatExchange{
			{Status: http.StatusInternalServerError, Body: `{"error":{"message":"boom"}}`},
		}},
	}
	got := make([]legacyGoldenResult, 0, len(cases))
	for _, tc := range cases {
		srv := newCompatServer(t, tc.script...)
		runner := &Runner{OpenAICompatBaseURL: srv.URL, Timeout: 30 * time.Second, Scorer: &CaptureScorer{}}
		target := ModelTarget{Display: "openai-compat/golden-model", Provider: openAICompatTransport, Model: "golden-model"}
		results, err := runner.RunAll(context.Background(), []ModelTarget{target}, []Trace{loadFixtureTrace(t, tc.trace)})
		if err != nil {
			t.Fatalf("%s: RunAll: %v", tc.name, err)
		}
		if len(results) != 1 {
			t.Fatalf("%s: %d results, want 1", tc.name, len(results))
		}
		r := results[0]
		g := legacyGoldenResult{Case: tc.name, Model: r.Model, TraceID: r.TraceID, CandidateProvider: r.CandidateProvider, Score: r.Score, Transcript: r.Transcript}
		// Pin the identity format independently of its producer, then mask
		// only the endpoint-derived digest (the port changes every run).
		sum := sha256.Sum256([]byte(srv.URL))
		if want := "openai-compat:" + hex.EncodeToString(sum[:])[:12]; g.CandidateProvider != want {
			t.Fatalf("%s: CandidateProvider %q, want %q", tc.name, g.CandidateProvider, want)
		}
		g.CandidateProvider = "openai-compat:<endpoint-sha256-12>"
		if r.Err != nil {
			g.Err = r.Err.Error()
		}
		// Masking zeroes the wall-clock values, which would hide this
		// deterministic relation (runner.go: LatencyMs sums the per-turn
		// round-trips and excludes scorer time), so assert it first. Error
		// results carry a zero Score, where both sides are 0. The scripted
		// delays make the check bite: each delayed turn must measure at least
		// its delay, so a LatencyMs that kept only one turn, or dropped the
		// slow one, cannot equal the sum of the entries.
		var turnSum, delaySum int64
		for _, ms := range g.Score.TurnLatenciesMs {
			turnSum += ms
		}
		for i, ex := range tc.script {
			if ex.Delay <= 0 {
				continue
			}
			want := ex.Delay.Milliseconds()
			delaySum += want
			if i >= len(g.Score.TurnLatenciesMs) || g.Score.TurnLatenciesMs[i] < want {
				t.Fatalf("%s: TurnLatenciesMs %v, want entry %d >= scripted delay %dms", tc.name, g.Score.TurnLatenciesMs, i, want)
			}
		}
		if g.Score.LatencyMs != turnSum || turnSum < delaySum {
			t.Fatalf("%s: LatencyMs %d, sum(TurnLatenciesMs) %d, scripted delays %dms; want LatencyMs == sum >= delays",
				tc.name, g.Score.LatencyMs, turnSum, delaySum)
		}
		g.Score.LatencyMs, g.Score.ScorerLatencyMs = 0, 0
		for i := range g.Score.TurnLatenciesMs {
			g.Score.TurnLatenciesMs[i] = 0
		}
		for _, rec := range srv.Received() {
			bodySum := sha256.Sum256(rec.Body)
			g.Requests = append(g.Requests, legacyGoldenRequest{
				Path:           rec.Path,
				Authorization:  rec.Authorization,
				AcceptEncoding: rec.AcceptEncoding,
				Correlation:    rec.Correlation,
				BodyBytes:      len(rec.Body),
				BodySHA256:     hex.EncodeToString(bodySum[:]),
			})
		}
		got = append(got, g)
	}
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	compareLegacyGolden(t, "runone-openai-compat.json", append(raw, '\n'))
}

// newLegacyOllamaRouter answers /api/chat by request content, so the reply
// does not depend on capture order. The smoke trace fails its first request
// with a 500, which exercises the legacy one-retry policy.
func newLegacyOllamaRouter(t *testing.T) *httptest.Server {
	t.Helper()
	var smokeCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		reply := map[string]any{"model": "golden-model", "done": true, "prompt_eval_count": 12, "eval_count": 6}
		switch {
		case strings.Contains(s, "Where is the provider retry budget") && strings.Contains(s, `"role":"tool"`):
			reply["message"] = map[string]any{"role": "assistant", "content": "RetryBudget defaults to 3 in provider/router.go."}
		case strings.Contains(s, "Where is the provider retry budget"):
			reply["message"] = map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"function": map[string]any{"name": "rag_search", "arguments": map[string]any{"query": "retry budget"}}},
			}}
		case strings.Contains(s, "Reply with exactly: smoke-ok"):
			if smokeCalls.Add(1) == 1 {
				http.Error(w, `{"error":"warming up"}`, http.StatusInternalServerError)
				return
			}
			reply["message"] = map[string]any{"role": "assistant", "content": "smoke-ok"}
		default:
			reply["message"] = map[string]any{"role": "assistant", "content": "The drift detector averages over a 35 minute window."}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLegacyGolden_CalibrateCaptureOllama(t *testing.T) {
	srv := newLegacyOllamaRouter(t)
	traces := []Trace{
		loadFixtureTrace(t, fixtureSmokeTrace),
		loadFixtureTrace(t, fixtureToolRouteTrace),
		loadFixtureTrace(t, fixturePairLegacyTrace),
		loadFixtureTrace(t, fixturePrefilledTrace),
	}
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	fixed := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:     &Runner{OllamaURL: srv.URL, Timeout: 30 * time.Second, Scorer: &CaptureScorer{}},
		Targets:    []ModelTarget{{Display: "ollama/golden-model", Provider: defaultBenchProvider, Model: "golden-model"}},
		Traces:     traces,
		OutputPath: out,
		Clock:      func() time.Time { return fixed },
		OllamaURL:  srv.URL,
		Stdout:     io.Discard,
	})
	if err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}
	for _, path := range []string{out, captureManifestPath(out)} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		raw = bytes.ReplaceAll(raw, []byte(srv.URL), []byte("http://golden.invalid"))
		compareLegacyGolden(t, "calibrate-capture-ollama-"+filepath.Base(path), raw)
	}
}
