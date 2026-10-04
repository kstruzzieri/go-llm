package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/mcpclient"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpDigestA = "sha256:d076c0d77d90e89d7022d158c501320cb52ff0acfb18157506f397b195feb99e"
const mcpDigestB = "sha256:5ed6cdea197afcbc274e95c7b9eb7fa76263b49fb300dd5a43409054e2ae9bf3"

type trustHTTPFixture struct {
	server  *gomcp.Server
	url     string
	calls   atomic.Int32
	deletes atomic.Int32
	// requests counts every HTTP request, so tests can prove no contact.
	requests atomic.Int32
	// down makes every request fail, so a pinned endpoint becomes unavailable.
	down atomic.Bool
}

func newTrustHTTPFixture(t *testing.T) *trustHTTPFixture {
	t.Helper()
	f := &trustHTTPFixture{server: gomcp.NewServer(&gomcp.Implementation{Name: "trust-test", Version: "1"}, nil)}
	f.set("Read\nfile", map[string]any{"type": "object"})
	handler := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return f.server }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Method == http.MethodDelete {
			f.deletes.Add(1)
		}
		if f.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	f.url = httpServer.URL
	return f
}
func (f *trustHTTPFixture) set(description string, schema any) {
	f.server.AddTool(&gomcp.Tool{Name: "read", Description: description, InputSchema: schema}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		f.calls.Add(1)
		return &gomcp.CallToolResult{}, nil
	})
}
func trustCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	in, out, diag := runTestFiles(t)
	err := run(append([]string{"mcp"}, args...), in, out, diag)
	return readRunTestFile(t, out), readRunTestFile(t, diag), err
}

// mcpConnection returns the candidate connection fingerprint that
// `golem mcp inspect` prints for one server specification under root.
func mcpConnection(t *testing.T, root, flag, spec string) string {
	t.Helper()
	out, _, err := trustCommand(t, "inspect", "-root", root, flag, spec)
	if err != nil {
		t.Fatal(err)
	}
	const label = "connection candidate: "
	i := strings.Index(out, label)
	if i < 0 {
		t.Fatalf("inspect output lacks %q: %q", label, out)
	}
	line := out[i+len(label):]
	return line[:strings.IndexByte(line, '\n')]
}

func TestMCPTrustOperator(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	f := newTrustHTTPFixture(t)
	args := []string{"-root", root, "-mcp-http", "fs=" + f.url}
	out, diag, err := trustCommand(t, append([]string{"inspect"}, args...)...)
	if err != nil || diag != "" {
		t.Fatalf("inspect: %v %s", err, diag)
	}
	pins, err := mcpclient.NewPinStore(root)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := mcpclient.Inspect(t.Context(), mcpClientImpl(), mcpclient.HTTPServer("fs", f.url), pins)
	if err != nil {
		t.Fatal(err)
	}
	conn := inspection.CandidateConnection.Fingerprint
	want := "server \"fs\"\npin file: " + strconv.QuoteToGraphic(inspection.PinPath) + "\npinned: \ncandidate: " + mcpDigestA + "\nadded: mcp__fs__read\nconnection pinned: \nconnection candidate: " + conn + "\nconnection kind: http\nconnection origin: " + strconv.QuoteToGraphic(f.url) + "\ncandidate mcp__fs__read\n  description: \"Read file\"\n  inputSchema: \"{\\\"type\\\":\\\"object\\\"}\"\n"
	if out != want {
		t.Fatalf("inspect output\ngot %q\nwant %q", out, want)
	}
	if _, err := os.Stat(inspection.PinPath); !os.IsNotExist(err) {
		t.Fatalf("inspection wrote pin: %v", err)
	}
	for _, digest := range []string{"", "sha256:bad", "sha256:" + strings.Repeat("A", 64), mcpDigestB} {
		_, _, err := trustCommand(t, append(append([]string{"approve"}, args...), "-digest", digest, "-connection", conn)...)
		if err == nil {
			t.Fatalf("approved wrong digest %q", digest)
		}
		if _, err := os.Stat(inspection.PinPath); !os.IsNotExist(err) {
			t.Fatal("failed approval wrote pin")
		}
	}
	out, diag, err = trustCommand(t, append(append([]string{"approve"}, args...), "-digest", mcpDigestA, "-connection", conn)...)
	if err != nil || out != "" || diag != "mcp: approved server \"fs\" "+mcpDigestA+"; added: mcp__fs__read\n" {
		t.Fatalf("approve: %v stdout=%q stderr=%q", err, out, diag)
	}
	before, err := os.ReadFile(inspection.PinPath)
	if err != nil {
		t.Fatal(err)
	}
	f.set("Read\nfiles", map[string]any{"type": "object"})
	_, _, err = trustCommand(t, append(append([]string{"approve"}, args...), "-digest", mcpDigestA, "-connection", conn)...)
	if err == nil {
		t.Fatal("approved stale digest")
	}
	after, _ := os.ReadFile(inspection.PinPath)
	if string(after) != string(before) {
		t.Fatal("stale approval replaced pin")
	}
	out, diag, err = trustCommand(t, append(append([]string{"approve"}, args...), "-digest", mcpDigestB, "-connection", conn)...)
	if err != nil || out != "" || diag != "mcp: approved server \"fs\" "+mcpDigestB+"; changed: mcp__fs__read (description)\n" {
		t.Fatalf("changed approval: %v %q %q", err, out, diag)
	}
	// other's own fingerprint passes the connection check, so the refusal
	// proves the catalog digest itself is bound to the alias.
	otherConn := mcpConnection(t, root, "-mcp-http", "other="+f.url)
	requests := f.requests.Load()
	_, _, err = trustCommand(t, "approve", "-root", root, "-mcp-http", "other="+f.url, "-digest", mcpDigestB, "-connection", otherConn)
	var refusal *mcpclient.AdmissionError
	if !errors.As(err, &refusal) || refusal.Reason != "digest_mismatch" || f.requests.Load() == requests {
		t.Fatalf("digest under wrong alias = (%v, %d requests), want digest_mismatch after fetching the catalog", err, f.requests.Load()-requests)
	}
	if f.calls.Load() != 0 {
		t.Fatal("trust command invoked a tool")
	}
	for range f.server.Sessions() {
		t.Fatal("trust command left session open")
	}
}

func TestMCPTrustApproveRequiresConnection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	f := newTrustHTTPFixture(t)
	if _, _, err := trustCommand(t, "approve", "-root", root, "-mcp-http", "fs="+f.url, "-digest", mcpDigestA); err == nil {
		t.Fatal("approve without -connection succeeded")
	}
	if n := f.requests.Load(); n != 0 {
		t.Fatalf("approve without -connection sent %d requests to the server", n)
	}
}

func TestMCPTrustArguments(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	f := newTrustHTTPFixture(t)
	for _, args := range [][]string{
		{}, {"unknown"}, {"inspect"}, {"inspect", "-mcp-http", f.url}, {"inspect", "-mcp-stdio", "env TOKEN=credential-value command"},
		{"inspect", "-mcp-http", "fs=" + f.url, "-mcp-http", "other=" + f.url},
		{"inspect", "-mcp-http", "fs=" + f.url, "extra"},
		{"inspect", "-mcp-http", "fs=" + f.url, "-mcp-tools", "fs=read"},
		{"inspect", "-root", "", "-mcp-http", "fs=" + f.url},
		{"inspect", "-root", filepath.Join(root, "missing"), "-mcp-http", "fs=" + f.url},
		{"inspect", "-mcp-stdio", `fs=env TOKEN=credential-value "`},
		{"inspect", "-mcp-http", "fs=", "-root", root},
	} {
		out, diag, err := trustCommand(t, args...)
		if err == nil || out != "" {
			t.Fatalf("args=%v err=%v out=%q", args, err, out)
		}
		if strings.Contains(diag+err.Error(), "credential-value") {
			t.Fatal("credentials leaked")
		}
	}
	// -digest and -connection are approve-only: inspect refuses them before
	// contacting the server.
	before := f.requests.Load()
	for _, extra := range [][]string{{"-connection", "hmac-sha256:" + strings.Repeat("0", 64)}, {"-digest", mcpDigestA}} {
		out, _, err := trustCommand(t, append([]string{"inspect", "-root", root, "-mcp-http", "fs=" + f.url}, extra...)...)
		if err == nil || err.Error() != "mcp: invalid command flags" || out != "" {
			t.Fatalf("inspect %s = (%v, %q), want \"mcp: invalid command flags\" and no output", extra[0], err, out)
		}
	}
	if n := f.requests.Load() - before; n != 0 {
		t.Fatalf("inspect with an approve-only flag sent %d requests", n)
	}
	t.Chdir(root)
	if _, _, err := trustCommand(t, "inspect", "-mcp-http", "fs="+f.url); err != nil {
		t.Fatalf("default root: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", root)
	if _, _, err := trustCommand(t, "inspect", "-mcp-http", "fs="+f.url); err == nil {
		t.Fatal("accepted in-workspace pins")
	}
}

func TestMCPTrustEarlyOneShot(t *testing.T) {
	for _, kind := range []string{"absent", "mismatch", "unavailable", "store", "invalid", "unreadable"} {
		for _, format := range []string{"text", "json", "stream-json"} {
			t.Run(kind+"/"+format, func(t *testing.T) {
				config, root := writeRunLifecycleConfig(t)
				var providerCalls atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					providerCalls.Add(1)
					http.Error(w, "unexpected provider bootstrap", 500)
				}))
				t.Cleanup(provider.Close)
				raw, err := os.ReadFile(config)
				if err != nil {
					t.Fatal(err)
				}
				// Replace the one base_url in the existing valid lifecycle config.
				start := strings.Index(string(raw), `"base_url": `) + len(`"base_url": `)
				end := start + strings.Index(string(raw)[start+1:], `"`) + 2
				raw = []byte(string(raw[:start]) + strconv.Quote(provider.URL) + string(raw[end:]))
				if err := os.WriteFile(config, raw, 0600); err != nil {
					t.Fatal(err)
				}
				f := newTrustHTTPFixture(t)
				pins, err := mcpclient.NewPinStore(root)
				if err != nil {
					t.Fatal(err)
				}
				endpoint := f.url
				if kind == "unavailable" {
					endpoint += "/?token=credential-value"
				}
				server := mcpclient.HTTPServer("fs", endpoint)
				inspection, err := mcpclient.Inspect(t.Context(), mcpClientImpl(), server, pins)
				if err != nil {
					t.Fatal(err)
				}
				var before []byte
				// Seed a matching pin wherever a record is in play, so
				// preflight passes and each kind reaches its own failure.
				if kind != "absent" && kind != "store" {
					if _, err := mcpclient.Approve(t.Context(), mcpClientImpl(), server, pins, mcpclient.ApprovalDigests{Catalog: mcpDigestA, Connection: inspection.CandidateConnection.Fingerprint}); err != nil {
						t.Fatal(err)
					}
					before, _ = os.ReadFile(inspection.PinPath)
				}
				if kind == "mismatch" || kind == "unreadable" {
					f.set("Read\nfiles", map[string]any{"type": "object"})
					if kind == "unreadable" {
						before = []byte("invalid pin")
						if err := os.WriteFile(inspection.PinPath, before, 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				switch kind {
				case "unavailable":
					f.down.Store(true)
				case "store":
					t.Setenv("XDG_DATA_HOME", root)
				case "invalid":
					f.server.AddTool(&gomcp.Tool{Name: "bad name", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) { return nil, nil })
				}
				in, out, diag := runTestFiles(t)
				err = run([]string{"-config", config, "-root", root, "-p", "hi", "-output-format", format, "-mcp-http", "fs=" + endpoint, "-no-project-context", "-no-git-context", "-no-rag"}, in, out, diag)
				if err == nil || exitCodeFor(err) != 1 {
					t.Fatalf("exit: %v (%d)", err, exitCodeFor(err))
				}
				want := ""
				if format != "text" {
					want = "{\"schema\":\"golem.result.v1\",\"status\":\"error\",\"answer\":null,\"stopReason\":null,\"model\":null,\"error\":{\"code\":\"mcp_untrusted\",\"message\":\"golem: MCP catalog admission failed\"},\"grounding\":null}\n"
				}
				if got := readRunTestFile(t, out); got != want {
					t.Fatalf("stdout=%q want=%q", got, want)
				}
				got := readRunTestFile(t, diag)
				if !strings.Contains(got, "mcp:") || strings.Contains(got, "credential-value") {
					t.Fatalf("diagnostics=%q", got)
				}
				reason := map[string]string{"absent": "pin_missing", "mismatch": "catalog_changed", "unavailable": "unavailable", "invalid": "invalid_catalog", "unreadable": "pin_unavailable"}[kind]
				if reason != "" && !strings.Contains(got, `warning: mcp: server "fs": `+reason) {
					t.Fatalf("diagnostics=%q, want reason %s", got, reason)
				}
				if providerCalls.Load() != 0 {
					t.Fatalf("provider requests=%d before MCP refusal", providerCalls.Load())
				}
				after, readErr := os.ReadFile(inspection.PinPath)
				if before != nil {
					if string(after) != string(before) {
						t.Fatal("changed pin")
					}
				} else if !os.IsNotExist(readErr) {
					t.Fatalf("created pin: %v", readErr)
				}
				for range f.server.Sessions() {
					t.Fatal("left MCP session open")
				}
			})
		}
	}
}

func TestMCPStartupSummary(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			config, root := writeRunLifecycleConfig(t)
			f := newTrustHTTPFixture(t)
			if empty {
				f.server.RemoveTools("read")
			}
			in, out, diag := runTestFiles(t)
			args := []string{"-config", config, "-root", root, "-mcp-http", "fs=" + f.url, "-no-probe", "-no-cap-probe", "-no-git-context", "-no-project-context", "-no-rag", "-no-memory", "-no-session", "-no-auto-index"}
			if err := run(args, in, out, diag); err != nil {
				t.Fatal(err)
			}
			got := readRunTestFile(t, diag)
			count := 1
			if empty {
				count = 0
			}
			if !strings.Contains(got, fmt.Sprintf("mcp: attached %d tool(s) from 1 configured server(s)\n", count)) || !strings.Contains(got, "use explicit alias= values for stable pins") || strings.Contains(got, "blocked") {
				t.Fatalf("first pin: %q", got)
			}
			if strings.Contains(readRunTestFile(t, out), "first pin") {
				t.Fatal("notice on stdout")
			}
			f.set("changed", map[string]any{"type": "object"})
			in, out, diag = runTestFiles(t)
			if err := run(args, in, out, diag); err != nil {
				t.Fatal(err)
			}
			got = readRunTestFile(t, diag)
			if !strings.Contains(got, "mcp: attached 0 tool(s) from 1 configured server(s); blocked 1 alias(es): fs (catalog_changed)") || strings.Contains(got, "description: ") {
				t.Fatalf("blocked summary: %q", got)
			}
		})
	}
}

func TestMCPTrustStdioProcess(t *testing.T) {
	description := os.Getenv("GOLEM_MCP_TRUST_DESCRIPTION")
	if description == "" {
		return
	}
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "fixture", Version: "1"}, nil)
	srv.AddTool(&gomcp.Tool{Name: "read", Description: description, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		return &gomcp.CallToolResult{}, nil
	})
	if err := srv.Run(context.Background(), &gomcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

const mcpEnvProbeMarker = "golem-mcp-envprobe"

// TestMCPEnvProbeProcess is not a test. Re-executed with mcpEnvProbeMarker,
// the test binary becomes a stdio MCP server whose one tool description
// reports the child's environment names, PROBE_TOKEN value and working
// directory, so each catalog digest and pin records what that launch saw.
func TestMCPEnvProbeProcess(t *testing.T) {
	if !slices.Contains(os.Args, mcpEnvProbeMarker) {
		return
	}
	var names []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	slices.Sort(names)
	wd, _ := os.Getwd()
	raw, _ := json.Marshal(map[string]any{"names": names, "token": os.Getenv("PROBE_TOKEN"), "cwd": wd})
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "envprobe", Version: "1"}, nil)
	srv.AddTool(&gomcp.Tool{Name: "probe", Description: string(raw), InputSchema: map[string]any{"type": "object"}}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		return &gomcp.CallToolResult{}, nil
	})
	if err := srv.Run(context.Background(), &gomcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// TestMCPStdioRunsInRootWithNamedEnv drives the real CLI. The REPL launch pins
// the probe's catalog, then `golem mcp inspect` launches it again: matching
// pinned and candidate identities show both launches saw the same thing, and
// the candidate description shows what: the canonical -root as working
// directory and only the baseline plus the -mcp-env names.
func TestMCPStdioRunsInRootWithNamedEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the literal baseline below is the Unix one; Windows builds this test without running it")
	}
	config, root := writeRunLifecycleConfig(t)
	t.Setenv("PROBE_TOKEN", "probe-value")
	t.Setenv("GOLEM_MCP_ENV_CANARY", "canary-value")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec := "probe=" + strconv.Quote(executable) + " -test.run=^TestMCPEnvProbeProcess$ -- " + mcpEnvProbeMarker
	in, out, diag := runTestFiles(t)
	if err := run([]string{"-config", config, "-root", root, "-mcp-stdio", spec, "-mcp-env", "probe=PROBE_TOKEN", "-no-probe", "-no-cap-probe", "-no-git-context", "-no-project-context", "-no-rag", "-no-memory", "-no-session", "-no-auto-index"}, in, out, diag); err != nil {
		t.Fatal(err)
	}
	if got := readRunTestFile(t, diag); !strings.Contains(got, "mcp: attached 1 tool(s) from 1 configured server(s)\n") {
		t.Fatalf("REPL startup: %q", got)
	}
	inspection, _, err := trustCommand(t, "inspect", "-root", root, "-mcp-stdio", spec, "-mcp-env", "probe=PROBE_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	field := func(label string) string {
		t.Helper()
		i := strings.Index(inspection, "\n"+label)
		if i < 0 {
			t.Fatalf("inspect output lacks %q: %q", label, inspection)
		}
		line := inspection[i+1+len(label):]
		return line[:strings.IndexByte(line, '\n')]
	}
	if field("pinned: ") != field("candidate: ") || field("connection pinned: ") != field("connection candidate: ") {
		t.Fatalf("REPL and inspect launches differ: %q", inspection)
	}
	description, err := strconv.Unquote(field("candidate mcp__probe__probe\n  description: "))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Names []string `json:"names"`
		Token string   `json:"token"`
		Cwd   string   `json:"cwd"`
	}
	if err := json.Unmarshal([]byte(description), &report); err != nil {
		t.Fatal(err)
	}
	var wantNames []string
	for _, name := range []string{"HOME", "LANG", "PATH", "PROBE_TOKEN", "TMPDIR", "USER"} {
		if _, ok := os.LookupEnv(name); ok {
			wantNames = append(wantNames, name)
		}
	}
	wantCwd, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(report.Names, wantNames) || report.Token != "probe-value" || report.Cwd != wantCwd {
		t.Fatalf("child saw (%q, %q, %q), want (%q, probe-value, %q)", report.Names, report.Token, report.Cwd, wantNames, wantCwd)
	}
}

func TestMCPTrustDerivedAliases(t *testing.T) {
	if _, err := exec.LookPath("env"); err != nil {
		t.Skip("requires env command")
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	first := "env GOLEM_MCP_TRUST_DESCRIPTION=first " + strconv.Quote(executable) + " -test.run=^TestMCPTrustStdioProcess$"
	second := "env GOLEM_MCP_TRUST_DESCRIPTION=second " + strconv.Quote(executable) + " -test.run=^TestMCPTrustStdioProcess$"
	servers, err := parseMCPServers([]string{first, second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if servers, err = withMCPPolicy(root, servers, 2, nil); err != nil {
		t.Fatal(err)
	}
	if servers[0].Alias != "env" || servers[1].Alias != "env2" {
		t.Fatalf("aliases=%v", servers)
	}
	mgr, warnings, err := connectMCP(t.Context(), root, servers, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(mgr.Tools()) != 2 || len(warnings) != 2 || len(mcpBlockedAliases(warnings)) != 0 {
		t.Fatalf("startup=%v %v", mgr.Tools(), warnings)
	}
	if err := mgr.Close(); err != nil {
		t.Fatal(err)
	}
	for _, warning := range warnings {
		if !strings.Contains(warning.Error(), "use explicit alias= values for stable pins") {
			t.Fatalf("notice=%v", warning)
		}
	}
	pins, err := mcpclient.NewPinStore(root)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := mcpclient.Inspect(t.Context(), mcpClientImpl(), servers[0], pins)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(prior.PinPath)
	if err != nil {
		t.Fatal(err)
	}
	// Configuration order changes ownership of derived names; there is no
	// transport fingerprint to silently reset the operator's alias decision.
	reordered, err := parseMCPServers([]string{second, first}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reordered, err = withMCPPolicy(root, reordered, 2, nil); err != nil {
		t.Fatal(err)
	}
	mgr, warnings, err = connectMCP(t.Context(), root, reordered, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(mgr.Tools()) != 0 || len(mcpBlockedAliases(warnings)) != 2 {
		t.Fatalf("reordered startup=%v %v", mgr.Tools(), warnings)
	}
	if err := mgr.Close(); err != nil {
		t.Fatal(err)
	}
	candidate, err := mcpclient.Inspect(t.Context(), mcpClientImpl(), reordered[1], pins)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := trustCommand(t, "inspect", "-root", root, "-mcp-stdio", first); err == nil {
		t.Fatal("derived alias accepted for inspection")
	}
	out, _, err := trustCommand(t, "inspect", "-root", root, "-mcp-stdio", "env2="+first)
	if err != nil || !strings.Contains(out, "server \"env2\"") || !strings.Contains(out, candidate.CandidateDigest) {
		t.Fatalf("env2 inspection=%q %v", out, err)
	}
	_, diag, err := trustCommand(t, "approve", "-root", root, "-mcp-stdio", "env2="+first, "-digest", candidate.CandidateDigest, "-connection", candidate.CandidateConnection.Fingerprint)
	if err != nil || !strings.Contains(diag, "changed: mcp__env2__read (description)") {
		t.Fatalf("env2 approval=%q %v", diag, err)
	}
	after, err := os.ReadFile(prior.PinPath)
	if err != nil || string(after) != string(before) {
		t.Fatal("env2 approval changed env pin")
	}
}

func TestMCPTrustInspectionQuoting(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	f := newTrustHTTPFixture(t)
	if _, _, err := trustCommand(t, "approve", "-root", root, "-mcp-http", "fs="+f.url, "-digest", mcpDigestA, "-connection", mcpConnection(t, root, "-mcp-http", "fs="+f.url)); err != nil {
		t.Fatal(err)
	}
	f.set("line\n\x00\x1b\u0081\u202e\xff", map[string]any{"type": "object", "description": "schema\n\x00\x1b\u0081\u202e"})
	out, _, err := trustCommand(t, "inspect", "-root", root, "-mcp-http", "fs="+f.url)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"changed: mcp__fs__read (description, inputSchema)\n",
		`  description: "Read file"`,
		`  description: "line \x00\x1b\u0081\u202e�"`,
		`  inputSchema: "{\"description\":\"schema\\n\\u0000\\u001b\u0081\u202e\",\"type\":\"object\"}"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing literal %q in %q", want, out)
		}
	}
	for _, unsafe := range []string{"\x00", "\x1b", "\u0081", "\u202e"} {
		if strings.Contains(out, unsafe) {
			t.Fatalf("raw control %q in inspection", unsafe)
		}
	}
}

func TestMCPTrustDisjointDiffAndHint(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	f := newTrustHTTPFixture(t)
	f.server.RemoveTools("read")
	names := make([]string, 128)
	add := func(prefix string) {
		for i := range 128 {
			name := fmt.Sprintf("%s%03d", prefix, i)
			names[i] = name
			f.server.AddTool(&gomcp.Tool{Name: name, Description: "private remote prose", InputSchema: map[string]any{"type": "object", "description": "private schema body"}}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) { return nil, nil })
		}
	}
	add("a")
	servers := []mcpclient.Server{mcpclient.HTTPServer("fs", f.url+"/?token=credential-value")}
	mgr, _, err := connectMCP(t.Context(), root, servers, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Close(); err != nil {
		t.Fatal(err)
	}
	f.server.RemoveTools(names...)
	add("b")
	mgr, warnings, err := connectMCP(t.Context(), root, servers, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Close() }()
	if len(warnings) != 1 || len(mgr.Tools()) != 0 {
		t.Fatalf("disjoint catalog admitted: %v", warnings)
	}
	pins, err := mcpclient.NewPinStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mcpclient.Inspect(t.Context(), mcpClientImpl(), servers[0], pins); err != nil {
		t.Fatal(err)
	}
	message := warnings[0].Error()
	if strings.Count(message, "mcp__fs__") != 256 {
		t.Fatalf("diff omitted names: %s", message)
	}
	if strings.Index(message, "added:") > strings.Index(message, "removed:") {
		t.Fatal("diff order")
	}
	for _, secret := range []string{"credential-value", f.url, "private remote prose", "private schema body"} {
		if strings.Contains(message, secret) {
			t.Fatalf("diagnostic leaked %q", secret)
		}
	}
	wantHint := "review with golem mcp inspect using the same -root, server and -mcp-env arguments and explicit alias=fs, then golem mcp approve with the -digest and -connection it prints"
	if !strings.HasSuffix(message, wantHint) {
		t.Fatalf("hint not bound: %s", message)
	}
}

func TestMCPTrustCleanupOnLaterFailure(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			config, root := writeRunLifecycleConfig(t)
			f := newTrustHTTPFixture(t)
			_, _, err := trustCommand(t, "approve", "-root", root, "-mcp-http", "fs="+f.url, "-digest", mcpDigestA, "-connection", mcpConnection(t, root, "-mcp-http", "fs="+f.url))
			if err != nil {
				t.Fatal(err)
			}
			before := f.deletes.Load()
			args := []string{"-config", config, "-root", root, "-p", "hi", "-mcp-http", "fs=" + f.url, "-no-project-context", "-no-git-context", "-no-rag"}
			if mixed {
				args = append(args, "-mcp-http", "missing="+f.url)
			} else {
				// Force a failure after admission while preparing provider clients.
				args = append(args, "-base-url", "http://127.0.0.1:1", "-no-probe", "-no-cap-probe")
			}
			in, out, diag := runTestFiles(t)
			err = run(args, in, out, diag)
			if err == nil {
				t.Fatal("expected later bootstrap/alias failure")
			}
			if f.deletes.Load() <= before {
				t.Fatal("healthy admitted session not closed")
			}
			for range f.server.Sessions() {
				t.Fatal("healthy session leaked")
			}
		})
	}
}

func TestMCPTrustGoalPlanStillReject(t *testing.T) {
	f := newTrustHTTPFixture(t)
	for _, mode := range []string{"-goal", "-plan"} {
		in, out, diag := runTestFiles(t)
		err := run([]string{mode, "unused", "-mcp-http", "fs=" + f.url}, in, out, diag)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "mcp") {
			t.Fatalf("%s err=%v", mode, err)
		}
		if f.deletes.Load() != 0 {
			t.Fatal("goal/plan connected MCP")
		}
	}
}

func TestMCPTrustParserDoesNotLeak(t *testing.T) {
	in, out, diag := runTestFiles(t)
	err := run([]string{"-config", filepath.Join(t.TempDir(), "absent.json"), "-root", t.TempDir(), "-mcp-stdio", `fs=env TOKEN=credential-value "`}, in, out, diag)
	if err == nil || strings.Contains(err.Error()+readRunTestFile(t, diag), "credential-value") {
		t.Fatalf("parser error: %v", err)
	}
}

func TestMCPTrustEmptyApproval(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	f := newTrustHTTPFixture(t)
	f.server.RemoveTools("read")
	const digest = "sha256:4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"
	out, diag, err := trustCommand(t, "approve", "-root", root, "-mcp-http", "fs="+f.url, "-digest", digest, "-connection", mcpConnection(t, root, "-mcp-http", "fs="+f.url))
	if err != nil || out != "" || diag != "mcp: approved server \"fs\" "+digest+"\n" {
		t.Fatalf("empty approval: %v %q %q", err, out, diag)
	}
	mgr, warnings, err := connectMCP(t.Context(), root, []mcpclient.Server{mcpclient.HTTPServer("fs", f.url)}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Close() }()
	if len(warnings) != 0 || len(mgr.Tools()) != 0 {
		t.Fatalf("empty pin rejected: %v", warnings)
	}
}

func TestMCPFirstPinNoticeSurvivesBootstrapFailure(t *testing.T) {
	config, root := writeRunLifecycleConfig(t)
	f := newTrustHTTPFixture(t)
	in, out, diag := runTestFiles(t)
	err := run([]string{"-config", config, "-root", root, "-mcp-http", "fs=" + f.url, "-base-url", "http://127.0.0.1:1", "-no-project-context", "-no-git-context", "-no-probe", "-no-cap-probe"}, in, out, diag)
	if err == nil {
		t.Fatal("expected bootstrap failure")
	}
	if got := readRunTestFile(t, diag); !strings.Contains(got, "first pin "+mcpDigestA) {
		t.Fatalf("lost first-pin notice on failure: %q", got)
	}
	for range f.server.Sessions() {
		t.Fatal("session leaked after bootstrap failure")
	}
}

func TestMCPTrustMalformedHTTPDoesNotLeak(t *testing.T) {
	for _, endpoint := range []string{"https://?token=credential-value", "https://bad%zz/?token=credential-value"} {
		for _, format := range []string{"text", "json", "stream-json"} {
			config, root := writeRunLifecycleConfig(t)
			in, out, diag := runTestFiles(t)
			err := run([]string{"-config", config, "-root", root, "-p", "hi", "-output-format", format, "-mcp-http", endpoint, "-no-project-context", "-no-git-context", "-no-probe", "-no-cap-probe", "-no-rag"}, in, out, diag)
			if err == nil {
				t.Fatal("malformed HTTP endpoint accepted")
			}
			for _, got := range []string{err.Error(), readRunTestFile(t, out), readRunTestFile(t, diag)} {
				if strings.Contains(got, "credential-value") {
					t.Fatalf("transport credentials in CLI output: %q", got)
				}
			}
		}
	}
}

// TestMCPFatalConfigNamesReason: a configuration Connect refuses outright
// reports the alias, reason and fixed rule (never the URL) on stderr, and the
// machine record and exit code stay the generic mcp_untrusted refusal.
func TestMCPFatalConfigNamesReason(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			config, root := writeRunLifecycleConfig(t)
			in, out, diag := runTestFiles(t)
			err := run([]string{"-config", config, "-root", root, "-p", "hi", "-output-format", format, "-mcp-http", "fs=https://user:canary@example.com/mcp", "-no-project-context", "-no-git-context", "-no-probe", "-no-cap-probe", "-no-rag"}, in, out, diag)
			if err == nil || exitCodeFor(err) != 1 || err.Error() != "golem: MCP catalog admission failed" {
				t.Fatalf("exit: %v (%d)", err, exitCodeFor(err))
			}
			want := ""
			if format == "json" {
				want = "{\"schema\":\"golem.result.v1\",\"status\":\"error\",\"answer\":null,\"stopReason\":null,\"model\":null,\"error\":{\"code\":\"mcp_untrusted\",\"message\":\"golem: MCP catalog admission failed\"},\"grounding\":null}\n"
			}
			if got := readRunTestFile(t, out); got != want {
				t.Fatalf("stdout=%q want=%q", got, want)
			}
			got := readRunTestFile(t, diag)
			if !strings.Contains(got, "mcp: server \"fs\": invalid_config: mcpclient: endpoint must not carry userinfo\n") {
				t.Fatalf("diagnostics=%q, want the alias, reason and rule", got)
			}
			if strings.Contains(got, "canary") || strings.Contains(got, "example.com") {
				t.Fatalf("diagnostics leaked endpoint text: %q", got)
			}
		})
	}
}

// An unusable pin store blocks each alias with a hint at what to check, and
// the alias is still reported as blocked.
func TestMCPPinStoreFailureHint(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", file)
	mgr, warnings, err := connectMCP(t.Context(), t.TempDir(), []mcpclient.Server{mcpclient.HTTPServer("fs", "https://example.com/mcp")}, true)
	if mgr != nil || err != nil || len(warnings) != 1 {
		t.Fatalf("connectMCP = (%v, %v, %v), want one warning", mgr, warnings, err)
	}
	if got, want := warnings[0].Error(), `server "fs": pin_unavailable; check -root and the user data directory (golem/mcp-pins, including connection-hmac.pem)`; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}
	if got := mcpBlockedAliases(warnings); !slices.Equal(got, []string{"fs (pin_unavailable)"}) {
		t.Fatalf("blocked = %q", got)
	}
}

func TestMCPToolsRunWiring(t *testing.T) {
	config, root := writeRunLifecycleConfig(t)
	f := newTrustHTTPFixture(t)
	in, out, diag := runTestFiles(t)
	args := []string{"-config", config, "-root", root, "-mcp-http", "fs=" + f.url, "-mcp-tools", "fs=", "-no-probe", "-no-cap-probe", "-no-git-context", "-no-project-context", "-no-rag", "-no-memory", "-no-session", "-no-auto-index"}
	if err := run(args, in, out, diag); err != nil {
		t.Fatal(err)
	}
	if got := readRunTestFile(t, diag); !strings.Contains(got, "mcp: attached 0 tool(s) from 1 configured server(s)\n") {
		t.Fatalf("-mcp-tools fs= did not reach Connect: %q", got)
	}
	in, out, diag = runTestFiles(t)
	err := run([]string{"-config", config, "-root", root, "-p", "hi", "-mcp-tools", "fs=credential-value!"}, in, out, diag)
	if err == nil || exitCodeFor(err) != 2 || err.Error() != "golem: -mcp-tools #1: alias is not a configured MCP server" {
		t.Fatalf("headless -mcp-tools error = %v (exit %d), want positional usage error, exit 2", err, exitCodeFor(err))
	}
	if strings.Contains(err.Error()+readRunTestFile(t, out)+readRunTestFile(t, diag), "credential-value") {
		t.Fatal("-mcp-tools value echoed")
	}
}

func TestMCPTrustRejectsToolSelection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	f := newTrustHTTPFixture(t)
	for _, action := range []string{"inspect", "approve"} {
		out, _, err := trustCommand(t, action, "-root", root, "-mcp-http", "fs="+f.url, "-mcp-tools", "fs=credential-value")
		if err == nil || out != "" || err.Error() != "mcp: inspect and approve do not take -mcp-tools; they always review the complete catalog" {
			t.Fatalf("%s with -mcp-tools = (%v, %q), want the explicit rejection", action, err, out)
		}
	}
	if f.deletes.Load() != 0 {
		t.Fatal("a rejected -mcp-tools command contacted the server")
	}
}
