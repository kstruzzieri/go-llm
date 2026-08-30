package main

import (
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestRejectedDestinationCredentialsAreNotRendered(t *testing.T) {
	tests := []struct {
		name  string
		grant string
		want  string
	}{
		{name: "userinfo", grant: "hosted=https://user:SECRET@api.example", want: "base URL must not carry userinfo"},
		{name: "query", grant: "hosted=https://api.example?api_key=SECRET", want: "base URL must not carry a query"},
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
			for _, want := range []string{tt.want, "expected provider=https://host/base"} {
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
		"-allow-destination", "hosted=https://HOST:443/base/",
		"-allow-destination=backup=http://example.com:80/root",
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
	if err := grants.Set("team=prod=https://HOST:443/base/"); err != nil {
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

func TestDestinationGrantsRequireEqualsGrammar(t *testing.T) {
	var grants destinationGrants
	if err := grants.Set("hosted/https://host/base"); err != nil {
		t.Fatalf("destinationGrants.Set() error = %v, want deferred validation", err)
	}
	if _, err := grants.policy(); err == nil || !strings.Contains(err.Error(), "expected provider=https://host/base") {
		t.Errorf("destinationGrants.policy() error = %v, want equals-only grammar", err)
	}
}
