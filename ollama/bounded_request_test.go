package ollama_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/responsebound"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

const showReply = `{"capabilities":["completion","tools"],"template":"{{ .Prompt }}","details":{"family":"llama"}}`

func boundedCtx(t *testing.T) context.Context {
	t.Helper()
	recipe, ok := fingerprint.RecipeFor(fingerprint.MethodOllamaShow)
	if !ok {
		t.Fatal("no ollama-show recipe")
	}
	return responsebound.WithLimit(context.Background(), recipe.MaxResponseBytes)
}

func show(ctx context.Context, url string) error {
	_, err := ollama.NewClient(ollama.WithBaseURL(url)).ShowModel(ctx, "m")
	return err
}

// writeBrokenBody sends status with a Content-Length the body never reaches,
// then either closes the connection (a short body) or resets it.
func writeBrokenBody(t *testing.T, w http.ResponseWriter, status int, reset bool) {
	t.Helper()
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{\"error\":\"cut", status, http.StatusText(status))
	_ = buf.Flush()
	if reset {
		// Let the client take the headers before the reset discards unread data.
		time.Sleep(100 * time.Millisecond)
		_ = conn.(*net.TCPConn).SetLinger(0)
	}
	_ = conn.Close()
}

func statusOf(err error) (int, bool) {
	var hs interface{ HTTPStatusCode() int }
	if errors.As(err, &hs) {
		return hs.HTTPStatusCode(), true
	}
	return 0, false
}

type redirectPair struct {
	origin, target *httptest.Server
	targetHits     atomic.Int64
	originHits     atomic.Int64
}

func newRedirectPair(t *testing.T, code int) *redirectPair {
	t.Helper()
	rp := &redirectPair{}
	rp.target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rp.targetHits.Add(1)
		_, _ = io.WriteString(w, showReply)
	}))
	t.Cleanup(rp.target.Close)
	rp.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rp.originHits.Add(1)
		http.Redirect(w, r, rp.target.URL+r.URL.Path, code)
	}))
	t.Cleanup(rp.origin.Close)
	return rp
}

func TestOllamaBoundedRequest_RedirectRefused(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			rp := newRedirectPair(t, code)
			err := show(boundedCtx(t), rp.origin.URL)
			var apiErr *ollama.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != code {
				t.Fatalf("err = %v, want an *APIError for %d", err, code)
			}
			if got := rp.originHits.Load(); got != 1 {
				t.Errorf("origin saw %d requests, want 1", got)
			}
			if got := rp.targetHits.Load(); got != 0 {
				t.Errorf("redirect target saw %d requests, want 0", got)
			}
		})
	}
}

func TestOllamaBoundedRequest_ErrorBodyReadFailureKeepsStatusAndCause(t *testing.T) {
	for _, status := range []int{400, 403, 404, 422} {
		for _, mode := range []struct {
			name  string
			reset bool
			cause error
		}{
			{"short body", false, io.ErrUnexpectedEOF},
			{"reset", true, syscall.ECONNRESET},
		} {
			t.Run(fmt.Sprintf("%d %s", status, mode.name), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					writeBrokenBody(t, w, status, mode.reset)
				}))
				defer srv.Close()
				err := show(boundedCtx(t), srv.URL)
				var rre *responsebound.RejectionReadError
				if !errors.As(err, &rre) {
					t.Fatalf("err = %v (%T), want a *RejectionReadError", err, err)
				}
				if rre.StatusCode != status {
					t.Errorf("StatusCode = %d, want %d", rre.StatusCode, status)
				}
				if !errors.Is(err, mode.cause) {
					t.Errorf("err = %v, want errors.Is %v", err, mode.cause)
				}
				var apiErr *ollama.APIError
				if errors.As(err, &apiErr) {
					t.Errorf("a failed read became an *APIError: %v", err)
				}
				if code, ok := statusOf(err); ok {
					t.Errorf("err exposes HTTPStatusCode() = %d", code)
				}
			})
		}
	}
}

func TestOllamaBoundedRequest_CappedErrorBodyIsAPIError(t *testing.T) {
	for name, body := range map[string]string{
		"over 4 KiB": `{"error":"` + strings.Repeat("x", 8*1024) + `"}`,
		"empty":      "",
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			err := show(boundedCtx(t), srv.URL)
			var apiErr *ollama.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
				t.Fatalf("err = %v, want an *APIError for 404", err)
			}
		})
	}
}

func TestOllamaOrdinaryRequest_Unchanged(t *testing.T) {
	t.Run("a plain client follows a 307", func(t *testing.T) {
		rp := newRedirectPair(t, http.StatusTemporaryRedirect)
		if err := show(context.Background(), rp.origin.URL); err != nil {
			t.Fatal(err)
		}
		if got := rp.targetHits.Load(); got != 1 {
			t.Errorf("target saw %d requests, want 1", got)
		}
	})
	for _, status := range []int{400, 429, 500} {
		for _, reset := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d reset=%v", status, reset), func(t *testing.T) {
				broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					writeBrokenBody(t, w, status, reset)
				}))
				defer broken.Close()
				clean := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":"cut"}`)
				}))
				defer clean.Close()

				brokenErr := show(context.Background(), broken.URL)
				cleanErr := show(context.Background(), clean.URL)
				var apiErr *ollama.APIError
				if !errors.As(brokenErr, &apiErr) || apiErr.StatusCode != status {
					t.Fatalf("broken body: err = %v, want an *APIError for %d", brokenErr, status)
				}
				if bi, ci := provider.IsInfrastructureError(brokenErr), provider.IsInfrastructureError(cleanErr); bi != ci {
					t.Errorf("IsInfrastructureError: broken body %v, clean body %v", bi, ci)
				}
			})
		}
	}
}
