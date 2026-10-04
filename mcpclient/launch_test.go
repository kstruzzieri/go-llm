package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestResolveLauncher(t *testing.T) {
	var got string
	fake := func(result string, err error) func(string) (string, error) {
		return func(name string) (string, error) { got = name; return result, err }
	}
	dir := filepath.Join(string(filepath.Separator), "work")
	for _, tt := range []struct {
		argv0, wantArg, result string
		err                    error
		ok                     bool
	}{
		{"server", "server", "/usr/bin/server", nil, true},
		{"./bin/server", filepath.Join(dir, "bin", "server"), "/work/bin/server", nil, true},
		{"/opt/server", "/opt/server", "/opt/server", nil, true},
		{"server", "server", "", &exec.Error{Name: "server", Err: exec.ErrDot}, false},
		{"server", "server", "server", nil, false},                    // relative result (GODEBUG=execerrdot=0)
		{"/opt/x/../server", "/opt/server", "/opt/server", nil, true}, // cleaned before lookup, not after
		{"/opt/server", "/opt/server", "/opt/./server", nil, true},    // lookPath's result is executed unchanged
	} {
		launcher, err := resolveLauncher(tt.argv0, dir, fake(tt.result, tt.err))
		if got != tt.wantArg || (err == nil) != tt.ok || (tt.ok && launcher != tt.result) {
			t.Errorf("resolveLauncher(%q) passed %q, returned (%q, %v); want %q, ok=%t", tt.argv0, got, launcher, err, tt.wantArg, tt.ok)
		}
	}
	if _, err := resolveLauncher("", dir, fake("/x", nil)); err == nil {
		t.Error("empty argv0 accepted")
	}
}

func testLaunchEnv(parent map[string]string) launchEnv {
	return launchEnv{
		lookup:   func(name string) (string, bool) { v, ok := parent[name]; return v, ok },
		lookPath: exec.LookPath,
		getwd:    os.Getwd,
		policy:   unixEnvPolicy,
	}
}

func TestPrepareStdioFreezesResolvedLaunch(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "server"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("server", filepath.Join(real, "link")); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(root, "linked")
	if err := os.Symlink(real, linkedDir); err != nil {
		t.Fatal(err)
	}
	wantDir, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	s := StdioServer("fs", []string{"./link", "--flag"}).WithDir(linkedDir).WithEnv(InheritEnv("TOKEN"), SetEnv("MODE", "x"))
	p, err := prepare(s, "/ws", testLaunchEnv(map[string]string{"PATH": "/bin", "HOME": "/h", "CANARY_SECRET": "c", "TOKEN": "t"}))
	if err != nil {
		t.Fatal(err)
	}
	// prepare must own its argv: StdioServer already copied the caller's
	// slice, so mutate the Server's own backing array instead.
	s.command[1] = "--changed"
	id := p.identity
	if id.dir != wantDir || id.launcher != filepath.Join(wantDir, "link") || id.target != filepath.Join(wantDir, "server") {
		t.Fatalf("identity paths = (%q, %q, %q), want (%q, %q, %q)", id.dir, id.launcher, id.target, wantDir, filepath.Join(wantDir, "link"), filepath.Join(wantDir, "server"))
	}
	if !slices.Equal(id.argv, []string{"./link", "--flag"}) || !slices.Equal(id.env, []string{"inherit:TOKEN", "set:MODE"}) || id.envBaseline != "unix-v1" || id.kind != "stdio" {
		t.Fatalf("identity = %+v", id)
	}
	cmd := p.transport.(*gomcp.CommandTransport).Command
	if cmd.Path != id.launcher || !slices.Equal(cmd.Args, []string{"./link", "--flag"}) || cmd.Dir != wantDir {
		t.Fatalf("command = (%q, %q, %q)", cmd.Path, cmd.Args, cmd.Dir)
	}
	if want := []string{"HOME=/h", "MODE=x", "PATH=/bin", "TOKEN=t"}; !slices.Equal(cmd.Env, want) {
		t.Fatalf("command env = %q, want %q", cmd.Env, want)
	}
}

func TestPrepareStdioDefaultsToProcessCwd(t *testing.T) {
	cwd := t.TempDir()
	le := testLaunchEnv(map[string]string{"PATH": os.Getenv("PATH")})
	le.getwd = func() (string, error) { return cwd, nil }
	p, err := prepare(StdioServer("fs", []string{"/bin/sh"}), "/ws", le)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(cwd)
	if p.identity.dir != want {
		t.Fatalf("default dir = %q, want %q", p.identity.dir, want)
	}
}

func TestPrepareStdioFailures(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		s      Server
		reason string
	}{
		// Validation rejects an empty command first; prepare must still
		// refuse it rather than panic in a Connect worker.
		"empty command":      {StdioServer("fs", nil).WithDir(dir), "launch_invalid"},
		"missing dir":        {StdioServer("fs", []string{"/bin/sh"}).WithDir(filepath.Join(dir, "missing")), "launch_invalid"},
		"dir is a file":      {StdioServer("fs", []string{"/bin/sh"}).WithDir(file), "launch_invalid"},
		"missing executable": {StdioServer("fs", []string{filepath.Join(dir, "nope")}).WithDir(dir), "launch_invalid"},
		"not executable":     {StdioServer("fs", []string{file}).WithDir(dir), "launch_invalid"},
		"unset inherited":    {StdioServer("fs", []string{"/bin/sh"}).WithDir(dir).WithEnv(InheritEnv("TOKEN")), "env_unset"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := prepare(tt.s, "/ws", testLaunchEnv(map[string]string{"PATH": "/bin"}))
			var failure *AdmissionError
			if !errors.As(err, &failure) || failure.Reason != tt.reason {
				t.Fatalf("prepare = %v, want %s", err, tt.reason)
			}
			if tt.reason == "env_unset" && (!slices.Equal(failure.Names, []string{"TOKEN"}) || failure.Error() != `server "fs": env_unset: TOKEN`) {
				t.Fatalf("env_unset = (%q, %q)", failure.Names, failure.Error())
			}
		})
	}
}

func TestPrepareHTTP(t *testing.T) {
	p, err := prepare(HTTPServer("api", "HTTPS://Example.COM/mcp?token=q"), "/ws", testLaunchEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.identity.kind != "http" || p.identity.origin != "https://example.com" || p.identity.endpoint != "/mcp?token=q" {
		t.Fatalf("http identity = %+v", p.identity)
	}
	sc := p.transport.(*gomcp.StreamableClientTransport)
	if sc.Endpoint != "https://example.com/mcp?token=q" || !sc.DisableStandaloneSSE || p.refusals == nil {
		t.Fatalf("http transport = (%q, %t, %v)", sc.Endpoint, sc.DisableStandaloneSSE, p.refusals)
	}
}

const envProbeMarker = "mcpclient-envprobe"

// TestMCPClientEnvProbeHelper is not a test. Re-executed with envProbeMarker,
// the test binary becomes a stdio MCP server whose probe tool reports its
// environment names, PROBE_* values, and working directory.
func TestMCPClientEnvProbeHelper(t *testing.T) {
	if !slices.Contains(os.Args, envProbeMarker) {
		return
	}
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "envprobe", Version: "1"}, nil)
	srv.AddTool(&gomcp.Tool{Name: "probe", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		names, values := []string{}, map[string]string{}
		for _, kv := range os.Environ() {
			name, value, _ := strings.Cut(kv, "=")
			names = append(names, name)
			if strings.HasPrefix(name, "PROBE_") {
				values[name] = value
			}
		}
		sort.Strings(names)
		wd, _ := os.Getwd()
		raw, _ := json.Marshal(map[string]any{"names": names, "values": values, "cwd": wd})
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: string(raw)}}}, nil
	})
	if err := srv.Run(context.Background(), &gomcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

type probeReport struct {
	Names  []string          `json:"names"`
	Values map[string]string `json:"values"`
	Cwd    string            `json:"cwd"`
}

func runEnvProbe(t *testing.T, s Server, le launchEnv) probeReport {
	t.Helper()
	m, w, err := connectWithHooks(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: testPins(t)}, &connectHooks{launch: &le})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if len(m.Tools()) != 1 {
		t.Fatalf("probe server not admitted: %v", w)
	}
	res, err := m.Tools()[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("probe call = (%+v, %v)", res, err)
	}
	var report probeReport
	if err := json.Unmarshal([]byte(res.Content), &report); err != nil {
		t.Fatalf("probe report %q: %v", res.Content, err)
	}
	return report
}

func probeServer(t *testing.T) Server {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return StdioServer("probe", []string{exe, "-test.run=^TestMCPClientEnvProbeHelper$", "--", envProbeMarker})
}

func TestStdioServerReceivesOnlyPolicyEnvironment(t *testing.T) {
	dir := t.TempDir()
	want, _ := filepath.EvalSymlinks(dir)
	report := runEnvProbe(t, probeServer(t).WithDir(dir).WithEnv(InheritEnv("PROBE_TOKEN"), SetEnv("PROBE_SET", "explicit")),
		testLaunchEnv(map[string]string{"PATH": "/usr/bin:/bin", "HOME": dir, "CANARY_SECRET": "canary", "PROBE_TOKEN": "tok"}))
	if wantNames := []string{"HOME", "PATH", "PROBE_SET", "PROBE_TOKEN"}; !slices.Equal(report.Names, wantNames) {
		t.Fatalf("child env names = %q, want exactly %q", report.Names, wantNames)
	}
	if report.Values["PROBE_TOKEN"] != "tok" || report.Values["PROBE_SET"] != "explicit" || report.Cwd != want {
		t.Fatalf("child values/cwd = (%v, %q), want (tok, explicit, %q)", report.Values, report.Cwd, want)
	}
}

func TestStdioServerWithEmptyParentGetsEmptyEnvironment(t *testing.T) {
	report := runEnvProbe(t, probeServer(t).WithDir(t.TempDir()), testLaunchEnv(nil))
	if len(report.Names) != 0 {
		t.Fatalf("child env names = %q, want none (empty must never mean inherit)", report.Names)
	}
}
