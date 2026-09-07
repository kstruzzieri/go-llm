package main

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/signing"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func auditWorkspace(ctx context.Context, root string) (result auditResult) {
	defer func() { result.scope, result.assurance = "workspace", "signed" }()
	canonical, err := agenttools.CanonicalWorkspaceRoot(root)
	if err != nil {
		return auditStoreFailure(root, "incomplete", "workspace-unavailable", "Workspace is unavailable or unsafe.")
	}
	path, err := checkpointDBPath(os.Getenv, canonical)
	if err != nil || validatePathOutsideWorkspace(path, canonical) != nil {
		return auditStoreFailure(root, "incomplete", "store-unavailable", "Store is unavailable or unsafe.")
	}
	return withAuditStore(ctx, path, func(db *sql.DB) auditResult {
		return scanAuditWorkspace(ctx, canonical, &checkpointStore{db: db, dbPath: path, workspaceHash: agenttools.ContentHash([]byte(canonical))}, readWorkspaceFileStable)
	})
}

type auditPathExpectation struct {
	state     fileState
	sequence  int64
	uncertain bool
}

func scanAuditWorkspace(ctx context.Context, root string, store *checkpointStore, readStable func(string, string, func()) (fileState, error)) (result auditResult) {
	result = auditResult{outcome: "valid"}
	defer func() {
		if ctx.Err() != nil {
			result = auditStoreReadFailure(store.dbPath, ctx.Err())
		}
	}()
	fail := func(outcome, code, target, message string) auditResult {
		result.outcome = outcome
		result.earlyStop = true
		result.diagnostics = append(result.diagnostics, auditDiagnostic{code: code, target: target, message: message})
		return result
	}
	evidenceFailure := func(err error, code, target, message string) auditResult {
		var sqliteErr *sqlite.Error
		if errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff != sqlite3.SQLITE_ERROR {
			failure := auditStoreReadFailure(store.dbPath, err)
			diagnostic := failure.diagnostics[0]
			return fail(failure.outcome, diagnostic.code, diagnostic.target, diagnostic.message)
		}
		return fail("violation", code, target, message)
	}
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return auditStoreReadFailure(store.dbPath, err)
	}
	if version != 3 {
		return fail("incomplete", "workspace-schema", store.dbPath, "Workspace evidence schema is unsigned or unsupported.")
	}
	// Validate the whole supported schema even when every ledger is empty.
	for _, query := range []string{
		`SELECT id,created_at,goal,state FROM checkpoints LIMIT 0`,
		`SELECT id,checkpoint_id,path,prior_content,prior_hash,existed,after_hash,summary,at,applied,restored,after_mode,forward_mutation_id,inverse_mutation_id FROM checkpoint_files LIMIT 0`,
		`SELECT sequence,mutation_id,intent_json,applied_json FROM mutation_receipts LIMIT 0`,
	} {
		rows, err := store.db.QueryContext(ctx, query)
		if err != nil {
			if ctx.Err() != nil {
				return auditStoreReadFailure(store.dbPath, err)
			}
			return evidenceFailure(err, "workspace-schema", store.dbPath, "Required workspace evidence structure is invalid.")
		}
		if err := rows.Close(); err != nil {
			return auditStoreReadFailure(store.dbPath, err)
		}
	}
	var invalid bool
	err := store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mutation_receipts WHERE sequence<=0 OR mutation_id IS NULL OR intent_json IS NULL)
 OR EXISTS(SELECT 1 FROM checkpoints WHERE state IS NULL OR state NOT IN ('open','completed','undoing'))
 OR EXISTS(SELECT 1 FROM checkpoint_files f LEFT JOIN checkpoints c ON c.id=f.checkpoint_id WHERE f.id<=0 OR c.id IS NULL OR f.existed IS NULL OR f.existed NOT IN (0,1) OR f.applied IS NULL OR f.applied NOT IN (0,1) OR f.restored IS NULL OR f.restored NOT IN (0,1) OR (f.restored=1 AND f.applied=0) OR (f.after_mode IS NOT NULL AND (typeof(f.after_mode)!='integer' OR f.after_mode<0 OR f.after_mode>511)))`).Scan(&invalid)
	if err != nil {
		return auditStoreReadFailure(store.dbPath, err)
	}
	if invalid {
		return fail("violation", "workspace-schema", store.dbPath, "Required workspace evidence structure is invalid.")
	}
	incomplete := func(code, target, message string) {
		result.outcome = "incomplete"
		if len(result.diagnostics) < 2 {
			result.diagnostics = append(result.diagnostics, auditDiagnostic{code: code, target: target, message: message})
		}
	}
	expected := map[string]auditPathExpectation{}
	// ponytail: O(completed inverses) transient IDs; a bounded persistent index
	// requires a separate integrity design if retained ledger size demands it.
	completed := map[string]bool{}
	var verifier signing.Verifier
	for cursor := int64(0); ; {
		entries, err := store.scanReceipts(ctx, cursor, 100)
		if err != nil {
			if ctx.Err() != nil {
				return auditStoreReadFailure(store.dbPath, err)
			}
			return evidenceFailure(err, "receipt-invalid", store.dbPath, "Retained receipt evidence is invalid.")
		}
		if len(entries) == 0 {
			break
		}
		if verifier == nil {
			base, err := dataDirBase(os.Getenv)
			if err != nil {
				return fail("incomplete", "workspace-key", store.dbPath, "Trusted workspace verifier is unavailable or mismatched.")
			}
			key := filepath.Join(base, "golem", "signing", "agent-ed25519.pem")
			if validatePathOutsideWorkspace(key, root) != nil {
				return fail("incomplete", "workspace-key", key, "Trusted workspace verifier is unavailable or mismatched.")
			}
			verifier, err = loadAuditWorkspaceVerifier(key)
			if err != nil {
				return fail("incomplete", "workspace-key", key, "Trusted workspace verifier is unavailable or mismatched.")
			}
		}
		for _, entry := range entries {
			if entry.intent.Body.AgentID != verifier.KeyID() || entry.intent.Signature.Alg != verifier.Algorithm() {
				return fail("incomplete", "workspace-key", entry.mutationID, "Trusted workspace verifier is unavailable or mismatched.")
			}
			intent, err := authenticateCheckpointReceipt(ctx, verifier, entry)
			if err != nil {
				return fail("violation", "receipt-invalid", entry.mutationID, "Retained receipt evidence is invalid.")
			}
			body := intent.Body
			if body.WorkspaceHash != store.workspaceHash {
				return fail("violation", "receipt-binding", entry.mutationID, "Receipt identity or lineage does not bind retained evidence.")
			}
			if body.UndoOf != "" {
				original, err := store.loadReceipt(ctx, body.UndoOf)
				if err != nil {
					return evidenceFailure(err, "receipt-binding", entry.mutationID, "Receipt identity or lineage does not bind retained evidence.")
				}
				if original.intent.Body.AgentID != verifier.KeyID() || original.intent.Signature.Alg != verifier.Algorithm() {
					return fail("incomplete", "workspace-key", original.mutationID, "Trusted workspace verifier is unavailable or mismatched.")
				}
				forward, err := authenticateCheckpointReceipt(ctx, verifier, original)
				if err != nil || bindInverseReceipt(forward, intent) != nil {
					return evidenceFailure(err, "receipt-binding", entry.mutationID, "Receipt identity or lineage does not bind retained evidence.")
				}
				if original.applied == nil {
					incomplete("receipt-unconfirmed", original.mutationID, "Retained mutation application is unconfirmed.")
				}
				if entry.applied != nil {
					if completed[body.UndoOf] {
						return fail("violation", "receipt-binding", entry.mutationID, "Receipt identity or lineage does not bind retained evidence.")
					}
					completed[body.UndoOf] = true
				}
			}
			result.checked++
			state := expected[body.Path]
			if entry.applied == nil {
				incomplete("receipt-unconfirmed", entry.mutationID, "Retained mutation application is unconfirmed.")
				state.uncertain = true
				if body.BeforeHash == "absent" || body.AfterHash == "absent" {
					state.state.modeKnown = false
				}
			} else {
				state.uncertain = false
				state.sequence = entry.sequence
				if body.AfterHash == "absent" {
					state.state = fileState{absent: true}
				} else {
					if body.BeforeHash == "absent" {
						state.state = fileState{}
					}
					state.state.absent = false
					state.state.hash = body.AfterHash
					if body.AfterMode != nil {
						state.state.mode = fs.FileMode(*body.AfterMode)
						state.state.modeKnown = true
					}
				}
			}
			expected[body.Path] = state
			cursor = entry.sequence
		}
	}
	// Check snapshots separately: pruning never removes the independent ledger.
	var interrupted bool
	if err := store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM checkpoints WHERE state IN ('open','undoing'))`).Scan(&interrupted); err != nil {
		return auditStoreReadFailure(store.dbPath, err)
	}
	if interrupted {
		incomplete("checkpoint-unconfirmed", store.dbPath, "Retained checkpoint coverage is unsigned or unconfirmed.")
	}
	type retainedFile struct {
		checkpointFile
		state checkpointState
	}
	for cursor := int64(0); ; {
		rows, err := store.db.QueryContext(ctx, `SELECT f.id,f.path,f.prior_content,f.prior_hash,f.existed,f.after_hash,f.applied,f.restored,f.after_mode,f.forward_mutation_id,f.inverse_mutation_id,c.state FROM checkpoint_files f JOIN checkpoints c ON c.id=f.checkpoint_id WHERE f.id>? ORDER BY f.id LIMIT 100`, cursor)
		if err != nil {
			return auditStoreReadFailure(store.dbPath, err)
		}
		var files []retainedFile
		for rows.Next() {
			var f retainedFile
			var mode sql.NullInt64
			if err = rows.Scan(&f.id, &f.path, &f.priorContent, &f.priorHash, &f.existed, &f.afterHash, &f.applied, &f.restored, &mode, &f.forwardMutationID, &f.inverseMutationID, &f.state); err != nil {
				break
			}
			f.afterMode, f.trackedMode = fs.FileMode(mode.Int64), mode.Valid
			files = append(files, f)
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			return evidenceFailure(err, "workspace-schema", store.dbPath, "Required workspace evidence structure is invalid.")
		}
		if len(files) == 0 {
			break
		}
		for _, f := range files {
			cursor = f.id
			unconfirmed := f.state != checkpointCompleted
			uncertainAfter := int64(0)
			if !f.forwardMutationID.Valid {
				if f.inverseMutationID.Valid {
					return fail("violation", "receipt-binding", f.path, "Receipt identity or lineage does not bind retained evidence.")
				}
				unconfirmed = true
			} else {
				forward, err := store.loadReceipt(ctx, f.forwardMutationID.String)
				if err != nil {
					return evidenceFailure(err, "receipt-binding", f.forwardMutationID.String, "Receipt identity or lineage does not bind retained evidence.")
				}
				intent, err := authenticateCheckpointReceipt(ctx, verifier, forward)
				if err != nil || store.bindForwardReceipt(f.checkpointFile, intent) != nil {
					return evidenceFailure(err, "receipt-binding", f.forwardMutationID.String, "Receipt identity or lineage does not bind retained evidence.")
				}
				if f.existed && agenttools.ContentHash(f.priorContent) != intent.Body.BeforeHash {
					return fail("violation", "checkpoint-content", f.forwardMutationID.String, "Retained checkpoint content differs from authenticated evidence.")
				}
				unconfirmed = unconfirmed || forward.applied == nil || f.restored && !completed[f.forwardMutationID.String]
				uncertainAfter = forward.sequence
				if f.inverseMutationID.Valid {
					inverse, err := store.loadReceipt(ctx, f.inverseMutationID.String)
					if err != nil {
						return evidenceFailure(err, "receipt-binding", f.inverseMutationID.String, "Receipt identity or lineage does not bind retained evidence.")
					}
					reverse, err := authenticateCheckpointReceipt(ctx, verifier, inverse)
					if err != nil || bindInverseReceipt(intent, reverse) != nil {
						return evidenceFailure(err, "receipt-binding", f.inverseMutationID.String, "Receipt identity or lineage does not bind retained evidence.")
					}
					unconfirmed = unconfirmed || inverse.applied == nil
					if inverse.applied == nil {
						uncertainAfter = inverse.sequence
					}
				}
				// Mutable undo bookkeeping has no ordering point of its own.
				if (f.restored || f.state == checkpointUndoing) && !completed[f.forwardMutationID.String] && !f.inverseMutationID.Valid {
					uncertainAfter = 0
				}
			}
			if unconfirmed {
				incomplete("checkpoint-unconfirmed", f.path, "Retained checkpoint coverage is unsigned or unconfirmed.")
				state := expected[f.path]
				if uncertainAfter == 0 || state.sequence <= uncertainAfter {
					state.uncertain = true
					expected[f.path] = state
				}
			}
		}
	}
	paths := make([]string, 0, len(expected))
	for path, state := range expected {
		if !state.uncertain {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		state, err := readStable(root, path, nil)
		if err != nil {
			return fail("incomplete", "workspace-unavailable", path, "Workspace state could not be observed consistently and safely.")
		}
		result.paths++
		want := expected[path].state
		if !want.equal(state) || want.modeKnown && want.mode != state.mode {
			return fail("violation", "workspace-drift", path, "Current workspace state differs from authenticated evidence.")
		}
	}
	return result
}

// The current producer stores only private PEM. Consume that existing format,
// immediately derive its public verifier, and retain no signing capability.
func loadAuditWorkspaceVerifier(path string) (signing.Verifier, error) {
	signer, err := signing.LoadEd25519(path)
	if err != nil {
		return nil, err
	}
	return signer.Verifier(), nil
}

// Two contained observations detect ordinary races, not an atomic snapshot.
func readWorkspaceFileStable(root, path string, afterObservation func()) (fileState, error) {
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		return fileState{}, err
	}
	observe := func() (fileState, error) {
		if afterObservation != nil {
			defer afterObservation()
		}
		content, mode, err := ws.ReadFileWithModeForUndo(path)
		if errors.Is(err, os.ErrNotExist) {
			return fileState{absent: true}, nil
		}
		if err != nil {
			// The workspace helper refuses nonregular paths without exporting its
			// sentinel. Inspect only types through a contained root, never their bytes.
			anchored, openErr := os.OpenRoot(root)
			if openErr != nil {
				return fileState{}, err
			}
			defer func() { _ = anchored.Close() }()
			prefix := ""
			parts := strings.Split(path, "/")
			for i, part := range parts {
				prefix = filepath.Join(prefix, part)
				info, statErr := anchored.Lstat(prefix)
				if statErr != nil {
					return fileState{}, err
				}
				if info.Mode()&os.ModeSymlink != 0 || i < len(parts)-1 && !info.IsDir() || i == len(parts)-1 && !info.Mode().IsRegular() {
					return fileState{hash: "nonregular:" + prefix, mode: info.Mode(), modeKnown: true}, nil
				}
			}
			return fileState{}, err
		}
		return fileState{hash: agenttools.ContentHash(content), mode: mode, modeKnown: true}, nil
	}
	first, err := observe()
	if err != nil {
		return fileState{}, err
	}
	second, err := observe()
	if err != nil {
		return fileState{}, err
	}
	if first != second {
		return fileState{}, errors.New("workspace changed between observations")
	}
	return second, nil
}
