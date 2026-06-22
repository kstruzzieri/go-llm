package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// EditFile applies a single exact-string replacement to a workspace file.
// old_string must occur exactly once; new_string may be empty (deletion). Plan
// computes the after-content and diff; Invoke re-verifies the file is unchanged
// and the match is still unique before writing atomically.
type EditFile struct {
	ws *Workspace
	j  Journal
	mutatingBase
}

// NewEditFile builds the edit_file tool bound to ws, reporting applied edits to
// journal (may be nil).
func NewEditFile(ws *Workspace, journal Journal) *EditFile {
	return &EditFile{ws: ws, j: journal}
}

type editFileArgs struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

func (*EditFile) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "edit_file",
		Description: "Replace an exact substring in an existing workspace file. old_string must appear exactly once (otherwise the edit is refused); new_string may be empty to delete it. The change is shown as a diff and requires user approval before it is applied.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"file path relative to the workspace root"},
    "old_string":{"type":"string","description":"exact text to replace; must occur exactly once"},
    "new_string":{"type":"string","description":"replacement text (empty to delete the match)"}
  },
  "required":["path","old_string","new_string"]
}`),
	}
}

func (*EditFile) Effect() agent.Effect {
	return agent.Effect{Class: agent.Write, Approval: agent.ApprovalOnWrite}
}

// computeEdit reads the file, validates uniqueness and size, and returns the
// before/after bytes. It is shared by Plan and the Invoke re-check.
func (t *EditFile) computeEdit(args editFileArgs) (before, after []byte, err error) {
	if args.Path == "" {
		return nil, nil, fmt.Errorf("path is required")
	}
	if args.OldString == "" {
		return nil, nil, fmt.Errorf("old_string is required")
	}
	before, err = t.ws.readAll(args.Path)
	if err != nil {
		return nil, nil, err
	}
	if len(before) > mutateMaxBytes {
		return nil, nil, fmt.Errorf("file exceeds size limit")
	}
	// Sniff only the first binarySniffBytes of the EXISTING file, mirroring
	// read_file's policy (a NUL early in the file marks it binary). The bytes we will
	// actually write (`after`) are NUL-scanned in full below, so an existing NUL past
	// the sniff window cannot slip through into a written result.
	sniff := before
	if len(sniff) > binarySniffBytes {
		sniff = sniff[:binarySniffBytes]
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return nil, nil, fmt.Errorf("binary file (NUL byte detected); refusing to edit")
	}
	n := strings.Count(string(before), args.OldString)
	if n == 0 {
		return nil, nil, fmt.Errorf("old_string not found")
	}
	if n > 1 {
		return nil, nil, fmt.Errorf("ambiguous: old_string occurs %d times", n)
	}
	after = []byte(strings.Replace(string(before), args.OldString, args.NewString, 1))
	if len(after) > mutateMaxBytes {
		return nil, nil, fmt.Errorf("result exceeds size limit")
	}
	if bytes.IndexByte(after, 0) >= 0 {
		return nil, nil, fmt.Errorf("new_string would introduce a NUL byte; refusing to write binary content")
	}
	return before, after, nil
}

func (t *EditFile) Plan(_ context.Context, raw json.RawMessage) (agent.ToolPlan, error) {
	eff := t.Effect()
	var args editFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("invalid arguments: %w", err)
	}
	before, after, err := t.computeEdit(args)
	if err != nil {
		return agent.ToolPlan{Effect: eff}, err
	}
	t.store(ContentHash(raw), pendingPlan{
		path:         args.Path,
		priorContent: before,
		priorExists:  true,
		beforeHash:   ContentHash(before),
		afterContent: after,
		afterHash:    ContentHash(after),
		summary:      fmt.Sprintf("edit %s", args.Path),
	})
	return agent.ToolPlan{Effect: eff, Preview: unifiedDiff(args.Path, before, after, true)}, nil
}

func (t *EditFile) Invoke(_ context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args editFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	// Recompute from the args to produce an accurate failure reason (e.g. "old_string
	// not found" if the file changed) and to re-read current bytes; only `before` is
	// kept, to re-verify the file is unchanged. The approved bytes come from the plan.
	before, _, err := t.computeEdit(args)
	if err != nil {
		return errResult(err.Error()), nil
	}
	pp, ok := t.consume(ContentHash(raw))
	if !ok {
		return errResult("mutation preview missing; retry"), nil
	}
	// The file must be byte-identical to what Plan previewed; then write exactly the
	// bytes the user approved (pp.afterContent), symmetric with write_file. The
	// before-hash match guarantees pp.afterContent equals a fresh recomputation.
	if ContentHash(before) != pp.beforeHash {
		return errResult("file changed since preview; retry"), nil
	}
	if err := t.ws.WriteFileAtomic(pp.path, pp.afterContent); err != nil {
		return errResult(err.Error()), nil
	}
	record(t.j, MutationRecord{
		Path: pp.path, PriorContent: pp.priorContent, Existed: true,
		AfterHash: pp.afterHash, Summary: pp.summary, At: time.Now(),
	})
	return agent.ToolResult{Content: pp.summary, Preview: pp.summary}, nil
}
