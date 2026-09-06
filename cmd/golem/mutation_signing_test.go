package main

import (
	"bytes"
	"context"
	"errors"
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
	if signer.KeyID() != verifier.KeyID() || signer.Algorithm() != signing.AlgEd25519 || !strings.Contains(notice, "new signing identity") {
		t.Fatalf("identity binding/notice = %q", notice)
	}
	path := filepath.Join(data, "golem", "signing", "agent-ed25519.pem")
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
	rec, body, _ := storeForward(t, s, strings.Repeat("A", 26), "old")
	body.AgentID = signer.KeyID()
	intent, err := agenttools.SignMutationReceipt(ctx, signer, body)
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
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = loadMutationSigning(ctx, testGetenv(data), root, s)
	if err == nil || !strings.Contains(err.Error(), signer.KeyID()) || !strings.Contains(err.Error(), "restore the matching key from backup") {
		t.Fatalf("missing historical identity: %v", err)
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
	want := "golem: signing key mismatch: receipt " + body.MutationID + " names kid " + signer.KeyID() + ", but key file " + strings.NewReplacer("\n", "\\n", "\x1b", "\\x1b").Replace(path) + " has kid " + other.KeyID() + "; writes disabled to preserve receipt integrity"
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
	checkpointSQL(t, s.db, `INSERT INTO mutation_receipts (mutation_id,intent_json) VALUES ('unchecked-control', '{}')`)
	_, _, _, err = loadMutationSigning(context.Background(), testGetenv(data), root, s)
	if err == nil || !strings.Contains(err.Error(), "receipt history invalid") || strings.Contains(err.Error(), "unchecked-control") {
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
