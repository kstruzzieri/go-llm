package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/configio"
	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// mcCalled is a completed chat-stream-tools reply: one valid get_time call, a
// finish reason, usage and [DONE].
const mcCalled = `data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":6,"total_tokens":46}}

data: [DONE]

`

// mcTag is one /api/tags entry.
type mcTag struct{ name, digest string }

// mcServer serves an openai-compat and an Ollama API at one address. A check of
// an openai-compat model is a chat request naming get_time, so it is counted
// apart from listing and lookup traffic; any other chat is a turn, answered
// "ok". Every /api/show is counted per model
// name; mcEnv.showChecks tells an Ollama check from a lookup.
type mcServer struct {
	*httptest.Server
	mu     sync.Mutex
	ids    []string // /v1/models
	tags   []mcTag  // /api/tags
	caps   string   // /api/show's capabilities array as JSON; "" leaves it out
	checks map[string]int
	shows  map[string]int
	// reply answers a check; n is how many checks of model came before. nil
	// answers mcCalled.
	reply func(w http.ResponseWriter, r *http.Request, model string, n int)
}

func newMCServer(t *testing.T) *mcServer {
	t.Helper()
	s := &mcServer{checks: map[string]int{}, shows: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *mcServer) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		s.mu.Lock()
		data := make([]map[string]string, 0, len(s.ids))
		for _, id := range s.ids {
			data = append(data, map[string]string{"id": id})
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	case "/api/tags":
		s.mu.Lock()
		models := make([]map[string]string, 0, len(s.tags))
		for _, tag := range s.tags {
			models = append(models, map[string]string{"name": tag.name, "digest": tag.digest})
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
	case "/api/show":
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.shows[body.Name]++
		caps := s.caps
		s.mu.Unlock()
		reply := `{"details":{"family":"mystery","parameter_size":"8B"}`
		if caps != "" {
			reply += `,"capabilities":` + caps
		}
		_, _ = io.WriteString(w, reply+"}")
	case "/v1/chat/completions":
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		if !bytes.Contains(raw, []byte("get_time")) { // a turn, not a check
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n"+
				`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\ndata: [DONE]\n\n")
			return
		}
		s.mu.Lock()
		n := s.checks[body.Model]
		s.checks[body.Model]++
		reply := s.reply
		s.mu.Unlock()
		if reply != nil {
			reply(w, r, body.Model, n)
			return
		}
		_, _ = io.WriteString(w, mcCalled)
	default:
		http.NotFound(w, r)
	}
}

// totalChecks is every openai-compat check the server has received.
func (s *mcServer) totalChecks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.checks {
		n += c
	}
	return n
}

func (s *mcServer) checksOf(model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks[model]
}

// showsOf is how many /api/show requests named any of models.
func (s *mcServer) showsOf(models ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range models {
		n += s.shows[m]
	}
	return n
}

func (s *mcServer) setCaps(caps string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caps = caps
}

// mcMark is one write and how many /api/show requests had arrived for each
// model when it was made.
type mcMark struct {
	text  string
	shows map[string]int
}

// mcWriter records each write with the server's /api/show counts at that
// moment, so a test can count what was sent between two lines.
type mcWriter struct {
	srv   *mcServer
	mu    sync.Mutex
	buf   strings.Builder
	marks []mcMark
}

func (w *mcWriter) Write(p []byte) (int, error) {
	w.srv.mu.Lock()
	shows := maps.Clone(w.srv.shows)
	w.srv.mu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	w.marks = append(w.marks, mcMark{text: string(p), shows: shows})
	return len(p), nil
}

func (w *mcWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// first is the first write containing s.
func (w *mcWriter) first(t *testing.T, s string) mcMark {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range w.marks {
		if strings.Contains(m.text, s) {
			return m
		}
	}
	t.Fatalf("no write contains %q in:\n%s", s, w.buf.String())
	return mcMark{}
}

// lines is every line containing s.
func lines(out, s string) []string {
	var got []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, s) {
			got = append(got, l)
		}
	}
	return got
}

// mcEnv is one golem models workspace: an isolated data directory, a
// workspace root and a config.
type mcEnv struct{ root, cfg string }

func newMCEnv(t *testing.T, cfgJSON string) mcEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	for _, env := range []string{"GO_LLM_BASE_URL", "GO_LLM_CONFIG"} {
		t.Setenv(env, "")
		if err := os.Unsetenv(env); err != nil {
			t.Fatal(err)
		}
	}
	e := mcEnv{root: filepath.Join(dir, "workspace"), cfg: filepath.Join(dir, "models.json")}
	if err := os.Mkdir(e.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.cfg, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e mcEnv) run(ctx context.Context, stdout, stderr io.Writer, extra ...string) error {
	return runModels(ctx, append([]string{"-config", e.cfg, "-root", e.root, "-no-probe"}, extra...), stdout, stderr)
}

// mustRun runs golem models and returns stdout and stderr.
func (e mcEnv) mustRun(t *testing.T, srv *mcServer, extra ...string) (*mcWriter, *mcWriter) {
	t.Helper()
	stdout, stderr := &mcWriter{srv: srv}, &mcWriter{srv: srv}
	if err := e.run(t.Context(), stdout, stderr, extra...); err != nil {
		t.Fatalf("golem models %v: %v\nstderr:\n%s", extra, err, stderr)
	}
	return stdout, stderr
}

// showChecks runs golem models with extra and returns how many /api/show
// requests naming models it sent beyond what a plain listing sends in the same
// state: its checks. The plain listing runs first; a listing sends no check.
func (e mcEnv) showChecks(t *testing.T, srv *mcServer, models []string, extra ...string) (int, *mcWriter, *mcWriter) {
	t.Helper()
	before := srv.showsOf(models...)
	_, _ = e.mustRun(t, srv)
	lookups := srv.showsOf(models...) - before
	before = srv.showsOf(models...)
	stdout, stderr := e.mustRun(t, srv, extra...)
	return srv.showsOf(models...) - before - lookups, stdout, stderr
}

// mcConfig is provider "lc" at url speaking format, whose agent chain is
// models in order, each an undeclared dense role with a 32768-token window.
func mcConfig(url, format string, models ...string) string {
	roles := make([]string, len(models))
	for i, m := range models {
		fallbacks := ""
		if i == 0 && len(models) > 1 {
			names := make([]string, 0, len(models)-1)
			for j := 1; j < len(models); j++ {
				names = append(names, fmt.Sprintf("%q", fmt.Sprintf("r%d", j)))
			}
			fallbacks = `, "fallbacks": [` + strings.Join(names, ", ") + `]`
		}
		roles[i] = fmt.Sprintf(`"r%d": {"name": %q, "provider": "lc", "type": "dense", "context_window": 32768%s}`, i, m, fallbacks)
	}
	return fmt.Sprintf(`{"providers": {"lc": {"base_url": %q, "api_format": %q}}, "models": {%s}, "defaults": {"agent": "r0"}}`,
		url, format, strings.Join(roles, ", "))
}

// mcOpen opens the shared store golem uses for root.
func mcOpen(t *testing.T, root string) *fingerprint.SharedStore {
	t.Helper()
	h, warn := openCapProbeStore(context.Background(), os.Getenv, root)
	if h == nil || warn != "" {
		t.Fatalf("openCapProbeStore: %v, %q", h, warn)
	}
	return h
}

// mcRows is every log row for model at url through format.
func mcRows(t *testing.T, root, url, format, model string) []fingerprint.EvidenceRow {
	t.Helper()
	b, err := provider.EvidenceBindingFor(url, format)
	if err != nil {
		t.Fatal(err)
	}
	h := mcOpen(t, root)
	defer func() { _ = h.Close() }()
	rows, err := h.Log().EvidenceRows(context.Background(), b.Endpoint, model)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// mcSeed appends rows to the log golem reads for root.
func mcSeed(t *testing.T, root string, rows ...fingerprint.EvidenceRow) {
	t.Helper()
	h := mcOpen(t, root)
	defer func() { _ = h.Close() }()
	if err := h.Log().AppendEvidence(context.Background(), rows...); err != nil {
		t.Fatal(err)
	}
}

// mcAnswers files answers for model at digest, as a check by method would.
func mcAnswers(t *testing.T, url, format, model, digest, method string, at time.Time, answers ...fingerprint.ProbeAnswer) []fingerprint.EvidenceRow {
	t.Helper()
	b, err := provider.EvidenceBindingFor(url, format)
	if err != nil {
		t.Fatal(err)
	}
	recipe, ok := fingerprint.RecipeFor(method)
	if !ok {
		t.Fatalf("no recipe %q", method)
	}
	return fingerprint.AnswerRows(b.Endpoint, b.Adapter, model, digest, recipe, answers, at)
}

func mcAnswer(capability string, state fingerprint.CapProbeState) fingerprint.ProbeAnswer {
	return fingerprint.ProbeAnswer{Capability: capability, Source: fingerprint.EvidenceSourceProbe, Kind: fingerprint.EvidenceKindAnswer, State: state}
}

// mcToolAnswers is a completed chat-stream-tools check of model whose
// tool_call answer is tool.
func mcToolAnswers(t *testing.T, url, model string, tool fingerprint.CapProbeState, at time.Time) []fingerprint.EvidenceRow {
	t.Helper()
	return mcAnswers(t, url, "openai-compat", model, "", fingerprint.MethodChatStreamTools, at,
		mcAnswer("chat", fingerprint.CapProbeYes), mcAnswer("stream", fingerprint.CapProbeYes), mcAnswer("tool_call", tool))
}

// mcLegacy saves a legacy capability_probes verdict and returns it.
func mcLegacy(t *testing.T, root, model string, state fingerprint.CapProbeState) fingerprint.CapProbe {
	t.Helper()
	row := fingerprint.CapProbe{
		BackendID: "lc", ModelName: model, Capability: "tool_call", State: state,
		ModelDigest: "lc/" + model, ProbeVersion: fingerprint.CurrentToolProbeVersion,
		TestedAt: time.Now().Add(-time.Hour).Truncate(time.Second),
	}
	h := mcOpen(t, root)
	defer func() { _ = h.Close() }()
	if err := h.Probes().SaveCapProbe(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	return row
}

// mcWantLegacy fails unless the legacy table still holds exactly want.
func mcWantLegacy(t *testing.T, root string, want fingerprint.CapProbe) {
	t.Helper()
	h := mcOpen(t, root)
	defer func() { _ = h.Close() }()
	got, err := h.Probes().GetCapProbe(context.Background(), want.BackendID, want.ModelName, want.Capability)
	if err != nil || got.State != want.State || !got.TestedAt.Equal(want.TestedAt) {
		t.Fatalf("legacy row = %+v, %v; want %+v untouched", got, err, want)
	}
}

// TestGolemModels_PlainListingSendsNoCheck pins that the plain listing, text
// and -json, reads the log and sends no check, for a model it has an answer
// for and for a cold one.
func TestGolemModels_PlainListingSendsNoCheck(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"seeded", "cold"}
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "seeded", "cold"))
	mcSeed(t, e.root, mcToolAnswers(t, srv.URL, "seeded", fingerprint.CapProbeYes, time.Now().Add(-time.Hour))...)

	stdout, _ := e.mustRun(t, srv)
	if got := stdout.String(); !strings.Contains(got, "lc/seeded\tcaps=chat|generate|stream|tool_call\ttool_call=yes (probe)") ||
		!strings.Contains(got, "lc/cold\tcaps=chat|generate|stream\ttool_call=MISSING (unknown)") {
		t.Fatalf("listing does not read the log:\n%s", got)
	}
	_, _ = e.mustRun(t, srv, "-json")
	if checks := srv.totalChecks(); checks != 0 {
		t.Fatalf("checks sent = %d; want 0", checks)
	}
}

// TestGolemModels_ReprobeOpenAICompatRunsForcedPlan pins that -reprobe runs a
// forced plan over fresh answers, sends exactly what it shows, and appends:
// the earlier rows and the legacy table stay. An unforced -probe-all over the
// same fresh answers is the control: nothing to check, nothing sent.
func TestGolemModels_ReprobeOpenAICompatRunsForcedPlan(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"mystery"}
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "mystery"))
	earlier := mcToolAnswers(t, srv.URL, "mystery", fingerprint.CapProbeYes, time.Now().Add(-2*time.Hour))
	mcSeed(t, e.root, earlier...)
	legacy := mcLegacy(t, e.root, "mystery", fingerprint.CapProbeNo)

	_, stderr := e.mustRun(t, srv, "-probe-all")
	if checks := srv.totalChecks(); checks != 0 || !strings.Contains(stderr.String(), "nothing to check") {
		t.Fatalf("unforced over fresh answers: %d checks, stderr:\n%s", checks, stderr)
	}

	started := time.Now().Truncate(time.Millisecond)
	stdout, stderr := e.mustRun(t, srv, "-reprobe")
	plan := lines(stderr.String(), `  "mystery": chat-stream-tools`)
	if len(plan) != 1 || !strings.Contains(plan[0], "usually 1 request, at most 2") {
		t.Fatalf("plan lines %q; want one item for mystery\nstderr:\n%s", plan, stderr)
	}
	result := lines(stderr.String(), `check result "mystery"`)
	if len(result) != 1 || !strings.HasSuffix(result[0], "; 1 request sent; saved") {
		t.Fatalf("result lines %q; want one saved result of 1 request", result)
	}
	if got := srv.checksOf("mystery"); got != 1 {
		t.Fatalf("checks received = %d; want the 1 the result reports", got)
	}
	if !strings.Contains(stdout.String(), "tool_call=yes (probe)") {
		t.Fatalf("stdout:\n%s", stdout)
	}
	rows := mcRows(t, e.root, srv.URL, "openai-compat", "mystery")
	if len(rows) != len(earlier)+3 {
		t.Fatalf("log rows = %d; want %d earlier plus 3", len(rows), len(earlier))
	}
	newer := 0
	for _, r := range rows {
		if !r.ObservedAt.Before(started) {
			newer++
		}
	}
	if newer != 3 {
		t.Fatalf("rows observed since the run started = %d; want 3", newer)
	}
	mcWantLegacy(t, e.root, legacy)
}

// TestModelsReprobe_DeletesNothingAndAppends pins that -reprobe keeps an
// earlier, different answer beside the new one and leaves the legacy table
// alone; the newer answer is what golem then reads.
func TestModelsReprobe_DeletesNothingAndAppends(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"mystery"}
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "mystery"))
	earlier := mcToolAnswers(t, srv.URL, "mystery", fingerprint.CapProbeNo, time.Now().Add(-2*time.Hour))
	mcSeed(t, e.root, earlier...)
	legacy := mcLegacy(t, e.root, "mystery", fingerprint.CapProbeYes)

	stdout, _ := e.mustRun(t, srv, "-reprobe")
	if !strings.Contains(stdout.String(), "tool_call=yes (probe)") {
		t.Fatalf("stdout after -reprobe:\n%s", stdout)
	}
	rows := mcRows(t, e.root, srv.URL, "openai-compat", "mystery")
	if len(rows) != len(earlier)+3 {
		t.Fatalf("log rows = %d; want %d earlier plus 3", len(rows), len(earlier))
	}
	keptNo := slices.ContainsFunc(rows, func(r fingerprint.EvidenceRow) bool {
		return r.Capability == "tool_call" && r.State == fingerprint.CapProbeNo && r.ObservedAt.Equal(earlier[2].ObservedAt)
	})
	if !keptNo {
		t.Fatalf("the earlier no is gone: %+v", rows)
	}
	mcWantLegacy(t, e.root, legacy)
}

// TestGolemModelsProbeAll_WritesLog pins that -probe-all files its answers in
// the log, not the legacy table, and that the next listing reads them without
// a check.
func TestGolemModelsProbeAll_WritesLog(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"mystery"}
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "mystery"))

	stdout, _ := e.mustRun(t, srv, "-probe-all")
	if !strings.Contains(stdout.String(), "tool_call=yes (probe)") {
		t.Fatalf("stdout:\n%s", stdout)
	}
	rows := mcRows(t, e.root, srv.URL, "openai-compat", "mystery")
	if len(rows) != 3 {
		t.Fatalf("log rows = %d; want 3", len(rows))
	}
	h := mcOpen(t, e.root)
	_, err := h.Probes().GetCapProbe(context.Background(), "lc", "mystery", "tool_call")
	_ = h.Close()
	if !errors.Is(err, fingerprint.ErrNotFound) {
		t.Fatalf("legacy row lookup = %v; want none written", err)
	}
	stdout, _ = e.mustRun(t, srv)
	if checks := srv.totalChecks(); checks != 1 || !strings.Contains(stdout.String(), "tool_call=yes (probe)") {
		t.Fatalf("checks = %d; plain listing after:\n%s", checks, stdout)
	}
}

// TestGolemModels_OllamaSealedAndShown pins an Ollama check end to end: the
// chain names m, the runtime lists m:latest at digest D, and /api/show names no
// digest. The plan shows one /api/show for m, exactly that is sent between the
// plan and its result, and the answers land at D, where the next listing reads
// them with the live array gone. Rerunning plans nothing; -reprobe checks
// again; an unrelated listed model is never checked. Without a capabilities
// array the check leaves an inconclusive attempt at D, which plans again.
func TestGolemModels_OllamaSealedAndShown(t *testing.T) {
	const digest = "sha256:d1"
	setup := func(t *testing.T) (*mcServer, mcEnv) {
		srv := newMCServer(t)
		srv.tags = []mcTag{{"m:latest", digest}, {"other:latest", "sha256:e2"}}
		return srv, newMCEnv(t, mcConfig(srv.URL, "ollama", "m"))
	}
	run := func(t *testing.T, srv *mcServer, e mcEnv, extra ...string) (int, *mcWriter, *mcWriter) {
		t.Helper()
		return e.showChecks(t, srv, []string{"m"}, extra...)
	}

	t.Run("sealed, filed at the digest and shown", func(t *testing.T) {
		srv, e := setup(t)
		srv.setCaps(`["completion","tools"]`)
		n, _, stderr := run(t, srv, e, "-probe-all")
		if plan := lines(stderr.String(), `  "m": ollama-show`); len(plan) != 1 || n != 1 {
			t.Fatalf("plan items %q, %d checks; want one ollama-show for m, sent once\nstderr:\n%s", plan, n, stderr)
		}
		plan, result := stderr.first(t, "check plan"), stderr.first(t, `check result "m"`)
		if result.shows["m"]-plan.shows["m"] != 1 {
			t.Fatalf("/api/show for m between the plan and its result = %d; want the 1 check", result.shows["m"]-plan.shows["m"])
		}
		rows := mcRows(t, e.root, srv.URL, "ollama", "m")
		toolYes := slices.ContainsFunc(rows, func(r fingerprint.EvidenceRow) bool {
			return r.Capability == "tool_call" && r.State == fingerprint.CapProbeYes
		})
		if !toolYes || slices.ContainsFunc(rows, func(r fingerprint.EvidenceRow) bool { return r.Digest != digest }) {
			t.Fatalf("rows %+v; want a tool_call yes, every row at %s", rows, digest)
		}
		if srv.showsOf("other") != 0 {
			t.Fatal("the unrelated model was checked")
		}
		for _, model := range []string{"other", "other:latest", "m:latest"} {
			if got := mcRows(t, e.root, srv.URL, "ollama", model); len(got) != 0 {
				t.Fatalf("rows for %q = %d; want 0", model, len(got))
			}
		}

		srv.setCaps("") // the live array is gone: only the stored answer can say yes
		n, stdout, stderr := run(t, srv, e, "-probe-all")
		if n != 0 || !strings.Contains(stderr.String(), "nothing to check") {
			t.Fatalf("second -probe-all sent %d checks:\n%s", n, stderr)
		}
		if !strings.Contains(stdout.String(), "lc/m\tcaps=chat|generate|stream|tool_call\ttool_call=yes (runtime)") {
			t.Fatalf("listing does not read the answer at the digest:\n%s", stdout)
		}

		if n, _, stderr = run(t, srv, e, "-reprobe"); n != 1 {
			t.Fatalf("-reprobe sent %d checks; want 1\nstderr:\n%s", n, stderr)
		}
	})

	t.Run("no capabilities array leaves an attempt that plans again", func(t *testing.T) {
		srv, e := setup(t)
		for i := range 2 {
			if n, _, stderr := run(t, srv, e, "-probe-all"); n != 1 || len(lines(stderr.String(), `  "m": ollama-show`)) != 1 {
				t.Fatalf("run %d sent %d checks; want a planned check each time\nstderr:\n%s", i, n, stderr)
			}
		}
		rows := mcRows(t, e.root, srv.URL, "ollama", "m")
		attempt := slices.ContainsFunc(rows, func(r fingerprint.EvidenceRow) bool {
			return r.Capability == "tool_call" && r.Kind == fingerprint.EvidenceKindAttempt &&
				r.State == fingerprint.CapProbeInconclusive && r.Digest == digest
		})
		if !attempt {
			t.Fatalf("rows %+v; want an inconclusive tool_call attempt at %s", rows, digest)
		}
	})
}

// over25 is a chain of 26 distinct models on one provider.
func over25() []string {
	models := make([]string, 26)
	for i := range models {
		models[i] = fmt.Sprintf("m%02d", i)
	}
	return models
}

// mcAtMost is the limit configio's refusal names, whatever its value.
var mcAtMost = regexp.MustCompile(`at most \d+`)

// wantBatchRefused fails unless stderr gives the refusal with its limit and
// the bounded instruction, and no plan, and nothing was checked or written.
func wantBatchRefused(t *testing.T, stderr string, rows int) {
	t.Helper()
	if !strings.Contains(stderr, "batch_too_large") || !mcAtMost.MatchString(stderr) ||
		!strings.Contains(stderr, "keep fewer of its models in the agent chain") || strings.Contains(stderr, "check plan") {
		t.Fatalf("stderr does not refuse the batch:\n%s", stderr)
	}
	if rows != 0 {
		t.Fatalf("log rows = %d; want 0", rows)
	}
}

// TestGolemModels_OllamaBatchOver25 pins that 26 Ollama entries on one
// provider are refused as one batch, with no /api/show sent for a check.
func TestGolemModels_OllamaBatchOver25(t *testing.T) {
	srv := newMCServer(t)
	models := over25()
	for _, m := range models {
		srv.tags = append(srv.tags, mcTag{m + ":latest", "sha256:" + m})
	}
	srv.setCaps(`["completion","tools"]`)
	e := newMCEnv(t, mcConfig(srv.URL, "ollama", models...))
	checks, _, stderr := e.showChecks(t, srv, models, "-probe-all")
	rows := 0
	for _, m := range models {
		rows += len(mcRows(t, e.root, srv.URL, "ollama", m))
	}
	wantBatchRefused(t, stderr.String(), rows)
	if checks != 0 {
		t.Fatalf("checks = %d; want 0", checks)
	}
}

// TestGolemReprobe_Over25UniqueModelsDoesNotAutoSplit pins that 26
// openai-compat entries under -reprobe are refused, not split into smaller
// unshown checks.
func TestGolemReprobe_Over25UniqueModelsDoesNotAutoSplit(t *testing.T) {
	srv := newMCServer(t)
	models := over25()
	srv.ids = models
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", models...))
	_, stderr := e.mustRun(t, srv, "-reprobe")
	rows := 0
	for _, m := range models {
		rows += len(mcRows(t, e.root, srv.URL, "openai-compat", m))
	}
	wantBatchRefused(t, stderr.String(), rows)
	if checks := srv.totalChecks(); checks != 0 {
		t.Fatalf("checks = %d; want 0", checks)
	}
}

var mcAge = regexp.MustCompile(`\d+[hms] ago`)

// TestGolemModels_PlanAndResultsOnStderr pins that a check prints its plan and
// one line per result on stderr, with model IDs quoted, while stdout keeps the
// plain listing's lines in chain order.
func TestGolemModels_PlanAndResultsOnStderr(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"a", "b"}
	srv.reply = func(w http.ResponseWriter, _ *http.Request, model string, _ int) {
		if model == "b" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, mcCalled)
	}
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "b", "a"))
	stdout, stderr := e.mustRun(t, srv, "-probe-all")
	errText := stderr.String()

	header := lines(errText, fmt.Sprintf("check plan: provider \"lc\" at %q", strings.TrimPrefix(srv.URL, "http://")))
	if len(header) != 1 || !strings.Contains(header[0], "2 models") {
		t.Fatalf("plan header %q\nstderr:\n%s", header, errText)
	}
	for _, m := range []string{`"a"`, `"b"`} {
		item := lines(errText, "  "+m+": chat-stream-tools")
		if len(item) != 1 || !strings.Contains(item[0], "requests up to 2048 output tokens per request") ||
			!strings.Contains(item[0], "1048576 response bytes") || !strings.Contains(item[0], "may load the model once") {
			t.Fatalf("plan item for %s: %q", m, item)
		}
	}
	if got := lines(errText, `check result "a"`); len(got) != 1 || !strings.Contains(got[0], "chat=yes stream=yes tool_call=yes") {
		t.Fatalf("result for a: %q", got)
	}
	if got := lines(errText, `check result "b"`); len(got) != 1 || !strings.Contains(got[0], "tool_call=inconclusive (request_rejected)") ||
		!strings.Contains(got[0], "2 requests sent") {
		t.Fatalf("result for b: %q", got)
	}
	if strings.Contains(stdout.String(), "check") {
		t.Fatalf("check output reached stdout:\n%s", stdout)
	}

	plain, _ := e.mustRun(t, srv)
	norm := func(s string) string { return mcAge.ReplaceAllString(s, "<age>") }
	if norm(stdout.String()) != norm(plain.String()) {
		t.Fatalf("stdout differs from the plain listing\n got:\n%s\nwant:\n%s", stdout, plain)
	}
	if first := strings.SplitN(stdout.String(), "\n", 2)[0]; !strings.HasPrefix(first, "lc/b\t") {
		t.Fatalf("first line %q; want the chain's first entry", first)
	}
}

// TestGolemModels_StoreDisabledCannotProbe pins today's message when the store
// cannot be opened at all: no check is sent and the listing still prints.
func TestGolemModels_StoreDisabledCannotProbe(t *testing.T) {
	old := openModelsStore
	openModelsStore = func(context.Context, func(string) string, string) (*fingerprint.SharedStore, string) {
		return nil, "fingerprint cache disabled: test"
	}
	t.Cleanup(func() { openModelsStore = old })
	srv := newMCServer(t)
	srv.ids = []string{"mystery"}
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "mystery"))
	for _, flag := range []string{"-probe-all", "-reprobe"} {
		stdout, stderr := e.mustRun(t, srv, flag)
		want := "cannot probe: capability-probe store unavailable (fingerprint cache disabled: test); showing cached provenance only"
		if !strings.Contains(stderr.String(), want) || strings.Contains(stderr.String(), "check plan") {
			t.Fatalf("%s stderr:\n%s", flag, stderr)
		}
		if !strings.Contains(stdout.String(), "lc/mystery\t") {
			t.Fatalf("%s stdout:\n%s", flag, stdout)
		}
	}
	if checks := srv.totalChecks(); checks != 0 {
		t.Fatalf("checks = %d; want 0", checks)
	}
}

// TestGolemModels_JSONReportsKnownNegative pins that -json reports a checked no
// as a known no, so the candidate is ineligible rather than unknown, and that
// an answer for a listed m:latest does not answer for a configured m.
func TestGolemModels_JSONReportsKnownNegative(t *testing.T) {
	candidate := func(t *testing.T, out string, sel string) configview.Candidate {
		t.Helper()
		var snap configview.Snapshot
		if err := json.Unmarshal([]byte(out), &snap); err != nil {
			t.Fatal(err)
		}
		for _, b := range snap.Bindings {
			for _, c := range b.Candidates {
				if c.Selector == sel {
					return c
				}
			}
		}
		t.Fatalf("no candidate %q in:\n%s", sel, out)
		return configview.Candidate{}
	}

	t.Run("checked no", func(t *testing.T) {
		srv := newMCServer(t)
		srv.ids = []string{"mystery"}
		e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "mystery"))
		out, _ := e.mustRun(t, srv, "-json")
		if c := candidate(t, out.String(), "lc/mystery"); c.Eligibility != "unknown" {
			t.Fatalf("control: candidate %+v; want unknown with nothing checked", c)
		}
		mcSeed(t, e.root, mcToolAnswers(t, srv.URL, "mystery", fingerprint.CapProbeNo, time.Now().Add(-time.Hour))...)
		out, _ = e.mustRun(t, srv, "-json")
		c := candidate(t, out.String(), "lc/mystery")
		if c.Eligibility != "ineligible" || !slices.Contains(c.Reasons, "missing_capability:tool_call") {
			t.Fatalf("candidate %+v; want ineligible for a known tool_call no", c)
		}
	})

	t.Run("m:latest does not answer for m", func(t *testing.T) {
		const digest = "sha256:d1"
		srv := newMCServer(t)
		srv.tags = []mcTag{{"m:latest", digest}}
		e := newMCEnv(t, mcConfig(srv.URL, "ollama", "m", "m:latest"))
		show := fingerprint.ProbeAnswer{Capability: "tool_call", Source: fingerprint.EvidenceSourceProvider, Kind: fingerprint.EvidenceKindAnswer, State: fingerprint.CapProbeNo}
		mcSeed(t, e.root, mcAnswers(t, srv.URL, "ollama", "m:latest", digest, fingerprint.MethodOllamaShow, time.Now().Add(-time.Hour), show)...)
		out, _ := e.mustRun(t, srv, "-json")
		if c := candidate(t, out.String(), "lc/m:latest"); c.Eligibility != "ineligible" {
			t.Fatalf("listed m:latest: %+v; want ineligible", c)
		}
		if c := candidate(t, out.String(), "lc/m"); c.Eligibility == "ineligible" {
			t.Fatalf("configured m: %+v; the m:latest listing must not answer for it", c)
		}
	})
}

// cancelFixture is an openai-compat model whose first check request gets a
// clean 400, so a second request would follow. onFirst runs when request one
// arrives; the handler then waits for the request to be abandoned, or up to
// five seconds, before answering 400.
func cancelFixture(t *testing.T, onFirst func()) (*mcServer, mcEnv) {
	t.Helper()
	srv := newMCServer(t)
	srv.ids = []string{"mystery"}
	srv.reply = func(w http.ResponseWriter, r *http.Request, _ string, n int) {
		if n == 0 && onFirst != nil {
			onFirst()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
		http.Error(w, "bad request", http.StatusBadRequest)
	}
	return srv, newMCEnv(t, mcConfig(srv.URL, "openai-compat", "mystery"))
}

// wantCancelled fails unless the run sent one request, returned the context
// error and printed exactly one cancelled line.
func wantCancelled(t *testing.T, srv *mcServer, err error, stderr string) {
	t.Helper()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runModels = %v; want the context error\nstderr:\n%s", err, stderr)
	}
	if got := srv.checksOf("mystery"); got != 1 {
		t.Fatalf("check requests = %d; want 1, no second request after the cancel", got)
	}
	if got := lines(stderr, "cancelled"); len(got) != 1 || got[0] != "cancelled" {
		t.Fatalf("cancelled lines %q; want exactly one\nstderr:\n%s", got, stderr)
	}
}

// TestGolemModels_CancelledReportsOnce pins that cancelling the parent context
// during a check sends nothing more and says so once. The control shows the
// fixture sends a second request when nothing cancels.
func TestGolemModels_CancelledReportsOnce(t *testing.T) {
	t.Run("control", func(t *testing.T) {
		srv, e := cancelFixture(t, nil)
		_, _ = e.mustRun(t, srv, "-probe-all")
		if got := srv.checksOf("mystery"); got != 2 {
			t.Fatalf("check requests = %d; want 2", got)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		srv, e := cancelFixture(t, cancel)
		stdout, stderr := &mcWriter{srv: srv}, &mcWriter{srv: srv}
		err := e.run(ctx, stdout, stderr, "-probe-all")
		wantCancelled(t, srv, err, stderr.String())
		if rows := mcRows(t, e.root, srv.URL, "openai-compat", "mystery"); len(rows) != 0 {
			t.Fatalf("rows = %d; want none from a cancelled check", len(rows))
		}
	})
}

// TestModelsReprobe_RemoteWithoutAllowSendsNothing pins that a remote chain
// entry without -allow-destination stops -reprobe before any request, the
// loopback entry's check included.
func TestModelsReprobe_RemoteWithoutAllowSendsNothing(t *testing.T) {
	var requests sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Store(r.URL.Path, true)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	e := newMCEnv(t, fmt.Sprintf(`{
		"providers": {"lc": {"base_url": %q, "api_format": "openai-compat"}, "far": {"base_url": "http://checks.example.test", "api_format": "openai-compat"}},
		"models": {"r0": {"name": "mystery", "provider": "lc", "type": "dense", "fallbacks": ["r1"]}, "r1": {"name": "remote", "provider": "far", "type": "dense"}},
		"defaults": {"agent": "r0"}
	}`, srv.URL))
	var stdout, stderr bytes.Buffer
	err := e.run(t.Context(), &stdout, &stderr, "-reprobe")
	if err == nil || !strings.Contains(err.Error()+stderr.String(), "-allow-destination") {
		t.Fatalf("runModels = %v; want the admission refusal naming -allow-destination\nstderr:\n%s", err, stderr.String())
	}
	sent := 0
	requests.Range(func(any, any) bool { sent++; return true })
	if sent != 0 {
		t.Fatal("a request reached the loopback endpoint")
	}
}

// TestModelsReprobe_ExplicitEntrySendsNothing pins that a forced check never
// plans an entry whose capabilities are declared.
func TestModelsReprobe_ExplicitEntrySendsNothing(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"mystery"}
	e := newMCEnv(t, fmt.Sprintf(`{
		"providers": {"lc": {"base_url": %q, "api_format": "openai-compat"}},
		"models": {"r0": {"name": "mystery", "provider": "lc", "type": "dense", "capabilities": ["chat", "stream", "tool_call"]}},
		"defaults": {"agent": "r0"}
	}`, srv.URL))
	stdout, stderr := e.mustRun(t, srv, "-reprobe")
	if !strings.Contains(stderr.String(), "nothing to check") || !strings.Contains(stdout.String(), "tool_call=yes (explicit)") {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if checks := srv.totalChecks(); checks != 0 {
		t.Fatalf("checks = %d; want 0", checks)
	}
}

// TestGolemModels_ListingCancelledReportsOnce pins that cancelling a plain
// listing (no check) while it reads an entry stops it: the context error, one
// cancelled line, nothing more sent, and no connectivity diagnostic on stdout
// for the interrupted entry or any after it. A listing of [a, b] sends five
// requests: the bootstrap listing, then a lookup and a tool_call explanation
// per entry; the cancel lands in a's lookup or in its explanation.
func TestGolemModels_ListingCancelledReportsOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   int32 // the request during which the caller cancels
	}{
		{"during a lookup", 2},
		{"during a tool_call explanation", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == tc.at {
					cancel()
					select {
					case <-r.Context().Done():
					case <-time.After(5 * time.Second):
					}
				}
				_, _ = io.WriteString(w, `{"data":[{"id":"a"},{"id":"b"}]}`)
			}))
			t.Cleanup(srv.Close)
			e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "a", "b"))
			var stdout, stderr bytes.Buffer
			err := e.run(ctx, &stdout, &stderr)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("runModels = %v; want the context error\nstdout:\n%s", err, stdout.String())
			}
			if got := lines(stderr.String(), "cancelled"); len(got) != 1 || got[0] != "cancelled" {
				t.Fatalf("cancelled lines %q; want exactly one\nstderr:\n%s", got, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout after the cancel:\n%s", stdout.String())
			}
			if got := requests.Load(); got != tc.at {
				t.Fatalf("requests = %d; want %d, none after the cancel", got, tc.at)
			}
		})
	}
}

// A recommendation-only config has no entries to look up after bootstrap.
// Cancel during its refresh, before there is any connection to a provider.
func TestGolemModels_RecommendationListingCancelledDuringBootstrap(t *testing.T) {
	e := newMCEnv(t, `{
		"providers": {"p": {"base_url": "http://127.0.0.1:1", "api_format": "openai-compat"}},
		"models": {"chat": {"name": "m", "provider": "p", "type": "dense"}},
		"defaults": {"chat": "chat"}
	}`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GetConn: func(string) { cancel() }})
	var stdout, stderr bytes.Buffer
	err := e.run(ctx, &stdout, &stderr)
	if !errors.Is(err, context.Canceled) || stdout.Len() != 0 {
		t.Fatalf("cancelled recommendation listing returned %v, stdout %q", err, stdout.String())
	}
	if got := lines(stderr.String(), "cancelled"); !slices.Equal(got, []string{"cancelled"}) {
		t.Fatalf("cancellation lines = %q; want one", got)
	}
}

// limitedPlanWriter simulates a destination that accepts only a prefix.
type limitedPlanWriter struct {
	left int
	err  error
}

func (w *limitedPlanWriter) Write(p []byte) (int, error) {
	n := min(w.left, len(p))
	w.left -= n
	if n < len(p) {
		return n, w.err
	}
	return n, nil
}

func TestGolemModels_IncompletePlanSendsNoCheck(t *testing.T) {
	c := modelsCheck{cfg: &config.Config{
		Providers: map[string]config.ProviderConfig{"p": {BaseURL: "http://127.0.0.1:1", APIFormat: "openai-compat"}},
		Models:    map[string]config.ModelConfig{"role": {Name: "m", Provider: "p", Type: "dense"}},
	}}
	diskFull := errors.New("disk full")
	for _, tc := range []struct {
		name string
		left int
		err  error
		want error
	}{
		{"header fails", 0, diskFull, diskFull},
		{"item partially written", 70, diskFull, diskFull},
		{"short write without error", 70, nil, io.ErrShortWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempted atomic.Bool
			ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
				GetConn: func(string) { attempted.Store(true) },
			})
			err := c.checkProvider(ctx, "p", []string{"m"}, &limitedPlanWriter{left: tc.left, err: tc.err})
			if !errors.Is(err, tc.want) || attempted.Load() {
				t.Fatalf("checkProvider returned %v, connection attempted %t; want %v before any connection", err, attempted.Load(), tc.want)
			}
		})
	}
}

// TestGolemModels_MemoryOnlyStoreSaysSo pins that a check saved to a store that
// fell back to memory says so, since its answer lasts only for the command. A
// file-backed store's "saved" is pinned by
// TestGolemModels_ReprobeOpenAICompatRunsForcedPlan.
func TestGolemModels_MemoryOnlyStoreSaysSo(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"mystery"}
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "mystery"))
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", blocker) // the store cannot be created, so it falls back to memory
	stdout, stderr := &mcWriter{srv: srv}, &mcWriter{srv: srv}
	if err := e.run(t.Context(), stdout, stderr, "-probe-all"); err != nil {
		t.Fatalf("golem models -probe-all: %v\nstderr:\n%s", err, stderr)
	}
	got := lines(stderr.String(), `check result "mystery"`)
	if len(got) != 1 || !strings.HasSuffix(got[0], "; 1 request sent; saved (memory only)") {
		t.Fatalf("result lines %q; want one saved to memory only\nstderr:\n%s", got, stderr)
	}
}

// TestPrintCheckPlan_LoadWording pins what a plan says it may load: nothing for
// a metadata read, the model once at a loopback endpoint, and, at a remote
// endpoint, what the server decides (ModelLoads 0 there is not "none").
func TestPrintCheckPlan_LoadWording(t *testing.T) {
	tests := []struct {
		name         string
		outputTokens int
		loads        int
		want         string
	}{
		{"metadata read", 0, 0, "loads no model"},
		{"loopback generation", 2048, 1, "may load the model once"},
		{"remote generation", 2048, 0, "the server may load the model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			if err := printCheckPlan(&out, configio.CheckPlan{Provider: "p", EndpointHost: "h:1", Items: []configio.CheckItem{{
				Model: "m", Methods: []string{"x"}, Capabilities: []string{"tool_call"}, UsualRequests: 1, MaxRequests: 2,
				MaxOutputTokensPerRequest: tt.outputTokens, ModelLoads: tt.loads,
			}}}); err != nil {
				t.Fatal(err)
			}
			item := lines(out.String(), `  "m": `)
			if len(item) != 1 || !strings.HasSuffix(item[0], "; "+tt.want) {
				t.Fatalf("plan item %q; want it to end %q", item, tt.want)
			}
		})
	}
}

// failingAppendStore is a store whose check-result writes fail.
type failingAppendStore struct{ fingerprint.EvidenceStore }

func (failingAppendStore) AppendCheckResult(context.Context, string, string, string, []fingerprint.EvidenceRow, *fingerprint.ModelAccess) error {
	return errors.New("disk full")
}

// TestGolemModels_StoreWriteFailureIsNotAFailedCheck pins that a completed
// check the store did not keep says "store write failed", while a failed
// check keeps a plain "not saved".
func TestGolemModels_StoreWriteFailureIsNotAFailedCheck(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"good", "bad"}
	srv.reply = func(w http.ResponseWriter, _ *http.Request, model string, _ int) {
		if model == "bad" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, mcCalled)
	}
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "good", "bad"))
	cfg, err := config.Load(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := mcOpen(t, e.root)
	t.Cleanup(func() { _ = h.Close() })
	c := &modelsCheck{cfg: cfg, store: failingAppendStore{h.Log()}}
	var errOut strings.Builder
	if err := c.checkProvider(t.Context(), "lc", []string{"good", "bad"}, &errOut); err != nil {
		t.Fatal(err)
	}
	if got := lines(errOut.String(), `check result "good"`); len(got) != 1 || !strings.HasSuffix(got[0], "; 1 request sent; not saved (store write failed)") {
		t.Fatalf("completed check: %q\n%s", got, errOut.String())
	}
	if got := lines(errOut.String(), `check result "bad"`); len(got) != 1 || !strings.HasSuffix(got[0], ": server_error; 1 request sent; not saved") {
		t.Fatalf("failed check: %q\n%s", got, errOut.String())
	}
}

// TestGolemCheckOutput_QuotesEndpointHost pins that a host from config is
// printed quoted on the plan header and the automatic progress line, so a
// right-to-left override reaches the terminal escaped, not as the raw rune.
func TestGolemCheckOutput_QuotesEndpointHost(t *testing.T) {
	host := provider.EndpointHost("http://ev\u202eil:11434")
	if host == "" {
		t.Fatal("fixture host did not parse")
	}
	var plan, progress strings.Builder
	if err := printCheckPlan(&plan, configio.CheckPlan{Provider: "p", EndpointHost: host}); err != nil {
		t.Fatal(err)
	}
	probeProgressPrinter(&progress)(provider.CapabilityProbeProgress{
		Key: provider.ModelKey{Provider: "p", Model: "m"}, EndpointHost: host, Method: "chat-stream-tools",
		Stage: "started", ReservedRequests: 2, MaxOutputTokensPerRequest: 2048,
	})
	for name, out := range map[string]string{"plan": plan.String(), "progress": progress.String()} {
		if strings.ContainsRune(out, '\u202e') || !strings.Contains(out, `"ev\u202eil:11434"`) {
			t.Fatalf("%s line %q; want the host quoted and escaped", name, out)
		}
	}
}
