package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// initializeBody is a JSON-RPC initialize request the SDK's streamable
// handler accepts.
const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`

// mcpHeaders sets the two headers the SDK's streamable handler requires on a
// POST.
func mcpHeaders(h http.Header) {
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/event-stream")
}

// newTestMCPServer returns a Server with a bare SDK server and one tool,
// "echo-extra", that reports the Authorization header and whether TokenInfo
// reached the handler as "authorization=<quoted> tokeninfo=<bool>". A
// non-empty token enables the HTTP bearer gate through its digest, the way
// finalizeHTTPBearer would.
func newTestMCPServer(t *testing.T, token string) *Server {
	t.Helper()

	s := &Server{
		mcpServer: gomcp.NewServer(&gomcp.Implementation{Name: "test", Version: "0"}, nil),
	}
	if token != "" {
		digest := sha256.Sum256([]byte(token))
		s.httpBearerDigest = &digest
	}
	s.mcpServer.AddTool(&gomcp.Tool{
		Name: "echo-extra", InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		text := fmt.Sprintf("authorization=%q tokeninfo=%t",
			req.Extra.Header.Get("Authorization"), req.Extra.TokenInfo != nil)
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: text}}}, nil
	})
	return s
}

// serveInitialize POSTs the initialize body to h after edit adjusts the
// request headers.
func serveInitialize(h http.Handler, edit func(http.Header)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(initializeBody))
	mcpHeaders(req.Header)
	if edit != nil {
		edit(req.Header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

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
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mock.Close()
	cfg := writeTestConfig(t, t.TempDir(), mock.URL)

	t.Run("token set", func(t *testing.T) {
		const tok = "Az09-._~+/abc=="
		s, err := NewServer(context.Background(), WithConfig(cfg), WithRAGDisabled(), WithHTTPBearerToken(tok))
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
		s, err := NewServer(context.Background(), WithConfig(cfg), WithRAGDisabled())
		if err != nil {
			t.Fatalf("NewServer() error = %v", err)
		}
		defer func() { _ = s.Close() }()

		if s.httpBearerDigest != nil {
			t.Errorf("httpBearerDigest = %x, want nil (authentication off)", *s.httpBearerDigest)
		}
	})
}

func TestPresentedHTTPBearer(t *testing.T) {
	tests := []struct {
		name   string
		auth   []string
		want   string
		wantOK bool
	}{
		{"token", []string{"Bearer tok"}, "tok", true},
		{"padded token", []string{"bearer   a/b+c=="}, "a/b+c==", true},
		{"no header", nil, "", false},
		{"two headers", []string{"Bearer tok", "Bearer tok"}, "", false},
		{"interior padding", []string{"Bearer a=b"}, "", false},
		{"empty token", []string{"Bearer "}, "", false},
		{"tab after the space", []string{"Bearer \ttok"}, "", false},
		{"trailing space", []string{"Bearer tok "}, "", false},
		{"trailing tab", []string{"Bearer tok\t"}, "", false},
		{"comma list", []string{"Bearer tok, Bearer tok"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for _, v := range tt.auth {
				h.Add("Authorization", v)
			}
			got, ok := presentedHTTPBearer(h)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("presentedHTTPBearer(%q) = (%q, %v), want (%q, %v)", tt.auth, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestRequireHTTPBearer(t *testing.T) {
	const tok = "Az09-._~+/abcDEF"
	want := sha256.Sum256([]byte(tok))
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:"+tok))

	tests := []struct {
		name   string
		auth   []string // Authorization values, one header line each
		url    string   // defaults to a plain /mcp URL
		header map[string]string
		pass   bool
	}{
		{name: "Bearer", auth: []string{"Bearer " + tok}, pass: true},
		{name: "lowercase scheme", auth: []string{"bearer " + tok}, pass: true},
		{name: "uppercase scheme", auth: []string{"BEARER " + tok}, pass: true},
		{name: "three spaces", auth: []string{"Bearer   " + tok}, pass: true},

		{name: "no header"},
		{name: "tab separator", auth: []string{"Bearer\t" + tok}},
		{name: "Basic scheme", auth: []string{basic}},
		{name: "space then tab", auth: []string{"Bearer \t" + tok}},
		{name: "scheme only", auth: []string{"Bearer"}},
		{name: "scheme and space only", auth: []string{"Bearer "}},
		{name: "wrong first byte", auth: []string{"Bearer B" + tok[1:]}},
		{name: "wrong last byte", auth: []string{"Bearer " + tok[:len(tok)-1] + "G"}},
		{name: "token plus a byte", auth: []string{"Bearer " + tok + "x"}},
		{name: "token minus a byte", auth: []string{"Bearer " + tok[:len(tok)-1]}},
		{name: "letter case changed", auth: []string{"Bearer Az09-._~+/abcdEF"}},
		{name: "two headers both valid", auth: []string{"Bearer " + tok, "Bearer " + tok}},
		{name: "comma-joined values", auth: []string{"Bearer " + tok + ", Bearer " + tok}},
		{name: "token only in query", url: "http://localhost/mcp?access_token=" + tok},
		{name: "token only in cookie", header: map[string]string{"Cookie": "token=" + tok}},
		{name: "token only in Proxy-Authorization", header: map[string]string{"Proxy-Authorization": "Bearer " + tok}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ })
			url := tt.url
			if url == "" {
				url = "http://localhost/mcp"
			}
			req := httptest.NewRequest(http.MethodPost, url, nil)
			for _, v := range tt.auth {
				req.Header.Add("Authorization", v)
			}
			for k, v := range tt.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()

			requireHTTPBearer(next, &want).ServeHTTP(rec, req)

			if tt.pass {
				if calls != 1 {
					t.Fatalf("next called %d times, want 1 (status %d)", calls, rec.Code)
				}
				return
			}
			if calls != 0 {
				t.Fatalf("next called %d times, want 0", calls)
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if got := rec.Body.String(); got != "unauthorized\n" {
				t.Errorf("body = %q, want %q", got, "unauthorized\n")
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="go-llm-mcp"` {
				t.Errorf("WWW-Authenticate = %q, want %q", got, `Bearer realm="go-llm-mcp"`)
			}

			// The response must echo neither the configured token nor
			// whatever token the client presented.
			text := rec.Body.String()
			for k, vs := range rec.Header() {
				text += "\n" + k + ": " + strings.Join(vs, "\n")
			}
			assertNoBearerEcho(t, text, tok)
			for _, v := range tt.auth {
				if _, presented, ok := strings.Cut(v, " "); ok {
					if presented = strings.TrimLeft(presented, " "); presented != "" {
						assertNoBearerEcho(t, text, presented)
					}
				}
			}
		})
	}
}

func TestRequireHTTPBearerStripsAuthorization(t *testing.T) {
	type ctxKey struct{}
	const tok = "Az09-._~+/abcDEF"
	want := sha256.Sum256([]byte(tok))

	var got *http.Request
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r })
	req := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, "kept"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Test", "kept")

	requireHTTPBearer(next, &want).ServeHTTP(httptest.NewRecorder(), req)

	if got == nil {
		t.Fatal("next was not called")
	}
	if v := got.Header.Values("Authorization"); len(v) != 0 {
		t.Errorf("next saw Authorization %q, want it stripped", v)
	}
	if v := got.Header.Get("X-Test"); v != "kept" {
		t.Errorf("X-Test = %q, want %q", v, "kept")
	}
	if v, _ := got.Context().Value(ctxKey{}).(string); v != "kept" {
		t.Errorf("context value = %q, want %q", v, "kept")
	}
}

// TestHTTPBearerComparatorIsConstantTime pins the shape of the secret
// comparison in the source: digestEqual is exactly one ConstantTimeCompare,
// hasHTTPBearer touches the stored digest only through digestEqual, and both
// gates touch it only through hasHTTPBearer.
func TestHTTPBearerComparatorIsConstantTime(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "http_auth.go", nil, 0)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil {
			funcs[fn.Name.Name] = fn
		}
	}

	t.Run("digestEqual", func(t *testing.T) {
		fn := funcs["digestEqual"]
		if fn == nil {
			t.Fatal("digestEqual not found in http_auth.go")
		}
		if len(fn.Body.List) != 1 {
			t.Fatalf("digestEqual has %d statements, want 1", len(fn.Body.List))
		}
		ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			t.Fatalf("digestEqual's statement is %T, want a single-value return", fn.Body.List[0])
		}
		const wantExpr = "subtle.ConstantTimeCompare(a[:], b[:]) == 1"
		if got := types.ExprString(ret.Results[0]); got != wantExpr {
			t.Errorf("digestEqual returns %q, want %q", got, wantExpr)
		}
	})

	// wantOnlyIn fails unless fn's want parameter is used exactly once, as an
	// argument to callee.
	wantOnlyIn := func(t *testing.T, name, callee string) {
		t.Helper()
		fn := funcs[name]
		if fn == nil {
			t.Fatalf("%s not found in http_auth.go", name)
		}
		allowed := map[*ast.Ident]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if fun, ok := call.Fun.(*ast.Ident); ok && fun.Name == callee {
				for _, arg := range call.Args {
					if id, ok := arg.(*ast.Ident); ok && id.Name == "want" {
						allowed[id] = true
					}
				}
			}
			return true
		})
		if len(allowed) != 1 {
			t.Errorf("%s passes want to %s %d times, want 1", name, callee, len(allowed))
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "want" && !allowed[id] {
				t.Errorf("%s uses want outside a %s argument at %v", name, callee, id.Pos())
			}
			return true
		})
	}
	t.Run("hasHTTPBearer", func(t *testing.T) { wantOnlyIn(t, "hasHTTPBearer", "digestEqual") })
	t.Run("requireHTTPBearer", func(t *testing.T) { wantOnlyIn(t, "requireHTTPBearer", "hasHTTPBearer") })
	t.Run("requireHTTPBearerBeforeH2C", func(t *testing.T) {
		wantOnlyIn(t, "requireHTTPBearerBeforeH2C", "hasHTTPBearer")
	})
}

func TestStreamableHTTPHandlerRequiresBearerWhenConfigured(t *testing.T) {
	const tok = "Az09-._~+/abc=="
	handler := streamableHTTPHandler(newTestMCPServer(t, tok))

	tests := []struct {
		name string
		edit func(http.Header)
		want int
	}{
		{"no header", nil, http.StatusUnauthorized},
		{"wrong token", func(h http.Header) { h.Set("Authorization", "Bearer wrong") }, http.StatusUnauthorized},
		{"configured token", func(h http.Header) { h.Set("Authorization", "Bearer "+tok) }, http.StatusOK},
		// The gate sits outside cross-origin protection: a hostile origin
		// without credentials is told 401, with them 403.
		{"hostile origin, no header", func(h http.Header) { h.Set("Origin", "https://malicious.example") }, http.StatusUnauthorized},
		{"hostile origin, configured token", func(h http.Header) {
			h.Set("Origin", "https://malicious.example")
			h.Set("Authorization", "Bearer "+tok)
		}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serveInitialize(handler, tt.edit)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body %q", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

// TestStreamableHTTPHandlerBodyLimitPrecedesBearer pins the gate inside the
// body limiter: an oversized declared body keeps its own status, with or
// without credentials.
func TestStreamableHTTPHandlerBodyLimitPrecedesBearer(t *testing.T) {
	const tok = "Az09-._~+/abc=="
	handler := streamableHTTPHandler(newTestMCPServer(t, tok))

	for _, auth := range []string{"", "Bearer " + tok} {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(initializeBody))
		mcpHeaders(req.Header)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		req.ContentLength = maxMCPHTTPBodyBytes + 1
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("Authorization %q: status = %d, want %d", auth, rec.Code, http.StatusRequestEntityTooLarge)
		}
	}
}

// TestHTTPBearerRequestsLogNothing is not parallel: it swaps the process
// logger. Nothing logs on the request path today; the test pins that a later
// log line cannot carry the configured or presented credential.
func TestHTTPBearerRequestsLogNothing(t *testing.T) {
	const (
		tok   = "Az09-._~+/abc=="
		wrong = "CANARY428wrong"
	)
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	handler := streamableHTTPHandler(newTestMCPServer(t, tok))
	for _, tt := range []struct {
		name string
		auth string
		want int
	}{
		{"configured token", "Bearer " + tok, http.StatusOK},
		{"wrong token", "Bearer " + wrong, http.StatusUnauthorized},
		{"no header", "", http.StatusUnauthorized},
	} {
		rec := serveInitialize(handler, func(h http.Header) {
			if tt.auth != "" {
				h.Set("Authorization", tt.auth)
			}
		})
		if rec.Code != tt.want {
			t.Fatalf("%s: status = %d, want %d", tt.name, rec.Code, tt.want)
		}
	}

	logged := buf.String()
	assertNoBearerEcho(t, logged, tok)
	assertNoBearerEcho(t, logged, wrong)
	if strings.Contains(logged, "CANARY428") {
		t.Errorf("log output echoes the presented value: %q", logged)
	}
}
