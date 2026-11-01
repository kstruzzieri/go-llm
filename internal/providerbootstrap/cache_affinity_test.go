package providerbootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// TestCacheAffinityReachesTransport: cache_affinity reaches the openai-compat
// client, and an empty value leaves the client to infer from the host, which
// for a loopback test server is none.
func TestCacheAffinityReachesTransport(t *testing.T) {
	for _, tc := range []struct{ affinity, want string }{
		{"opencode", "thread-1"},
		{"", ""},
	} {
		t.Run("cache_affinity="+tc.affinity, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("x-opencode-session")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			}))
			defer srv.Close()
			p, _, err := buildProvider("hosted", config.ProviderConfig{
				BaseURL: srv.URL, APIFormat: "openai-compat", CacheAffinity: tc.affinity,
			}, nil, provider.Destination{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Chat(context.Background(), provider.ChatRequest{
				Model: "m", Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}}, SessionID: "thread-1",
			}); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if got != tc.want {
				t.Errorf("x-opencode-session = %q, want %q", got, tc.want)
			}
		})
	}
}
