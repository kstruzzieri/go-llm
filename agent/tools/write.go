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

// Origin declares this tool's observations workspace-local (#436 spec D4):
// detectors tag, never block, what it returns.
func (*WriteFile) Origin() agent.Origin { return agent.OriginWorkspace }

// Plan computes the preview and stashes the pending plan. It never mutates. On any
// validation failure it returns an error so dispatch reports the failure without
// asking the user to approve an empty diff.
func (t *WriteFile) Plan(_ context.Context, raw json.RawMessage) (agent.ToolPlan, error) {
	eff := t.Effect()
	var args writeFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Path == "" {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("path is required")
	}
	if len(args.Content) > mutateMaxBytes {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("content exceeds size limit")
	}
	if bytes.IndexByte([]byte(args.Content), 0) >= 0 {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("content contains NUL byte; refusing to write binary content")
	}
	_, priorExists, err := t.ws.resolveWriteTarget(args.Path)
	if err != nil {
		return agent.ToolPlan{Effect: eff}, toolVisibleError(err)
	}
	var prior []byte
	beforeHash := absentHash
	if priorExists {
		prior, err = t.ws.readAll(args.Path)
		if err != nil {
			return agent.ToolPlan{Effect: eff}, toolVisibleError(err)
		}
		if len(prior) > mutateMaxBytes {
			return agent.ToolPlan{Effect: eff}, fmt.Errorf("file exceeds size limit")
		}
		if bytes.IndexByte(prior, 0) >= 0 {
			return agent.ToolPlan{Effect: eff}, fmt.Errorf("binary file (NUL byte detected); refusing to overwrite")
		}
		beforeHash = ContentHash(prior)
	}
	content := []byte(args.Content)
	preview := unifiedDiff(args.Path, prior, content, priorExists)
	t.store(ContentHash(raw), pendingPlan{
		path:         args.Path,
		priorContent: prior,
		priorExists:  priorExists,
		beforeHash:   beforeHash,
		afterContent: content,
		afterHash:    ContentHash(content),
		summary:      fmt.Sprintf("write %s", args.Path),
	})
	return agent.ToolPlan{Effect: eff, Preview: preview, ApprovalKey: WriteClassApprovalKey}, nil
}

func (t *WriteFile) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	pp, ok := t.consume(ContentHash(raw))
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
		return errResult(toolVisibleError(err).Error()), nil
	}
	curHash := absentHash
	if nowExists {
		cur, rerr := t.ws.readAll(pp.path)
		if rerr != nil {
			return errResult(toolVisibleError(rerr).Error()), nil
		}
		curHash = ContentHash(cur)
	}
	if nowExists != pp.priorExists || curHash != pp.beforeHash {
		return errResult("file changed since preview; retry"), nil
	}
	// Residual TOCTOU window: an external process could change the file's content
	// between this re-read and the rename below. WriteFileAtomic re-checks path TYPE
	// (symlink/dir) before renaming but not content; without OS file locks this is an
	// accepted limitation for a local single-user coding agent.
	rec := MutationRecord{
		Path: pp.path, PriorContent: pp.priorContent, Existed: pp.priorExists,
		AfterHash: pp.afterHash, Summary: pp.summary, At: time.Now(),
	}
	toolErr, internalErr := runJournaledWrite(ctx, t.j, rec, func() error {
		return t.ws.WriteFileAtomic(pp.path, pp.afterContent)
	})
	if internalErr != nil {
		return agent.ToolResult{}, internalErr
	}
	if toolErr != nil {
		return errResult(toolVisibleError(toolErr).Error()), nil
	}
	return agent.ToolResult{Content: pp.summary, Preview: pp.summary}, nil
}
