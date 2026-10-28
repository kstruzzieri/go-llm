package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// assertNoBearerEcho fails when text contains the bearer token, the
// "Bearer <token>" header value, or the token's SHA-256 digest in any
// rendering a diagnostic might use (hex and the three base64 alphabets).
// The digest is recomputed here, independently of the server under test.
func assertNoBearerEcho(t *testing.T, text, token string) {
	t.Helper()

	digest := sha256.Sum256([]byte(token))
	forbidden := map[string]string{
		"digest (hex)":           fmt.Sprintf("%x", digest),
		"digest (base64 std)":    base64.StdEncoding.EncodeToString(digest[:]),
		"digest (base64 url)":    base64.URLEncoding.EncodeToString(digest[:]),
		"digest (base64 rawurl)": base64.RawURLEncoding.EncodeToString(digest[:]),
	}
	// Every string contains "", so the token forms are only meaningful for a
	// non-empty token.
	if token != "" {
		forbidden["token"] = token
		forbidden["Authorization value"] = "Bearer " + token
	}
	for what, needle := range forbidden {
		if strings.Contains(text, needle) {
			t.Errorf("text echoes the %s %q: %q", what, needle, text)
		}
	}
}

func TestValidHTTPBearerToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{"single letter", "a", true},
		{"all punctuation classes", "Az09-._~+/", true},
		{"trailing padding", "abc==", true},
		{"max length", strings.Repeat("a", 4096), true},

		{"empty", "", false},
		{"padding only", "=", false},
		{"leading padding", "=abc", false},
		{"interior padding", "a=b", false},
		{"space", "a b", false},
		{"tab", "a\tb", false},
		{"trailing newline", "tok\n", false},
		{"trailing carriage return", "tok\r", false},
		{"leading space", " tok", false},
		{"trailing space", "tok ", false},
		{"comma", "a,b", false},
		{"quote", "a\"b", false},
		{"non-ASCII", "é", false},
		{"over max length", strings.Repeat("a", 4097), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validHTTPBearerToken(tt.token); got != tt.want {
				t.Fatalf("validHTTPBearerToken(%q) = %v, want %v", tt.token, got, tt.want)
			}
		})
	}
}

func TestNewServerRejectsInvalidHTTPBearerTokenBeforeSideEffects(t *testing.T) {
	const canary = "CANARY428"

	tests := []struct {
		name  string
		token string
	}{
		{name: "empty", token: ""},
		{name: "space", token: canary + " has space"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int64
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer mock.Close()

			dir := t.TempDir()
			ragDB := filepath.Join(dir, "rag.db")
			transcriptDB := filepath.Join(dir, "t.db")
			feedbackDB := filepath.Join(dir, "f.db")
			memoryDB := filepath.Join(dir, "m.db")

			s, err := NewServer(context.Background(),
				WithConfig(writeTestConfig(t, dir, mock.URL)),
				WithRAGPath(ragDB),
				WithTranscriptStore(transcriptDB),
				WithRetrievalFeedback(feedbackDB),
				WithAgentMemoryPath(memoryDB),
				WithHTTPBearerToken(tt.token),
			)
			if err == nil {
				_ = s.Close()
				t.Fatal("NewServer() error = nil, want ErrInvalidHTTPBearerToken")
			}
			if !errors.Is(err, ErrInvalidHTTPBearerToken) {
				t.Fatalf("NewServer() error = %v, want errors.Is ErrInvalidHTTPBearerToken", err)
			}
			for _, path := range []string{ragDB, transcriptDB, feedbackDB, memoryDB} {
				if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
					t.Errorf("os.Stat(%q) error = %v, want fs.ErrNotExist (no initialization side effects)", path, statErr)
				}
			}
			if got := requests.Load(); got != 0 {
				t.Errorf("mock provider got %d requests, want 0", got)
			}
			assertNoBearerEcho(t, err.Error(), tt.token)
			if strings.Contains(err.Error(), canary) {
				t.Errorf("error echoes the rejected value: %q", err)
			}
		})
	}

	// With a valid explicit config, Step 1 has no observable side effect, so
	// only a missing config can show validation running after it.
	t.Run("missing_config", func(t *testing.T) {
		dir := t.TempDir()
		s, err := NewServer(context.Background(),
			WithConfig(filepath.Join(dir, "missing.json")),
			WithHTTPBearerToken(canary+" has space"),
		)
		if err == nil {
			_ = s.Close()
			t.Fatal("NewServer() error = nil, want ErrInvalidHTTPBearerToken")
		}
		if !errors.Is(err, ErrInvalidHTTPBearerToken) {
			t.Fatalf("NewServer() error = %v, want ErrInvalidHTTPBearerToken before the config load", err)
		}
		if strings.Contains(err.Error(), canary) {
			t.Errorf("error echoes the rejected value: %q", err)
		}
	})
}

func TestFinalizeHTTPBearerKeepsOnlyDigest(t *testing.T) {
	t.Run("token set", func(t *testing.T) {
		const tok = "Az09-._~+/abc=="
		s, err := NewServer(context.Background(), WithRAGDisabled(), WithHTTPBearerToken(tok))
		if err != nil {
			t.Fatalf("NewServer() error = %v", err)
		}
		defer func() { _ = s.Close() }()

		if s.httpBearerToken != "" {
			t.Errorf("httpBearerToken = %q, want it cleared after finalize", s.httpBearerToken)
		}
		if s.httpBearerDigest == nil {
			t.Fatal("httpBearerDigest = nil, want the SHA-256 of the token")
		}
		if want := sha256.Sum256([]byte(tok)); *s.httpBearerDigest != want {
			t.Errorf("httpBearerDigest = %x, want %x", *s.httpBearerDigest, want)
		}
	})

	t.Run("option omitted", func(t *testing.T) {
		s, err := NewServer(context.Background(), WithRAGDisabled())
		if err != nil {
			t.Fatalf("NewServer() error = %v", err)
		}
		defer func() { _ = s.Close() }()

		if s.httpBearerDigest != nil {
			t.Errorf("httpBearerDigest = %x, want nil (authentication off)", *s.httpBearerDigest)
		}
	})
}
