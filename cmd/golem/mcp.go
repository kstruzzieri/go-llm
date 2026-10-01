package main

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kstruzzieri/go-llm/internal/mcpstdio"
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
		for n := 1; ; n++ {
			cand := a
			if n > 1 {
				suffix := strconv.Itoa(n)
				if maxBase := 64 - len(suffix); len(cand) > maxBase {
					cand = cand[:maxBase]
				}
				cand += suffix
			}
			if !used[cand] {
				used[cand] = true
				return cand
			}
		}
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
		fields, err := mcpstdio.ParseCommand(spec)
		if err != nil {
			return nil, fmt.Errorf("-mcp-stdio %q: %w", f, err)
		}
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
		u, err := url.Parse(spec)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return nil, fmt.Errorf("-mcp-http: expected an absolute http or https URL with a host")
		}
		if alias == "" {
			// Derive only from a parsed host, never credential-bearing URL text.
			alias = derive(u.Host)
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

var mcpToolNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// maxMCPToolName mirrors mcpclient's composed-name cap: "mcp__<alias>__<name>"
// must fit the strict provider function-name limit of 64 bytes.
const maxMCPToolName = 64

// applyMCPTools applies repeatable -mcp-tools 'alias=a,b' values to parsed
// servers; 'alias=' selects no tools. Errors name the flag occurrence and
// entry by position and never echo supplied text.
func applyMCPTools(servers []mcpclient.Server, flags []string) ([]mcpclient.Server, error) {
	index := make(map[string]int, len(servers))
	for i, server := range servers {
		index[server.Alias] = i
	}
	selected := make(map[string]bool, len(flags))
	for n, raw := range flags {
		alias, list, ok := strings.Cut(strings.TrimSpace(raw), "=")
		if !ok || !golemAliasRE.MatchString(alias) {
			return nil, fmt.Errorf("-mcp-tools #%d: expected alias=name[,name...]", n+1)
		}
		i, known := index[alias]
		if !known {
			return nil, fmt.Errorf("-mcp-tools #%d: alias is not a configured MCP server", n+1)
		}
		if selected[alias] {
			return nil, fmt.Errorf("-mcp-tools #%d: alias is already selected", n+1)
		}
		selected[alias] = true
		var names []string
		if list != "" {
			names = strings.Split(list, ",")
		}
		if len(names) > 128 {
			return nil, fmt.Errorf("-mcp-tools #%d: more than 128 names", n+1)
		}
		seen := make(map[string]bool, len(names))
		for e, name := range names {
			if !mcpToolNameRE.MatchString(name) || len("mcp__"+alias+"__"+name) > maxMCPToolName {
				return nil, fmt.Errorf("-mcp-tools #%d: entry %d is not a tool name for this alias", n+1, e+1)
			}
			if seen[name] {
				return nil, fmt.Errorf("-mcp-tools #%d: entry %d repeats a name", n+1, e+1)
			}
			seen[name] = true
		}
		servers[i] = servers[i].WithTools(names...)
	}
	return servers, nil
}
