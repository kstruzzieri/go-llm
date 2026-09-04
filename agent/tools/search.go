package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
)

// Search greps the workspace tree with RE2. It skips ignore-set directories,
// binary files (NUL sniff), and symlink entries (never read or descended).
type Search struct {
	ws *Workspace
}

// NewSearch builds the search tool bound to ws.
func NewSearch(ws *Workspace) *Search { return &Search{ws: ws} }

type searchArgs struct {
	Pattern string `json:"pattern"`
	Regex   bool   `json:"regex"`
}

// Spec is the model-facing schema.
func (*Search) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "search",
		Description: "Search file contents under the workspace root. Literal substring by default; set regex:true for an RE2 pattern. Returns path:line: text. Skips .git, vendor, node_modules, .superpowers, binary files, and symlinks.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "pattern":{"type":"string","description":"text to find (literal unless regex:true)"},
    "regex":{"type":"boolean","description":"treat pattern as an RE2 regular expression"}
  },
  "required":["pattern"]
}`),
	}
}

// Effect declares read-only, never-approval, with OutputCap above the byte self-cap.
func (*Search) Effect() agent.Effect {
	return agent.Effect{
		Class:     agent.Read,
		Approval:  agent.ApprovalNever,
		OutputCap: searchMaxBytes + markerHeadroom,
	}
}

// Origin declares this tool's observations workspace-local (#436 spec D4):
// detectors tag, never block, what it returns.
func (*Search) Origin() agent.Origin { return agent.OriginWorkspace }

// Invoke walks and matches. Expected failures return (ToolResult{IsError:true}, nil).
func (t *Search) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args searchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if args.Pattern == "" {
		return errResult("pattern is required"), nil
	}
	expr := args.Pattern
	if !args.Regex {
		expr = regexp.QuoteMeta(args.Pattern)
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return errResult("invalid regex: " + err.Error()), nil
	}

	var out strings.Builder
	matches := 0
	truncated := false

	walkErr := t.ws.walk(ctx, func(rel string, d fs.DirEntry) error {
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never read a symlink
		}
		fileTruncated, err := t.searchFile(rel, re, &out, &matches)
		if err != nil {
			return err
		}
		if fileTruncated {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll {
		return errResult(toolErrMessage(walkErr)), nil
	}

	content := strings.TrimRight(out.String(), "\n")
	if matches == 0 {
		content = "no matches"
	}
	if truncated {
		content += fmt.Sprintf("\n[truncated after %d matches / %d bytes]", searchMaxMatches, searchMaxBytes)
	}
	return agent.ToolResult{Content: content, Truncated: truncated}, nil
}

// searchFile opens one file (TOCTOU-hardened), skips it if binary or unreadable,
// and appends matching lines. It returns true when a cap was reached (the caller
// stops the walk). The file is closed before returning — never deferred to the
// end of the walk — so large trees do not exhaust descriptors.
func (t *Search) searchFile(rel string, re *regexp.Regexp, out *strings.Builder, matches *int) (bool, error) {
	f, err := t.ws.openRegularFile(rel)
	if err != nil {
		return false, nil // unreadable or raced file: skip, not fatal
	}
	defer func() { _ = f.Close() }()

	sniff := make([]byte, binarySniffBytes)
	n, err := f.Read(sniff)
	if err != nil && err != io.EOF {
		return false, nil
	}
	if bytes.IndexByte(sniff[:n], 0) >= 0 {
		return false, nil // binary: skip
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, nil
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if !re.MatchString(text) {
			continue
		}
		entry := fmt.Sprintf("%s:%d: %s\n", rel, line, text)
		if *matches >= searchMaxMatches || out.Len()+len(entry) > searchMaxBytes {
			return true, nil
		}
		out.WriteString(entry)
		(*matches)++
	}
	if err := sc.Err(); err != nil {
		return false, nil // overlong/unreadable line: skip rest of file, not fatal
	}
	return false, nil
}
