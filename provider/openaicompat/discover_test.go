package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// discoverServer builds a /v1/models httptest server returning the given ids
// with the given status. It records the last Authorization header seen.
func discoverServer(t *testing.T, status int, ids []string, lastAuth *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lastAuth != nil {
			lastAuth.Store(r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/models" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		if status/100 != 2 {
			return
		}
		type entry struct {
			ID string `json:"id"`
		}
		data := make([]entry, len(ids))
		for i, id := range ids {
			data[i] = entry{ID: id}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rawServer returns a fixed raw body (for malformed-JSON cases).
func rawServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDiscoverBaseURL_FirstCandidateWins(t *testing.T) {
	hit := discoverServer(t, http.StatusOK, []string{"gemma4:31b"}, nil)
	var secondHits atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
	}))
	t.Cleanup(second.Close)

	got, err := DiscoverBaseURL(context.Background(), []string{hit.URL, second.URL}, "gemma4:31b")
	if err != nil {
		t.Fatalf("DiscoverBaseURL error: %v", err)
	}
	if got != hit.URL {
		t.Fatalf("got %q, want first candidate %q", got, hit.URL)
	}
	if n := secondHits.Load(); n != 0 {
		t.Fatalf("second candidate probed %d times, want 0 (first hit wins)", n)
	}
}

func TestDiscoverBaseURL_ScanHitAtLaterCandidate(t *testing.T) {
	wrongModel := discoverServer(t, http.StatusOK, []string{"other-model"}, nil)
	notFound := discoverServer(t, http.StatusNotFound, nil, nil)
	hit := discoverServer(t, http.StatusOK, []string{"a", "gemma4:31b"}, nil)

	// "http://127.0.0.1:1" is a reliably-refused connection.
	cands := []string{"http://127.0.0.1:1", wrongModel.URL, notFound.URL, hit.URL}
	got, err := DiscoverBaseURL(context.Background(), cands, "gemma4:31b")
	if err != nil {
		t.Fatalf("DiscoverBaseURL error: %v", err)
	}
	if got != hit.URL {
		t.Fatalf("got %q, want %q", got, hit.URL)
	}
}

func TestDiscoverBaseURL_RejectsNearMissIDs(t *testing.T) {
	// Exact id match only: prefixes, suffixes, and case variants are rejected.
	srv := discoverServer(t, http.StatusOK,
		[]string{"gemma4:31b-extra", "prefix-gemma4:31b", "GEMMA4:31B"}, nil)
	_, err := DiscoverBaseURL(context.Background(), []string{srv.URL}, "gemma4:31b")
	if err == nil {
		t.Fatal("want error for near-miss ids, got nil")
	}
}

func TestDiscoverBaseURL_RejectsMalformedAndEmpty(t *testing.T) {
	tests := []struct {
		name string
		srv  func(t *testing.T) *httptest.Server
	}{
		{"non-JSON body", func(t *testing.T) *httptest.Server { return rawServer(t, "<html>nope</html>") }},
		{"missing data key", func(t *testing.T) *httptest.Server { return rawServer(t, `{"object":"list"}`) }},
		{"empty data", func(t *testing.T) *httptest.Server { return discoverServer(t, http.StatusOK, nil, nil) }},
		{"non-2xx", func(t *testing.T) *httptest.Server {
			return discoverServer(t, http.StatusInternalServerError, nil, nil)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := tt.srv(t)
			if _, err := DiscoverBaseURL(context.Background(), []string{srv.URL}, "gemma4:31b"); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestDiscoverBaseURL_ErrorNamesCandidatesRedacted(t *testing.T) {
	srv := discoverServer(t, http.StatusOK, []string{"other"}, nil)
	// Candidate with userinfo: must appear redacted, never with the password.
	_, err := DiscoverBaseURL(context.Background(),
		[]string{"http://user:sekret@127.0.0.1:1", srv.URL}, "gemma4:31b")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, "sekret") {
		t.Fatalf("error leaks userinfo: %q", msg)
	}
	if !strings.Contains(msg, srv.URL) {
		t.Fatalf("error does not name tried candidate %q: %q", srv.URL, msg)
	}
	if !strings.Contains(msg, `"gemma4:31b"`) {
		t.Fatalf("error does not name the wanted model: %q", msg)
	}
}

func TestDiscoverBaseURL_EmptyInputs(t *testing.T) {
	if _, err := DiscoverBaseURL(context.Background(), nil, "m"); err == nil {
		t.Fatal("want error for no candidates, got nil")
	}
	srv := discoverServer(t, http.StatusOK, []string{"m"}, nil)
	if _, err := DiscoverBaseURL(context.Background(), []string{srv.URL}, ""); err == nil {
		t.Fatal("want error for empty wantModel, got nil")
	}
}

func TestDiscoverBaseURL_ContextCancellationStopsScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv := discoverServer(t, http.StatusOK, []string{"gemma4:31b"}, nil)
	_, err := DiscoverBaseURL(ctx, []string{srv.URL}, "gemma4:31b")
	if err == nil {
		t.Fatal("want context error, got nil")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("want context.Canceled in error, got: %v", err)
	}
}

func TestRedactURLUserinfo(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		// Parses OK (Opaque form) but User is nil, Host is empty, and the
		// string still contains "@": treated as unparseable-looking and
		// redacted wholesale rather than printed with the raw "@" segment.
		{"parses opaque with unresolved userinfo-looking text", "not a url with @ in it", "<invalid url>"},
		// Fails url.Parse (invalid URL escape) and contains "@".
		{"fails to parse and contains @", "%zz@host", "<invalid url>"},
		// Fails url.Parse (invalid URL escape) and does NOT contain "@".
		{"fails to parse without @", "%zz", "%zz"},
		{"happy path strips userinfo", "http://u:p@127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"no userinfo unchanged", "http://127.0.0.1:8080", "http://127.0.0.1:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactURLUserinfo(tt.raw); got != tt.want {
				t.Fatalf("redactURLUserinfo(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestDiscoverBaseURL_CancelDuringFinalProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		http.NotFound(w, r) // miss: the outer loop must observe ctx.Err(), not treat this as "no candidate serves"
	}))
	t.Cleanup(srv.Close)

	_, err := DiscoverBaseURL(ctx, []string{srv.URL}, "gemma4:31b")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
	if strings.Contains(err.Error(), "no candidate serves") {
		t.Fatalf("err masks cancellation behind the generic no-candidate message: %v", err)
	}
}

func TestDiscoverBaseURL_SendsAPIKey(t *testing.T) {
	var auth atomic.Value
	srv := discoverServer(t, http.StatusOK, []string{"gemma4:31b"}, &auth)
	_, err := DiscoverBaseURL(context.Background(), []string{srv.URL}, "gemma4:31b", WithAPIKey("k123"))
	if err != nil {
		t.Fatalf("DiscoverBaseURL error: %v", err)
	}
	if got, _ := auth.Load().(string); got != "Bearer k123" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer k123")
	}
}
