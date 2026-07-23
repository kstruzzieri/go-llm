package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
)

const binarySniffBytes = 8 * 1024 // NUL-sniff window

// ReadFile reads a single file confined to the workspace root. Symlinks are
// never followed; the read is TOCTOU-hardened via Workspace.openRegularFile.
type ReadFile struct {
	ws *Workspace
}

// NewReadFile builds the read_file tool bound to ws.
func NewReadFile(ws *Workspace) *ReadFile { return &ReadFile{ws: ws} }

type readFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// Spec is the model-facing schema.
func (*ReadFile) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "read_file",
		Description: "Read a UTF-8 text file from the workspace. Optional 1-based inclusive start_line/end_line. Paths are relative to the workspace root; escaping the root or following a symlink is refused.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"file path relative to the workspace root"},
    "start_line":{"type":"integer","description":"1-based first line (optional)"},
    "end_line":{"type":"integer","description":"1-based last line, inclusive (optional)"}
  },
  "required":["path"]
}`),
	}
}

// Effect declares read-only, never-approval, with an OutputCap above the 256 KiB
// self-cap so the runtime (default 64 KiB) does not re-truncate the result.
func (*ReadFile) Effect() agent.Effect {
	return agent.Effect{
		Class:     agent.Read,
		Approval:  agent.ApprovalNever,
		OutputCap: readFileMaxBytes + markerHeadroom,
	}
}

// Invoke reads the file. All expected failures return (ToolResult{IsError:true}, nil).
func (t *ReadFile) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args readFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if args.Path == "" {
		return errResult("path is required"), nil
	}
	if args.StartLine < 0 || args.EndLine < 0 {
		return errResult("invalid line range: negative line number"), nil
	}
	if args.StartLine == 0 && args.EndLine > 0 {
		return errResult("invalid line range: end_line set without start_line"), nil
	}
	if args.StartLine > 0 && args.EndLine > 0 && args.EndLine < args.StartLine {
		return errResult(fmt.Sprintf("invalid line range: end_line %d < start_line %d", args.EndLine, args.StartLine)), nil
	}

	f, err := t.ws.openRegularFile(args.Path)
	if err != nil {
		return errResult(err.Error()), nil
	}
	defer func() { _ = f.Close() }()

	// Read at most readFileMaxBytes+1 so we can detect overflow.
	buf, err := io.ReadAll(io.LimitReader(f, int64(readFileMaxBytes+1)))
	if err != nil {
		return errResult(err.Error()), nil
	}
	sniff := buf
	if len(sniff) > binarySniffBytes {
		sniff = sniff[:binarySniffBytes]
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return errResult("binary file (NUL byte detected); refusing to read"), nil
	}

	truncated := false
	if len(buf) > readFileMaxBytes {
		buf = buf[:readFileMaxBytes]
		truncated = true
	}
	content := string(buf)

	if args.StartLine > 0 {
		content, err = sliceLines(content, args.StartLine, args.EndLine)
		if err != nil {
			return errResult(err.Error()), nil
		}
	}
	if truncated {
		content += fmt.Sprintf("\n[truncated after %d bytes]", readFileMaxBytes)
	}
	return agent.ToolResult{Content: content, Truncated: truncated}, nil
}

// sliceLines returns lines [start,end] (1-based, inclusive). end<=0 means EOF.
// Note: the returned slice is newline-joined WITHOUT a trailing newline, so a
// ranged read of a file's final lines drops the file's trailing newline; a
// whole-file read (no range) preserves the original bytes verbatim.
func sliceLines(content string, start, end int) (string, error) {
	lines := strings.Split(content, "\n")
	// a trailing newline yields a final empty element; drop it for counting
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if start > len(lines) {
		return "", fmt.Errorf("invalid line range: start_line %d exceeds file length %d", start, len(lines))
	}
	last := end
	if last <= 0 || last > len(lines) {
		last = len(lines)
	}
	return strings.Join(lines[start-1:last], "\n"), nil
}

// errResult is the shared IsError constructor for tool-level failures.
func errResult(msg string) agent.ToolResult {
	return agent.ToolResult{IsError: true, Content: msg}
}
