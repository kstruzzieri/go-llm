package main

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kstruzzieri/go-llm/mcpclient"
)

const mcpClientName = "golem"

var golemAliasRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// stringSliceFlag is a repeatable string flag (-mcp-stdio a -mcp-stdio b).
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

// splitAlias splits "[alias=]spec". The left of the first '=' is the alias only
// if it fully matches the alias regex; otherwise the whole value is the spec.
// This keeps URLs with query strings and `env KEY=val cmd` stdio forms intact.
// Caveat (documented in flag help): a bare leading `KEY=val cmd` is read as
// alias=KEY; use `env KEY=val cmd` to pass environment variables.
func splitAlias(value string) (alias, spec string) {
	if i := strings.IndexByte(value, '='); i >= 0 {
		if cand := value[:i]; golemAliasRE.MatchString(cand) {
			return cand, value[i+1:]
		}
	}
	return "", value
}

func sanitizeAlias(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// parseMCPServers turns the repeatable flag values into mcpclient.Server configs,
// deriving aliases when omitted and rejecting explicit-alias collisions. (Connect
// also rejects duplicates, fatally; this gives a clearer per-flag message.)
func parseMCPServers(stdioFlags, httpFlags []string) ([]mcpclient.Server, error) {
	used := make(map[string]bool)
	var servers []mcpclient.Server

	derive := func(base string) string {
		a := sanitizeAlias(base)
		if a == "" {
			a = "mcp"
		}
		cand := a
		for n := 2; used[cand]; n++ {
			cand = fmt.Sprintf("%s%d", a, n)
		}
		used[cand] = true
		return cand
	}
	claimExplicit := func(flagName, raw, alias string) error {
		if used[alias] {
			return fmt.Errorf("-%s %q: duplicate alias %q", flagName, raw, alias)
		}
		used[alias] = true
		return nil
	}

	for _, f := range stdioFlags {
		alias, spec := splitAlias(strings.TrimSpace(f))
		fields := strings.Fields(spec)
		if len(fields) == 0 {
			return nil, fmt.Errorf("-mcp-stdio %q: empty command", f)
		}
		if alias == "" {
			alias = derive(filepath.Base(fields[0]))
		} else if err := claimExplicit("mcp-stdio", f, alias); err != nil {
			return nil, err
		}
		servers = append(servers, mcpclient.StdioServer(alias, fields))
	}

	for _, f := range httpFlags {
		alias, spec := splitAlias(strings.TrimSpace(f))
		spec = strings.TrimSpace(spec)
		if spec == "" {
			return nil, fmt.Errorf("-mcp-http %q: empty endpoint", f)
		}
		if alias == "" {
			host := spec
			if u, err := url.Parse(spec); err == nil && u.Host != "" {
				host = u.Host
			}
			alias = derive(host)
		} else if err := claimExplicit("mcp-http", f, alias); err != nil {
			return nil, err
		}
		servers = append(servers, mcpclient.HTTPServer(alias, spec))
	}
	return servers, nil
}

// needsApprover reports whether the REPL must install the interactive approver.
// MCP tools are ApprovalAlways, so they require it even with no write/exec.
func needsApprover(allowWrite, allowExec, mcpAttached bool) bool {
	return allowWrite || allowExec || mcpAttached
}

func mcpClientImpl() mcpclient.Implementation {
	return mcpclient.Implementation{Name: mcpClientName, Version: "dev"}
}
