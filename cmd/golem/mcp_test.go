package main

import (
	"reflect"
	"testing"
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
