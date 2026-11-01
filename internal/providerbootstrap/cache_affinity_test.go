package providerbootstrap

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// TestCacheAffinityReachesTransport pins every value config accepts for
// cache_affinity to what the openai-compat client puts on the wire, so a mode
// config accepts but the client does not know cannot silently become none.
// Empty leaves the client to infer from the host: none for a loopback server.
func TestCacheAffinityReachesTransport(t *testing.T) {
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	const session = "thread-1"
	for _, tc := range []struct {
		affinity string
		check    func(t *testing.T, opencode, sessionHdr string, cacheKey *string)
	}{
		{"opencode", func(t *testing.T, oc, sh string, key *string) {
			if oc != session || sh != "" || key != nil {
				t.Errorf("x-opencode-session=%q x-session-id=%q prompt_cache_key=%v, want only the raw session id", oc, sh, key)
			}
		}},
		{"openai", func(t *testing.T, oc, sh string, key *string) {
			if key == nil || !hex32.MatchString(*key) || oc != "" || sh != "" {
				t.Errorf("x-opencode-session=%q x-session-id=%q prompt_cache_key=%v, want only a 32-hex prompt_cache_key", oc, sh, key)
			}
		}},
		{"openrouter", func(t *testing.T, oc, sh string, key *string) {
			if !hex32.MatchString(sh) || oc != "" || key != nil {
				t.Errorf("x-opencode-session=%q x-session-id=%q prompt_cache_key=%v, want only a 32-hex x-session-id", oc, sh, key)
			}
		}},
		{"none", nil},
		{"", nil},
	} {
		t.Run("cache_affinity="+tc.affinity, func(t *testing.T) {
			var oc, sh string
			var body struct {
				Key *string `json:"prompt_cache_key"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				oc, sh = r.Header.Get("x-opencode-session"), r.Header.Get("x-session-id")
				raw, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Errorf("request body: %v", err)
				}
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
				Model: "m", Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}}, SessionID: session,
			}); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if tc.check != nil {
				tc.check(t, oc, sh, body.Key)
			} else if oc != "" || sh != "" || body.Key != nil {
				t.Errorf("x-opencode-session=%q x-session-id=%q prompt_cache_key=%v, want nothing", oc, sh, body.Key)
			}
		})
	}
}
