// Package opsfixture serves fake llama-swap v235 and Ollama runtime surfaces
// for golem ops tests. Every request is recorded before routing, and every
// model-dispatched route counts as a would-be model load, so a test can prove
// both that observation happened and that nothing could have loaded a model.
package opsfixture

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Sentinels are unique strings placed in fields golem ops must never retain.
const (
	SentinelCmd    = "SENTINEL-CMD-7f3a"
	SentinelProxy  = "SENTINEL-PROXY-7f3a"
	SentinelError  = "SENTINEL-ERROR-7f3a"
	SentinelMeta   = "SENTINEL-META-7f3a"
	SentinelPath   = "SENTINEL-PATH-7f3a"
	SentinelQuery  = "SENTINEL-QUERY-7f3a"
	SentinelAPIKey = "SENTINEL-APIKEY-7f3a"
)

// AllSentinels lists every sentinel for absence checks.
var AllSentinels = []string{SentinelCmd, SentinelProxy, SentinelError, SentinelMeta, SentinelPath, SentinelQuery, SentinelAPIKey}

// Default bodies. DefaultRunning carries cmd and proxy sentinels.
const (
	DefaultVersion = `{"build_date":"2026-07-03T16:20:46Z","commit":"c59816b","version":"v235"}`
	DefaultRunning = `{"running":[{"model":"gemma4:31b","state":"ready","cmd":"` + SentinelCmd + ` --model /m.gguf","proxy":"http://127.0.0.1:5800/` + SentinelProxy + `","ttl":600,"name":"","description":""}]}`
	DefaultModels  = `{"object":"list","data":[{"id":"gemma4:31b","object":"model","owned_by":"llama-swap"},{"id":"qwen3-embedding:8b","object":"model","owned_by":"llama-swap"},{"id":"peer-model","object":"model","owned_by":"llama-swap","meta":{"llamaswap":{"peerID":"lab"}}}]}`
)

// Request is one recorded request: method and raw request URI.
type Request struct{ Method, URI string }

// Row is one llama-swap v235 activity row, in the server's own JSON shape.
type Row struct {
	ID         int
	At         time.Time
	Model      string
	Path       string
	Status     int
	Input      int
	Output     int
	Cache      int
	PromptPS   float64
	TokensPS   float64
	DurationMs int
	Error      string
	Meta       string
}

// MetricsJSON renders rows exactly as llama-swap v235's /api/metrics does,
// including fields golem ops must discard.
func MetricsJSON(rows ...Row) string {
	type tokens struct {
		Cache    int     `json:"cache_tokens"`
		Draft    int     `json:"draft_tokens"`
		DraftAcc int     `json:"draft_acc_tokens"`
		Input    int     `json:"input_tokens"`
		Output   int     `json:"output_tokens"`
		PromptPS float64 `json:"prompt_per_second"`
		TokensPS float64 `json:"tokens_per_second"`
	}
	type entry struct {
		ID              int               `json:"id"`
		Timestamp       time.Time         `json:"timestamp"`
		Model           string            `json:"model"`
		ReqPath         string            `json:"req_path"`
		RespContentType string            `json:"resp_content_type"`
		RespStatusCode  int               `json:"resp_status_code"`
		Tokens          tokens            `json:"tokens"`
		DurationMs      int               `json:"duration_ms"`
		HasCapture      bool              `json:"has_capture"`
		ErrorMsg        string            `json:"error_msg,omitempty"`
		Metadata        map[string]string `json:"metadata,omitempty"`
	}
	out := make([]entry, 0, len(rows))
	for _, r := range rows {
		e := entry{ID: r.ID, Timestamp: r.At, Model: r.Model, ReqPath: r.Path, RespContentType: "application/json",
			RespStatusCode: r.Status, DurationMs: r.DurationMs, ErrorMsg: r.Error,
			Tokens: tokens{Cache: r.Cache, Draft: -1, DraftAcc: -1, Input: r.Input, Output: r.Output, PromptPS: r.PromptPS, TokensPS: r.TokensPS}}
		if r.Meta != "" {
			e.Metadata = map[string]string{"client": r.Meta}
		}
		out = append(out, e)
	}
	b, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// DefaultRows returns three gemma rows near now: timings, usage-only, and a
// 500 whose error text and path carry sentinels.
func DefaultRows(now time.Time) []Row {
	return []Row{
		{ID: 0, At: now.Add(-3 * time.Minute), Model: "gemma4:31b", Path: "/v1/chat/completions", Status: 200, Input: 900, Output: 300, Cache: 100, PromptPS: 910.5, TokensPS: 41.25, DurationMs: 8200},
		{ID: 1, At: now.Add(-2 * time.Minute), Model: "gemma4:31b", Path: "/v1/chat/completions", Status: 200, Input: 1200, Output: 80, PromptPS: -1, TokensPS: -1, DurationMs: 2100, Meta: SentinelMeta},
		{ID: 2, At: now.Add(-1 * time.Minute), Model: "gemma4:31b", Path: "/" + SentinelPath + "/x?" + SentinelQuery + "=1", Status: 500, DurationMs: 40, Error: SentinelError},
	}
}

var dispatchExact = map[string]bool{
	"/props": true, "/slots": true, "/v1/chat/completions": true, "/v1/completions": true,
	"/v1/embeddings": true, "/completion": true, "/embedding": true, "/infill": true,
	"/unload": true, "/api/models/unload": true,
	// Also proxied to a model: they appear as activity-row req_path values.
	"/embeddings": true, "/v1/rerank": true, "/rerank": true, "/reranking": true,
}

func isDispatch(path string) bool {
	return dispatchExact[path] || strings.HasPrefix(path, "/upstream/") || strings.HasPrefix(path, "/api/models/unload/")
}

// Server is the shared recording core of both fakes.
type Server struct {
	srv   *httptest.Server
	Loads atomic.Int64 // model-dispatched requests, counted on receipt

	inflight    atomic.Int64
	maxInflight atomic.Int64

	mu           sync.Mutex
	requests     []Request
	bodies       map[string]string
	status       map[string]int
	hold         map[string]chan struct{}
	auth         []string
	pathInflight map[string]int64
}

func newServer(t testing.TB, bodies map[string]string) *Server {
	s := &Server{bodies: bodies, status: map[string]int{}, hold: map[string]chan struct{}{}, pathInflight: map[string]int64{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// NewLlamaSwap starts a fake llama-swap v235 with default bodies.
func NewLlamaSwap(t testing.TB) *Server {
	return newServer(t, map[string]string{
		"/api/version": DefaultVersion,
		"/running":     DefaultRunning,
		"/api/metrics": MetricsJSON(DefaultRows(time.Now().UTC())...),
		"/v1/models":   DefaultModels,
	})
}

// NewOllama starts a fake Ollama serving ps as /api/ps.
func NewOllama(t testing.TB, ps string) *Server {
	return newServer(t, map[string]string{"/api/ps": ps})
}

// URL is the server root.
func (s *Server) URL() string { return s.srv.URL }

// SetBody replaces the body served for an exact path.
func (s *Server) SetBody(path, body string) {
	s.mu.Lock()
	s.bodies[path] = body
	s.mu.Unlock()
}

// SetStatus makes an exact path answer status with a sentinel-bearing body.
func (s *Server) SetStatus(path string, status int) {
	s.mu.Lock()
	s.status[path] = status
	s.mu.Unlock()
}

// HoldFor blocks requests for path until the returned release runs or the
// client gives up. Release is idempotent and also runs at test cleanup, so a
// failed assertion can never leave server shutdown waiting on a handler.
func (s *Server) HoldFor(t testing.TB, path string) func() {
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	s.mu.Lock()
	s.hold[path] = ch
	s.mu.Unlock()
	return release
}

// InflightOn is the number of requests currently being served for path.
func (s *Server) InflightOn(path string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pathInflight[path]
}

// Requests returns every request received, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Auth returns every Authorization header received, in order.
func (s *Server) Auth() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth...)
}

// MaxInflight is the highest number of concurrent requests observed.
func (s *Server) MaxInflight() int64 { return s.maxInflight.Load() }

// Inflight is the number of requests currently being served.
func (s *Server) Inflight() int64 { return s.inflight.Load() }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	n := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for {
		m := s.maxInflight.Load()
		if n <= m || s.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	s.mu.Lock()
	s.requests = append(s.requests, Request{r.Method, r.RequestURI})
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	// Counted before any hold: a dispatch request whose client gives up still
	// reached a model route.
	dispatch := isDispatch(r.URL.Path)
	if dispatch {
		s.Loads.Add(1)
	}
	body, ok := s.bodies[r.URL.Path]
	status := s.status[r.URL.Path]
	hold := s.hold[r.URL.Path]
	s.pathInflight[r.URL.Path]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.pathInflight[r.URL.Path]--
		s.mu.Unlock()
	}()
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	if dispatch {
		_, _ = w.Write([]byte("{}"))
		return
	}
	if r.Method != http.MethodGet || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"` + SentinelError + `"}`))
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}
