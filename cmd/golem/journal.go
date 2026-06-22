package main

import (
	"fmt"
	"io"
	"os"
	"sync"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// mutationJournal is the in-session undo stack. It implements agenttools.Journal,
// receiving a record after each applied write, and restores the most recent
// mutation on /undo via the same containment-checked Workspace primitives the tools
// use. Records are RAM-only (lost at exit), which is acceptable for an interactive
// REPL. peek -> verify -> restore -> pop: a refused or failed undo leaves the
// record on the stack so nothing is lost.
type mutationJournal struct {
	ws   *agenttools.Workspace
	mu   sync.Mutex
	recs []agenttools.MutationRecord
}

func newMutationJournal(ws *agenttools.Workspace) *mutationJournal {
	return &mutationJournal{ws: ws}
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

	cur, err := j.ws.ReadFileForUndo(rec.Path)
	curExists := err == nil
	curHash := ""
	if err != nil && !os.IsNotExist(err) {
		_, _ = fmt.Fprintf(out, "cannot undo %s: file changed since golem wrote it\n", rec.Path)
		return // leave the record on the stack
	}
	if curExists {
		curHash = agenttools.ContentHash(cur)
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

	if rec.Existed {
		if werr := j.ws.WriteFileAtomic(rec.Path, rec.PriorContent); werr != nil {
			_, _ = fmt.Fprintf(out, "undo failed for %s: %v\n", rec.Path, werr)
			return
		}
	} else {
		if rerr := j.ws.RemoveFile(rec.Path); rerr != nil {
			_, _ = fmt.Fprintf(out, "undo failed for %s: %v\n", rec.Path, rerr)
			return
		}
	}
	j.recs = j.recs[:len(j.recs)-1] // pop only on success
	_, _ = fmt.Fprintf(out, "undid %s\n", rec.Path)
}
