package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/signing"
)

// loadMutationSigning initializes one write-enabled runtime. Current-workspace
// history requires the existing key; it can never silently create a replacement.
func loadMutationSigning(ctx context.Context, getenv func(string) string, root string, store *checkpointStore) (signing.Signer, signing.Verifier, string, error) {
	base, err := dataDirBase(getenv)
	if err != nil {
		return nil, nil, "", err
	}
	keyPath := filepath.Join(base, "golem", "signing", "agent-ed25519.pem")
	if err := validatePathOutsideWorkspace(keyPath, root); err != nil {
		return nil, nil, "", fmt.Errorf("golem: signing key path: %w", checkpointDisplayError{cause: err})
	}
	entries, err := store.scanReceipts(ctx, 0, 1)
	if err != nil {
		return nil, nil, "", mutationHistoryError{cause: err}
	}
	var signer *signing.Ed25519Signer
	created := false
	if len(entries) > 0 {
		signer, err = signing.LoadEd25519(keyPath)
		if errors.Is(err, os.ErrNotExist) {
			intent, _ := decodeStoredMutationReceipt(entries[0].intentJSON) // scan validated structure, not the claim
			return nil, nil, "", fmt.Errorf("golem: signing key missing: key file %s is required by receipt %s claiming kid %s; restore the matching key from backup; writes disabled to preserve receipt integrity", checkpointDisplayText(keyPath), intent.Body.MutationID, intent.Body.AgentID)
		}
	} else {
		signer, created, err = signing.LoadOrCreateEd25519(keyPath)
	}
	if err != nil {
		return nil, nil, "", fmt.Errorf("golem: signing key: %w", checkpointDisplayError{cause: err})
	}
	verifier := signer.Verifier()
	// Bounded pages include retained evidence after snapshot pruning and undo.
	for cursor := int64(0); ; {
		entries, err := store.scanReceipts(ctx, cursor, 100)
		if err != nil {
			return nil, nil, "", mutationHistoryError{cause: err}
		}
		if len(entries) == 0 {
			break
		}
		for _, entry := range entries {
			intent, _ := decodeStoredMutationReceipt(entry.intentJSON)
			if intent.Body.AgentID != signer.KeyID() {
				return nil, nil, "", fmt.Errorf("golem: signing key mismatch: receipt %s names kid %s, but key file %s has kid %s; writes disabled to preserve receipt integrity", intent.Body.MutationID, intent.Body.AgentID, checkpointDisplayText(keyPath), signer.KeyID())
			}
			if _, err := authenticateCheckpointReceipt(ctx, verifier, entry); err != nil {
				return nil, nil, "", mutationHistoryError{cause: err}
			}
			if intent.Body.WorkspaceHash != store.workspaceHash {
				return nil, nil, "", mutationHistoryError{cause: errors.New("workspace identity mismatch")}
			}
			cursor = entry.sequence
		}
	}
	notice := ""
	if created {
		notice = fmt.Sprintf("new signing identity: kid %s (key file %s)", signer.KeyID(), checkpointDisplayText(keyPath))
	}
	return signer, verifier, notice, nil
}

// authenticateCheckpointReceipt authenticates both phases before callers rely
// on any claimed transition. Store reads already enforce canonical bytes/pairing.
func authenticateCheckpointReceipt(ctx context.Context, verifier signing.Verifier, entry checkpointReceipt) (agenttools.MutationReceipt, error) {
	if err := validateCheckpointReceipt(entry); err != nil {
		return agenttools.MutationReceipt{}, err
	}
	intent, err := decodeStoredMutationReceipt(entry.intentJSON)
	if err != nil {
		return agenttools.MutationReceipt{}, err
	}
	if err := agenttools.VerifyMutationReceipt(ctx, verifier, intent); err != nil {
		return agenttools.MutationReceipt{}, err
	}
	if entry.appliedJSON != nil {
		applied, err := decodeStoredMutationReceipt(entry.appliedJSON)
		if err != nil {
			return agenttools.MutationReceipt{}, err
		}
		if err := agenttools.VerifyMutationReceipt(ctx, verifier, applied); err != nil {
			return agenttools.MutationReceipt{}, err
		}
	}
	return intent, nil
}

// Decoder diagnostics may contain unchecked record bytes. Retain the cause for
// errors.Is/As without echoing it or claiming every storage failure is tampering.
type mutationHistoryError struct{ cause error }

func (e mutationHistoryError) Error() string {
	return "golem: mutation receipt history invalid or unavailable; writes disabled"
}
func (e mutationHistoryError) Unwrap() error { return e.cause }
