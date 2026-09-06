package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/signing"
)

// errInterruptedUndoPending blocks new model turns while an interrupted undo
// exists (D5): the workspace is mid-rollback and new writes on top of a
// half-restored tree would make the remaining restore unsafe.
var errInterruptedUndoPending = errors.New("golem: an interrupted undo exists; run /undo to resume it before new work")

// checkpointJournal is the interactive REPL's durable undo journal (#355). It
// implements agenttools.PreparingJournal: every mutation persists a
// write-ahead intent BEFORE the workspace rename and marks it applied after,
// so every attempted change has signed write-ahead evidence. Intents group
// into one checkpoint per runOnce turn. Any signing, observation, or storage failure
// latches the journal and cancels the captured run context (D7); an expected
// quota refusal cancels only the current turn.
type checkpointJournal struct {
	ws       *agenttools.Workspace
	store    *checkpointStore
	signer   signing.Signer
	verifier signing.Verifier

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

	// crashAfterRestore, when non-nil, runs after a file's content is
	// restored but before its progress flag persists. Test seam for the
	// kill-and-rerun crash test; never set in production.
	crashAfterRestore func(path string)
}

func newCheckpointJournal(ws *agenttools.Workspace, store *checkpointStore, signer signing.Signer, verifier signing.Verifier) *checkpointJournal {
	return &checkpointJournal{ws: ws, store: store, signer: signer, verifier: verifier, prepSem: make(chan struct{}, 1)}
}

// fileState is a live or simulated file state: a content hash, or absent.
// mode/modeKnown carry the complete file mode where available (live reads,
// and simulated states derived from them); equal() deliberately compares
// content identity only — mode participates solely through matchesAfter's
// tracked-record guard (#443).
type fileState struct {
	absent    bool
	hash      string
	mode      fs.FileMode
	modeKnown bool
}

func (a fileState) equal(b fileState) bool {
	return a.absent == b.absent && a.hash == b.hash
}

// undoTargetFrom is the state a file reaches after undoing f, derived from
// the state cur it currently holds. A create undoes to absent. An update
// undoes via WriteFileAtomic, which preserves the existing permission bits —
// so the simulated state carries cur's mode forward, keeping a tracked
// older record verifiable across intervening legacy updates (#443).
func undoTargetFrom(cur fileState, f checkpointFile) fileState {
	if !f.existed {
		return fileState{absent: true}
	}
	return fileState{hash: f.priorHash, mode: cur.mode, modeKnown: cur.modeKnown}
}

// liveState reads a workspace file through the same containment-checked
// primitive the RAM journal uses, taking bytes and the complete mode from
// one open handle. Absence is a state, not an error; any other read failure
// (symlink, directory, permission) is surfaced so callers refuse rather
// than guess.
func (j *checkpointJournal) liveState(path string) (fileState, error) {
	cur, mode, err := j.ws.ReadFileWithModeForUndo(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileState{absent: true}, nil
		}
		return fileState{}, err
	}
	return fileState{hash: agenttools.ContentHash(cur), mode: mode, modeKnown: true}, nil
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
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			j.fatal = err
		}
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
	path, err := j.ws.CanonicalPathForUndo(rec.Path)
	if err != nil {
		release()
		return nil, fmt.Errorf("golem: canonicalize checkpoint path: %w", err)
	}
	rec.Path = filepath.ToSlash(path)
	intent, err := j.signIntent(rec)
	if err != nil {
		j.latch(err)
		release()
		return nil, err
	}
	raw, err := signing.MarshalCanonical(intent)
	if err != nil {
		j.latch(err)
		release()
		return nil, err
	}
	cpID, fileID, err := j.store.prepareSignedIntent(context.Background(), goal, at, rec, raw)
	if err != nil {
		if checkpointQuotaOnly(err) {
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
	return &checkpointPrepared{j: j, fileID: fileID, intent: intent, release: release}, nil
}

func checkpointQuotaOnly(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs := joined.Unwrap()
		if len(errs) == 0 {
			return false
		}
		for _, child := range errs {
			if !checkpointQuotaOnly(child) {
				return false
			}
		}
		return true
	}
	if child := errors.Unwrap(err); child != nil {
		return checkpointQuotaOnly(child)
	}
	return err == errCheckpointQuota
}

// Record cannot witness a write: only the prepared protocol observes landing.
// Keep this method solely to satisfy Journal and fail closed for direct callers.
func (j *checkpointJournal) Record(agenttools.MutationRecord) {
	j.latch(errors.New("golem: Record cannot attest a mutation; use Prepare before writing"))
}

func (j *checkpointJournal) signIntent(rec agenttools.MutationRecord) (agenttools.MutationReceipt, error) {
	if j.signer == nil || j.verifier == nil {
		return agenttools.MutationReceipt{}, signing.ErrUninitializedKey
	}
	before := "absent"
	if rec.Existed {
		before = agenttools.ContentHash(rec.PriorContent)
	}
	body := agenttools.MutationReceiptBody{
		Kind: "intent", MutationID: rand.Text(),
		WorkspaceHash: j.store.workspaceHash, Path: rec.Path,
		BeforeHash: before, AfterHash: rec.AfterHash,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), AgentID: j.signer.KeyID(),
	}
	if rec.TrackedMode {
		if rec.AfterMode > 0o777 {
			return agenttools.MutationReceipt{}, errors.New("golem: invalid tracked mutation mode")
		}
		mode := uint32(rec.AfterMode)
		body.AfterMode = &mode
	}
	intent, err := agenttools.SignMutationReceipt(context.Background(), j.signer, body)
	if err != nil {
		return agenttools.MutationReceipt{}, err
	}
	if err := agenttools.VerifyMutationReceipt(context.Background(), j.verifier, intent); err != nil {
		return agenttools.MutationReceipt{}, err
	}
	return intent, nil
}

// checkpointPrepared is the open intent handle for one mutation. Exactly one
// of Commit or Abort runs; either releases the serialization slot.
type checkpointPrepared struct {
	j       *checkpointJournal
	fileID  int64
	intent  agenttools.MutationReceipt
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
			err = p.commit()
		} else {
			err = p.j.store.abortIntent(context.Background(), p.fileID)
		}
		if err != nil {
			p.j.latch(err)
		}
	})
	return err
}

// commit signs only the observed expected after-state. Unlike undo's
// matchesAfter, a vanished create is a mismatch here, never an applied write.
func (p *checkpointPrepared) commit() error {
	body := p.intent.Body
	live, err := p.j.liveState(body.Path)
	if err != nil {
		return fmt.Errorf("golem: mutation after-state: %w", checkpointDisplayError{cause: err})
	}
	if live.absent || live.hash != body.AfterHash || body.AfterMode != nil && (!live.modeKnown || live.mode != fs.FileMode(*body.AfterMode)) {
		return fmt.Errorf("golem: mutation after-state mismatch for %s", checkpointDisplayText(body.Path))
	}
	body.Kind, body.Timestamp = "applied", time.Now().UTC().Format(time.RFC3339Nano)
	applied, err := agenttools.SignMutationReceipt(context.Background(), p.j.signer, body)
	if err != nil {
		return err
	}
	if err := agenttools.VerifyMutationReceipt(context.Background(), p.j.verifier, applied); err != nil {
		return err
	}
	raw, err := signing.MarshalCanonical(applied)
	if err != nil {
		return err
	}
	return p.j.store.commitSignedIntent(context.Background(), p.fileID, raw)
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

// recoverStartup classifies interrupted forward writes conservatively for
// undo bookkeeping. It authenticates evidence but never signs historical success:
// even live == prior could hide a same-byte write, so unresolved intents survive.
func (j *checkpointJournal) recoverStartup(ctx context.Context) (string, error) {
	groups, err := j.store.loadGroups(ctx, checkpointOpen, 0, false)
	if err != nil {
		return "", err
	}
	notices := []string{}
	for _, g := range groups {
		// Authenticate the whole group before changing any recovery progress.
		unconfirmed := 0
		for _, f := range g.files {
			if !f.forwardMutationID.Valid || f.forwardMutationID.String == "" {
				return "", errors.New("golem: checkpoint has no verifiable forward intent")
			}
			entry, err := j.store.loadReceipt(ctx, f.forwardMutationID.String)
			if err != nil {
				return "", mutationHistoryError{cause: err}
			}
			intent, err := authenticateCheckpointReceipt(ctx, j.verifier, entry)
			if err != nil {
				return "", mutationHistoryError{cause: err}
			}
			if err := j.store.bindForwardReceipt(f, intent); err != nil {
				return "", err
			}
			if entry.appliedJSON == nil {
				unconfirmed++
			}
		}
		kept := 0
		for _, f := range g.files {
			if f.applied {
				kept++
				continue
			}
			live, err := j.liveState(f.path)
			if err != nil {
				return "", fmt.Errorf("golem: checkpoint recovery cannot classify %s: %w", checkpointDisplayText(f.path), checkpointDisplayError{cause: err})
			}
			if live.equal(undoTargetFrom(live, f)) {
				if err := j.store.recoverDropIntent(ctx, f.id); err != nil {
					return "", err
				}
				continue
			}
			if err := j.store.recoverCommitIntent(ctx, f.id); err != nil {
				return "", err
			}
			kept++
		}
		if err := j.store.seal(ctx, g.id); err != nil {
			return "", err
		}
		if kept > 0 {
			notices = append(notices, fmt.Sprintf("recovered an interrupted turn as a checkpoint (%d file(s)); /undo can revert it", kept))
		}
		if unconfirmed > 0 {
			notices = append(notices, fmt.Sprintf("recovered %d unconfirmed mutation attempt(s); no applied receipt", unconfirmed))
		}
	}
	return strings.Join(notices, "\n"), nil
}

// checkpointUndoRefusal is the exact refusal text shared with the RAM
// journal: printed for every divergence classification, including non-absence
// read errors (symlink/directory replacement), so refusal output never leaks
// why a path could not be classified.
const checkpointUndoRefusal = "cannot undo %s: file changed since golem wrote it\n"

// checkpointDisplayText preserves ordinary text while escaping controls and
// other non-graphic runes so a path or nested path error cannot forge output.
func checkpointDisplayText(text string) string {
	quoted := strconv.QuoteToGraphic(text)
	return quoted[1 : len(quoted)-1]
}

// checkpointDisplayError keeps errors.Is/As working while preventing a nested
// filesystem error from reintroducing a control-bearing path into its text.
type checkpointDisplayError struct{ cause error }

func (e checkpointDisplayError) Error() string { return checkpointDisplayText(e.cause.Error()) }
func (e checkpointDisplayError) Unwrap() error { return e.cause }

// matchesAfter reports whether cur is a valid starting state for undoing f:
// exactly the recorded after state, or — parity with the RAM journal — an
// already-absent file when the record was a create. A tracked record (#443
// promotion) additionally requires the complete mode to match: identical
// bytes with drifted permission, special, or type bits refuse, and an
// unknown mode fails closed rather than guessing.
func matchesAfter(cur fileState, f checkpointFile) bool {
	if cur.absent {
		return !f.existed
	}
	if cur.hash != f.afterHash {
		return false
	}
	if f.trackedMode {
		return cur.modeKnown && cur.mode == f.afterMode
	}
	return true
}

// undo reverts the n newest completed checkpoints (#355): resume-first when an
// undo was interrupted, all-file chain preflight before any change, reverse
// mutation order, persisted per-file progress, and deletion only when a guard
// proves every applied row restored.
func (j *checkpointJournal) undo(ctx context.Context, out io.Writer, n int) {
	interrupted, err := j.store.undoingGroups(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(out, "undo failed: %v\n", err)
		return
	}
	if len(interrupted) > 0 {
		if j.restoreGroups(ctx, out, interrupted) {
			_, _ = fmt.Fprintln(out, "resumed interrupted undo; run /undo again to undo more")
		}
		return
	}

	groups, err := j.store.newestCompleted(ctx, n)
	if err != nil {
		_, _ = fmt.Fprintf(out, "undo failed: %v\n", err)
		return
	}
	if len(groups) == 0 {
		_, _ = fmt.Fprintln(out, "nothing to undo")
		return
	}
	if len(groups) < n {
		_, _ = fmt.Fprintf(out, "cannot undo %d: only %d checkpoint(s) available\n", n, len(groups))
		return
	}

	// Preflight every file across every checkpoint being undone, simulating
	// the chain so a path written in several turns validates end to end. Any
	// mismatch or unclassifiable read: change nothing, retain every record.

	expected := map[string]fileState{}
	for _, g := range groups {
		for _, f := range g.files {
			cur, seen := expected[f.path]
			if !seen {
				live, lerr := j.liveState(f.path)
				if lerr != nil {
					_, _ = fmt.Fprintf(out, checkpointUndoRefusal, checkpointDisplayText(f.path))
					return
				}
				cur = live
			}
			if !matchesAfter(cur, f) {
				_, _ = fmt.Fprintf(out, checkpointUndoRefusal, checkpointDisplayText(f.path))
				return
			}
			expected[f.path] = undoTargetFrom(cur, f)
		}
	}

	// Commit intent, then restore. From here a crash or failure resumes via
	// the next /undo.
	ids := make([]int64, len(groups))
	for i, g := range groups {
		ids[i] = g.id
	}
	if err := j.store.markUndoing(ctx, ids); err != nil {
		_, _ = fmt.Fprintf(out, "undo failed: %v\n", err)
		return
	}
	j.restoreGroups(ctx, out, groups)
}

// restoreGroups applies checkpoint groups newest first, files in reverse
// mutation order, skipping rows whose progress flag is already set. Restore
// and resume are one loop: every file is classified by its live state, so
// replaying after a crash is idempotent. Returns true when every group
// completed and was deleted.
func (j *checkpointJournal) restoreGroups(ctx context.Context, out io.Writer, groups []checkpointGroup) bool {
	for _, g := range groups {
		for _, f := range g.files {
			if f.restored {
				continue
			}
			if !j.restoreFile(ctx, out, f) {
				_, _ = fmt.Fprintln(out, "undo interrupted; run /undo to resume")
				return false
			}
		}
		if err := j.store.deleteRestored(ctx, g.id); err != nil {
			j.latch(err)
			_, _ = fmt.Fprintf(out, "undo failed: %v\n", err)
			return false
		}
		_, _ = fmt.Fprintf(out, "undid checkpoint: %s\n", g.goal)
	}
	return true
}

// restoreFile restores one recorded mutation. Live-state classification makes
// it idempotent: target already reached (a crash after the restore but before
// its flag, or an already-absent created file) marks progress without
// writing; the recorded after state applies the restore; anything else is a
// divergence — refuse with the exact shared message and stop, leaving the
// checkpoint in undoing for a later resume.
func (j *checkpointJournal) restoreFile(ctx context.Context, out io.Writer, f checkpointFile) bool {
	cur, err := j.liveState(f.path)
	if err != nil {
		_, _ = fmt.Fprintf(out, checkpointUndoRefusal, checkpointDisplayText(f.path))
		return false
	}
	if !cur.equal(undoTargetFrom(cur, f)) {
		if !matchesAfter(cur, f) {
			_, _ = fmt.Fprintf(out, checkpointUndoRefusal, checkpointDisplayText(f.path))
			return false
		}
		if f.existed {
			if werr := j.ws.WriteFileAtomic(f.path, f.priorContent); werr != nil {
				_, _ = fmt.Fprintf(out, "undo failed for %s: %s\n",
					checkpointDisplayText(f.path), checkpointDisplayText(werr.Error()))
				return false
			}
		} else if rerr := j.ws.RemoveFile(f.path); rerr != nil {
			_, _ = fmt.Fprintf(out, "undo failed for %s: %s\n",
				checkpointDisplayText(f.path), checkpointDisplayText(rerr.Error()))
			return false
		}
	}
	if j.crashAfterRestore != nil {
		j.crashAfterRestore(f.path)
	}
	if err := j.store.markRestored(ctx, f.id); err != nil {
		j.latch(err)
		_, _ = fmt.Fprintf(out, "undo failed for %s: %s\n",
			checkpointDisplayText(f.path), checkpointDisplayText(err.Error()))
		return false
	}
	_, _ = fmt.Fprintf(out, "undid %s\n", checkpointDisplayText(f.path))
	return true
}

// formatCheckpointBytes renders a prior-content byte count for /checkpoints.
func formatCheckpointBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// listCheckpoints prints this workspace's checkpoints newest first: index 1
// is what /undo 1 reverts when no interrupted undo is pending. Goal labels
// were stored control-safe, so one row can never render as several.
func (j *checkpointJournal) listCheckpoints(ctx context.Context, out io.Writer) {
	infos, err := j.store.list(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(out, "checkpoints failed: %v\n", err)
		return
	}
	if len(infos) == 0 {
		_, _ = fmt.Fprintln(out, "no checkpoints")
		return
	}
	for i, in := range infos {
		marker := ""
		switch in.state {
		case checkpointUndoing:
			marker = "[interrupted undo] "
		case checkpointOpen:
			marker = "[in progress] "
		case checkpointCompleted:
		}
		fileWord := "files"
		if in.files == 1 {
			fileWord = "file"
		}
		_, _ = fmt.Fprintf(out, "%3d  %s  %d %s  %s  %s%s\n",
			i+1, in.createdAt.Local().Format("2006-01-02 15:04:05"),
			in.files, fileWord, formatCheckpointBytes(in.bytes), marker, in.goal)
	}
}
