package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestRejectedDestinationCredentialsAreNotRendered(t *testing.T) {
	tests := []struct {
		name  string
		grant string
		want  string
	}{
		{name: "canonical userinfo", grant: "hosted/https://user:SECRET@api.example", want: "base URL must not carry userinfo"},
		{name: "legacy userinfo", grant: "hosted=https://user:SECRET@api.example", want: `expected "<provider>/<base URL>"`},
		{name: "canonical query", grant: "hosted/https://api.example?api_key=SECRET", want: "base URL must not carry a query"},
		{name: "legacy query", grant: "hosted=https://api.example?api_key=SECRET", want: `expected "<provider>/<base URL>"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestInvalidDestinationHelper$")
			cmd.Env = append(os.Environ(), "GO_LLM_MCP_TEST_GRANT="+tt.grant)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("go-llm-mcp -allow-destination %q error = nil, want rejection", tt.grant)
			}
			message := string(out)
			if strings.Contains(message, "SECRET") {
				t.Errorf("go-llm-mcp rendered rejected credential: %q", message)
			}
			for _, want := range []string{tt.want, "-allow-destination"} {
				if !strings.Contains(message, want) {
					t.Errorf("go-llm-mcp rejection = %q, want substring %q", message, want)
				}
			}
		})
	}
}

func TestInvalidDestinationHelper(t *testing.T) {
	grant := os.Getenv("GO_LLM_MCP_TEST_GRANT")
	if grant == "" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("go-llm-mcp", flag.ExitOnError)
	os.Args = []string{"go-llm-mcp", "-allow-destination", grant}
	main()
}

func TestDestinationGrantsParseRepeatableExactDestinations(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	var grants destinationGrants
	flags.Var(&grants, "allow-destination", "")
	if err := flags.Parse([]string{
		"-allow-destination", "hosted/https://HOST:443/base/",
		"-allow-destination=backup/http://example.com:80/root",
	}); err != nil {
		t.Fatalf("FlagSet.Parse() error = %v", err)
	}

	policy, err := grants.policy()
	if err != nil {
		t.Fatalf("destinationGrants.policy() error = %v", err)
	}
	want := []struct {
		provider string
		baseURL  string
	}{
		{provider: "hosted", baseURL: "https://host/base"},
		{provider: "backup", baseURL: "http://example.com/root"},
	}
	if len(grants) != len(want) {
		t.Fatalf("destination grants count = %d, want %d", len(grants), len(want))
	}
	for i, expected := range want {
		dest, err := provider.NewDestination(expected.provider, expected.baseURL)
		if err != nil {
			t.Fatal(err)
		}
		if !policy.Permits(dest) {
			t.Errorf("destination policy does not permit parsed grant %d (%s)", i, dest)
		}
	}
	sibling, err := provider.NewDestination("hosted", "https://host/other")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Permits(sibling) {
		t.Errorf("destination policy unexpectedly permits ungranted sibling %s", sibling)
	}
}

func TestDestinationGrantsPreserveEqualsInProviderName(t *testing.T) {
	var grants destinationGrants
	if err := grants.Set("team=prod/https://HOST:443/base/"); err != nil {
		t.Fatalf("destinationGrants.Set() error = %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("destination grants count = %d, want 1", len(grants))
	}
	policy, err := grants.policy()
	if err != nil {
		t.Fatalf("destinationGrants.policy() error = %v", err)
	}
	dest, err := provider.NewDestination("team=prod", "https://host/base")
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Permits(dest) {
		t.Errorf("destination policy does not permit parsed grant %s", dest)
	}
}

// -allow-destination accepts only canonical grants; legacy values fail
// during policy construction so flag parsing cannot echo a credential.
func TestDestinationGrantsCanonicalOnly(t *testing.T) {
	tests := []struct {
		name     string
		grant    string
		provider string
		baseURL  string
		wantErr  bool
	}{
		{name: "canonical https", grant: "hosted/https://HOST:443/base/", provider: "hosted", baseURL: "https://host/base"},
		{name: "canonical http", grant: "backup/http://example.com:80/root", provider: "backup", baseURL: "http://example.com/root"},
		{name: "legacy https", grant: "hosted=https://HOST:443/base/", wantErr: true},
		{name: "legacy provider with equals", grant: "team=prod=https://host/base", wantErr: true},
		{name: "legacy http", grant: "backup=http://example.com:80/root", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var grants destinationGrants
			if err := grants.Set(tt.grant); err != nil {
				t.Fatalf("destinationGrants.Set() error = %v", err)
			}
			policy, err := grants.policy()
			if tt.wantErr {
				if !errors.Is(err, provider.ErrDestinationInvalid) || !strings.Contains(err.Error(), `expected "<provider>/<base URL>"`) {
					t.Fatalf("destinationGrants.policy() error = %v, want canonical-grammar hint", err)
				}
				if strings.Contains(err.Error(), tt.grant) {
					t.Errorf("destinationGrants.policy() echoes rejected value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("destinationGrants.policy() error = %v", err)
			}
			dest, err := provider.NewDestination(tt.provider, tt.baseURL)
			if err != nil {
				t.Fatal(err)
			}
			if !policy.Permits(dest) {
				t.Errorf("destination policy does not permit parsed grant %s", dest)
			}
		})
	}
}

func TestDestinationGrantsRejectMissingSeparator(t *testing.T) {
	var grants destinationGrants
	if err := grants.Set("not-a-destination"); err != nil {
		t.Fatalf("destinationGrants.Set() error = %v, want deferred validation", err)
	}
	if _, err := grants.policy(); err == nil || !strings.Contains(err.Error(), `expected "<provider>/<base URL>"`) {
		t.Errorf("destinationGrants.policy() error = %v, want canonical-grammar hint", err)
	}
}

// tokenEnvName is the documented variable name, spelled out so a typo in
// bearerTokenEnv cannot pass by agreeing with itself.
const tokenEnvName = "GO_LLM_MCP_TOKEN"

func TestBearerTokenOption(t *testing.T) {
	tests := []struct {
		name      string
		transport string
		// set and value describe GO_LLM_MCP_TOKEN in the fake environment.
		set   bool
		value string
		// noLookup marks rows where reading the environment at all is a bug.
		noLookup bool
		wantOpt  bool
		wantErr  string
	}{
		{name: "stdio never reads the environment", transport: "stdio", set: true, value: "tok", noLookup: true},
		{name: "http unset keeps legacy behavior", transport: "http"},
		{name: "http set but empty is an error", transport: "http", set: true, wantErr: tokenEnvName},
		{name: "http valid value", transport: "http", set: true, value: "tok-._~+/=", wantOpt: true},
		// Grammar is NewServer's job; the option must carry the value as-is.
		{name: "http malformed value reaches NewServer", transport: "http", set: true, value: "a b", wantOpt: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				if tt.noLookup {
					t.Errorf("lookup(%q) called, want no environment read", key)
				}
				if key != tokenEnvName {
					t.Errorf("lookup(%q), want %q", key, tokenEnvName)
					return "", false
				}
				return tt.value, tt.set
			}
			opt, err := bearerTokenOption(tt.transport, lookup)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("bearerTokenOption() error = %v, want substring %q", err, tt.wantErr)
				}
				if opt != nil {
					t.Error("bearerTokenOption() returned an option alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("bearerTokenOption() error = %v", err)
			}
			if (opt != nil) != tt.wantOpt {
				t.Errorf("bearerTokenOption() option non-nil = %t, want %t", opt != nil, tt.wantOpt)
			}
		})
	}
}

// bearerHelperTimeout bounds every re-exec of the test binary.
const bearerHelperTimeout = 10 * time.Second

// TestBearerTokenHelper re-runs main() for the subprocess tests below. Its
// sentinel is separate from the token value so that a set-but-empty token is
// still a request to run.
func TestBearerTokenHelper(t *testing.T) {
	if os.Getenv("GO_LLM_MCP_TEST_HELPER") != "bearer" {
		return
	}
	if token, ok := os.LookupEnv("GO_LLM_MCP_TEST_TOKEN"); ok {
		t.Setenv(tokenEnvName, token)
	} else if err := os.Unsetenv(tokenEnvName); err != nil {
		t.Fatal(err)
	}
	flag.CommandLine = flag.NewFlagSet("go-llm-mcp", flag.ExitOnError)
	os.Args = append([]string{"go-llm-mcp"}, strings.Split(os.Getenv("GO_LLM_MCP_TEST_ARGS"), "\x1f")...)
	main()
}

// countingProvider returns a mock Ollama endpoint and its request counter.
func countingProvider(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = fmt.Fprint(w, `{"models":[]}`)
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

// bearerHelperCommand re-execs the test binary as go-llm-mcp with args plus
// -config pointing at a synthetic models.json whose only provider is
// providerURL. token == nil leaves GO_LLM_MCP_TOKEN unset in the child. The
// child's HOME and XDG_CONFIG_HOME are empty temp dirs and GO_LLM_CONFIG is
// removed, so no real configuration is read.
func bearerHelperCommand(ctx context.Context, t *testing.T, dir, providerURL string, token *string, args ...string) *exec.Cmd {
	t.Helper()

	configPath := filepath.Join(dir, "models.json")
	data := `{
  "providers": {"ollama": {"base_url": "` + providerURL + `"}},
  "models": {
    "general": {"name": "test-chat", "provider": "ollama", "type": "dense"},
    "embedding": {"name": "test-embed", "provider": "ollama", "type": "embedding", "dimensions": 8}
  },
  "defaults": {"chat": "general", "embedding": "embedding"}
}`
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBearerTokenHelper$")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GO_LLM_") && !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env,
		"HOME="+t.TempDir(),
		"XDG_CONFIG_HOME="+t.TempDir(),
		"GO_LLM_MCP_TEST_HELPER=bearer",
		"GO_LLM_MCP_TEST_ARGS="+strings.Join(append([]string{"-config", configPath}, args...), "\x1f"),
	)
	if token != nil {
		cmd.Env = append(cmd.Env, "GO_LLM_MCP_TEST_TOKEN="+*token)
	}
	return cmd
}

// assertNoEcho fails if text contains token, its Authorization rendering or
// its SHA-256 digest in any of the common encodings. It mirrors the mcp
// package's assertNoBearerEcho, including the rule that an empty token has no
// token forms to look for (every string contains "").
func assertNoEcho(t *testing.T, text, token string) {
	t.Helper()

	digest := sha256.Sum256([]byte(token))
	forbidden := map[string]string{
		"digest (hex)":           fmt.Sprintf("%x", digest),
		"digest (base64 std)":    base64.StdEncoding.EncodeToString(digest[:]),
		"digest (base64 url)":    base64.URLEncoding.EncodeToString(digest[:]),
		"digest (base64 rawurl)": base64.RawURLEncoding.EncodeToString(digest[:]),
	}
	if token != "" {
		forbidden["token"] = token
		forbidden["Authorization value"] = "Bearer " + token
	}
	for what, needle := range forbidden {
		if strings.Contains(text, needle) {
			t.Errorf("output echoes the %s %q: %q", what, needle, text)
		}
	}
}

func TestBearerTokenEnvRejectsWhitespaceWithoutEcho(t *testing.T) {
	for _, token := range []string{"", "CANARY428tok\n", "CANARY428tok\r", " CANARY428tok", "CANARY428 tok"} {
		t.Run(fmt.Sprintf("%q", token), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), bearerHelperTimeout)
			defer cancel()

			dir := t.TempDir()
			mock, hits := countingProvider(t)
			dbPath := filepath.Join(dir, "t.db")
			cmd := bearerHelperCommand(ctx, t, dir, mock.URL, &token,
				"-transport", "http", "-addr", "127.0.0.1:0", "-no-rag", "-conversation-db", dbPath)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("go-llm-mcp with %s=%q exited 0, want rejection; output: %s", tokenEnvName, token, out)
			}
			if ctx.Err() != nil {
				t.Fatalf("go-llm-mcp did not exit within %s; output: %s", bearerHelperTimeout, out)
			}
			message := string(out)
			if !strings.Contains(message, tokenEnvName) {
				t.Errorf("rejection = %q, want it to name %s", message, tokenEnvName)
			}
			if strings.Contains(message, "CANARY428") {
				t.Errorf("rejection rendered part of the token: %q", message)
			}
			assertNoEcho(t, message, token)
			if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Stat(%q) error = %v, want not-exist: startup touched the database before rejecting the token", dbPath, err)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("provider received %d requests before the token was rejected, want 0", n)
			}
		})
	}
}

// TestStdioIgnoresBearerTokenEnv pins main's transport wiring: with
// -transport stdio, a set-but-empty or malformed GO_LLM_MCP_TOKEN must not
// abort startup. The exit code is not asserted (the server ends when stdin
// reaches EOF); what matters is that the process got through option building
// and NewServer, which the provider health probe shows, and never complained
// about the variable.
func TestStdioIgnoresBearerTokenEnv(t *testing.T) {
	for _, token := range []string{"", "a b"} {
		t.Run(fmt.Sprintf("%q", token), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), bearerHelperTimeout)
			defer cancel()

			mock, hits := countingProvider(t)
			// cmd.Stdin is nil, so the child reads /dev/null and sees EOF.
			cmd := bearerHelperCommand(ctx, t, t.TempDir(), mock.URL, &token, "-transport", "stdio", "-no-rag")
			out, _ := cmd.CombinedOutput()
			if hits.Load() == 0 {
				t.Errorf("provider saw no requests: startup stopped before NewServer finished; output: %s", out)
			}
			if strings.Contains(string(out), tokenEnvName) {
				t.Errorf("stdio output mentions %s, want the variable ignored: %s", tokenEnvName, out)
			}
		})
	}
}

// h2cPriorKnowledgeClient returns a client that speaks HTTP/2 over plain TCP
// (prior knowledge). Only unencrypted HTTP/2 is enabled on its transport, so a
// request cannot silently fall back to HTTP/1.1.
func h2cPriorKnowledgeClient() *http.Client {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{Protocols: &protocols},
	}
}

// postInitialize sends an MCP initialize request to url, with auth as the
// Authorization value when non-empty, and returns the status code.
func postInitialize(ctx context.Context, c *http.Client, url, auth string) (int, error) {
	const body = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// TestBinaryInstallsBearerToken proves main installs the option: a non-nil
// return from bearerTokenOption does not.
func TestBinaryInstallsBearerToken(t *testing.T) {
	token := "bearer428-Tok_en.1~+/="
	tests := []struct {
		name       string
		token      *string
		wantNoAuth int
		wantBearer int // 0: not probed
	}{
		{name: "token set gates the listener", token: &token, wantNoAuth: http.StatusUnauthorized, wantBearer: http.StatusOK},
		{name: "token unset leaves the listener open", wantNoAuth: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), bearerHelperTimeout)
			defer cancel()

			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := l.Addr().String()
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}

			mock, hits := countingProvider(t)
			cmd := bearerHelperCommand(ctx, t, t.TempDir(), mock.URL, tt.token,
				"-transport", "http", "-addr", addr, "-no-rag")
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			stop := func() { _ = cmd.Process.Kill(); <-done }
			t.Cleanup(stop)

			client := h2cPriorKnowledgeClient()
			t.Cleanup(client.CloseIdleConnections)
			url := "http://" + addr + "/mcp"

			// The first answer from the listener is the unauthenticated probe.
			var status int
			for {
				status, err = postInitialize(ctx, client, url, "")
				if err == nil {
					break
				}
				select {
				case <-done:
					t.Fatalf("go-llm-mcp exited before listening: %v; output:\n%s", cmd.ProcessState, out.String())
				case <-ctx.Done():
					stop()
					t.Fatalf("listener at %s never answered: %v; output:\n%s", addr, err, out.String())
				case <-time.After(25 * time.Millisecond):
				}
			}
			if status != tt.wantNoAuth {
				t.Errorf("initialize without Authorization = %d, want %d", status, tt.wantNoAuth)
			}
			if tt.wantBearer != 0 {
				got, err := postInitialize(ctx, client, url, "Bearer "+token)
				if err != nil {
					t.Fatalf("initialize with bearer: %v", err)
				}
				if got != tt.wantBearer {
					t.Errorf("initialize with the configured bearer = %d, want %d", got, tt.wantBearer)
				}
			}
			// The counter must be live for the zero assertion above to mean anything.
			if hits.Load() == 0 {
				t.Error("provider mock saw no requests from a started server")
			}
		})
	}
}
