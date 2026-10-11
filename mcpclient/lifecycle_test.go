package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const lifecycleMarker = "mcpclient-lifecycle"

// TestMCPClientLifecycleHelper is not a test. Re-executed with
// lifecycleMarker MODE DIR, the test binary becomes a stdio server that
// writes its pid to DIR/pid, touches DIR/<method> for every request it reads
// (slashes as '_') and DIR/eof when its stdin closes, then exits. MODE
// "unsupported" answers initialize with a protocol version no SDK supports;
// "hang-init" never answers initialize; "hang-list" answers initialize and
// never answers tools/list; "exit" exits before reading anything; "serve" is
// a real server with tools ping and block (block returns only when its
// request is cancelled).
func TestMCPClientLifecycleHelper(t *testing.T) {
	i := slices.Index(os.Args, lifecycleMarker)
	if i < 0 || i+2 >= len(os.Args) {
		return
	}
	mode, dir := os.Args[i+1], os.Args[i+2]
	touch := func(name string, data []byte) {
		_ = os.WriteFile(filepath.Join(dir, strings.ReplaceAll(name, "/", "_")), data, 0o600)
	}
	touch("pid", []byte(strconv.Itoa(os.Getpid())))
	switch mode {
	case "exit":
		os.Exit(0)
	case "serve":
		srv := gomcp.NewServer(&gomcp.Implementation{Name: "lifecycle", Version: "1"}, nil)
		obj := map[string]any{"type": "object"}
		srv.AddTool(&gomcp.Tool{Name: "ping", InputSchema: obj}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "pong"}}}, nil
		})
		srv.AddTool(&gomcp.Tool{Name: "block", InputSchema: obj}, func(ctx context.Context, _ *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			touch("block", nil)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		_ = srv.Run(context.Background(), &gomcp.StdioTransport{})
		touch("eof", nil)
		os.Exit(0)
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Method == "" {
			continue
		}
		touch(msg.Method, nil)
		version := ""
		switch {
		case msg.Method == "initialize" && mode == "unsupported":
			version = "1999-01-01"
		case msg.Method == "initialize" && mode == "hang-list":
			version = msg.Params.ProtocolVersion
		}
		if version != "" {
			out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
				"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "lifecycle", "version": "1"},
			}})
			_, _ = os.Stdout.Write(append(out, '\n'))
		}
	}
	touch("eof", nil)
	os.Exit(0)
}

func lifecycleServer(t *testing.T, mode string) (Server, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := StdioServer("life", []string{exe, "-test.run=^TestMCPClientLifecycleHelper$", "--", lifecycleMarker, mode, dir})
	// Under -race every exit sleeps 1s by default, eating Close's grace before SIGTERM.
	return s.WithEnv(SetEnv("GORACE", "atexit_sleep_ms=0")), dir
}

// waitMarker reports whether the helper touched name within 10s. It returns
// an error instead of failing the test so goroutines may call it; "eof" means
// the transport closed the child's stdin, and the helper exits right after.
func waitMarker(dir, name string) error {
	path := filepath.Join(dir, strings.ReplaceAll(name, "/", "_"))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("helper never reached %q", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cancelAt cancels once the helper touches name (or the wait times out, so a
// missing marker can never leave the caller blocked); the returned channel
// carries the wait's result for the test goroutine to check.
func cancelAt(dir, name string, cancel context.CancelFunc) <-chan error {
	done := make(chan error, 1)
	go func() {
		err := waitMarker(dir, name)
		cancel()
		done <- err
	}()
	return done
}

func connectLifecycle(t *testing.T, ctx context.Context, s Server) (*Manager, []error) {
	t.Helper()
	le := testLaunchEnv(map[string]string{"PATH": "/usr/bin:/bin"})
	m, warns, err := connectWithHooks(ctx, Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: testPins(t)}, &connectHooks{launch: &le})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, warns
}

// wantAdmission requires the lone warning to be an *AdmissionError with the
// given reason.
func wantAdmission(t *testing.T, m *Manager, warns []error, reason string) {
	t.Helper()
	if len(m.Tools()) != 0 || len(warns) != 1 {
		t.Fatalf("got tools=%d warnings=%v, want one admission failure", len(m.Tools()), warns)
	}
	if got := admission(t, warns).Reason; got != reason {
		t.Fatalf("admission reason = %q, want %q", got, reason)
	}
}

// The #406 bug: go-sdk v1.6.1 returns the unsupported-version error without
// closing its session.
func TestConnectUnsupportedProtocolReleasesChild(t *testing.T) {
	s, dir := lifecycleServer(t, "unsupported")
	m, warns := connectLifecycle(t, context.Background(), s)
	wantAdmission(t, m, warns, "unavailable")
	if err := waitMarker(dir, "eof"); err != nil {
		t.Fatalf("%v: child not released", err)
	}
}

func TestConnectCanceledDuringInitializeReleasesChild(t *testing.T) {
	s, dir := lifecycleServer(t, "hang-init")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reached := cancelAt(dir, "initialize", cancel)
	m, warns := connectLifecycle(t, ctx, s)
	if err := <-reached; err != nil {
		t.Fatal(err)
	}
	wantAdmission(t, m, warns, "canceled")
	if err := waitMarker(dir, "eof"); err != nil {
		t.Fatalf("%v: child not released", err)
	}
}

// Partial startup: initialized, then cancelled before the catalog completes.
func TestConnectCanceledDuringListReleasesChild(t *testing.T) {
	s, dir := lifecycleServer(t, "hang-list")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reached := cancelAt(dir, "tools/list", cancel)
	m, warns := connectLifecycle(t, ctx, s)
	if err := <-reached; err != nil {
		t.Fatal(err)
	}
	wantAdmission(t, m, warns, "canceled")
	if err := waitMarker(dir, "eof"); err != nil {
		t.Fatalf("%v: child not released", err)
	}
}

// A session outlives its setup context (connectOne cancels its own
// WithTimeout ctx on return; endSetup also cancels the parent); cancelling one
// request leaves the shared server serving; closing the Manager twice returns
// promptly both times and releases the child. The second Close exercises
// Manager/session idempotency: JSON-RPC drops its closer after the first close,
// so it never reaches the transport again.
func TestSessionLifecycleAfterSetup(t *testing.T) {
	s, dir := lifecycleServer(t, "serve")
	setup, endSetup := context.WithCancel(context.Background())
	m, warns := connectLifecycle(t, setup, s)
	endSetup()
	if len(m.Tools()) != 2 {
		t.Fatalf("serve not admitted: %v", warns)
	}
	tool := func(name string) agent.Tool {
		for _, tl := range m.Tools() {
			if strings.HasSuffix(tl.Spec().Name, name) {
				return tl
			}
		}
		t.Fatalf("no tool %q", name)
		return nil
	}
	ping := func(when string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := tool("ping").Invoke(ctx, json.RawMessage(`{}`))
		if err != nil || res.IsError || res.Content != "pong" {
			t.Fatalf("ping %s = (%+v, %v)", when, res, err)
		}
	}
	ping("after setup")
	reqCtx, cancelReq := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelReq()
	reached := cancelAt(dir, "block", cancelReq)
	res, err := tool("block").Invoke(reqCtx, json.RawMessage(`{}`))
	if werr := <-reached; werr != nil {
		t.Fatal(werr)
	}
	if err == nil && !res.IsError {
		t.Fatalf("cancelled block returned success: %+v", res)
	}
	ping("after a cancelled request")
	if _, err := os.Stat(filepath.Join(dir, "eof")); err == nil {
		t.Fatal("request cancellation closed the server")
	}
	for i := range 2 {
		done := make(chan error, 1)
		go func() { done <- m.Close() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Close #%d: %v", i+1, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("Close #%d hung", i+1)
		}
	}
	if err := waitMarker(dir, "eof"); err != nil {
		t.Fatalf("%v: child not released", err)
	}
}

// HTTP: the same failure must delete the server-side session it was assigned.
// The redirect case answers that DELETE with a refused redirect, so Close
// fails with errRedirectRefused: the close error must stay in the chain
// without renaming the reason.
func TestConnectUnsupportedProtocolDeletesHTTPSession(t *testing.T) {
	for _, tt := range []struct {
		name     string
		redirect bool
	}{{"plain", false}, {"redirect", true}} {
		t.Run(tt.name, func(t *testing.T) {
			var deletes, strays atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					if r.Header.Get("Mcp-Session-Id") == "s1" {
						deletes.Add(1)
					} else {
						strays.Add(1)
					}
					if tt.redirect {
						http.Redirect(w, r, "/elsewhere", http.StatusFound)
					}
					return
				}
				var msg struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				_ = json.NewDecoder(r.Body).Decode(&msg)
				if msg.Method != "initialize" {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Mcp-Session-Id", "s1")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
					"protocolVersion": "1999-01-01", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "old", "version": "1"},
				}})
			}))
			defer srv.Close()
			m, warns := connectLifecycle(t, context.Background(), HTTPServer("api", srv.URL+"/mcp"))
			wantAdmission(t, m, warns, "unavailable")
			if got, stray := deletes.Load(), strays.Load(); got != 1 || stray != 0 {
				t.Fatalf("DELETE for session s1 = %d, without it = %d; want 1 and 0", got, stray)
			}
			failure := admission(t, warns)
			if got := errors.Is(failure, errRedirectRefused); got != tt.redirect {
				t.Fatalf("errors.Is(failure, errRedirectRefused) = %v, want %v (failure: %v)", got, tt.redirect, failure)
			}
		})
	}
}
