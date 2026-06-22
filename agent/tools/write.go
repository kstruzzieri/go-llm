package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// WriteFile creates or overwrites a workspace file. It is a PlanningTool: Plan
// computes the diff preview and stashes the resulting content keyed by raw-args
// hash; Invoke re-checks the previewed state and writes atomically.
type WriteFile struct {
	ws *Workspace
	j  Journal
	mutatingBase
}

// NewWriteFile builds the write_file tool bound to ws, reporting applied writes
// to journal (may be nil).
func NewWriteFile(ws *Workspace, journal Journal) *WriteFile {
	return &WriteFile{ws: ws, j: journal}
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (*WriteFile) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "write_file",
		Description: "Create a new file or fully overwrite an existing one in the workspace. Paths are relative to the workspace root; escaping the root or following a symlink is refused. The change is shown as a diff and requires user approval before it is applied.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"file path relative to the workspace root"},
    "content":{"type":"string","description":"the full new file content"}
  },
  "required":["path","content"]
}`),
	}
}

func (*WriteFile) Effect() agent.Effect {
	return agent.Effect{Class: agent.Write, Approval: agent.ApprovalOnWrite}
}

// Plan computes the preview and stashes the pending plan. It never mutates. On any
// validation failure it returns a ToolPlan with no Preview so the call is still
// gated (Effect is Write) but the matching Invoke will fail closed (no pending plan).
func (t *WriteFile) Plan(_ context.Context, raw json.RawMessage) (agent.ToolPlan, error) {
	eff := t.Effect()
	var args writeFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolPlan{Effect: eff}, nil
	}
	if args.Path == "" || len(args.Content) > mutateMaxBytes {
		return agent.ToolPlan{Effect: eff}, nil
	}
	if bytes.IndexByte([]byte(args.Content), 0) >= 0 {
		return agent.ToolPlan{Effect: eff}, nil // refuse binary content (no approvable preview)
	}
	_, priorExists, err := t.ws.resolveWriteTarget(args.Path)
	if err != nil {
		return agent.ToolPlan{Effect: eff}, nil
	}
	var prior []byte
	beforeHash := absentHash
	if priorExists {
		prior, err = t.ws.readAll(args.Path)
		if err != nil || len(prior) > mutateMaxBytes {
			return agent.ToolPlan{Effect: eff}, nil
		}
		beforeHash = contentHash(prior)
	}
	content := []byte(args.Content)
	preview := unifiedDiff(args.Path, prior, content, priorExists)
	t.store(contentHash(raw), pendingPlan{
		path:         args.Path,
		priorContent: prior,
		priorExists:  priorExists,
		beforeHash:   beforeHash,
		afterContent: content,
		afterHash:    contentHash(content),
		summary:      fmt.Sprintf("write %s", args.Path),
	})
	return agent.ToolPlan{Effect: eff, Preview: preview}, nil
}

func (t *WriteFile) Invoke(_ context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	pp, ok := t.consume(contentHash(raw))
	if !ok {
		return errResult("mutation preview missing; retry"), nil
	}
	// Defensive invariant: Plan already rejects oversize content (no plan is stored),
	// so this never fires in normal flow; it guards against a future carrier change.
	if len(pp.afterContent) > mutateMaxBytes {
		return errResult("content exceeds size limit"), nil
	}
	_, nowExists, err := t.ws.resolveWriteTarget(pp.path)
	if err != nil {
		return errResult(err.Error()), nil
	}
	curHash := absentHash
	if nowExists {
		cur, rerr := t.ws.readAll(pp.path)
		if rerr != nil {
			return errResult(rerr.Error()), nil
		}
		curHash = contentHash(cur)
	}
	if nowExists != pp.priorExists || curHash != pp.beforeHash {
		return errResult("file changed since preview; retry"), nil
	}
	// Residual TOCTOU window: an external process could change the file's content
	// between this re-read and the rename below. WriteFileAtomic re-checks path TYPE
	// (symlink/dir) before renaming but not content; without OS file locks this is an
	// accepted limitation for a local single-user coding agent.
	if err := t.ws.WriteFileAtomic(pp.path, pp.afterContent); err != nil {
		return errResult(err.Error()), nil
	}
	record(t.j, MutationRecord{
		Path: pp.path, PriorContent: pp.priorContent, Existed: pp.priorExists,
		AfterHash: pp.afterHash, Summary: pp.summary, At: time.Now(),
	})
	return agent.ToolResult{Content: pp.summary, Preview: pp.summary}, nil
}
