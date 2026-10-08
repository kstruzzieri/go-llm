package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// mutationJournal is the RAM-only undo stack used by AgentFlow task mode's
// compositeJournal (agentflow_driver.go). The interactive REPL's /undo is
// backed by the durable checkpointJournal (#355); this type deliberately
// keeps the pre-#355 semantics for task mode until a task-mode requirement
// is filed. It implements agenttools.Journal, receiving a record after each
// applied write, and restores the most recent mutation via the same
// containment-checked Workspace primitives the tools use. peek -> verify ->
// restore -> pop: a refused or failed undo leaves the record on the stack so
// nothing is lost.
type mutationJournal struct {
	ws   *agenttools.Workspace
	mu   sync.Mutex
	recs []agenttools.MutationRecord
}

func newMutationJournal(ws *agenttools.Workspace) *mutationJournal {
	return &mutationJournal{ws: ws}
}

// readForUndo is ReadFileWithModeForUndo for both undo journals, whose
// not-exist result is evidence about the target only while the root is
// reachable: a root renamed away also reads as not-exist, yet the file lives
// on wherever the root went. %v drops the root error's not-exist chain, so no
// caller mistakes it for absence.
// ponytail: the root is checked after the read, so a root moved away and back
// within one read still reads as absent; closing that needs the read itself to
// report which component was missing.
func readForUndo(ws *agenttools.Workspace, path string) ([]byte, fs.FileMode, error) {
	cur, mode, err := ws.ReadFileWithModeForUndo(path)
	if os.IsNotExist(err) {
		if rootErr := ws.VerifyRoot(); rootErr != nil {
			return nil, 0, fmt.Errorf("golem: workspace root unreachable: %v", rootErr)
		}
	}
	return cur, mode, err
}

// Record pushes a successful mutation. Safe for concurrent use.
func (j *mutationJournal) Record(rec agenttools.MutationRecord) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.recs = append(j.recs, rec)
}

// undo reverts the most recent mutation, writing status to out.
func (j *mutationJournal) undo(out io.Writer) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.recs) == 0 {
		_, _ = fmt.Fprintln(out, "nothing to undo")
		return
	}
	rec := j.recs[len(j.recs)-1] // peek

	// Bytes and mode come from ONE open handle so they cannot race apart;
	// the mode participates only for tracked records (#443 promotion).
	cur, curMode, err := readForUndo(j.ws, rec.Path)
	curExists := err == nil
	curHash := ""
	if err != nil && !os.IsNotExist(err) {
		_, _ = fmt.Fprintf(out, "cannot undo %s: file changed since golem wrote it\n", rec.Path)
		return // leave the record on the stack
	}
	if curExists {
		curHash = agenttools.ContentHash(cur)
	}
	if rec.TrackedMode && curExists && curMode != rec.AfterMode {
		// A tracked created file whose complete mode drifted — rwx, special
		// bits, or type — must not be deleted even with identical bytes.
		_, _ = fmt.Fprintf(out, "cannot undo %s: file changed since golem wrote it\n", rec.Path)
		return // leave the record on the stack
	}
	if !curExists {
		if !rec.Existed {
			// The created file is already gone — desired state reached. Pop and report.
			j.recs = j.recs[:len(j.recs)-1]
			_, _ = fmt.Fprintf(out, "undid %s (already absent)\n", rec.Path)
			return
		}
		_, _ = fmt.Fprintf(out, "cannot undo %s: file changed since golem wrote it\n", rec.Path)
		return // leave the record on the stack
	}
	if curHash != rec.AfterHash {
		_, _ = fmt.Fprintf(out, "cannot undo %s: file changed since golem wrote it\n", rec.Path)
		return // leave the record on the stack
	}

	expected := agenttools.FilePrecondition{Exists: true, Hash: rec.AfterHash}
	if rec.TrackedMode {
		expected.CheckMode, expected.Mode = true, rec.AfterMode
	}
	if rec.Existed {
		err = j.ws.WriteFileAtomicIfMatch(rec.Path, rec.PriorContent, expected)
	} else {
		err = j.ws.RemoveFileIfMatch(rec.Path, expected)
	}
	if err != nil {
		// Same two lines as checkpoint restoreFile: the shared exact refusal,
		// then the cause (which may carry joined cleanup failures).
		if errors.Is(err, agenttools.ErrPreconditionMismatch) {
			_, _ = fmt.Fprintf(out, checkpointUndoRefusal, rec.Path)
		}
		_, _ = fmt.Fprintf(out, "undo failed for %s: %v\n", rec.Path, err)
		return
	}
	j.recs = j.recs[:len(j.recs)-1] // pop only on success
	_, _ = fmt.Fprintf(out, "undid %s\n", rec.Path)
}
