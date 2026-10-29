package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The production-path fixture's synthetic remote origin. It is in the
// httptest certificate, so TLS verifies against the test server.
const (
	outboundOrigin   = "https://example.com"
	outboundDialAddr = "example.com:443"
	outboundAnswer   = "remote answer"
	// dispatchSensitiveSummaryText is dispatchSensitiveSummary, spelled out.
	dispatchSensitiveSummaryText = "Partial result unavailable: blocked: sensitive content"
	// dispatchSensitiveErrorText is dispatchSensitiveError, spelled out, with
	// the suffix dispatch adds because a sensitive block withholds the
	// child's model identity.
	dispatchSensitiveErrorText = "blocked: sensitive content; child model identity unavailable"
)

// outboundBackend is a TLS openai-compat server reached as outboundOrigin.
// It records every chat POST and, separately, counts the requests on any path
// whose raw body carries the fixture, before anything decodes it: the requests
// the content guard must stop.
type outboundBackend struct {
	mu           sync.Mutex
	bodies       []string
	fixturePosts int
}

func (b *outboundBackend) fixturePostCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fixturePosts
}

func (b *outboundBackend) chatBodies() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.bodies)
}

// outboundEnv is one run() environment: a temp workspace, data dir and
// config whose only provider is "remote" at outboundOrigin, with api_key
// "${GOLEM_T_KEY}" set to the fixture through t.Setenv.
type outboundEnv struct {
	configPath, root, data string
	backend                *outboundBackend
}

// newOutboundEnv starts the backend, answering each chat request with
// script's SSE chunks, and routes http.DefaultTransport to it. The guarded
// remote clients clone http.DefaultTransport and wrap its dialer only for
// "localhost" (provider/destination_transport.go), so they dial the server
// while admission, the destination guard and the content guard all see
// outboundOrigin, a non-loopback destination.
func newOutboundEnv(t *testing.T, fixture string, script func(wireRequest) []string) outboundEnv {
	t.Helper()
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	// run() reads os.UserConfigDir (consultants.json, REPL commands); keep the
	// developer's real config out of the run.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GOLEM_T_KEY", fixture)

	b := &outboundBackend{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if leaksFixture(string(body)) {
			b.mu.Lock()
			b.fixturePosts++
			b.mu.Unlock()
		}
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"agent-model"}]}`)
		case "/v1/chat/completions":
			var req wireRequest
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			b.mu.Lock()
			b.bodies = append(b.bodies, string(body))
			b.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			for _, c := range script(req) {
				_, _ = io.WriteString(w, "data: "+c+"\n\n")
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	if !slices.Contains(srv.Certificate().DNSNames, "example.com") {
		t.Fatal("the httptest certificate does not name example.com")
	}
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	addr := srv.Listener.Addr().String()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
			if target != outboundDialAddr {
				return nil, fmt.Errorf("unexpected dial to %s", target)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}
	orig := http.DefaultTransport
	http.DefaultTransport = tr
	t.Cleanup(func() {
		http.DefaultTransport = orig
		tr.CloseIdleConnections()
	})

	configPath := filepath.Join(t.TempDir(), "models.json")
	t.Setenv("GO_LLM_CONFIG", configPath)
	configJSON := fmt.Sprintf(`{
  "providers": {"remote": {"base_url": %q, "api_format": "openai-compat", "api_key": "${GOLEM_T_KEY}", "timeout": "5s"}},
  "models": {"agent": {"name": "agent-model", "provider": "remote", "type": "dense", "context_window": 32768,
    "capabilities": ["chat", "generate", "stream", "tool_call"]}},
  "defaults": {"agent": "agent", "summarize": "agent"}
}`, outboundOrigin)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return outboundEnv{configPath: configPath, root: t.TempDir(), data: data, backend: b}
}

// run drives run() with goal on piped stdin and returns stdout and stderr
// together. The REPL reports a failed turn on stdout and returns nil.
func (e outboundEnv) run(t *testing.T, goal string, extra ...string) string {
	t.Helper()
	stdin, stdout, stderr := runTestFiles(t)
	if _, err := io.WriteString(stdin, goal+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-config", e.configPath, "-root", e.root,
		"-allow-destination", "remote/" + outboundOrigin, "-trace",
		"-no-probe", "-no-cap-probe", "-no-memory", "-no-rag",
		"-no-project-context", "-no-git-context", "-no-auto-index", "-no-editor"}, extra...)
	err := run(args, stdin, stdout, stderr)
	out := readRunTestFile(t, stdout) + readRunTestFile(t, stderr)
	if err != nil {
		t.Fatal("run failed")
	}
	return out
}

func (e outboundEnv) traceFiles(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.data, "golem", "traces"))
	if err != nil {
		t.Fatal("cannot inspect the trace directory")
	}
	return len(entries)
}

func (e outboundEnv) savedSessions(t *testing.T) int {
	t.Helper()
	s, _, err := openSession(context.Background(), filepath.Join(e.data, "golem", "sessions.db"), "workspace:probe")
	if err != nil {
		t.Fatal("cannot open the session store")
	}
	defer func() { _ = s.Close() }()
	list, err := s.store.List(context.Background())
	if err != nil {
		t.Fatal("cannot list sessions")
	}
	return len(list)
}

// turnOf reports the last user message of req and how many tool messages
// follow it, so a script can tell a parent turn from a dispatch child's.
func turnOf(req wireRequest) (user string, tools int) {
	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			user, tools = m.Content, 0
		case "tool":
			tools++
		}
	}
	return user, tools
}

// TestOutboundDenialProductionPath (#424 D6 Production installation, consult
// 424-F6): from flag parsing through config load, bootstrap, parent and
// child construction, to the transport, against a non-loopback destination.
// The fixture is a configured key of no known shape. With -interceptors no
// request carrying it is sent; without the flag the same request is. Not
// parallel: newOutboundEnv swaps http.DefaultTransport.
func TestOutboundDenialProductionPath(t *testing.T) {
	fixture := configuredSecretFixture()
	answer := func(wireRequest) []string { return sseAnswer(outboundAnswer) }

	// The goal carries the key. The guard refuses the first chat request, so
	// the run fails with the fixed message and leaves no trace and no session
	// row; the control shows the same run sends it, then saves both.
	t.Run("parent", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			args    []string
			guarded bool
		}{
			{"interceptors", []string{"-interceptors"}, true},
			{"control: no flag", nil, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				env := newOutboundEnv(t, fixture, answer)
				out := env.run(t, "deploy with "+fixture, tc.args...)
				fixturePosts := env.backend.fixturePostCount()
				if !tc.guarded {
					if fixturePosts != 1 {
						t.Errorf("fixture POSTs = %d, want 1: the control must reach the server", fixturePosts)
					}
					if !strings.Contains(out, outboundAnswer) || strings.Contains(out, sensitiveFixedMessage) {
						t.Error("the control run did not complete with the server's answer")
					}
					if env.traceFiles(t) != 1 || env.savedSessions(t) != 1 {
						t.Error("the control run did not write its trace and session row")
					}
					return
				}
				if fixturePosts != 0 {
					t.Errorf("fixture POSTs = %d, want 0", fixturePosts)
				}
				if !strings.Contains(out, "error: "+sensitiveFixedMessage+"\n") || !strings.Contains(out, sensitiveTraceWarning+"\n") {
					t.Error("the denied run did not print the fixed message and the trace warning")
				}
				if strings.Contains(out, outboundAnswer) {
					t.Error("the denied run printed the server's answer")
				}
				// Tripwire: the denial's error never carries the key, so this
				// cannot fail now; the fixed-message check above discriminates.
				if leaksFixture(out) {
					t.Error("the denied run printed the fixture")
				}
				if n := env.traceFiles(t); n != 0 {
					t.Errorf("trace files = %d, want 0", n)
				}
				if n := env.savedSessions(t); n != 0 {
					t.Errorf("session rows = %d, want 0", n)
				}
			})
		}
	})

	// The goal is clean; secret.txt holds the key. The parent dispatches a
	// child that reads it, so the child's next request would carry the tool
	// result. With -interceptors the child's guard refuses it and the parent's
	// dispatch result is the fixed sensitive summary.
	t.Run("dispatch child", func(t *testing.T) {
		const parentGoal, childTask = "survey the workspace", "read secret.txt"
		script := func(req wireRequest) []string {
			user, tools := turnOf(req)
			switch {
			case user == childTask && tools == 0:
				return sseToolCall("r1", "read_file", `{"path":"secret.txt"}`)
			case user == childTask:
				return sseAnswer("child answer")
			case tools == 0:
				return sseToolCall("d1", "dispatch", `{"tasks":["`+childTask+`"]}`)
			}
			return sseAnswer(outboundAnswer)
		}
		for _, tc := range []struct {
			name    string
			args    []string
			guarded bool
		}{
			{"interceptors", []string{"-interceptors", "-dispatch"}, true},
			{"control: no flag", []string{"-dispatch"}, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				env := newOutboundEnv(t, fixture, script)
				if err := os.WriteFile(filepath.Join(env.root, "secret.txt"), []byte("deploy key: "+fixture+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				env.run(t, parentGoal, tc.args...)
				fixturePosts := env.backend.fixturePostCount()
				if !tc.guarded {
					if fixturePosts < 1 {
						t.Errorf("fixture POSTs = %d, want at least 1: the control must reach the server", fixturePosts)
					}
					return
				}
				if fixturePosts != 0 {
					t.Errorf("fixture POSTs = %d, want 0", fixturePosts)
				}
				var summaries, errs []string
				for _, body := range env.backend.chatBodies() {
					var req wireRequest
					if err := json.Unmarshal([]byte(body), &req); err != nil {
						t.Fatal(err)
					}
					for _, m := range req.toolMessages() {
						if m.ToolCallID != "d1" {
							continue
						}
						var envelope dispatchTestEnvelope
						if err := json.Unmarshal([]byte(unframe(t, m.Content)), &envelope); err != nil || len(envelope.Results) != 1 {
							t.Fatal("the dispatch result is not a one-child envelope")
						}
						summaries = append(summaries, envelope.Results[0].Summary)
						errs = append(errs, envelope.Results[0].Error)
					}
				}
				if !slices.Equal(summaries, []string{dispatchSensitiveSummaryText}) {
					t.Errorf("parent's dispatch results = %q, want one %q", summaries, dispatchSensitiveSummaryText)
				}
				if !slices.Equal(errs, []string{dispatchSensitiveErrorText}) {
					t.Errorf("parent's dispatch errors = %q, want one %q", errs, dispatchSensitiveErrorText)
				}
			})
		}
	})
}
