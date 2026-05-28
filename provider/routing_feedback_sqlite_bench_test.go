package provider

import (
	"context"
	"testing"
	"time"
)

func BenchmarkSQLiteFeedbackStoreRecordBatch(b *testing.B) {
	store, err := OpenSQLiteFeedbackStore(context.Background(), ":memory:", SQLiteFeedbackStoreConfig{
		MaxRetainedSamples: 1000,
	})
	if err != nil {
		b.Fatalf("OpenSQLiteFeedbackStore: %v", err)
	}
	defer store.Close()

	k := FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}
	items := make([]FeedbackItem, 8)
	for i := range items {
		items[i] = FeedbackItem{Key: k, Signal: FeedbackSignal{Kind: RoutingSignalSuccess, At: time.Now()}}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := store.RecordBatch(context.Background(), items); err != nil {
			b.Fatalf("RecordBatch: %v", err)
		}
	}
}

func BenchmarkSQLiteFeedbackStoreGet(b *testing.B) {
	store, err := OpenSQLiteFeedbackStore(context.Background(), ":memory:", SQLiteFeedbackStoreConfig{
		MaxRetainedSamples: 1000,
	})
	if err != nil {
		b.Fatalf("OpenSQLiteFeedbackStore: %v", err)
	}
	defer store.Close()

	k := FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}
	items := make([]FeedbackItem, 50)
	for i := range items {
		items[i] = FeedbackItem{Key: k, Signal: FeedbackSignal{Kind: RoutingSignalSuccess, At: time.Now()}}
	}
	if err := store.RecordBatch(context.Background(), items); err != nil {
		b.Fatalf("seed RecordBatch: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.Get(context.Background(), k); err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}
