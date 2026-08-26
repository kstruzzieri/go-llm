package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// errInterruptedUndoPending blocks new model turns while an interrupted undo
// exists (D5): the workspace is mid-rollback and new writes on top of a
// half-restored tree would make the remaining restore unsafe.
var errInterruptedUndoPending = errors.New("golem: an interrupted undo exists; run /undo to resume it before new work")

// checkpointJournal is the interactive REPL's durable undo journal (#355). It
// implements agenttools.PreparingJournal: every mutation persists a
// write-ahead intent BEFORE the workspace rename and marks it applied after,
// so no applied change can exist that the journal never saw. Intents group
// into one checkpoint per runOnce turn. Any storage or hardening failure
// latches the journal and cancels the captured run context (D7); an expected
// quota refusal cancels only the current turn.
type checkpointJournal struct {
	ws    *agenttools.Workspace
	store *checkpointStore

	// prepSem serializes each mutation from Prepare through its Commit/Abort
	// so DB intent order always equals filesystem write order.
	prepSem chan struct{}

	mu       sync.Mutex
	turnOpen bool
	goal     string
	turnAt   time.Time
	cancel   context.CancelFunc
	cpID     int64 // 0 = no intent recorded this turn yet
	fatal    error // first storage/hardening failure; latches until process exit
	quota    error // quota refusal for this turn; cleared by sealTurn
}

func newCheckpointJournal(ws *agenttools.Workspace, store *checkpointStore) *checkpointJournal {
	return &checkpointJournal{ws: ws, store: store, prepSem: make(chan struct{}, 1)}
}

// fileState is a live or simulated file state: a content hash, or absent.
type fileState struct {
	absent bool
	hash   string
}

func (a fileState) equal(b fileState) bool {
	return a.absent == b.absent && a.hash == b.hash
}

// undoTarget is the state a file reaches after undoing f.
func undoTarget(f checkpointFile) fileState {
	if !f.existed {
		return fileState{absent: true}
	}
	return fileState{hash: f.priorHash}
}

// liveState reads a workspace file through the same containment-checked
// primitive the RAM journal uses. Absence is a state, not an error; any other
// read failure (symlink, directory, permission) is surfaced so callers refuse
// rather than guess.
func (j *checkpointJournal) liveState(path string) (fileState, error) {
	cur, err := j.ws.ReadFileForUndo(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileState{absent: true}, nil
		}
		return fileState{}, err
	}
	return fileState{hash: agenttools.ContentHash(cur)}, nil
}

// latch records the first fatal failure and cancels the captured run context.
func (j *checkpointJournal) latch(err error) {
	j.mu.Lock()
	if j.fatal == nil {
		j.fatal = err
	}
	cancel := j.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// beginTurn arms the journal for one runOnce turn, capturing that run's
// cancel function. It refuses the turn while the journal is latched or an
// interrupted undo exists; slash commands remain usable either way.
func (j *checkpointJournal) beginTurn(ctx context.Context, goal string, cancel context.CancelFunc) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.fatal != nil {
		return fmt.Errorf("golem: checkpoint journal disabled by an earlier failure: %w", j.fatal)
	}
	n, err := j.store.countState(ctx, checkpointUndoing)
	if err != nil {
		j.fatal = err
		return err
	}
	if n > 0 {
		return errInterruptedUndoPending
	}
	j.turnOpen, j.goal, j.turnAt, j.cancel = true, goal, time.Now(), cancel
	j.cpID, j.quota = 0, nil
	return nil
}

// Prepare persists one write-ahead intent for the current turn, lazily
// creating the turn's open checkpoint. It blocks until any earlier prepared
// mutation resolves, keeping DB order equal to filesystem order.
func (j *checkpointJournal) Prepare(rec agenttools.MutationRecord) (agenttools.PreparedMutation, error) {
	j.prepSem <- struct{}{}
	release := func() { <-j.prepSem }

	j.mu.Lock()
	if j.fatal != nil {
		err := fmt.Errorf("golem: checkpoint journal disabled by an earlier failure: %w", j.fatal)
		j.mu.Unlock()
		release()
		return nil, err
	}
	if !j.turnOpen {
		j.mu.Unlock()
		release()
		return nil, errors.New("golem: mutation outside an active turn")
	}
	goal, at, cancel := j.goal, j.turnAt, j.cancel
	j.mu.Unlock()

	cpID, fileID, err := j.store.prepareIntent(context.Background(), goal, at, rec)
	if err != nil {
		if errors.Is(err, errCheckpointQuota) {
			// Expected policy refusal (D7/D8): cancel this turn, keep the
			// journal healthy for the next one.
			j.mu.Lock()
			j.quota = err
			j.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		} else {
			j.latch(err)
		}
		release()
		return nil, err
	}
	j.mu.Lock()
	j.cpID = cpID
	j.mu.Unlock()
	return &checkpointPrepared{j: j, fileID: fileID, release: release}, nil
}

// Record satisfies agenttools.Journal for compatibility; production tools use
// the preparing path automatically. A direct call still routes through
// prepare+commit so no mutation can bypass the write-ahead store.
func (j *checkpointJournal) Record(rec agenttools.MutationRecord) {
	p, err := j.Prepare(rec)
	if err != nil {
		return
	}
	_ = p.Commit()
}

// checkpointPrepared is the open intent handle for one mutation. Exactly one
// of Commit or Abort runs; either releases the serialization slot.
type checkpointPrepared struct {
	j       *checkpointJournal
	fileID  int64
	release func()
	once    sync.Once
}

func (p *checkpointPrepared) Commit() error { return p.resolve(true) }
func (p *checkpointPrepared) Abort() error  { return p.resolve(false) }

func (p *checkpointPrepared) resolve(commit bool) error {
	err := errors.New("golem: prepared mutation already resolved")
	p.once.Do(func() {
		defer p.release()
		if commit {
			err = p.j.store.commitIntent(context.Background(), p.fileID)
		} else {
			err = p.j.store.abortIntent(context.Background(), p.fileID)
		}
		if err != nil {
			p.j.latch(err)
		}
	})
	return err
}

// sealTurn completes the turn's checkpoint on every runOnce exit path
// (success, cancellation, provider error): writes applied before an interrupt
// must stay undoable. The returned error joins this turn's quota refusal, any
// latched failure, and a seal failure; a seal failure latches and keeps the
// checkpoint ID so nothing pretends the seal happened.
func (j *checkpointJournal) sealTurn(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.turnOpen = false
	j.cancel = nil
	var errs []error
	if j.quota != nil {
		errs = append(errs, j.quota)
		j.quota = nil
	}
	if j.fatal != nil {
		errs = append(errs, j.fatal)
	}
	if j.cpID != 0 {
		if err := j.store.seal(ctx, j.cpID); err != nil {
			if j.fatal == nil {
				j.fatal = err
			}
			errs = append(errs, err)
			return errors.Join(errs...)
		}
		j.cpID = 0
	}
	return errors.Join(errs...)
}

// recoverStartup reconciles a stale open checkpoint left by a crashed process
// (spec section 1.5). With the workspace lease held this is safe: for each
// write-ahead intent that never committed, the live file decides — back at
// (or never left) the recorded prior/absent target means the write never
// landed and the intent is dropped; any other observable state keeps the
// record by marking it applied, so a later /undo refuses on hash mismatch
// rather than overwriting unseen work. A non-absence read error fails startup
// and leaves the row open: recovery must not guess. The recovered checkpoint
// seals to completed (or is deleted when empty) and one notice is returned.
func (j *checkpointJournal) recoverStartup(ctx context.Context) (string, error) {
	groups, err := j.store.loadGroups(ctx, checkpointOpen, 0, false)
	if err != nil {
		return "", err
	}
	if len(groups) == 0 {
		return "", nil
	}
	notice := ""
	for _, g := range groups { // the partial unique index allows at most one
		kept := 0
		for _, f := range g.files {
			if f.applied {
				kept++
				continue
			}
			live, lerr := j.liveState(f.path)
			if lerr != nil {
				return "", fmt.Errorf("golem: checkpoint recovery cannot classify %s: %w", f.path, lerr)
			}
			if live.equal(undoTarget(f)) {
				if aerr := j.store.abortIntent(ctx, f.id); aerr != nil {
					return "", aerr
				}
				continue
			}
			if cerr := j.store.commitIntent(ctx, f.id); cerr != nil {
				return "", cerr
			}
			kept++
		}
		if serr := j.store.seal(ctx, g.id); serr != nil {
			return "", serr
		}
		if kept > 0 {
			notice = fmt.Sprintf("recovered an interrupted turn as a checkpoint (%d file(s)); /undo can revert it", kept)
		}
	}
	return notice, nil
}
