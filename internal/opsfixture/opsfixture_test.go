package opsfixture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLlamaSwapRecordsEveryRequestAndCountsDispatch(t *testing.T) {
	f := NewLlamaSwap(t)
	for _, p := range []string{"/api/version", "/running", "/props?model=x", "/upstream/m/props", "/nope"} {
		resp, err := http.Get(f.URL() + p)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	got := f.Requests()
	want := []Request{{"GET", "/api/version"}, {"GET", "/running"}, {"GET", "/props?model=x"}, {"GET", "/upstream/m/props"}, {"GET", "/nope"}}
	if len(got) != len(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d = %v, want %v", i, got[i], want[i])
		}
	}
	if n := f.Loads.Load(); n != 2 {
		t.Fatalf("would-be loads = %d, want 2 (/props and /upstream)", n)
	}
}

func TestLlamaSwapStatusOverrideCarriesSentinel(t *testing.T) {
	f := NewLlamaSwap(t)
	f.SetStatus("/api/metrics", http.StatusInternalServerError)
	resp, err := http.Get(f.URL() + "/api/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 500 || !strings.Contains(string(body), SentinelError) {
		t.Fatalf("status/body = %d %q", resp.StatusCode, body)
	}
}

// waitFor polls cond for up to 2s.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

// abandonHeld sends a request to a held path, waits until the server is
// serving it, cancels the client, and reports whether the handler returned.
func abandonHeld(t *testing.T, f *Server, method, uri string, body io.Reader) bool {
	t.Helper()
	p, _, _ := strings.Cut(uri, "?")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx, method, f.URL()+uri, body)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	if !waitFor(func() bool { return f.InflightOn(p) == 1 }) {
		t.Fatalf("%s %s never reached the server", method, uri)
	}
	cancel()
	<-done
	return waitFor(func() bool { return f.InflightOn(p) == 0 })
}

func TestHeldDispatchCountsAsLoadWhenClientGivesUp(t *testing.T) {
	f := NewLlamaSwap(t)
	f.HoldFor(t, "/props")
	if !abandonHeld(t, f, http.MethodGet, "/props?model=x", nil) {
		t.Fatal("held handler did not return after the client gave up")
	}
	if n := f.Loads.Load(); n != 1 {
		t.Fatalf("would-be loads = %d, want 1: a dispatch request counts when received, even if never answered", n)
	}
	if got := f.Requests(); len(got) != 1 || got[0] != (Request{"GET", "/props?model=x"}) {
		t.Fatalf("requests = %v, want the held dispatch recorded on receipt", got)
	}
}

func TestHeldRequestWithBodyReturnsWhenClientGivesUp(t *testing.T) {
	f := NewLlamaSwap(t)
	f.HoldFor(t, "/v1/chat/completions")
	if !abandonHeld(t, f, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)) {
		t.Fatal("held handler with an unread body did not return after the client gave up")
	}
}

// TestEveryV235DispatchRouteCountsAsLoad lists llama-swap v235's
// model-dispatched routes independently of dispatchExact, plus the
// llama-server extras and non-canonical paths v235 redirects to a route.
func TestEveryV235DispatchRouteCountsAsLoad(t *testing.T) {
	f := NewLlamaSwap(t)
	paths := []string{
		// modelPostJSONRoutes
		"/v1/chat/completions", "/v1/responses", "/v1/completions", "/v1/messages",
		"/v1/messages/count_tokens", "/v1/embeddings", "/reranking", "/rerank", "/v1/rerank",
		"/v1/reranking", "/infill", "/completion", "/v1/audio/speech", "/v1/audio/voices",
		"/v1/images/generations", "/sdapi/v1/txt2img", "/sdapi/v1/img2img",
		"/v/chat/completions", "/v/responses", "/v/completions", "/v/messages",
		"/v/messages/count_tokens", "/v/embeddings", "/v/rerank", "/v/reranking",
		// modelPostFormRoutes
		"/v1/audio/transcriptions", "/v1/images/edits",
		// modelGetRoutes (/v1/audio/voices is listed above)
		"/sdapi/v1/loras", "/props",
		// operations and passthrough
		"/unload", "/api/models/unload", "/api/models/unload/m", "/upstream/m/v1/models",
		// llama-server extras
		"/slots", "/embedding",
		// ServeMux cleans these and redirects to /props
		"//props", "/x/../props",
	}
	for i, p := range paths {
		resp, err := http.Get(f.URL() + p)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if n := f.Loads.Load(); n != int64(i+1) {
			t.Fatalf("%s: would-be loads = %d, want %d", p, n, i+1)
		}
	}
	if got := f.Requests(); got[len(got)-1].URI != "/x/../props" {
		t.Fatalf("last request = %v, want the raw /x/../props", got[len(got)-1])
	}
}

func TestOllamaLoadAndMutateRoutesCountAsLoads(t *testing.T) {
	f := NewOllama(t, `{"models":[]}`)
	loads := []string{
		"/api/generate", "/api/chat", "/api/embed", "/api/embeddings", "/api/pull", "/api/push",
		"/api/create", "/api/copy", "/api/delete", "/api/blobs/sha256:x",
		"/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/responses",
	}
	reads := []string{"/api/ps", "/api/tags", "/api/version", "/api/show", "/v1/models", "/v1/models/llama3"}
	var want int64
	for i, p := range append(append([]string(nil), loads...), reads...) {
		resp, err := http.Post(f.URL()+p, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if i < len(loads) {
			want++
		}
		if n := f.Loads.Load(); n != want {
			t.Fatalf("%s: would-be loads = %d, want %d", p, n, want)
		}
	}
}

func TestOllamaOverrides(t *testing.T) {
	f := NewOllama(t, `{"models":[]}`)
	f.SetStatus("/api/version", http.StatusServiceUnavailable)
	f.SetBody("/api/tags", `{"models":[{"name":"x"}]}`)
	for _, tc := range []struct {
		path   string
		status int
		want   string
	}{
		{"/api/ps", 200, `{"models":[]}`},
		{"/api/version", 503, SentinelError},
		{"/api/tags", 200, `{"models":[{"name":"x"}]}`},
	} {
		resp, err := http.Get(f.URL() + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status || !strings.Contains(string(body), tc.want) {
			t.Fatalf("%s: status/body = %d %q, want %d containing %q", tc.path, resp.StatusCode, body, tc.status, tc.want)
		}
	}
}

func TestAuthRecordsBothCredentialHeaders(t *testing.T) {
	f := NewLlamaSwap(t)
	for _, h := range []map[string]string{
		{"Authorization": "Bearer k1"},
		{"x-api-key": "k2"},
		{"Authorization": "Bearer k1", "x-api-key": "k2"},
		{},
	} {
		req, _ := http.NewRequest(http.MethodGet, f.URL()+"/api/version", nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	want := []string{"Bearer k1", "\nx-api-key: k2", "Bearer k1\nx-api-key: k2", ""}
	if got := f.Auth(); !reflect.DeepEqual(got, want) {
		t.Fatalf("auth = %q, want %q", got, want)
	}
}

func TestContainsSentinel(t *testing.T) {
	for _, s := range AllSentinels {
		if !ContainsSentinel("x" + s + "y") {
			t.Fatalf("ContainsSentinel misses %q", s)
		}
	}
	for in, want := range map[string]bool{
		"sentinel-cmd-7f3a": true, // case-folded
		"clipped SENTINEL-": true,
		"Sentinel-Name":     true,
		"":                  false,
		"SENTINEL":          false,
		"sentinel cmd":      false,
	} {
		if got := ContainsSentinel(in); got != want {
			t.Fatalf("ContainsSentinel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDefaultBodiesMatchV235Shape(t *testing.T) {
	for name, body := range map[string]string{"version": DefaultVersion, "running": DefaultRunning, "models": DefaultModels} {
		if !strings.HasSuffix(body, "}\n") {
			t.Fatalf("%s body lacks json.Encoder's trailing newline", name)
		}
	}
	var running struct {
		Running []map[string]any `json:"running"`
	}
	if err := json.Unmarshal([]byte(DefaultRunning), &running); err != nil || len(running.Running) != 1 ||
		running.Running[0]["name"] != SentinelName || running.Running[0]["description"] != SentinelDescription ||
		!strings.Contains(fmt.Sprint(running.Running[0]["cmd"]), SentinelCmd) ||
		!strings.Contains(fmt.Sprint(running.Running[0]["proxy"]), SentinelProxy) {
		t.Fatalf("running = %+v, %v: cmd, proxy, name and description must carry sentinels", running, err)
	}
	var models struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(DefaultModels), &models); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range models.Data {
		ids = append(ids, fmt.Sprint(m["id"]))
		if _, ok := m["created"].(float64); !ok {
			t.Fatalf("model %v lacks v235's numeric created", m["id"])
		}
	}
	if strings.Join(ids, ",") != "gemma4:31b,qwen3-embedding:8b,peer-model" {
		t.Fatalf("model ids = %v", ids)
	}
	local := models.Data[1]
	meta, _ := local["meta"].(map[string]any)
	ls, _ := meta["llamaswap"].(map[string]any)
	if _, peer := ls["peerID"]; peer || ls == nil || !strings.Contains(fmt.Sprint(ls), SentinelModelMeta) ||
		local["name"] != SentinelName || local["description"] != SentinelDescription {
		t.Fatalf("local model = %+v: want sentinel name, description and a non-peer meta.llamaswap", local)
	}

	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	if loc := DefaultRows(now)[0].At.Location(); loc != time.UTC {
		t.Fatalf("DefaultRows zone = %v, want the caller's", loc)
	}
	bodies := DefaultRunning + DefaultModels + MetricsJSON(DefaultRows(now)...)
	for _, s := range regexp.MustCompile(`SENTINEL-[A-Z]+-7f3a`).FindAllString(bodies, -1) {
		if !slices.Contains(AllSentinels, s) {
			t.Fatalf("default bodies carry %s, which AllSentinels omits", s)
		}
	}
	for _, s := range AllSentinels {
		if s != SentinelAPIKey && !strings.Contains(bodies, s) {
			t.Fatalf("no default body carries %s", s)
		}
	}
	resp, err := http.Get(NewLlamaSwap(t).URL() + "/api/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var rows []struct {
		Timestamp   string            `json:"timestamp"`
		ReqPath     string            `json:"req_path"`
		ContentType string            `json:"resp_content_type"`
		ErrorMsg    string            `json:"error_msg"`
		Metadata    map[string]string `json:"metadata"`
		Tokens      struct {
			Cache int `json:"cache_tokens"`
		} `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil || len(rows) != 3 {
		t.Fatalf("metrics = %+v, %v", rows, err)
	}
	for _, r := range rows {
		if !strings.HasSuffix(r.Timestamp, "-04:00") {
			t.Fatalf("timestamp %q: want a local-time offset like v235's", r.Timestamp)
		}
	}
	if !strings.Contains(rows[0].ContentType, SentinelContentType) ||
		!strings.Contains(fmt.Sprint(rows[1].Metadata), SentinelMeta) ||
		!strings.Contains(rows[2].ErrorMsg, SentinelError) ||
		!strings.Contains(rows[2].ReqPath, SentinelPath) || !strings.Contains(rows[2].ReqPath, SentinelQuery) {
		t.Fatalf("metrics rows = %+v: resp_content_type, metadata, error_msg and req_path must carry sentinels", rows)
	}
	if rows[1].Tokens.Cache != -1 {
		t.Fatalf("usage-only row cache_tokens = %d, want -1 as v235 reports a missing count", rows[1].Tokens.Cache)
	}
}
