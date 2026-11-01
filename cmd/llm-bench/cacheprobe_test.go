package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

const cacheProbeFixtures = "testdata/cacheprobe"

func TestCommonPrefixRatio(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prev, next string
		want       float64
	}{
		{"identical", "abcdef", "abcdef", 1.0},
		{"half shared", "abcd", "abXY", 0.5},
		{"empty prev", "", "abcd", 0},
		{"empty next", "abcd", "", 0},
		{"disjoint", "abcd", "wxyz", 0},
		{"next extends prev", "ab", "abcd", 1.0},
		{"next truncates prev", "abcd", "ab", 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commonPrefixRatio([]byte(tc.prev), []byte(tc.next)); got != tc.want {
				t.Errorf("commonPrefixRatio(%q, %q) = %v, want %v", tc.prev, tc.next, got, tc.want)
			}
		})
	}
}

// probeServer is an openai-compat fake whose n-th request reports
// prompt_tokens 1000*n and cached_tokens 100*n, and records the session
// header and body each request carried.
type probeServer struct {
	*httptest.Server
	mu       sync.Mutex
	n        int
	sessions []string
	hosts    []string // each request's Host header
	bodies   [][]byte
	silent   bool // omit prompt_tokens_details, like a backend without prefix-cache counters
}

func newProbeServer(t *testing.T) *probeServer {
	t.Helper()
	ps := &probeServer{}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ps.mu.Lock()
		ps.n++
		n := ps.n
		ps.sessions = append(ps.sessions, r.Header.Get("x-opencode-session"))
		ps.hosts = append(ps.hosts, r.Host)
		ps.bodies = append(ps.bodies, body)
		silent := ps.silent
		ps.mu.Unlock()
		details := fmt.Sprintf(`,"prompt_tokens_details":{"cached_tokens":%d}`, 100*n)
		if silent {
			details = ""
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"model":"probe-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"length"}],`+
			`"usage":{"prompt_tokens":%d,"completion_tokens":1,"total_tokens":%d%s}}`,
			1000*n, 1000*n+1, details)
	}))
	t.Cleanup(ps.Close)
	return ps
}

func (ps *probeServer) count() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.n
}

func (ps *probeServer) seenSessions() []string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return slices.Clone(ps.sessions)
}

// promptCacheKeys returns each request's prompt_cache_key, nil where none was sent.
func (ps *probeServer) promptCacheKeys(t *testing.T) []*string {
	t.Helper()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	keys := make([]*string, len(ps.bodies))
	for i, b := range ps.bodies {
		var body struct {
			Key *string `json:"prompt_cache_key"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatalf("request %d body: %v", i, err)
		}
		keys[i] = body.Key
	}
	return keys
}

// caller builds the probe's real ModelCaller exactly as the CLI does, through
// the candidate transport, with the default cache affinity.
func (ps *probeServer) caller(t *testing.T) agent.ModelCaller {
	t.Helper()
	return ps.callerWithAffinity(t, "")
}

// callerWithAffinity is caller with an explicit -cache-affinity mode.
func (ps *probeServer) callerWithAffinity(t *testing.T, mode string) agent.ModelCaller {
	t.Helper()
	c, err := newProbeCaller(ModelTarget{Display: "openai-compat/probe-model", Provider: openAICompatTransport, Model: "probe-model"},
		candidateTransportOptions{openAICompatBaseURL: ps.URL, cacheAffinity: mode})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// forwardRecorder records every request the probe's wrapper forwards.
type forwardRecorder struct {
	next agent.ModelCaller
	mu   sync.Mutex
	reqs []provider.ChatRequest
}

func (c *forwardRecorder) Chat(ctx context.Context, req provider.ChatRequest, on func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	return c.next.Chat(ctx, req, on)
}

func runProbe(t *testing.T, ps *probeServer, sc probeScenario) []probeRow {
	t.Helper()
	rows, err := runCacheProbe(t.Context(), ps.caller(t), cacheProbeFixtures, sc)
	if err != nil {
		t.Fatalf("runCacheProbe(%s): %v", sc, err)
	}
	return rows
}

func TestCacheProbeRowsFromFakeServer(t *testing.T) {
	ps := newProbeServer(t)
	rows := runProbe(t, ps, probeUncompacted)

	if len(rows) != 12 {
		t.Fatalf("rows = %d, want 12 (9 steps + 3 follow-up turns)", len(rows))
	}
	if ps.count() != 12 {
		t.Errorf("server saw %d requests, want 12: every request is forwarded exactly once", ps.count())
	}
	if rows[3].Cached != 400 {
		t.Errorf("rows[3].Cached = %d, want 400", rows[3].Cached)
	}
	for i, r := range rows {
		wantTurn, wantStep := 1, i+1
		if i >= 9 {
			wantTurn, wantStep = i-7, 1
		}
		if r.Turn != wantTurn || r.Step != wantStep {
			t.Errorf("rows[%d] turn/step = %d/%d, want %d/%d", i, r.Turn, r.Step, wantTurn, wantStep)
		}
		if r.Scenario != probeUncompacted {
			t.Errorf("rows[%d].Scenario = %q, want %q", i, r.Scenario, probeUncompacted)
		}
		if r.PromptTokens != 1000*(i+1) || r.Cached != 100*(i+1) {
			t.Errorf("rows[%d] prompt/cached = %d/%d, want %d/%d: usage must come from the real response", i, r.PromptTokens, r.Cached, 1000*(i+1), 100*(i+1))
		}
		if !r.CachedReported {
			t.Errorf("rows[%d].CachedReported = false", i)
		}
		if r.PrefixRatio < 0 || r.PrefixRatio > 1 {
			t.Errorf("rows[%d].PrefixRatio = %v, want within [0,1]", i, r.PrefixRatio)
		}
	}
	if rows[0].PrefixRatio != 0 {
		t.Errorf("first request has no predecessor: PrefixRatio = %v, want 0", rows[0].PrefixRatio)
	}
}

// A backend that sends no counter must read as unreported, not as a measured
// zero: the table shows n/a, never a misleading 0.
func TestCacheProbeUnreportedCounters(t *testing.T) {
	ps := newProbeServer(t)
	ps.mu.Lock()
	ps.silent = true
	ps.mu.Unlock()
	rows := runProbe(t, ps, probeUncompacted)
	var b strings.Builder
	if err := writeProbeTable(&b, rows); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if r.CachedReported || r.Cached != 0 {
			t.Errorf("rows[%d] cached = %d (reported %v), want 0 unreported", i, r.Cached, r.CachedReported)
		}
	}
	if got := strings.Count(b.String(), "| n/a |"); got != 12 {
		t.Errorf("table shows n/a %d times, want 12:\n%s", got, b.String())
	}
}

// TestCacheProbePrefixRatioStable pins what per-result fence keys buy: each
// tool frame keeps its key on every later render, so consecutive requests
// share every byte before the new step's messages. runCacheProbe itself
// fails an uncompacted run in which any step reported a compaction, so a nil
// error is the "no step compacted" assertion.
func TestCacheProbePrefixRatioStable(t *testing.T) {
	ps := newProbeServer(t)
	rows := runProbe(t, ps, probeUncompacted)

	var ratios []float64
	for _, r := range rows {
		if r.Turn == 1 && r.Step >= 3 {
			ratios = append(ratios, r.PrefixRatio)
		}
	}
	if len(ratios) != 7 {
		t.Fatalf("steps 3-9 = %d rows, want 7", len(ratios))
	}
	slices.Sort(ratios)
	median := ratios[len(ratios)/2]
	t.Logf("median prefix ratio over steps 3-9: %.4f (min %.4f, max %.4f)", median, ratios[0], ratios[len(ratios)-1])
	if median < 0.95 {
		t.Errorf("median prefix ratio = %.4f, want at least 0.95 under per-result tool-frame keys", median)
	}
}

func TestCacheProbePressureCompacts(t *testing.T) {
	ps := newProbeServer(t)
	// A pressure run that never compacted would be mislabeled, so
	// runCacheProbe errors on it: a nil error proves compaction ran.
	pressure := &forwardRecorder{next: ps.caller(t)}
	rows, err := runCacheProbe(t.Context(), pressure, cacheProbeFixtures, probePressure)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 12 {
		t.Fatalf("rows = %d, want 12", len(rows))
	}
	for i, r := range rows {
		if r.Scenario != probePressure {
			t.Errorf("rows[%d].Scenario = %q, want %q", i, r.Scenario, probePressure)
		}
		if r.PrefixRatio < 0 || r.PrefixRatio > 1 {
			t.Errorf("rows[%d].PrefixRatio = %v, want within [0,1]", i, r.PrefixRatio)
		}
	}

	// Compaction must reach the wire, not just the step counter: turn 1's last
	// request (the 9th) carries fewer messages under pressure than uncompacted.
	whole := &forwardRecorder{next: ps.caller(t)}
	if _, err := runCacheProbe(t.Context(), whole, cacheProbeFixtures, probeUncompacted); err != nil {
		t.Fatal(err)
	}
	if got, ref := len(pressure.reqs[8].Messages), len(whole.reqs[8].Messages); got >= ref {
		t.Errorf("turn 1 step 9 carries %d messages under pressure, want fewer than the %d uncompacted: the pressure budget no longer shrinks the request", got, ref)
	}
}

func TestCheckProbeCompaction(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sc          probeScenario
		compactions int
		wantErr     string
	}{
		{"uncompacted stayed whole", probeUncompacted, 0, ""},
		{"uncompacted compacted", probeUncompacted, 1, "compacted"},
		{"pressure compacted", probePressure, 3, ""},
		{"pressure never compacted", probePressure, 0, "never compacted"},
		{"unknown scenario", probeScenario("bogus"), 0, "unknown scenario"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkProbeCompaction(tc.sc, tc.compactions)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestCacheProbeSessionIDStable(t *testing.T) {
	ps := newProbeServer(t)
	// The fake is loopback, which infers no affinity; opencode mode is the one
	// that puts the raw session id on the wire.
	cc := &forwardRecorder{next: ps.callerWithAffinity(t, "opencode")}
	if _, err := runCacheProbe(t.Context(), cc, cacheProbeFixtures, probeUncompacted); err != nil {
		t.Fatal(err)
	}
	if len(cc.reqs) != 12 {
		t.Fatalf("captured %d requests, want 12", len(cc.reqs))
	}
	first := cc.reqs[0].SessionID
	if !strings.HasPrefix(first, "probe-") || len(first) <= len("probe-") {
		t.Fatalf("session id = %q, want probe-<random>", first)
	}
	for i, req := range cc.reqs {
		if req.SessionID != first {
			t.Errorf("request %d session id = %q, want %q: one id per probe invocation", i, req.SessionID, first)
		}
		if req.Options.NumPredict != 1 {
			t.Errorf("request %d NumPredict = %d, want 1: only usage is taken from the real response", i, req.Options.NumPredict)
		}
	}
	for i, s := range ps.seenSessions() {
		if s != first {
			t.Errorf("server request %d carried session header %q, want %q", i, s, first)
		}
	}

	// A second invocation must get its own id, or two probes would share a cache slot.
	cc2 := &forwardRecorder{next: ps.caller(t)}
	if _, err := runCacheProbe(t.Context(), cc2, cacheProbeFixtures, probeUncompacted); err != nil {
		t.Fatal(err)
	}
	if cc2.reqs[0].SessionID == first {
		t.Errorf("second probe reused session id %q", first)
	}
}

// TestCacheProbePrefixRatioIsMessagesOnly recomputes every ratio from the
// requests the real caller received. Tools and options serialize after the
// messages, so a whole-request comparison would divide by a longer prev and
// under-read a stable history; only the messages array may be compared.
func TestCacheProbePrefixRatioIsMessagesOnly(t *testing.T) {
	ps := newProbeServer(t)
	fr := &forwardRecorder{next: ps.caller(t)}
	rows, err := runCacheProbe(t.Context(), fr, cacheProbeFixtures, probeUncompacted)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(fr.reqs) {
		t.Fatalf("rows = %d, forwarded requests = %d", len(rows), len(fr.reqs))
	}
	var prev []byte
	for i, req := range fr.reqs {
		cur, err := json.Marshal(req.Messages)
		if err != nil {
			t.Fatal(err)
		}
		if want := commonPrefixRatio(prev, cur); rows[i].PrefixRatio != want {
			t.Errorf("rows[%d].PrefixRatio = %v, want %v from the request's messages alone", i, rows[i].PrefixRatio, want)
		}
		prev = cur
	}
	if len(fr.reqs[1].Tools) == 0 {
		t.Error("requests carry no tool schemas: the messages-only comparison is untested")
	}
}

// TestCacheProbeSendsSystemPromptAndHistory pins what makes the probe's
// requests Golem-shaped: the first message of every request is Golem's system
// prompt, and a follow-up turn's request carries the earlier turns as History.
// Dropping either would keep every other probe test green while the measured
// prefix stopped resembling a real session.
func TestCacheProbeSendsSystemPromptAndHistory(t *testing.T) {
	ps := newProbeServer(t)
	fr := &forwardRecorder{next: ps.caller(t)}
	if _, err := runCacheProbe(t.Context(), fr, cacheProbeFixtures, probeUncompacted); err != nil {
		t.Fatal(err)
	}
	if len(fr.reqs) != 12 {
		t.Fatalf("forwarded %d requests, want 12", len(fr.reqs))
	}
	system := golem.SystemPrompt(false, false)
	for i, req := range fr.reqs {
		if len(req.Messages) == 0 || req.Messages[0].Role != "system" || !strings.HasPrefix(req.Messages[0].Content, system) {
			t.Errorf("request %d does not open with Golem's system prompt: first message %+v", i, firstMessage(req))
		}
	}

	// reqs[9] is the first request of turn 2 (turn 1 is 8 reads + the answer).
	turn2 := fr.reqs[9].Messages
	var sawGoal, sawAnswer bool
	for _, m := range turn2 {
		switch {
		case m.Role == "user" && strings.Contains(m.Content, "Read f1.txt through f8.txt"):
			sawGoal = true
		case m.Role == "assistant" && m.Content == "Probe answer 1.":
			sawAnswer = true
		}
	}
	if !sawGoal || !sawAnswer {
		t.Errorf("turn 2 request carries turn 1 goal=%v answer=%v, want both: History must reach the model\nmessages: %+v", sawGoal, sawAnswer, turn2)
	}
}

func firstMessage(req provider.ChatRequest) any {
	if len(req.Messages) == 0 {
		return nil
	}
	return req.Messages[0]
}

func TestCacheProbeFixtures(t *testing.T) {
	for i := 1; i <= 8; i++ {
		b, err := os.ReadFile(filepath.Join(cacheProbeFixtures, fmt.Sprintf("f%d.txt", i)))
		if err != nil {
			t.Fatal(err)
		}
		if len(b) < 5500 || len(b) > 6500 {
			t.Errorf("f%d.txt is %d bytes, want ~6 KB so 8 reads pass every provider's caching minimum", i, len(b))
		}
	}
}

func TestCacheProbeRejectsBadInput(t *testing.T) {
	ps := newProbeServer(t)
	for _, tc := range []struct {
		name, dir string
		sc        probeScenario
		wantErr   string
	}{
		{"unknown scenario", cacheProbeFixtures, probeScenario("bogus"), "unknown scenario"},
		{"missing fixture dir", filepath.Join(t.TempDir(), "none"), probeUncompacted, "f1.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCacheProbe(t.Context(), ps.caller(t), tc.dir, tc.sc)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
	if ps.count() != 0 {
		t.Errorf("server saw %d requests, want 0: bad input fails before any call", ps.count())
	}
}

// failingCaller is a real caller whose n-th request (1-based) fails.
type failingCaller struct {
	next agent.ModelCaller
	n    int
	err  error
	seen int
}

func (c *failingCaller) Chat(ctx context.Context, req provider.ChatRequest, on func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.seen++
	if c.seen == c.n {
		return agent.ModelResult{}, c.err
	}
	return c.next.Chat(ctx, req, on)
}

// A failed real request is reported with its turn and step under one
// "cache probe:" prefix, not wrapped twice.
func TestCacheProbeChatFailureWrapsOnce(t *testing.T) {
	ps := newProbeServer(t)
	boom := fmt.Errorf("server went away")
	fc := &failingCaller{next: ps.caller(t), n: 3, err: boom}
	_, err := runCacheProbe(t.Context(), fc, cacheProbeFixtures, probeUncompacted)
	if err == nil {
		t.Fatal("runCacheProbe succeeded despite a failed request")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap the caller's error", err)
	}
	const want = "cache probe: turn 1: step 3: server went away"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if n := strings.Count(err.Error(), "cache probe:"); n != 1 {
		t.Errorf("error %q has the cache probe prefix %d times, want once", err.Error(), n)
	}
}

func TestWriteProbeTable(t *testing.T) {
	rows := []probeRow{
		{Scenario: probePressure, Turn: 1, Step: 1, PromptTokens: 1200, Cached: 0, CachedReported: true, PrefixRatio: 0},
		{Scenario: probePressure, Turn: 1, Step: 2, PromptTokens: 2400, Cached: 1100, CachedReported: true, PrefixRatio: 0.9876},
		{Scenario: probePressure, Turn: 2, Step: 1, PromptTokens: 900, CachedReported: false, PrefixRatio: 0.25},
	}
	var b strings.Builder
	if err := writeProbeTable(&b, rows); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"scenario pressure",
		"serialization proxy",
		"ground truth",
		"| Turn | Step | Prompt tokens | Cached | Prefix ratio (proxy) |",
		"| 1 | 1 | 1200 | 0 | 0.000 |",
		"| 1 | 2 | 2400 | 1100 | 0.988 |",
		"| 2 | 1 | 900 | n/a | 0.250 |",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, b.String())
		}
	}
}

func TestRunCacheProbeModeValidation(t *testing.T) {
	ps := newProbeServer(t)
	valid := cacheProbeOptions{
		BaseURL:  ps.URL,
		Models:   "openai-compat/probe-model",
		Out:      filepath.Join(t.TempDir(), "out.md"),
		Scenario: string(probeUncompacted),
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*cacheProbeOptions)
		wantErr string
	}{
		{"no base url", func(o *cacheProbeOptions) { o.BaseURL = " " }, "-cache-probe requires -candidate-base-url"},
		{"no models", func(o *cacheProbeOptions) { o.Models = "" }, "-cache-probe requires -models"},
		{"two models", func(o *cacheProbeOptions) { o.Models = "openai-compat/a,openai-compat/b" }, "exactly one"},
		{"ollama target", func(o *cacheProbeOptions) { o.Models = "probe-model" }, "openai-compat"},
		{"bad selector", func(o *cacheProbeOptions) { o.Models = "nope/model" }, "nope"},
		{"no out", func(o *cacheProbeOptions) { o.Out = "" }, "-cache-probe requires -cache-probe-out"},
		{"bad scenario", func(o *cacheProbeOptions) { o.Scenario = "bogus" }, `-cache-probe-scenario must be "uncompacted" or "pressure"`},
		{"empty scenario", func(o *cacheProbeOptions) { o.Scenario = "" }, `-cache-probe-scenario must be`},
		// Everything else is valid: only the unwritable output stops the run, and it
		// must do so before the first request, not after a multi-minute probe.
		{"out in a missing directory", func(o *cacheProbeOptions) {
			o.Out = filepath.Join(t.TempDir(), "no-such-dir", "out.md")
		}, "open -cache-probe-out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := valid
			tc.mutate(&opts)
			err := runCacheProbeMode(t.Context(), opts, &strings.Builder{})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
	if ps.count() != 0 {
		t.Errorf("server saw %d requests, want 0: validation fails before any call", ps.count())
	}
	if _, err := os.Stat(valid.Out); err == nil {
		t.Error("a rejected run wrote the output file")
	}
}

// The output file is opened before the run so a bad path fails early, but it
// must not be truncated until the table is ready: a failed run keeps whatever
// table was there, and a successful one replaces it completely.
func TestRunCacheProbeModeOutputFileLifecycle(t *testing.T) {
	opts := func(url, out string) cacheProbeOptions {
		return cacheProbeOptions{BaseURL: url, Models: "openai-compat/probe-model", Out: out, Scenario: string(probeUncompacted)}
	}

	t.Run("failed run keeps the old table", func(t *testing.T) {
		ps := newProbeServer(t)
		ps.Close() // every request now fails
		out := filepath.Join(t.TempDir(), "probe.md")
		if err := os.WriteFile(out, []byte("old table"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runCacheProbeMode(t.Context(), opts(ps.URL, out), &strings.Builder{}); err == nil {
			t.Fatal("runCacheProbeMode succeeded against a closed server")
		}
		if b, err := os.ReadFile(out); err != nil || string(b) != "old table" {
			t.Errorf("output after a failed run = %q (err %v), want %q untouched", b, err, "old table")
		}
	})

	t.Run("successful run replaces a longer old table", func(t *testing.T) {
		ps := newProbeServer(t)
		out := filepath.Join(t.TempDir(), "probe.md")
		if err := os.WriteFile(out, []byte(strings.Repeat("stale table\n", 2000)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runCacheProbeMode(t.Context(), opts(ps.URL, out), &strings.Builder{}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(b), "# Cache probe: scenario uncompacted") || strings.Contains(string(b), "stale") {
			t.Errorf("output keeps bytes of the old table (%d bytes):\n%.200s", len(b), b)
		}
	})

	// A device or pipe target (-cache-probe-out /dev/stdout) must work: nothing
	// may ftruncate the output, which fails with EINVAL on Linux devices.
	t.Run("device target is written", func(t *testing.T) {
		if f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err != nil {
			t.Skipf("%s is not writable here: %v", os.DevNull, err)
		} else {
			_ = f.Close()
		}
		ps := newProbeServer(t)
		if err := runCacheProbeMode(t.Context(), opts(ps.URL, os.DevNull), &strings.Builder{}); err != nil {
			t.Fatalf("runCacheProbeMode with -cache-probe-out %s: %v", os.DevNull, err)
		}
		if ps.count() != 12 {
			t.Errorf("server saw %d requests, want 12: the run must complete", ps.count())
		}
	})
}

func TestRunCacheProbeModeWritesTable(t *testing.T) {
	for _, sc := range []probeScenario{probeUncompacted, probePressure} {
		t.Run(string(sc), func(t *testing.T) {
			ps := newProbeServer(t)
			out := filepath.Join(t.TempDir(), "probe.md")
			err := runCacheProbeMode(t.Context(), cacheProbeOptions{
				BaseURL: ps.URL, Models: "openai-compat/probe-model", Out: out, Scenario: string(sc),
				// FixtureDir empty: the embedded fixtures serve the CLI from any cwd.
			}, &strings.Builder{})
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			got := string(b)
			if !strings.Contains(got, "scenario "+string(sc)) || !strings.Contains(got, "serialization proxy") {
				t.Errorf("table header lacks the scenario or the proxy label:\n%s", got)
			}
			if rows := strings.Count(got, "\n| ") - 1; rows != 12 { // minus the header row
				t.Errorf("table has %d data rows, want 12:\n%s", rows, got)
			}
		})
	}
}

// TestCacheProbeAffinityFlag runs the binary: with -cache-affinity openai every
// probe request body carries the same keyed prompt_cache_key; with the default,
// a loopback -candidate-base-url infers none and no request carries one, while
// an https opencode.ai base URL infers opencode (ruling P3-b: the default is
// "infer", not "none"). An unknown mode fails before any request.
func TestCacheProbeAffinityFlag(t *testing.T) {
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	runAt := func(t *testing.T, baseURL string, env []string, extra ...string) ([]byte, error) {
		t.Helper()
		args := append([]string{
			"-cache-probe", "-models", "openai-compat/probe-model", "-candidate-base-url", baseURL,
			"-cache-probe-out", filepath.Join(t.TempDir(), "probe.md"),
		}, extra...)
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(append(os.Environ(), "LLM_BENCH_TEST_MAIN=1"), env...)
		return cmd.CombinedOutput()
	}
	run := func(t *testing.T, ps *probeServer, extra ...string) ([]byte, error) {
		t.Helper()
		return runAt(t, ps.URL, nil, extra...)
	}

	t.Run("default infers opencode for an https opencode.ai URL", func(t *testing.T) {
		ps := newProbeServer(t)
		// LLM_BENCH_TEST_REDIRECT sends every request to the fake (TestMain).
		// The unreachable proxies make a broken redirect fail closed instead of
		// reaching the real host; loopback, the fake, bypasses them.
		out, err := runAt(t, "https://opencode.ai/zen/go", []string{
			"LLM_BENCH_TEST_REDIRECT=" + ps.URL,
			"HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "NO_PROXY=",
		})
		if err != nil {
			t.Fatalf("probe failed: %v\n%s", err, out)
		}
		sessions := ps.seenSessions()
		if len(sessions) != 12 {
			t.Fatalf("server saw %d requests, want 12", len(sessions))
		}
		ps.mu.Lock()
		hosts := slices.Clone(ps.hosts)
		ps.mu.Unlock()
		for i, s := range sessions {
			if hosts[i] != "opencode.ai" {
				t.Fatalf("request %d Host = %q, want opencode.ai: the redirect seam did not apply", i, hosts[i])
			}
			if !strings.HasPrefix(s, "probe-") || s != sessions[0] {
				t.Errorf("request %d x-opencode-session = %q, want the probe's one session id (first %q)", i, s, sessions[0])
			}
		}
	})

	t.Run("openai", func(t *testing.T) {
		ps := newProbeServer(t)
		if out, err := run(t, ps, "-cache-affinity", "openai"); err != nil {
			t.Fatalf("probe failed: %v\n%s", err, out)
		}
		keys := ps.promptCacheKeys(t)
		if len(keys) != 12 {
			t.Fatalf("server saw %d requests, want 12", len(keys))
		}
		for i, k := range keys {
			if k == nil || !hex32.MatchString(*k) {
				t.Fatalf("request %d prompt_cache_key = %v, want 32 hex", i, k)
			}
			if *k != *keys[0] {
				t.Errorf("request %d prompt_cache_key = %q, want %q: one key per probe session", i, *k, *keys[0])
			}
		}
	})

	t.Run("default", func(t *testing.T) {
		ps := newProbeServer(t)
		if out, err := run(t, ps); err != nil {
			t.Fatalf("probe failed: %v\n%s", err, out)
		}
		keys := ps.promptCacheKeys(t)
		if len(keys) != 12 {
			t.Fatalf("server saw %d requests, want 12", len(keys))
		}
		for i, k := range keys {
			if k != nil {
				t.Errorf("request %d sent prompt_cache_key %q under the default affinity", i, *k)
			}
		}
		for i, s := range ps.seenSessions() {
			if s != "" {
				t.Errorf("request %d sent x-opencode-session %q to a non-OpenCode host", i, s)
			}
		}
	})

	t.Run("unknown mode", func(t *testing.T) {
		ps := newProbeServer(t)
		out, err := run(t, ps, "-cache-affinity", "OpenAI")
		if err == nil {
			t.Fatalf("llm-bench accepted -cache-affinity OpenAI:\n%s", out)
		}
		if !strings.Contains(string(out), `llm-bench: -cache-affinity must be one of none, openai, openrouter, opencode, or empty to infer from -candidate-base-url; got "OpenAI"`) {
			t.Fatalf("missing the validation message:\n%s", out)
		}
		if n := ps.count(); n != 0 {
			t.Fatalf("server saw %d request(s) before the mode was rejected", n)
		}
	})
}
