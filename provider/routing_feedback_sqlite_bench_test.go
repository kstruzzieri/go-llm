package provider

import (
	"context"
	"testing"
	"time"
)

// BenchmarkSQLiteFeedbackStoreRecordBatch measures the synchronous
// write-path latency the PR2 carry-in feedbackWriteTimeout (currently
// 1s) needs to bound.
//
// LIMITATIONS — read these before treating the numbers as a ceiling:
//   - Uses ":memory:" SQLite. File-backed deployments incur fsync,
//     journal-mode transitions, and busy-timeout effects that this
//     baseline does NOT measure. PR5+ may re-run against a file-backed
//     fixture if a deployment surfaces contention.
//   - Single-key fixture. Production has many distinct
//     (provider, model, use_case) keys; per-key indexes don't see the
//     same hot row. Retention pressure on a single key over-exercises
//     the DELETE path relative to a balanced workload.
//   - 8-item batch size mirrors PR2's per-attempt decomposition
//     (success + up-to-N fallback failures); production batches are
//     typically smaller (1-3 items).
//
// Per-iteration At timestamps and per-iteration unique keys would help
// retention realism but add overhead that biases the latency number.
// We trade realism for a stable upper bound here; the realism gap is
// called out explicitly so the timeout decision can be revisited
// against representative production traces.
func BenchmarkSQLiteFeedbackStoreRecordBatch(b *testing.B) {
	store, err := OpenSQLiteFeedbackStore(context.Background(), ":memory:", SQLiteFeedbackStoreConfig{
		MaxRetainedSamples: 1000,
	})
	if err != nil {
		b.Fatalf("OpenSQLiteFeedbackStore: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	k := FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}
	items := make([]FeedbackItem, 8)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Stamp At freshly per iteration so retention's
		// ORDER BY at_ns DESC, id DESC actually orders rows by time and
		// the trim hits a real ordering — not the degenerate
		// "every row shares one at_ns" case.
		now := time.Now()
		for j := range items {
			items[j] = FeedbackItem{Key: k, Signal: FeedbackSignal{Kind: RoutingSignalSuccess, At: now.Add(time.Duration(j) * time.Microsecond)}}
		}
		if err := store.RecordBatch(context.Background(), items); err != nil {
			b.Fatalf("RecordBatch: %v", err)
		}
	}
}

// BenchmarkSQLiteFeedbackStoreGet measures the read-path latency that
// the routing-hot-path pays per scoring decision when feedback is on.
// Same ":memory:" caveat as RecordBatch above. Seeds the cap with
// distinct timestamps so the aggregation loop walks an ordered range.
func BenchmarkSQLiteFeedbackStoreGet(b *testing.B) {
	store, err := OpenSQLiteFeedbackStore(context.Background(), ":memory:", SQLiteFeedbackStoreConfig{
		MaxRetainedSamples: 1000,
	})
	if err != nil {
		b.Fatalf("OpenSQLiteFeedbackStore: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	k := FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}
	seed := make([]FeedbackItem, 50)
	base := time.Now()
	for i := range seed {
		seed[i] = FeedbackItem{
			Key:    k,
			Signal: FeedbackSignal{Kind: RoutingSignalSuccess, At: base.Add(time.Duration(i) * time.Millisecond)},
		}
	}
	if err := store.RecordBatch(context.Background(), seed); err != nil {
		b.Fatalf("seed RecordBatch: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.Get(context.Background(), k); err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}
