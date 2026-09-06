package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/memory"
	"github.com/kstruzzieri/go-llm/signing"
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

// Retention defaults (issue #355): strict per-workspace undo snapshot caps.
// Receipt metadata survives pruning and grows indefinitely; retention requires
// an explicit future policy rather than silently discarding audit evidence.
const (
	defaultMaxCheckpoints = 50
	defaultMaxPriorBytes  = 64 << 20 // 64 MiB of prior content
)

// checkpointSchemaVersion is the newest schema this binary understands. The
// schema is internal: #347 consumes the semantic mutation/turn contract, never
// these tables.
// v2 (#443) adds the nullable checkpoint_files.after_mode column: the exact
// tracked permission bits of a promoted create, NULL for every legacy and
// write/edit row.
// v3 (#445) adds an independent receipt ledger and nullable unique references.
const checkpointSchemaVersion = 3

// errCheckpointLeaseHeld reports that another live golem process owns this
// workspace's checkpoint store. Checkpoints fail closed on contention (D10):
// there is no last-writer-wins mode for undo history.
var errCheckpointLeaseHeld = errors.New("golem: another golem process holds this workspace's checkpoint store")

// checkpointStore persists per-turn mutation checkpoints in one hardened
// per-workspace SQLite DB under the per-user data dir, owned exclusively via
// an OS file lease for the store's lifetime. It knows nothing about the
// Workspace; restoring files is checkpointJournal's job. Deleted rows never
// shrink the DB file (no vacuum). Retention bounds prior-content snapshots;
// receipt metadata and its SQLite overhead are not bounded by those caps.
type checkpointStore struct {
	db            *sql.DB
	dbPath        string
	lease         *flockLease
	workspaceHash string

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
	root, err = agenttools.CanonicalWorkspaceRoot(root)
	if err != nil {
		return "", err
	}
	hash := strings.TrimPrefix(workspaceID(root), "workspace:")
	return filepath.Join(base, "golem", "checkpoints", hash+".db"), nil
}

// openCheckpointStore acquires this workspace's exclusive checkpoint lease,
// opens (creating if missing) the hardened DB, migrates to the current schema, and
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
	canonicalRoot, err := agenttools.CanonicalWorkspaceRoot(root)
	if err != nil {
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
		workspaceHash:  agenttools.ContentHash([]byte(canonicalRoot)),
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

// migrate steps the schema to the current version in one transaction per
// starting version, so a crash mid-migration leaves the old version intact
// and a clean retry. v0 creates the schema; v1/v2 gain nullable columns and
// the independent ledger additively — existing rows are never copied or replaced
// (a destructive table rebuild would risk undo history). A database from a
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
	var stmts []string
	switch version {
	case 0:
		stmts = []string{
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
				after_mode    INTEGER,
				CHECK (restored = 0 OR applied = 1)
			)`,
			`CREATE INDEX checkpoint_files_checkpoint
				ON checkpoint_files(checkpoint_id, id DESC)`,
		}
	case 1:
		stmts = []string{
			`ALTER TABLE checkpoint_files ADD COLUMN after_mode INTEGER`,
		}
	case 2:
	default:
		return fmt.Errorf("golem: checkpoint db %s has unexpected schema v%d", s.dbPath, version)
	}
	stmts = append(stmts,
		`CREATE TABLE mutation_receipts (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			mutation_id TEXT NOT NULL UNIQUE,
			intent_json TEXT NOT NULL,
			applied_json TEXT
		)`,
		`ALTER TABLE checkpoint_files ADD COLUMN forward_mutation_id TEXT
			REFERENCES mutation_receipts(mutation_id) DEFAULT NULL`,
		`ALTER TABLE checkpoint_files ADD COLUMN inverse_mutation_id TEXT
			REFERENCES mutation_receipts(mutation_id) DEFAULT NULL`,
		`CREATE UNIQUE INDEX checkpoint_files_forward_mutation ON checkpoint_files(forward_mutation_id)`,
		`CREATE UNIQUE INDEX checkpoint_files_inverse_mutation ON checkpoint_files(inverse_mutation_id)`,
	)
	stmts = append(stmts, fmt.Sprintf("PRAGMA user_version = %d", checkpointSchemaVersion))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("golem: checkpoint migrate begin: %w", err)
	}
	if version == 1 || version == 2 {
		var interrupted int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkpoints WHERE state IN ('open', 'undoing')`).Scan(&interrupted); err != nil {
			return errors.Join(fmt.Errorf("golem: checkpoint migration recovery check: %w", err), tx.Rollback())
		}
		if interrupted != 0 {
			return errors.Join(errors.New("golem: finish checkpoint recovery/undo with the old binary before upgrading"), tx.Rollback())
		}
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
	// afterMode/trackedMode mirror MutationRecord (#443): the exact tracked
	// permission bits of a promoted create. A NULL column loads untracked.
	afterMode         fs.FileMode
	trackedMode       bool
	forwardMutationID sql.NullString
	inverseMutationID sql.NullString
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

// prepareSignedIntent persists canonical signed evidence and its snapshot in one
// transaction with strict size admission. Pruning commits only with accepted
// evidence. The caller authenticates the signature; storage binds its metadata.
func (s *checkpointStore) prepareSignedIntent(ctx context.Context, goal string, at time.Time, rec agenttools.MutationRecord, intentJSON []byte) (cpID, fileID int64, err error) {
	path := canonicalCheckpointPath(rec.Path)
	priorHash := ""
	existed := 0
	if rec.Existed {
		priorHash = agenttools.ContentHash(rec.PriorContent)
		existed = 1
	}
	intent, err := decodeStoredMutationReceipt(intentJSON)
	if err != nil {
		return 0, 0, err
	}
	f := checkpointFile{path: path, priorHash: priorHash, existed: rec.Existed, afterHash: rec.AfterHash, afterMode: rec.AfterMode, trackedMode: rec.TrackedMode}
	if err := s.bindForwardReceipt(f, intent); err != nil {
		return 0, 0, err
	}
	mutationID := intent.Body.MutationID
	err = s.execTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_receipts (mutation_id, intent_json) VALUES (?, ?)`, mutationID, string(intentJSON)); err != nil {
			return fmt.Errorf("golem: insert forward intent: %w", err)
		}
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
		var afterMode any // NULL for every untracked record
		if rec.TrackedMode {
			afterMode = int64(rec.AfterMode)
		}
		res, ierr := tx.ExecContext(ctx,
			`INSERT INTO checkpoint_files
			 (checkpoint_id, path, prior_content, prior_hash, existed, after_hash, summary, at, after_mode, forward_mutation_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			cpID, path, rec.PriorContent, priorHash, existed, rec.AfterHash,
			rec.Summary, formatCheckpointTime(rec.At), afterMode, mutationID)
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

// abortIntent discards a prepared row whose workspace write failed.
func (s *checkpointStore) abortIntent(ctx context.Context, fileID int64) error {
	return s.execTx(ctx, func(tx *sql.Tx) error {
		var mutationID sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT forward_mutation_id FROM checkpoint_files WHERE id = ?`, fileID).Scan(&mutationID); err != nil {
			return err
		}
		if mutationID.Valid {
			_, forward, err := s.boundForwardTx(ctx, tx, fileID)
			if err != nil {
				return err
			}
			if forward.appliedJSON != nil {
				return errors.New("golem: cannot abort completed evidence")
			}
		}
		// Remove the reference first, then its definitely unused intent. Recovery
		// instead uses recoverDropIntent and preserves unresolved evidence.
		res, err := tx.ExecContext(ctx,
			`DELETE FROM checkpoint_files WHERE id = ? AND applied = 0 AND inverse_mutation_id IS NULL
			 AND checkpoint_id IN (SELECT id FROM checkpoints WHERE state = ?)`, fileID, checkpointOpen)
		if err != nil {
			return fmt.Errorf("golem: abort intent: %w", err)
		}
		if err := requireOneRow(res, "abort intent"); err != nil {
			return err
		}
		if mutationID.Valid {
			res, err := tx.ExecContext(ctx, `DELETE FROM mutation_receipts WHERE mutation_id = ? AND applied_json IS NULL
			 AND NOT EXISTS (SELECT 1 FROM checkpoint_files WHERE forward_mutation_id = ? OR inverse_mutation_id = ?)`, mutationID.String, mutationID.String, mutationID.String)
			if err != nil {
				return fmt.Errorf("golem: abort evidence: %w", err)
			}
			return requireOneRow(res, "abort evidence")
		}
		return nil
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
	return s.queryFiles(ctx, cpID, appliedOnly, true)
}

var errCheckpointMetadata = errors.New("golem: invalid stored checkpoint metadata")

// loadFileMetadata omits prior-content blobs for checkpoint listings. Invalid
// modes return the rows plus errCheckpointMetadata so the listing can still
// authenticate their references and attribute the metadata fault to this group.
func (s *checkpointStore) loadFileMetadata(ctx context.Context, cpID int64, appliedOnly bool) ([]checkpointFile, error) {
	return s.queryFiles(ctx, cpID, appliedOnly, false)
}

func (s *checkpointStore) queryFiles(ctx context.Context, cpID int64, appliedOnly, withContent bool) ([]checkpointFile, error) {
	content := "NULL"
	if withContent {
		content = "prior_content"
	}
	q := `SELECT id, path, ` + content + `, prior_hash, existed, after_hash, applied, restored, after_mode, forward_mutation_id, inverse_mutation_id
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
	var metadataErr error
	for rows.Next() {
		var f checkpointFile
		var existed, applied, restored int
		var afterMode sql.NullInt64
		var forward, inverse sql.NullString
		if err := rows.Scan(&f.id, &f.path, &f.priorContent, &f.priorHash,
			&existed, &f.afterHash, &applied, &restored, &afterMode, &forward, &inverse); err != nil {
			return nil, fmt.Errorf("golem: checkpoint files scan: %w", err)
		}
		f.forwardMutationID, f.inverseMutationID = forward, inverse
		f.existed, f.applied, f.restored = existed != 0, applied != 0, restored != 0
		if afterMode.Valid {
			if afterMode.Int64 < 0 || afterMode.Int64 > 0o777 {
				metadataErr = fmt.Errorf("%w: mode", errCheckpointMetadata)
				if withContent {
					return nil, metadataErr
				}
			} else {
				f.trackedMode = true
				f.afterMode = fs.FileMode(afterMode.Int64)
			}
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("golem: checkpoint files rows: %w", err)
	}
	return out, metadataErr
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

// checkpointReceipt is the sole stored whole-envelope representation. Sequence
// orders a bounded scan, not timestamps; gaps do not prove ledger completeness.
type checkpointReceipt struct {
	sequence    int64
	mutationID  string
	intentJSON  []byte
	appliedJSON []byte
}

func decodeStoredMutationReceipt(raw []byte) (agenttools.MutationReceipt, error) {
	receipt, err := agenttools.DecodeMutationReceipt(raw)
	if err != nil {
		return agenttools.MutationReceipt{}, err
	}
	canonical, err := signing.MarshalCanonical(receipt)
	if err != nil {
		return agenttools.MutationReceipt{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return agenttools.MutationReceipt{}, errors.New("golem: noncanonical stored mutation receipt")
	}
	return receipt, nil
}

func matchingAppliedReceipt(intent, applied agenttools.MutationReceipt) error {
	a, b := intent.Body, applied.Body
	if a.Kind != "intent" || b.Kind != "applied" || intent.Signature.Alg != applied.Signature.Alg || intent.Signature.KeyID != applied.Signature.KeyID {
		return errors.New("golem: applied receipt does not match intent")
	}
	if (a.AfterMode == nil) != (b.AfterMode == nil) || a.AfterMode != nil && *a.AfterMode != *b.AfterMode {
		return errors.New("golem: applied receipt mode differs from intent")
	}
	b.Kind, b.Timestamp, b.AfterMode = a.Kind, a.Timestamp, a.AfterMode
	if a != b {
		return errors.New("golem: applied receipt body differs from intent")
	}
	return nil
}

func validateCheckpointReceipt(entry checkpointReceipt) error {
	intent, err := decodeStoredMutationReceipt(entry.intentJSON)
	if err != nil {
		return err
	}
	if intent.Body.Kind != "intent" || intent.Body.MutationID != entry.mutationID {
		return errors.New("golem: ledger intent identity mismatch")
	}
	if entry.appliedJSON != nil {
		applied, err := decodeStoredMutationReceipt(entry.appliedJSON)
		if err != nil {
			return err
		}
		return matchingAppliedReceipt(intent, applied)
	}
	return nil
}

func scanCheckpointReceipt(row *sql.Row) (checkpointReceipt, error) {
	var entry checkpointReceipt
	if err := row.Scan(&entry.sequence, &entry.mutationID, &entry.intentJSON, &entry.appliedJSON); err != nil {
		return checkpointReceipt{}, err
	}
	if err := validateCheckpointReceipt(entry); err != nil {
		return checkpointReceipt{}, err
	}
	return entry, nil
}

func (s *checkpointStore) loadReceipt(ctx context.Context, mutationID string) (checkpointReceipt, error) {
	return scanCheckpointReceipt(s.db.QueryRowContext(ctx, `SELECT sequence, mutation_id, intent_json, applied_json FROM mutation_receipts WHERE mutation_id = ?`, mutationID))
}

func loadReceiptTx(ctx context.Context, tx *sql.Tx, mutationID string) (checkpointReceipt, error) {
	return scanCheckpointReceipt(tx.QueryRowContext(ctx, `SELECT sequence, mutation_id, intent_json, applied_json FROM mutation_receipts WHERE mutation_id = ?`, mutationID))
}

// scanReceipts is metadata-only and starts at the independent ledger, so pruning
// or completed undo never hides evidence. Authentication belongs to the caller.
func (s *checkpointStore) scanReceipts(ctx context.Context, afterSequence int64, limit int) ([]checkpointReceipt, error) {
	if afterSequence < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("golem: receipt scan requires nonnegative cursor and limit 1..1000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, mutation_id, intent_json, applied_json
	 FROM mutation_receipts WHERE sequence > ? ORDER BY sequence ASC LIMIT ?`, afterSequence, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries []checkpointReceipt
	for rows.Next() {
		var entry checkpointReceipt
		if err := rows.Scan(&entry.sequence, &entry.mutationID, &entry.intentJSON, &entry.appliedJSON); err != nil {
			return nil, err
		}
		if err := validateCheckpointReceipt(entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (s *checkpointStore) bindForwardReceipt(f checkpointFile, intent agenttools.MutationReceipt) error {
	body := intent.Body
	beforeHash := "absent"
	if f.existed {
		beforeHash = f.priorHash
	}
	if body.Kind != "intent" || body.UndoOf != "" || body.WorkspaceHash != s.workspaceHash || body.Path != f.path || body.BeforeHash != beforeHash || body.AfterHash != f.afterHash || (body.AfterMode != nil) != f.trackedMode || body.AfterMode != nil && fs.FileMode(*body.AfterMode) != f.afterMode {
		return errors.New("golem: forward intent does not bind checkpoint metadata")
	}
	return nil
}

func (s *checkpointStore) boundForwardTx(ctx context.Context, tx *sql.Tx, fileID int64) (checkpointFile, checkpointReceipt, error) {
	var f checkpointFile
	var mode sql.NullInt64
	var forward, inverse sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, path, prior_hash, existed, after_hash, after_mode, forward_mutation_id, inverse_mutation_id
	 FROM checkpoint_files WHERE id = ?`, fileID).Scan(&f.id, &f.path, &f.priorHash, &f.existed, &f.afterHash, &mode, &forward, &inverse)
	if err != nil {
		return f, checkpointReceipt{}, err
	}
	if mode.Valid && (mode.Int64 < 0 || mode.Int64 > 0o777) {
		return f, checkpointReceipt{}, errors.New("golem: invalid stored checkpoint mode")
	}
	f.afterMode, f.trackedMode = fs.FileMode(mode.Int64), mode.Valid
	f.forwardMutationID, f.inverseMutationID = forward, inverse
	if !forward.Valid || forward.String == "" {
		return f, checkpointReceipt{}, errors.New("golem: checkpoint has no verifiable forward intent")
	}
	if inverse.Valid && inverse.String == "" {
		return f, checkpointReceipt{}, errors.New("golem: checkpoint has an invalid inverse reference")
	}
	entry, err := loadReceiptTx(ctx, tx, forward.String)
	if err != nil {
		return f, entry, err
	}
	intent, err := decodeStoredMutationReceipt(entry.intentJSON)
	if err != nil {
		return f, entry, err
	}
	return f, entry, s.bindForwardReceipt(f, intent)
}

func boundInverseTx(ctx context.Context, tx *sql.Tx, forward checkpointReceipt, inverseID string) (checkpointReceipt, error) {
	entry, err := loadReceiptTx(ctx, tx, inverseID)
	if err != nil {
		return entry, err
	}
	original, err := decodeStoredMutationReceipt(forward.intentJSON)
	if err != nil {
		return entry, err
	}
	inverse, err := decodeStoredMutationReceipt(entry.intentJSON)
	if err != nil {
		return entry, err
	}
	if err := bindInverseReceipt(original, inverse); err != nil {
		return entry, err
	}
	return entry, nil
}

func bindInverseReceipt(forward, inverse agenttools.MutationReceipt) error {
	f, i := forward.Body, inverse.Body
	if f.Kind != "intent" || f.UndoOf != "" || i.Kind != "intent" || i.UndoOf != f.MutationID || i.WorkspaceHash != f.WorkspaceHash || i.Path != f.Path || i.BeforeHash != f.AfterHash || i.AfterHash != f.BeforeHash || i.AfterMode != nil {
		return errors.New("golem: inverse intent does not reverse forward intent")
	}
	return nil
}

func commitReceiptTx(ctx context.Context, tx *sql.Tx, entry checkpointReceipt, appliedJSON []byte) error {
	intent, err := decodeStoredMutationReceipt(entry.intentJSON)
	if err != nil {
		return err
	}
	applied, err := decodeStoredMutationReceipt(appliedJSON)
	if err != nil {
		return err
	}
	if err := matchingAppliedReceipt(intent, applied); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE mutation_receipts SET applied_json = ? WHERE mutation_id = ? AND applied_json IS NULL`, string(appliedJSON), entry.mutationID)
	if err != nil {
		return fmt.Errorf("golem: commit receipt: %w", err)
	}
	return requireOneRow(res, "commit receipt")
}

// commitSignedIntent atomically records observed evidence and snapshot progress.
func (s *checkpointStore) commitSignedIntent(ctx context.Context, fileID int64, appliedJSON []byte) error {
	return s.execTx(ctx, func(tx *sql.Tx) error {
		_, entry, err := s.boundForwardTx(ctx, tx, fileID)
		if err != nil {
			return err
		}
		if err := commitReceiptTx(ctx, tx, entry, appliedJSON); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE checkpoint_files SET applied = 1 WHERE id = ? AND applied = 0
		 AND inverse_mutation_id IS NULL AND checkpoint_id IN (SELECT id FROM checkpoints WHERE state = ?)`, fileID, checkpointOpen)
		if err != nil {
			return err
		}
		return requireOneRow(res, "commit signed intent")
	})
}

// recoverCommitIntent preserves an uncertain forward as undoable bookkeeping;
// it never fabricates an observed applied receipt.
func (s *checkpointStore) recoverCommitIntent(ctx context.Context, fileID int64) error {
	return s.recoverForwardIntent(ctx, fileID, true)
}

// recoverDropIntent removes a snapshot whose target was already reached while
// retaining the unresolved intent. It is not a definite Abort.
func (s *checkpointStore) recoverDropIntent(ctx context.Context, fileID int64) error {
	return s.recoverForwardIntent(ctx, fileID, false)
}

func (s *checkpointStore) recoverForwardIntent(ctx context.Context, fileID int64, keep bool) error {
	return s.execTx(ctx, func(tx *sql.Tx) error {
		_, entry, err := s.boundForwardTx(ctx, tx, fileID)
		if err != nil {
			return err
		}
		if entry.appliedJSON != nil {
			return errors.New("golem: recovery requires unconfirmed forward intent")
		}
		query := `DELETE FROM checkpoint_files`
		if keep {
			query = `UPDATE checkpoint_files SET applied = 1`
		}
		res, err := tx.ExecContext(ctx, query+` WHERE id = ? AND applied = 0 AND inverse_mutation_id IS NULL
		 AND checkpoint_id IN (SELECT id FROM checkpoints WHERE state = ?)`, fileID, checkpointOpen)
		if err != nil {
			return err
		}
		return requireOneRow(res, "recover forward intent")
	})
}

// prepareInverseIntent preserves previous unresolved attempts if a retry needs
// a new identity. Completed inverse evidence must be reconciled, never replaced.
func (s *checkpointStore) prepareInverseIntent(ctx context.Context, fileID int64, intentJSON []byte) error {
	inverse, err := decodeStoredMutationReceipt(intentJSON)
	if err != nil {
		return err
	}
	return s.execTx(ctx, func(tx *sql.Tx) error {
		f, forward, err := s.boundForwardTx(ctx, tx, fileID)
		if err != nil {
			return err
		}
		original, err := decodeStoredMutationReceipt(forward.intentJSON)
		if err != nil {
			return err
		}
		if err := bindInverseReceipt(original, inverse); err != nil {
			return err
		}
		if f.inverseMutationID.Valid {
			previous, err := boundInverseTx(ctx, tx, forward, f.inverseMutationID.String)
			if err != nil {
				return err
			}
			if previous.appliedJSON != nil {
				return errors.New("golem: completed inverse requires reconciliation")
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_receipts (mutation_id, intent_json) VALUES (?, ?)`, inverse.Body.MutationID, string(intentJSON)); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE checkpoint_files SET inverse_mutation_id = ? WHERE id = ? AND applied = 1 AND restored = 0
		 AND checkpoint_id IN (SELECT id FROM checkpoints WHERE state = ?)`, inverse.Body.MutationID, fileID, checkpointUndoing)
		if err != nil {
			return err
		}
		return requireOneRow(res, "prepare inverse intent")
	})
}

func (s *checkpointStore) commitInverseIntent(ctx context.Context, fileID int64, appliedJSON []byte) error {
	return s.execTx(ctx, func(tx *sql.Tx) error {
		f, forward, err := s.boundForwardTx(ctx, tx, fileID)
		if err != nil {
			return err
		}
		inverse, err := boundInverseTx(ctx, tx, forward, f.inverseMutationID.String)
		if err != nil {
			return err
		}
		if err := commitReceiptTx(ctx, tx, inverse, appliedJSON); err != nil {
			return err
		}
		return markSignedRestoredTx(ctx, tx, fileID)
	})
}

func markSignedRestoredTx(ctx context.Context, tx *sql.Tx, fileID int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE checkpoint_files SET restored = 1 WHERE id = ? AND applied = 1 AND restored = 0
	 AND checkpoint_id IN (SELECT id FROM checkpoints WHERE state = ?)`, fileID, checkpointUndoing)
	if err != nil {
		return err
	}
	return requireOneRow(res, "restore signed intent")
}

// recoverRestored records an already-reached target, after caller authentication
// and filesystem guards. Missing/incomplete inverse evidence stays unconfirmed.
func (s *checkpointStore) recoverRestored(ctx context.Context, fileID int64) error {
	return s.execTx(ctx, func(tx *sql.Tx) error {
		f, forward, err := s.boundForwardTx(ctx, tx, fileID)
		if err != nil {
			return err
		}
		if f.inverseMutationID.Valid {
			if _, err := boundInverseTx(ctx, tx, forward, f.inverseMutationID.String); err != nil {
				return err
			}
		}
		return markSignedRestoredTx(ctx, tx, fileID)
	})
}

// reconcileInverseIntent binds retained completed inverse evidence to the row
// and records progress without filesystem mutation or new evidence. A different
// unresolved inverse remains in the ledger after its reference is replaced.
func (s *checkpointStore) reconcileInverseIntent(ctx context.Context, fileID int64, inverseID string) error {
	return s.execTx(ctx, func(tx *sql.Tx) error {
		f, forward, err := s.boundForwardTx(ctx, tx, fileID)
		if err != nil {
			return err
		}
		if f.inverseMutationID.Valid {
			if _, err := boundInverseTx(ctx, tx, forward, f.inverseMutationID.String); err != nil {
				return err
			}
		}
		inverse, err := boundInverseTx(ctx, tx, forward, inverseID)
		if err != nil {
			return err
		}
		if inverse.appliedJSON == nil {
			return errors.New("golem: inverse receipt is unconfirmed")
		}
		res, err := tx.ExecContext(ctx, `UPDATE checkpoint_files SET inverse_mutation_id = ? WHERE id = ?`, inverseID, fileID)
		if err != nil {
			return err
		}
		if err := requireOneRow(res, "reconcile inverse reference"); err != nil {
			return err
		}
		return markSignedRestoredTx(ctx, tx, fileID)
	})
}
