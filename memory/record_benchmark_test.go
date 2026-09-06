package memory

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/signing"
)

func BenchmarkRecordSearch50(b *testing.B) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	signer, err := signing.NewEd25519Signer(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
	if err != nil {
		b.Fatal(err)
	}
	ring, err := signing.NewKeyring(signer.Verifier())
	if err != nil {
		b.Fatal(err)
	}
	store, err := NewMemoryRecordStore(ctx, db, RecordStoreConfig{Signer: signer, Verifiers: ring, Initialize: true})
	if err != nil {
		b.Fatal(err)
	}
	content := strings.Repeat("bounded search memory record ", 150)[:4096]
	for range 50 {
		if _, err := store.Create(ctx, CreateRecordParams{
			Kind: KindSemantic, Content: content, WorkspaceID: "benchmark",
		}); err != nil {
			b.Fatal(err)
		}
	}
	opts := RecordSearchOptions{WorkspaceID: "benchmark", Limit: 50}
	b.ReportAllocs()
	b.SetBytes(50 * 4096)
	b.ResetTimer()
	for range b.N {
		records, err := store.Search(ctx, "bounded", opts)
		if err != nil || len(records) != 50 {
			b.Fatalf("search returned %d records: %v", len(records), err)
		}
	}
}
