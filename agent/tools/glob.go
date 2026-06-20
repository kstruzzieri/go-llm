package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
)

// matchGlob matches a slash-separated relative path against a glob pattern,
// anchored at the workspace root. "*" and "?" match within a single segment
// (they never cross "/"); "**" matches zero or more whole segments. This is a
// deliberately small, root-anchored matcher — unlike rag's gitignore matcher,
// which matches a basename at any depth.
func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

// Glob lists workspace paths matching a glob pattern (supports **). Directories
// are marked with a trailing "/"; symlink entries are emitted marked and never
// followed.
type Glob struct {
	ws *Workspace
}

// NewGlob builds the glob tool bound to ws.
func NewGlob(ws *Workspace) *Glob { return &Glob{ws: ws} }

type globArgs struct {
	Pattern string `json:"pattern"`
}

// Spec is the model-facing schema.
func (*Glob) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "glob",
		Description: "List workspace paths matching a glob pattern. '*' and '?' match within one path segment; '**' matches across directories (e.g. **/*.go). Paths are relative to the root; directories end with '/'; symlinks are marked and never followed.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "pattern":{"type":"string","description":"glob pattern, e.g. **/*.go or cmd/*/main.go"}
  },
  "required":["pattern"]
}`),
	}
}

// Effect declares read-only, never-approval. The entry self-cap is count-based
// (listMaxEntries); OutputCap is a generous byte backstop (listOutputCap) above a
// typical 1000-entry render so the truncation marker is not runtime-cut.
func (*Glob) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever, OutputCap: listOutputCap}
}

// Invoke walks and matches relative paths against the pattern.
func (t *Glob) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args globArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if args.Pattern == "" {
		return errResult("pattern is required"), nil
	}

	var entries []string
	truncated := false
	walkErr := t.ws.walk(ctx, func(rel string, d fs.DirEntry) error {
		if !matchGlob(args.Pattern, rel) {
			return nil
		}
		if len(entries) >= listMaxEntries {
			truncated = true
			return fs.SkipAll
		}
		entries = append(entries, markEntry(rel, d))
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll {
		return errResult("glob failed: " + walkErr.Error()), nil
	}
	return renderEntries(entries, truncated), nil
}

// markEntry renders a relative path with a kind marker: "/" for directories,
// " (symlink)" for symlinks. Regular files are bare.
func markEntry(rel string, d fs.DirEntry) string {
	switch {
	case d.Type()&fs.ModeSymlink != 0:
		return rel + " (symlink)"
	case d.IsDir():
		return rel + "/"
	default:
		return rel
	}
}

// renderEntries sorts for determinism and appends a marker when truncated.
func renderEntries(entries []string, truncated bool) agent.ToolResult {
	sort.Strings(entries)
	content := strings.Join(entries, "\n")
	if content == "" {
		content = "no entries"
	}
	if truncated {
		content += fmt.Sprintf("\n[truncated after %d entries]", listMaxEntries)
	}
	return agent.ToolResult{Content: content, Truncated: truncated}
}

// List lists the immediate children of one directory under the workspace root.
// A symlinked-directory target is rejected; symlink entries inside a real
// directory are emitted marked and never followed.
type List struct {
	ws *Workspace
}

// NewList builds the list tool bound to ws.
func NewList(ws *Workspace) *List { return &List{ws: ws} }

type listArgs struct {
	Path string `json:"path"`
}

// Spec is the model-facing schema.
func (*List) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "list",
		Description: "List the immediate entries of a directory under the workspace root (non-recursive). Defaults to the root when path is omitted. Directories end with '/'; symlinks are marked and never followed; a symlinked directory target is refused.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"directory relative to the root; omit or '.' for the root"}
  }
}`),
	}
}

// Effect declares read-only, never-approval; same OutputCap rationale as Glob.Effect.
func (*List) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever, OutputCap: listOutputCap}
}

// Invoke reads a single directory level. Expected failures return IsError.
func (t *List) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args listArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	p := args.Path
	if p == "" {
		p = "."
	}
	abs, err := t.ws.resolveDir(p)
	if err != nil {
		return errResult(err.Error()), nil
	}
	dirents, err := os.ReadDir(abs)
	if err != nil {
		return errResult(err.Error()), nil
	}

	relBase, err := filepath.Rel(t.ws.root, abs)
	if err != nil {
		return errResult(err.Error()), nil
	}
	base := filepath.ToSlash(relBase)
	if base == "." {
		base = ""
	}
	var entries []string
	truncated := false
	for _, d := range dirents {
		if d.IsDir() && ignoreDirs[d.Name()] {
			continue
		}
		if len(entries) >= listMaxEntries {
			truncated = true
			break
		}
		rel := d.Name()
		if base != "" {
			rel = base + "/" + d.Name()
		}
		entries = append(entries, markEntry(rel, d))
	}
	return renderEntries(entries, truncated), nil
}

// matchSegments matches pattern segments against name segments. Each "**"
// fans out across every possible skip count, so the worst case is combinatorial
// in (path-depth, number of "**" segments). In practice the matched names come
// from a real filesystem walk (bounded directory depth) and patterns carry 1–3
// "**", so the cost is small; this matcher is not intended for adversarial
// unbounded pattern/path input.
func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true // trailing ** matches every remaining segment
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}
