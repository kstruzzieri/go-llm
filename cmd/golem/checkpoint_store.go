package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/memory"
)

// checkpointState is the lifecycle of one persisted checkpoint (#355).
// open: the turn is in progress, or a process died mid-turn (startup
// reconciliation resolves those). completed: sealed at turn end; the only
// state retention may prune. undoing: an undo committed intent; never pruned,
// deleted only when every applied file row is restored.
type checkpointState string

const (
	checkpointOpen      checkpointState = "open"
	checkpointCompleted checkpointState = "completed"
	checkpointUndoing   checkpointState = "undoing"
)

// Retention defaults (issue #355): strict per-workspace caps.
const (
	defaultMaxCheckpoints = 50
	defaultMaxPriorBytes  = 64 << 20 // 64 MiB of prior content
)

// checkpointSchemaVersion is the newest schema this binary understands. The
// schema is internal: #347 consumes the semantic mutation/turn contract, never
// these tables.
const checkpointSchemaVersion = 1

// errCheckpointLeaseHeld reports that another live golem process owns this
// workspace's checkpoint store. Checkpoints fail closed on contention (D10):
// there is no last-writer-wins mode for undo history.
var errCheckpointLeaseHeld = errors.New("golem: another golem process holds this workspace's checkpoint store")

// checkpointStore persists per-turn mutation checkpoints in one hardened
// per-workspace SQLite DB under the per-user data dir, owned exclusively via
// an OS file lease for the store's lifetime. It knows nothing about the
// Workspace; restoring files is checkpointJournal's job. Deleted rows never
// shrink the DB file (no vacuum): the disk high-water mark is bounded by the
// retention caps plus SQLite overhead.
type checkpointStore struct {
	db     *sql.DB
	dbPath string
	lease  *flockLease

	maxCheckpoints int
	maxPriorBytes  int64
}

// checkpointDBPath locates the per-workspace checkpoint DB OUTSIDE the repo:
// <dataDirBase>/golem/checkpoints/<workspace-hash>.db, where the hash is the
// stable identity workspaceID derives from the canonical root.
func checkpointDBPath(getenv func(string) string, root string) (string, error) {
	base, err := dataDirBase(getenv)
	if err != nil {
		return "", err
	}
	hash := strings.TrimPrefix(workspaceID(root), "workspace:")
	return filepath.Join(base, "golem", "checkpoints", hash+".db"), nil
}

// openCheckpointStore acquires this workspace's exclusive checkpoint lease,
// opens (creating if missing) the hardened DB, migrates to the v1 schema, and
// re-secures every on-disk file. Any failure releases whatever was acquired
// and returns the error: -allow-write startup fails closed on it (D6).
func openCheckpointStore(ctx context.Context, getenv func(string) string, root string) (*checkpointStore, error) {
	path, err := checkpointDBPath(getenv, root)
	if err != nil {
		return nil, err
	}
	if err := validatePathOutsideWorkspace(path, root); err != nil {
		return nil, err
	}
	// Create and re-chmod the leaf directory only; existing parents may be
	// shared/user-configured (mirrors memory.PrepareDBFile's stance).
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("golem: create checkpoint dir %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("golem: chmod checkpoint dir %q: %w", dir, err)
	}
	lease, err := acquireFlockLease(path + ".lock")
	if err != nil {
		if errors.Is(err, errIndexWriterLeaseHeld) {
			return nil, fmt.Errorf("%w (lock %s)", errCheckpointLeaseHeld, path+".lock")
		}
		return nil, err
	}
	db, err := memory.OpenHardenedDB(ctx, path)
	if err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	s := &checkpointStore{
		db:             db,
		dbPath:         path,
		lease:          lease,
		maxCheckpoints: defaultMaxCheckpoints,
		maxPriorBytes:  defaultMaxPriorBytes,
	}
	fail := func(err error) (*checkpointStore, error) {
		return nil, errors.Join(err, db.Close(), memory.SecureDBFiles(path), lease.Close())
	}
	// The single connection relies on ON DELETE CASCADE for checkpoint_files.
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return fail(fmt.Errorf("golem: checkpoint foreign_keys: %w", err))
	}
	if err := s.migrate(ctx); err != nil {
		return fail(err)
	}
	if err := memory.SecureDBFiles(path); err != nil {
		return nil, errors.Join(err, db.Close(), lease.Close())
	}
	return s, nil
}

// migrate applies the v1 schema and version stamp in one transaction, so a
// crash mid-migration leaves version 0 and a clean retry. A database from a
// newer binary is rejected rather than guessed at.
func (s *checkpointStore) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("golem: checkpoint schema version: %w", err)
	}
	if version > checkpointSchemaVersion {
		return fmt.Errorf("golem: checkpoint db %s is schema v%d; this binary supports up to v%d",
			s.dbPath, version, checkpointSchemaVersion)
	}
	if version == checkpointSchemaVersion {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("golem: checkpoint migrate begin: %w", err)
	}
	stmts := []string{
		`CREATE TABLE checkpoints (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT    NOT NULL,
			goal       TEXT    NOT NULL,
			state      TEXT    NOT NULL
				CHECK (state IN ('open', 'completed', 'undoing'))
		)`,
		`CREATE UNIQUE INDEX checkpoints_one_open
			ON checkpoints(state) WHERE state = 'open'`,
		`CREATE INDEX checkpoints_state_id ON checkpoints(state, id DESC)`,
		`CREATE TABLE checkpoint_files (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			checkpoint_id INTEGER NOT NULL
				REFERENCES checkpoints(id) ON DELETE CASCADE,
			path          TEXT    NOT NULL,
			prior_content BLOB,
			prior_hash    TEXT    NOT NULL,
			existed       INTEGER NOT NULL CHECK (existed IN (0, 1)),
			after_hash    TEXT    NOT NULL,
			summary       TEXT    NOT NULL,
			at            TEXT    NOT NULL,
			applied       INTEGER NOT NULL DEFAULT 0 CHECK (applied IN (0, 1)),
			restored      INTEGER NOT NULL DEFAULT 0 CHECK (restored IN (0, 1)),
			CHECK (restored = 0 OR applied = 1)
		)`,
		`CREATE INDEX checkpoint_files_checkpoint
			ON checkpoint_files(checkpoint_id, id DESC)`,
		fmt.Sprintf("PRAGMA user_version = %d", checkpointSchemaVersion),
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return errors.Join(fmt.Errorf("golem: checkpoint migrate: %w", err), tx.Rollback())
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("golem: checkpoint migrate commit: %w", err)
	}
	return nil
}

// secure re-chmods the DB file and sidecars (the #237 per-write invariant).
func (s *checkpointStore) secure() error { return memory.SecureDBFiles(s.dbPath) }

// Close releases the DB handle and the workspace lease, joining both errors.
func (s *checkpointStore) Close() error {
	return errors.Join(s.db.Close(), s.lease.Close())
}

// errCheckpointQuota reports a Prepare refused by strict size admission (D8):
// the new intent cannot fit inside maxPriorBytes even after pruning every
// completed checkpoint, because open/undoing records are never pruned. It is
// an expected policy refusal, not store damage: the journal cancels the turn
// but does not latch.
var errCheckpointQuota = errors.New("golem: checkpoint prior-content quota exceeded")

// checkpointInfo is one /checkpoints listing row.
type checkpointInfo struct {
	id        int64
	createdAt time.Time
	goal      string
	state     checkpointState
	files     int
	bytes     int64
}

// checkpointFile is one recorded mutation loaded from the store.
type checkpointFile struct {
	id           int64
	path         string
	priorContent []byte
	priorHash    string
	existed      bool
	afterHash    string
	applied      bool
	restored     bool
}

// checkpointGroup is one checkpoint with its file rows in reverse mutation
// order (newest write first) — the order undo applies them.
type checkpointGroup struct {
	id    int64
	goal  string
	state checkpointState
	files []checkpointFile
}

// checkpointGoalMaxRunes bounds the stored turn-goal label (D11).
const checkpointGoalMaxRunes = 160

// sanitizeCheckpointGoal flattens the turn goal to one control-safe line and
// truncates rune-safely (D11): a pasted newline, ANSI escape, or bidi format
// character cannot forge or reorder /checkpoints rows. FlattenRecordContent
// strips control runes; QuoteToGraphic then escapes the remaining non-graphic
// ones (bidi controls are format characters, not control characters).
func sanitizeCheckpointGoal(goal string) string {
	q := strconv.QuoteToGraphic(agenttools.FlattenRecordContent(goal))
	q = q[1 : len(q)-1] // QuoteToGraphic guarantees the outer ASCII quotes
	runes := []rune(q)
	if len(runes) > checkpointGoalMaxRunes {
		runes = runes[:checkpointGoalMaxRunes]
	}
	return string(runes)
}

// canonicalCheckpointPath is the stored spelling of a workspace-relative path.
// Required for chain simulation when the same file is written as a.go, ./a.go,
// or x/../a.go in different turns.
func canonicalCheckpointPath(p string) string {
	return filepath.ToSlash(filepath.Clean(p))
}

func formatCheckpointTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// execTx runs fn inside one transaction and re-secures the DB files afterward
// on every path (WAL/SHM can be recreated honoring the umask by any write). A
// hardening failure is an operation failure, never ignored.
func (s *checkpointStore) execTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("golem: checkpoint tx begin: %w", err)
	}
	if err := fn(tx); err != nil {
		rerr := tx.Rollback()
		if rerr == sql.ErrTxDone {
			rerr = nil
		}
		return errors.Join(err, rerr, s.secure())
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(fmt.Errorf("golem: checkpoint tx commit: %w", err), s.secure())
	}
	return s.secure()
}

// requireOneRow converts a conditional UPDATE/DELETE result into a hard state
// machine: a missing row or invalid source state is an error, never silence.
func requireOneRow(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("golem: checkpoint %s rows: %w", op, err)
	}
	if n != 1 {
		return fmt.Errorf("golem: checkpoint %s: no row in the required state", op)
	}
	return nil
}

// deleteCheckpointTx removes one checkpoint and its file rows, requiring the
// checkpoint row to still be in wantState at deletion time. File rows are
// deleted explicitly rather than trusting FK cascade: the foreign_keys pragma
// is per-connection, and a recycled pool connection would silently drop it.
func deleteCheckpointTx(ctx context.Context, tx *sql.Tx, cpID int64, wantState checkpointState) error {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM checkpoints WHERE id = ? AND state = ?`, cpID, wantState)
	if err != nil {
		return fmt.Errorf("golem: checkpoint delete %d: %w", cpID, err)
	}
	if err := requireOneRow(res, fmt.Sprintf("delete %d (%s)", cpID, wantState)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM checkpoint_files WHERE checkpoint_id = ?`, cpID); err != nil {
		return fmt.Errorf("golem: checkpoint delete %d files: %w", cpID, err)
	}
	return nil
}

// prepareIntent persists one write-ahead mutation intent (applied = 0) for the
// current turn, lazily creating the turn's open checkpoint, all in one
// transaction with strict size admission (D8): oldest completed checkpoints
// are pruned until the intent fits, and if protected open/undoing rows leave
// insufficient capacity the intent is refused with errCheckpointQuota before
// any workspace write happens. A refused admission rolls back, so pruning
// only commits together with the accepted intent.
func (s *checkpointStore) prepareIntent(ctx context.Context, goal string, at time.Time, rec agenttools.MutationRecord) (cpID, fileID int64, err error) {
	path := canonicalCheckpointPath(rec.Path)
	priorHash := ""
	existed := 0
	if rec.Existed {
		priorHash = agenttools.ContentHash(rec.PriorContent)
		existed = 1
	}
	err = s.execTx(ctx, func(tx *sql.Tx) error {
		newBytes := int64(len(rec.PriorContent))
		for {
			var total int64
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(SUM(LENGTH(prior_content)), 0) FROM checkpoint_files`).Scan(&total); err != nil {
				return fmt.Errorf("golem: checkpoint admission size: %w", err)
			}
			if total+newBytes <= s.maxPriorBytes {
				break
			}
			var victim int64
			verr := tx.QueryRowContext(ctx,
				`SELECT id FROM checkpoints WHERE state = ? ORDER BY id LIMIT 1`,
				checkpointCompleted).Scan(&victim)
			if verr == sql.ErrNoRows {
				return fmt.Errorf("%w: %d prior bytes held by active records, intent needs %d of %d",
					errCheckpointQuota, total, newBytes, s.maxPriorBytes)
			}
			if verr != nil {
				return fmt.Errorf("golem: checkpoint admission victim: %w", verr)
			}
			if err := deleteCheckpointTx(ctx, tx, victim, checkpointCompleted); err != nil {
				return err
			}
		}
		verr := tx.QueryRowContext(ctx,
			`SELECT id FROM checkpoints WHERE state = ?`, checkpointOpen).Scan(&cpID)
		if verr == sql.ErrNoRows {
			res, ierr := tx.ExecContext(ctx,
				`INSERT INTO checkpoints (created_at, goal, state) VALUES (?, ?, ?)`,
				formatCheckpointTime(at), sanitizeCheckpointGoal(goal), checkpointOpen)
			if ierr != nil {
				return fmt.Errorf("golem: checkpoint create: %w", ierr)
			}
			if cpID, ierr = res.LastInsertId(); ierr != nil {
				return fmt.Errorf("golem: checkpoint create id: %w", ierr)
			}
		} else if verr != nil {
			return fmt.Errorf("golem: checkpoint find open: %w", verr)
		}
		res, ierr := tx.ExecContext(ctx,
			`INSERT INTO checkpoint_files
			 (checkpoint_id, path, prior_content, prior_hash, existed, after_hash, summary, at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			cpID, path, rec.PriorContent, priorHash, existed, rec.AfterHash,
			rec.Summary, formatCheckpointTime(rec.At))
		if ierr != nil {
			return fmt.Errorf("golem: checkpoint intent %s: %w", path, ierr)
		}
		if fileID, ierr = res.LastInsertId(); ierr != nil {
			return fmt.Errorf("golem: checkpoint intent id: %w", ierr)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return cpID, fileID, nil
}

// commitIntent marks a prepared row applied after the workspace rename landed.
func (s *checkpointStore) commitIntent(ctx context.Context, fileID int64) error {
	return s.execTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE checkpoint_files SET applied = 1
			 WHERE id = ? AND applied = 0
			   AND checkpoint_id IN (SELECT id FROM checkpoints WHERE state = ?)`,
			fileID, checkpointOpen)
		if err != nil {
			return fmt.Errorf("golem: checkpoint commit intent %d: %w", fileID, err)
		}
		return requireOneRow(res, fmt.Sprintf("commit intent %d", fileID))
	})
}

// abortIntent discards a prepared row whose workspace write failed.
func (s *checkpointStore) abortIntent(ctx context.Context, fileID int64) error {
	return s.execTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM checkpoint_files
			 WHERE id = ? AND applied = 0
			   AND checkpoint_id IN (SELECT id FROM checkpoints WHERE state = ?)`,
			fileID, checkpointOpen)
		if err != nil {
			return fmt.Errorf("golem: checkpoint abort intent %d: %w", fileID, err)
		}
		return requireOneRow(res, fmt.Sprintf("abort intent %d", fileID))
	})
}

// seal completes the turn's checkpoint: it requires zero unapplied intents,
// deletes an all-aborted empty checkpoint instead of publishing it, applies
// count retention in the same transaction, and conditionally transitions
// open -> completed.
func (s *checkpointStore) seal(ctx context.Context, cpID int64) error {
	return s.execTx(ctx, func(tx *sql.Tx) error {
		var total, unapplied int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(SUM(1 - applied), 0)
			 FROM checkpoint_files WHERE checkpoint_id = ?`, cpID).Scan(&total, &unapplied); err != nil {
			return fmt.Errorf("golem: checkpoint seal count: %w", err)
		}
		if unapplied > 0 {
			return fmt.Errorf("golem: checkpoint seal %d: %d unapplied intent(s)", cpID, unapplied)
		}
		if total == 0 {
			return deleteCheckpointTx(ctx, tx, cpID, checkpointOpen)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE checkpoints SET state = ? WHERE id = ? AND state = ?`,
			checkpointCompleted, cpID, checkpointOpen)
		if err != nil {
			return fmt.Errorf("golem: checkpoint seal %d: %w", cpID, err)
		}
		if err := requireOneRow(res, fmt.Sprintf("seal %d", cpID)); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM checkpoints WHERE state = ?
			 ORDER BY id DESC LIMIT -1 OFFSET ?`, checkpointCompleted, s.maxCheckpoints)
		if err != nil {
			return fmt.Errorf("golem: checkpoint count retention: %w", err)
		}
		var victims []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("golem: checkpoint count retention scan: %w", err)
			}
			victims = append(victims, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("golem: checkpoint count retention rows: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("golem: checkpoint count retention close: %w", err)
		}
		for _, id := range victims {
			if err := deleteCheckpointTx(ctx, tx, id, checkpointCompleted); err != nil {
				return err
			}
		}
		return nil
	})
}

// markUndoing commits undo intent for the given checkpoints in one
// transaction, requiring every row to transition completed -> undoing. After
// this they are never pruned and survive crashes until fully restored.
func (s *checkpointStore) markUndoing(ctx context.Context, ids []int64) error {
	return s.execTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			res, err := tx.ExecContext(ctx,
				`UPDATE checkpoints SET state = ? WHERE id = ? AND state = ?`,
				checkpointUndoing, id, checkpointCompleted)
			if err != nil {
				return fmt.Errorf("golem: checkpoint mark undoing %d: %w", id, err)
			}
			if err := requireOneRow(res, fmt.Sprintf("mark undoing %d", id)); err != nil {
				return err
			}
		}
		return nil
	})
}

// markRestored persists per-file undo progress. Only an applied, unrestored
// row of an undoing checkpoint can transition: an invalid state cannot record
// a file as restored.
func (s *checkpointStore) markRestored(ctx context.Context, fileID int64) error {
	return s.execTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE checkpoint_files SET restored = 1
			 WHERE id = ? AND applied = 1 AND restored = 0
			   AND checkpoint_id IN (SELECT id FROM checkpoints WHERE state = ?)`,
			fileID, checkpointUndoing)
		if err != nil {
			return fmt.Errorf("golem: checkpoint mark restored %d: %w", fileID, err)
		}
		return requireOneRow(res, fmt.Sprintf("mark restored %d", fileID))
	})
}

// deleteRestored deletes an undoing checkpoint only when a guard query proves
// zero applied rows remain unrestored — the guard, not the caller's belief,
// decides (a crash between a restore and its flag must never orphan work).
func (s *checkpointStore) deleteRestored(ctx context.Context, cpID int64) error {
	return s.execTx(ctx, func(tx *sql.Tx) error {
		var pending int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM checkpoint_files
			 WHERE checkpoint_id = ? AND applied = 1 AND restored = 0`, cpID).Scan(&pending); err != nil {
			return fmt.Errorf("golem: checkpoint delete pending: %w", err)
		}
		if pending > 0 {
			return fmt.Errorf("golem: checkpoint %d has %d unrestored file(s)", cpID, pending)
		}
		return deleteCheckpointTx(ctx, tx, cpID, checkpointUndoing)
	})
}

// list returns this workspace's checkpoints newest first with file counts and
// prior-content byte totals.
func (s *checkpointStore) list(ctx context.Context) ([]checkpointInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.created_at, c.goal, c.state,
		        COUNT(f.id), COALESCE(SUM(LENGTH(f.prior_content)), 0)
		 FROM checkpoints c LEFT JOIN checkpoint_files f ON f.checkpoint_id = c.id
		 GROUP BY c.id ORDER BY c.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("golem: checkpoint list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []checkpointInfo
	for rows.Next() {
		var in checkpointInfo
		var created, state string
		if err := rows.Scan(&in.id, &created, &in.goal, &state, &in.files, &in.bytes); err != nil {
			return nil, fmt.Errorf("golem: checkpoint list scan: %w", err)
		}
		in.state = checkpointState(state)
		if in.createdAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("golem: checkpoint list time: %w", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("golem: checkpoint list rows: %w", err)
	}
	return out, nil
}

// loadGroups loads checkpoint groups in the given state, newest checkpoint
// first, files in reverse mutation order, up to limit (0 = no limit).
// appliedOnly excludes write-ahead intents that never landed — undo consumes
// only applied rows; startup reconciliation reads everything.
func (s *checkpointStore) loadGroups(ctx context.Context, state checkpointState, limit int, appliedOnly bool) ([]checkpointGroup, error) {
	q := `SELECT id, goal FROM checkpoints WHERE state = ? ORDER BY id DESC`
	args := []any{state}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("golem: checkpoint load: %w", err)
	}
	var groups []checkpointGroup
	for rows.Next() {
		g := checkpointGroup{state: state}
		if err := rows.Scan(&g.id, &g.goal); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("golem: checkpoint load scan: %w", err)
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("golem: checkpoint load rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("golem: checkpoint load close: %w", err)
	}
	for i := range groups {
		if groups[i].files, err = s.loadFiles(ctx, groups[i].id, appliedOnly); err != nil {
			return nil, err
		}
	}
	return groups, nil
}

func (s *checkpointStore) loadFiles(ctx context.Context, cpID int64, appliedOnly bool) ([]checkpointFile, error) {
	q := `SELECT id, path, prior_content, prior_hash, existed, after_hash, applied, restored
	      FROM checkpoint_files WHERE checkpoint_id = ?`
	if appliedOnly {
		q += ` AND applied = 1`
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, q, cpID)
	if err != nil {
		return nil, fmt.Errorf("golem: checkpoint files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []checkpointFile
	for rows.Next() {
		var f checkpointFile
		var existed, applied, restored int
		if err := rows.Scan(&f.id, &f.path, &f.priorContent, &f.priorHash,
			&existed, &f.afterHash, &applied, &restored); err != nil {
			return nil, fmt.Errorf("golem: checkpoint files scan: %w", err)
		}
		f.existed, f.applied, f.restored = existed != 0, applied != 0, restored != 0
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("golem: checkpoint files rows: %w", err)
	}
	return out, nil
}

// newestCompleted loads the n newest completed checkpoints (applied rows only)
// for undo.
func (s *checkpointStore) newestCompleted(ctx context.Context, n int) ([]checkpointGroup, error) {
	return s.loadGroups(ctx, checkpointCompleted, n, true)
}

// undoingGroups loads every interrupted-undo checkpoint, newest first.
func (s *checkpointStore) undoingGroups(ctx context.Context) ([]checkpointGroup, error) {
	return s.loadGroups(ctx, checkpointUndoing, 0, true)
}

// countState reports how many checkpoints sit in the given state.
func (s *checkpointStore) countState(ctx context.Context, state checkpointState) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checkpoints WHERE state = ?`, state).Scan(&n); err != nil {
		return 0, fmt.Errorf("golem: checkpoint count %s: %w", state, err)
	}
	return n, nil
}
