// Package opsfixture serves fake llama-swap v235 and Ollama runtime surfaces
// for golem ops tests. Every request is recorded before routing, and every
// route that could load, run or change a model counts as a would-be model load
// (Server.Loads), so a test can prove both that observation happened and that
// nothing could have loaded a model.
package opsfixture

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
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

	SentinelName        = "SENTINEL-NAME-7f3a"
	SentinelDescription = "SENTINEL-DESCRIPTION-7f3a"
	SentinelModelMeta   = "SENTINEL-MODELMETA-7f3a"
	SentinelContentType = "SENTINEL-CONTENTTYPE-7f3a"
)

// AllSentinels lists every sentinel for absence checks.
var AllSentinels = []string{SentinelCmd, SentinelProxy, SentinelError, SentinelMeta, SentinelPath, SentinelQuery, SentinelAPIKey,
	SentinelName, SentinelDescription, SentinelModelMeta, SentinelContentType}

// ContainsSentinel reports whether s holds any sentinel by their shared
// "SENTINEL-" prefix, case-insensitively, so a clipped or case-folded leak is
// still caught.
func ContainsSentinel(s string) bool {
	return strings.Contains(strings.ToLower(s), "sentinel-")
}

// Default bodies, ending in a newline like v235's json.Encoder output. Every
// never-retained field carries a sentinel: cmd, proxy, name and description in
// DefaultRunning; name, description and a non-peer meta.llamaswap on the local
// qwen3-embedding:8b model in DefaultModels.
//
// ponytail: v235 sorts /v1/models data by id; this order is kept because
// decoder tests pin it.
const (
	DefaultVersion = `{"build_date":"2026-07-03T16:20:46Z","commit":"c59816b","version":"v235"}` + "\n"
	DefaultRunning = `{"running":[{"model":"gemma4:31b","state":"ready","cmd":"` + SentinelCmd + ` --model /m.gguf","proxy":"http://127.0.0.1:5800/` + SentinelProxy + `","ttl":600,"name":"` + SentinelName + `","description":"` + SentinelDescription + `"}]}` + "\n"
	DefaultModels  = `{"data":[` +
		`{"id":"gemma4:31b","object":"model","created":1791158400,"owned_by":"llama-swap"},` +
		`{"id":"qwen3-embedding:8b","object":"model","created":1791158400,"owned_by":"llama-swap","name":"` + SentinelName + `","description":"` + SentinelDescription + `","meta":{"llamaswap":{"note":"` + SentinelModelMeta + `"}}},` +
		`{"id":"peer-model","object":"model","created":1791158400,"owned_by":"llama-swap","name":"lab: peer-model","meta":{"llamaswap":{"peerID":"lab"}}}` +
		`],"object":"list"}` + "\n"
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
	// ContentType is resp_content_type; empty means "application/json".
	ContentType string
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
		if r.ContentType != "" {
			e.RespContentType = r.ContentType
		}
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

// DefaultRows returns three gemma rows near now, in now's zone: timings (its
// content type carries a sentinel), usage-only (no cache count, so -1 as v235
// reports it; its metadata carries a sentinel), and a 500 whose error text and
// path carry sentinels.
func DefaultRows(now time.Time) []Row {
	return []Row{
		{ID: 0, At: now.Add(-3 * time.Minute), Model: "gemma4:31b", Path: "/v1/chat/completions", Status: 200, Input: 900, Output: 300, Cache: 100, PromptPS: 910.5, TokensPS: 41.25, DurationMs: 8200, ContentType: "application/json; " + SentinelContentType},
		{ID: 1, At: now.Add(-2 * time.Minute), Model: "gemma4:31b", Path: "/v1/chat/completions", Status: 200, Input: 1200, Output: 80, Cache: -1, PromptPS: -1, TokensPS: -1, DurationMs: 2100, Meta: SentinelMeta},
		{ID: 2, At: now.Add(-1 * time.Minute), Model: "gemma4:31b", Path: "/" + SentinelPath + "/x?" + SentinelQuery + "=1", Status: 500, DurationMs: 40, Error: SentinelError},
	}
}

// dispatchExact is every model-dispatched route in llama-swap v235
// internal/server/server.go modelPostJSONRoutes, modelPostFormRoutes,
// modelGetRoutes, plus /unload and /api/models/unload. /slots and /embedding
// are llama-server dispatch paths, kept as extra cover.
var dispatchExact = map[string]bool{
	"/v1/chat/completions": true, "/v1/responses": true, "/v1/completions": true,
	"/v1/messages": true, "/v1/messages/count_tokens": true, "/v1/embeddings": true,
	"/reranking": true, "/rerank": true, "/v1/rerank": true, "/v1/reranking": true,
	"/infill": true, "/completion": true, "/v1/audio/speech": true, "/v1/audio/voices": true,
	"/v1/images/generations": true, "/sdapi/v1/txt2img": true, "/sdapi/v1/img2img": true,
	"/v/chat/completions": true, "/v/responses": true, "/v/completions": true, "/v/messages": true,
	"/v/messages/count_tokens": true, "/v/embeddings": true, "/v/rerank": true, "/v/reranking": true,
	"/v1/audio/transcriptions": true, "/v1/images/edits": true,
	"/sdapi/v1/loras": true, "/props": true,
	"/unload": true, "/api/models/unload": true,
	"/slots": true, "/embedding": true,
}

func llamaSwapDispatch(p string) bool {
	return dispatchExact[p] || strings.HasPrefix(p, "/upstream/") || strings.HasPrefix(p, "/api/models/unload/")
}

// ollamaDispatchExact lists the native Ollama routes that load, run or change
// a model. ollamaDispatch adds /api/blobs/ (blob upload) and every /v1/ route
// except the /v1/models listing, so OpenAI-compatible routes newer Ollama
// releases add (/v1/responses, /v1/messages) count too.
var ollamaDispatchExact = map[string]bool{
	"/api/generate": true, "/api/chat": true, "/api/embed": true, "/api/embeddings": true,
	"/api/pull": true, "/api/push": true, "/api/create": true, "/api/copy": true, "/api/delete": true,
}

func ollamaDispatch(p string) bool {
	return ollamaDispatchExact[p] || strings.HasPrefix(p, "/api/blobs/") ||
		(strings.HasPrefix(p, "/v1/") && p != "/v1/models" && !strings.HasPrefix(p, "/v1/models/"))
}

// Server is the shared recording core of both fakes.
type Server struct {
	srv      *httptest.Server
	dispatch func(path string) bool
	Loads    atomic.Int64 // would-be loads (see package doc), counted on receipt

	mu           sync.Mutex
	requests     []Request
	bodies       map[string]string
	status       map[string]int
	hold         map[string]chan struct{}
	auth         []string
	pathInflight map[string]int64
}

func newServer(t testing.TB, bodies map[string]string, dispatch func(string) bool) *Server {
	s := &Server{dispatch: dispatch, bodies: bodies, status: map[string]int{}, hold: map[string]chan struct{}{}, pathInflight: map[string]int64{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// NewLlamaSwap starts a fake llama-swap v235 with default bodies. Its default
// rows are stamped in a fixed UTC-4 zone: v235 stamps rows in local time.
func NewLlamaSwap(t testing.TB) *Server {
	return newServer(t, map[string]string{
		"/api/version": DefaultVersion,
		"/running":     DefaultRunning,
		"/api/metrics": MetricsJSON(DefaultRows(time.Now().In(time.FixedZone("", -4*3600)))...),
		"/v1/models":   DefaultModels,
	}, llamaSwapDispatch)
}

// DefaultOllamaVersion is the fake Ollama's /api/version body.
const DefaultOllamaVersion = `{"version":"0.33.0"}`

// NewOllama starts a fake Ollama serving ps as /api/ps, and /api/version.
func NewOllama(t testing.TB, ps string) *Server {
	return newServer(t, map[string]string{"/api/ps": ps, "/api/version": DefaultOllamaVersion}, ollamaDispatch)
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
// client gives up. The request body is drained first, because net/http only
// notices a vanished client once the body has been read. Release is idempotent
// and also runs at test cleanup, so a failed assertion can never leave server
// shutdown waiting on a handler.
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

// Auth returns one entry per request, in order: the Authorization header
// (empty when absent), followed by "\nx-api-key: <value>" when the request
// also carries x-api-key, which v235 accepts as an API key too.
func (s *Server) Auth() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if k := r.Header.Get("X-Api-Key"); k != "" {
		auth += "\nx-api-key: " + k
	}
	s.mu.Lock()
	s.requests = append(s.requests, Request{r.Method, r.RequestURI})
	s.auth = append(s.auth, auth)
	// Counted before any hold: a dispatch request whose client gives up still
	// reached a model route. The cleaned path also counts: v235's ServeMux
	// redirects //props and /x/../props to /props.
	dispatch := s.dispatch(r.URL.Path) || s.dispatch(path.Clean(r.URL.Path))
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
	_, _ = io.Copy(io.Discard, r.Body)
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
