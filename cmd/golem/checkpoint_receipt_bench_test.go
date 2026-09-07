package main

import (
	"context"
	"crypto/rand"
	"strconv"
	"testing"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/signing"
)

// BenchmarkCheckpointReceiptHistory measures startup and the shared list/undo
// authentication pass over retained Ed25519 evidence after snapshot pruning.
// Half the mutations are inverses. The smallest fixture shares a scan page with
// its originals; larger fixtures reference originals in earlier pages.
func BenchmarkCheckpointReceiptHistory(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			ctx := context.Background()
			root, data := b.TempDir(), b.TempDir()
			getenv := testGetenv(data)
			store, err := openCheckpointStore(ctx, getenv, root)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = store.Close() })
			signer, verifier, _, err := loadMutationSigning(ctx, getenv, root, store)
			if err != nil {
				b.Fatal(err)
			}
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			stmt, err := tx.PrepareContext(ctx, `INSERT INTO mutation_receipts (mutation_id, intent_json, applied_json) VALUES (?, ?, ?)`)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = stmt.Close() }()
			forwards := make([]string, count/2)
			for i := range count {
				body := agenttools.MutationReceiptBody{
					Kind: "intent", MutationID: rand.Text(), WorkspaceHash: store.workspaceHash,
					Path: "file.txt", BeforeHash: agenttools.ContentHash([]byte("before")),
					AfterHash: agenttools.ContentHash([]byte("after")),
					Timestamp: "2026-09-06T12:00:00Z", AgentID: signer.KeyID(),
				}
				if i < len(forwards) {
					forwards[i] = body.MutationID
				} else {
					body.UndoOf = forwards[i-len(forwards)]
					body.BeforeHash, body.AfterHash = body.AfterHash, body.BeforeHash
				}
				var raw [2][]byte
				for phase, kind := range []string{"intent", "applied"} {
					body.Kind = kind
					receipt, err := agenttools.SignMutationReceipt(ctx, signer, body)
					if err != nil {
						b.Fatal(err)
					}
					raw[phase], err = signing.MarshalCanonical(receipt)
					if err != nil {
						b.Fatal(err)
					}
				}
				if _, err := stmt.ExecContext(ctx, body.MutationID, string(raw[0]), string(raw[1])); err != nil {
					b.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			journal := newCheckpointJournal(nil, store, signer, verifier)
			b.Run("startup", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, _, _, err := loadMutationSigning(ctx, getenv, root, store); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("evidence", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := journal.checkpointEvidenceFor(ctx, nil); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
