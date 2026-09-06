package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/signing"
)

func TestMutationSigningIdentityLifecycle(t *testing.T) {
	root, data := t.TempDir(), filepath.Join(t.TempDir(), "data\nforged\x1b")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := openCheckpointStore(ctx, testGetenv(data), root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	signer, verifier, notice, err := loadMutationSigning(ctx, testGetenv(data), root, s)
	if err != nil {
		t.Fatal(err)
	}
	if signer.KeyID() != verifier.KeyID() || signer.Algorithm() != signing.AlgEd25519 {
		t.Fatalf("identity binding/notice = %q", notice)
	}
	path := filepath.Join(data, "golem", "signing", "agent-ed25519.pem")
	escapedPath := strings.NewReplacer("\n", "\\n", "\x1b", "\\x1b").Replace(path)
	private, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertNoPrivateKeyDiagnostic(t, notice, private)
	if want := "new signing identity: kid " + signer.KeyID() + " (key file " + escapedPath + ")"; notice != want {
		t.Fatalf("new identity notice = %q, want %q", notice, want)
	}
	for _, p := range []string{filepath.Dir(path), path} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0600)
		if info.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Fatalf("mode = %o", info.Mode().Perm())
		}
	}
	next, _, notice, err := loadMutationSigning(ctx, testGetenv(data), root, s)
	if err != nil || next.KeyID() != signer.KeyID() || notice != "" {
		t.Fatalf("reload identity/notice: %q, %v", notice, err)
	}
	if reloaded, err := os.ReadFile(path); err != nil || !bytes.Equal(private, reloaded) {
		t.Fatal("reload overwrote the signing key")
	}
	rec, body, _ := storeForward(t, s, strings.Repeat("A", 26), "old")
	body.AgentID = signer.KeyID()
	// Multiple mismatches must name the first retained claim in scan order.
	for _, id := range []string{body.MutationID, strings.Repeat("B", 26)} {
		nextBody := body
		nextBody.MutationID = id
		intent, err := agenttools.SignMutationReceipt(ctx, signer, nextBody)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := signing.MarshalCanonical(intent)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.prepareSignedIntent(ctx, "history", testNow, rec, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	missingSigner, missingVerifier, missingNotice, err := loadMutationSigning(ctx, testGetenv(data), root, s)
	if err != nil {
		assertNoPrivateKeyDiagnostic(t, err.Error(), private)
	}
	wantMissing := "golem: signing key missing: key file " + escapedPath + " is required by receipt " + body.MutationID + " claiming kid " + signer.KeyID() + "; restore the matching key from backup; writes disabled to preserve receipt integrity"
	if err == nil || err.Error() != wantMissing || missingSigner != nil || missingVerifier != nil || missingNotice != "" {
		t.Fatalf("missing historical identity: %v, want %q and no identity/notice", err, wantMissing)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("missing historical key was recreated")
	}
	other, _, err := signing.LoadOrCreateEd25519(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = loadMutationSigning(ctx, testGetenv(data), root, s)
	if err != nil {
		assertNoPrivateKeyDiagnostic(t, err.Error(), private)
		assertNoPrivateKeyDiagnostic(t, err.Error(), before)
	}
	want := "golem: signing key mismatch: receipt " + body.MutationID + " names kid " + signer.KeyID() + ", but key file " + escapedPath + " has kid " + other.KeyID() + "; writes disabled to preserve receipt integrity"
	if err == nil || err.Error() != want || strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), "\nforged") {
		t.Fatalf("mismatch diagnostic = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("mismatched key was overwritten")
	}
}

func TestMutationSigningRefusesInvalidHistoryBeforeCreatingKey(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	s, err := openCheckpointStore(context.Background(), testGetenv(data), root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkpointSQL(t, s.db, `INSERT INTO mutation_receipts (mutation_id,intent_json) VALUES (?, '{}')`, "unchecked\ncontrol\x1b")
	_, _, _, err = loadMutationSigning(context.Background(), testGetenv(data), root, s)
	if err == nil || err.Error() != "golem: mutation receipt history invalid or unavailable; writes disabled" {
		t.Fatalf("invalid history = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(data, "golem", "signing", "agent-ed25519.pem")); !os.IsNotExist(err) {
		t.Fatal("created key before rejecting invalid history")
	}
}

func TestMutationSigningRefusesUnsafePaths(t *testing.T) {
	for _, kind := range []string{"inside", "directory-mode", "file-mode", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root, data := t.TempDir(), t.TempDir()
			s, err := openCheckpointStore(context.Background(), testGetenv(data), root)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			key := filepath.Join(data, "golem", "signing", "agent-ed25519.pem")
			if kind == "inside" {
				data = root
			} else {
				if _, _, err := signing.LoadOrCreateEd25519(key); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "directory-mode":
					err = os.Chmod(filepath.Dir(key), 0755)
				case "file-mode":
					err = os.Chmod(key, 0644)
				case "symlink":
					err = os.Rename(key, key+".real")
					if err == nil {
						err = os.Symlink(key+".real", key)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, _, _, err := loadMutationSigning(context.Background(), testGetenv(data), root, s); err == nil {
				t.Fatal("unsafe signing path accepted")
			}
		})
	}
}

func TestMutationSigningPreservesCancellation(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	s, err := openCheckpointStore(context.Background(), testGetenv(data), root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err = loadMutationSigning(ctx, testGetenv(data), root, s)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost initialization cancellation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "golem", "signing", "agent-ed25519.pem")); !os.IsNotExist(err) {
		t.Fatal("canceled initialization created key")
	}
}

func TestMutationSigningRejectsCorruptRetainedEvidence(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	ctx := context.Background()
	s, err := openCheckpointStore(ctx, testGetenv(data), root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	signer, _, _, err := loadMutationSigning(ctx, testGetenv(data), root, s)
	if err != nil {
		t.Fatal(err)
	}
	_, body, _ := storeForward(t, s, strings.Repeat("A", 26), "old")
	body.AgentID = signer.KeyID()
	receipt, err := agenttools.SignMutationReceipt(ctx, signer, body)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Signature.Bytes[0] ^= 1
	raw, err := signing.MarshalCanonical(receipt)
	if err != nil {
		t.Fatal(err)
	}
	checkpointSQL(t, s.db, `INSERT INTO mutation_receipts (mutation_id,intent_json) VALUES (?,?)`, body.MutationID, string(raw))
	_, _, _, err = loadMutationSigning(ctx, testGetenv(data), root, s)
	if err == nil || !errors.Is(err, signing.ErrInvalidSignature) {
		t.Fatalf("invalid retained evidence not authenticated or lost cause: %v", err)
	}
}

// Check the actual temp key's encoded material without ever printing it on failure.
func assertNoPrivateKeyDiagnostic(t *testing.T, diagnostic string, private []byte) {
	t.Helper()
	if strings.Contains(diagnostic, "PRIVATE KEY") {
		t.Fatal("diagnostic exposed a private-key marker")
	}
	for _, line := range strings.Split(string(private), "\n") {
		if line != "" && !strings.HasPrefix(line, "-----") && strings.Contains(diagnostic, line) {
			t.Fatal("diagnostic exposed private-key material")
		}
	}
}

// Reopen after the host releases its lease: these are durable records verified
// against the persistent Ed25519 identity, not a test journal's signer.
func readHostMutationReceipts(t *testing.T, data, root string) []agenttools.MutationReceipt {
	t.Helper()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openCheckpointStore(context.Background(), testGetenv(data), root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	signer, err := signing.LoadEd25519(filepath.Join(data, "golem", "signing", "agent-ed25519.pem"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.scanReceipts(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var receipts []agenttools.MutationReceipt
	for _, entry := range entries {
		for i, raw := range [][]byte{entry.intentJSON, entry.appliedJSON} {
			receipt, err := agenttools.DecodeMutationReceipt(raw)
			if err != nil {
				t.Fatalf("persisted receipt phase %d: %v", i, err)
			}
			if err := agenttools.VerifyMutationReceipt(context.Background(), signer.Verifier(), receipt); err != nil {
				t.Fatalf("persisted receipt phase %d signature: %v", i, err)
			}
			if receipt.Signature.Alg != signing.AlgEd25519 || receipt.Body.AgentID != signer.KeyID() ||
				receipt.Body.WorkspaceHash != fmt.Sprintf("%x", sha256.Sum256([]byte(root))) {
				t.Fatalf("persisted receipt phase %d has wrong host identity: %+v", i, receipt.Body)
			}
			if i == 1 {
				receipts = append(receipts, receipt)
			}
		}
	}
	return receipts
}
