package main

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/mcpclient"
)

func TestSplitAlias(t *testing.T) {
	tests := []struct{ in, alias, spec string }{
		{"fs=npx server", "fs", "npx server"},
		{"npx server", "", "npx server"},
		{"https://h/p?token=x", "", "https://h/p?token=x"},
		{"env FOO=bar mycmd", "", "env FOO=bar mycmd"},
		{"FOO=bar mycmd", "FOO", "bar mycmd"},
	}
	for _, tt := range tests {
		a, s := splitAlias(tt.in)
		if a != tt.alias || s != tt.spec {
			t.Errorf("splitAlias(%q) = (%q,%q), want (%q,%q)", tt.in, a, s, tt.alias, tt.spec)
		}
	}
}

func TestParseMCPServersDerivesAndDedupes(t *testing.T) {
	servers, err := parseMCPServers(
		[]string{"npx mcp-fs /tmp", "fs2=npx mcp-fs /var"},
		[]string{"https://api.example.com/mcp"},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(servers) != 3 {
		t.Fatalf("got %d servers", len(servers))
	}
	seen := map[string]bool{}
	for _, s := range servers {
		if s.Alias == "" || seen[s.Alias] {
			t.Fatalf("alias %q empty or duplicate", s.Alias)
		}
		seen[s.Alias] = true
	}
}

func TestParseMCPServersStdioQuotedArgs(t *testing.T) {
	servers, err := parseMCPServers([]string{`fs="/tmp/my server" --config "Project A.json" 'single quoted arg' bare`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := serverCommand(servers[0])
	want := []string{"/tmp/my server", "--config", "Project A.json", "single quoted arg", "bare"}
	if len(got) != len(want) {
		t.Fatalf("command = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("command = %#v, want %#v", got, want)
		}
	}
}

func TestParseMCPServersDerivedAliasSuffixStaysValid(t *testing.T) {
	long := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	servers, err := parseMCPServers([]string{long, long}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers[1].Alias) > 64 {
		t.Fatalf("alias %q length = %d, want <= 64", servers[1].Alias, len(servers[1].Alias))
	}
	if servers[0].Alias == servers[1].Alias {
		t.Fatalf("aliases must be unique: %q", servers[0].Alias)
	}
}

func serverCommand(server any) []string {
	v := reflect.ValueOf(server).FieldByName("command")
	out := make([]string, v.Len())
	for i := range out {
		out[i] = v.Index(i).String()
	}
	return out
}

func TestParseMCPServersExplicitAliasCollision(t *testing.T) {
	if _, err := parseMCPServers([]string{"a=x", "a=y"}, nil); err == nil {
		t.Fatal("explicit duplicate alias must error")
	}
}

func TestParseMCPServersEmptyCommand(t *testing.T) {
	if _, err := parseMCPServers([]string{"   "}, nil); err == nil {
		t.Fatal("empty stdio command must error")
	}
}

func TestApproverInstalledWhenMCPAttached(t *testing.T) {
	if !needsApprover(false, false, true) {
		t.Fatal("MCP-attached read-only session must install the approver")
	}
	if needsApprover(false, false, false) {
		t.Fatal("plain read-only session must NOT install an approver")
	}
	if !needsApprover(true, false, false) {
		t.Fatal("write session must install the approver")
	}
}

func TestParseMCPServersRejectsMalformedHTTP(t *testing.T) {
	for _, spec := range []string{"https://?token=credential-value", "https://bad%zz/?token=credential-value", "https://:443/?token=credential-value", "/path?token=credential-value", "ftp://example.com/?token=credential-value"} {
		for _, prefix := range []string{"", "fs="} {
			servers, err := parseMCPServers(nil, []string{prefix + spec})
			if err == nil || err.Error() != "-mcp-http: expected an absolute http or https URL with a host" || len(servers) != 0 {
				t.Fatalf("malformed HTTP yielded servers=%v err=%v", servers, err)
			}
		}
	}
}

func TestParseMCPServersHTTPAliasesExcludeCredentials(t *testing.T) {
	servers, err := parseMCPServers(nil, []string{
		"https://user:credential-value@api.example.com:8443/mcp?token=credential-value",
		"https://api.example.com:8443/other?token=other-credential",
		"stable=https://api.example.com:8443/mcp?token=credential-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"apiexamplecom8443", "apiexamplecom84432", "stable"} {
		if servers[i].Alias != want {
			t.Fatalf("alias %d=%q, want %q", i, servers[i].Alias, want)
		}
	}
}

func serverSelection(server mcpclient.Server) (names []string, set bool) {
	v := reflect.ValueOf(server)
	tools := v.FieldByName("tools")
	for i := 0; i < tools.Len(); i++ {
		names = append(names, tools.Index(i).String())
	}
	return names, v.FieldByName("toolsSet").Bool()
}

func TestApplyMCPTools(t *testing.T) {
	parse := func(t *testing.T) []mcpclient.Server {
		t.Helper()
		servers, err := parseMCPServers([]string{"fs=server one", "npx other"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return servers
	}
	servers, err := applyMCPTools(parse(t), []string{"fs=read,write", "npx="})
	if err != nil {
		t.Fatal(err)
	}
	if names, set := serverSelection(servers[0]); !set || !reflect.DeepEqual(names, []string{"read", "write"}) {
		t.Fatalf("fs selection = (%q, %v), want ([read write], true)", names, set)
	}
	if names, set := serverSelection(servers[1]); !set || len(names) != 0 {
		t.Fatalf("npx selection = (%q, %v), want explicit-empty", names, set)
	}
	if _, set := serverSelection(parse(t)[0]); set {
		t.Fatal("no -mcp-tools flag must leave selection omitted")
	}
	// Tool names cannot contain spaces, so a space after a comma is trimmed
	// instead of being reported as an unexplained bad entry.
	spaced, err := applyMCPTools(parse(t), []string{"fs=read, write"})
	if err != nil {
		t.Fatalf("space after a comma rejected: %v", err)
	}
	if names, _ := serverSelection(spaced[0]); !reflect.DeepEqual(names, []string{"read", "write"}) {
		t.Fatalf("spaced selection = %q, want [read write]", names)
	}
	// "mcp__fs__" is 9 bytes and composed names are capped at 64, so the
	// longest remote name for alias fs is 55 bytes.
	if _, err := applyMCPTools(parse(t), []string{"fs=" + strings.Repeat("a", 55)}); err != nil {
		t.Fatalf("55-byte name for alias fs rejected: %v", err)
	}
	many := make([]string, 129)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	if _, err := applyMCPTools(parse(t), []string{"fs=" + strings.Join(many[:128], ",")}); err != nil {
		t.Fatalf("128 names for alias fs rejected: %v", err)
	}
	for _, tt := range []struct {
		flags []string
		want  string
	}{
		{[]string{"fs"}, "-mcp-tools #1: expected alias=name[,name...]"},
		{[]string{"bad alias=read"}, "-mcp-tools #1: expected alias=name[,name...]"},
		{[]string{"missing=read"}, "-mcp-tools #1: alias is not a configured MCP server"},
		{[]string{"fs=read", "fs=write"}, "-mcp-tools #2: alias is already selected"},
		{[]string{"fs=read,,write"}, "-mcp-tools #1: entry 2 is not a tool name for this alias"},
		{[]string{"fs=read, ,write"}, "-mcp-tools #1: entry 2 is not a tool name for this alias"},
		{[]string{"fs=re ad"}, "-mcp-tools #1: entry 1 is not a tool name for this alias"},
		{[]string{"fs=read,credential-value!"}, "-mcp-tools #1: entry 2 is not a tool name for this alias"},
		{[]string{"fs=" + strings.Repeat("a", 56)}, "-mcp-tools #1: entry 1 is not a tool name for this alias"},
		{[]string{"fs=read,read"}, "-mcp-tools #1: entry 2 repeats a name"},
		{[]string{"fs=" + strings.Join(many, ",")}, "-mcp-tools #1: more than 128 names"},
	} {
		_, err := applyMCPTools(parse(t), tt.flags)
		if err == nil || err.Error() != tt.want {
			t.Fatalf("applyMCPTools(%q) error = %v, want %q", tt.flags, err, tt.want)
		}
		if strings.Contains(err.Error(), "credential-value") {
			t.Fatalf("applyMCPTools echoed supplied text: %v", err)
		}
	}
}

func TestMCPToolsRejectedInGoalAndPlan(t *testing.T) {
	for _, mode := range []string{"-goal", "-plan"} {
		in, out, diag := runTestFiles(t)
		err := run([]string{mode, "unused", "-mcp-tools", "fs=read"}, in, out, diag)
		// The exact mode rejection: without the guard, -mcp-tools fails later with a different error (an unknown alias for -goal, the approval-flag requirement for -plan).
		if err == nil || !strings.Contains(err.Error(), "does not attach MCP tools") {
			t.Fatalf("%s with -mcp-tools err = %v, want the mode's MCP rejection", mode, err)
		}
	}
}

// TestMCPToolsValidationMatchesLibrary keeps the CLI's positional pre-check in
// step with mcpclient's fatal selection validation.
func TestMCPToolsValidationMatchesLibrary(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	pins, err := mcpclient.NewPinStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	many := make([]string, 129)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	for _, names := range [][]string{{strings.Repeat("a", 55)}, {strings.Repeat("a", 56)}, many[:128], many, {"a.b"}, {"a", "a"}} {
		server := mcpclient.HTTPServer("fs", "http://127.0.0.1:1")
		_, cliErr := applyMCPTools([]mcpclient.Server{server}, []string{"fs=" + strings.Join(names, ",")})
		_, libErr := mcpclient.Inspect(t.Context(), mcpClientImpl(), server.WithTools(names...), pins)
		var ae *mcpclient.AdmissionError
		libRejects := errors.As(libErr, &ae) && ae.Reason == "invalid_config"
		if (cliErr != nil) != libRejects {
			t.Fatalf("%d names: CLI rejects=%v, library rejects=%v (%v)", len(names), cliErr != nil, libRejects, libErr)
		}
	}
}

func TestWithMCPPolicy(t *testing.T) {
	root := t.TempDir()
	parse := func(t *testing.T) []mcpclient.Server {
		t.Helper()
		servers, err := parseMCPServers([]string{"fs=server one", "npx other"}, []string{"api=https://example.com/mcp"})
		if err != nil {
			t.Fatal(err)
		}
		return servers
	}
	// The space after the comma must be trimmed, not rejected (PR 1 parity).
	servers, err := withMCPPolicy(root, parse(t), 2, []string{"fs=GITHUB_TOKEN, HTTPS_PROXY"})
	if err != nil {
		t.Fatal(err)
	}
	for i, wantDir := range []string{root, root, ""} {
		if got := reflect.ValueOf(servers[i]).FieldByName("dir").String(); got != wantDir {
			t.Fatalf("server %d dir = %q, want %q", i, got, wantDir)
		}
	}
	if got := reflect.ValueOf(servers[0]).FieldByName("env").Len(); got != 2 {
		t.Fatalf("fs env additions = %d, want 2", got)
	}
	for _, tt := range []struct {
		flags []string
		want  string
	}{
		{[]string{"fs"}, "-mcp-env #1: expected alias=NAME[,NAME...]"},
		{[]string{"fs="}, "-mcp-env #1: expected alias=NAME[,NAME...]"},
		{[]string{"missing=A"}, "-mcp-env #1: alias is not a configured MCP server"},
		{[]string{"api=A"}, "-mcp-env #1: alias is not a stdio MCP server"},
		{[]string{"fs=A", "fs=B"}, "-mcp-env #2: alias is already configured"},
		{[]string{"fs=A,1BAD"}, "-mcp-env #1: entry 2 is not a variable name"},
		{[]string{"fs=TOKEN=credential-value"}, "-mcp-env #1: entry 1 is not a variable name"},
		{[]string{"fs=A,A"}, "-mcp-env #1: entry 2 repeats a name"},
	} {
		_, err := withMCPPolicy(root, parse(t), 2, tt.flags)
		if err == nil || err.Error() != tt.want || strings.Contains(err.Error(), "credential-value") {
			t.Fatalf("withMCPPolicy(%q) error = %v, want %q", tt.flags, err, tt.want)
		}
	}
}

func TestMCPEnvRejectedInGoalAndPlan(t *testing.T) {
	for _, mode := range []string{"-goal", "-plan"} {
		in, out, diag := runTestFiles(t)
		err := run([]string{mode, "unused", "-mcp-env", "fs=A"}, in, out, diag)
		// The exact mode rejection, not a later -mcp-env alias error.
		if err == nil || !strings.Contains(err.Error(), "does not attach MCP tools") {
			t.Fatalf("%s with -mcp-env err = %v, want the mode's MCP rejection", mode, err)
		}
	}
}

func TestMCPEnvInvalidThroughCLIDoesNotLeak(t *testing.T) {
	in, out, diag := runTestFiles(t)
	err := run([]string{"-mcp-stdio", "fs=server", "-mcp-env", "fs=TOKEN=credential-value", "-no-project-context", "-no-git-context"}, in, out, diag)
	if err == nil || !strings.Contains(err.Error(), "-mcp-env #1: entry 1 is not a variable name") {
		t.Fatalf("invalid -mcp-env err = %v", err)
	}
	if strings.Contains(err.Error()+readRunTestFile(t, out)+readRunTestFile(t, diag), "credential-value") {
		t.Fatal("invalid -mcp-env echoed the supplied text")
	}
}
