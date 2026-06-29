package main

import "testing"

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
