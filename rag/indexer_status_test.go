package rag

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var errTestEmbed = errors.New("test embed failure")

// stubEmbedder returns a fixed-dim vector per input with a stable vector space.
func stubEmbedder(vsid string) Embedder {
	return EmbedderFunc(func(_ context.Context, model string, inputs []string) (EmbedResult, error) {
		vecs := make([][]float64, len(inputs))
		for i := range vecs {
			vecs[i] = []float64{1, 0, 0}
		}
		return EmbedResult{Embeddings: vecs, Model: "m", Provider: "p", VectorSpaceID: vsid}, nil
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIndexDirectoryWithStatus_Counts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package a\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(dir, "b.md"), "# B\n\nsome text\n")
	writeFile(t, filepath.Join(dir, "skip.bin"), "binary-ish")              // excluded by extension
	writeFile(t, filepath.Join(dir, "node_modules", "c.go"), "package c\n") // excluded dir

	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	idx, err := NewIndexerWithEmbedder(stubEmbedder("p/m"), store, WithEmbeddingModel("p/m"))
	if err != nil {
		t.Fatal(err)
	}

	st, err := idx.IndexDirectoryWithStatus(context.Background(), dir, WithExclude("node_modules"))
	if err != nil {
		t.Fatalf("IndexDirectoryWithStatus: %v", err)
	}
	if st.TotalFiles != 2 {
		t.Errorf("TotalFiles = %d, want 2 (a.go, b.md)", st.TotalFiles)
	}
	if st.IndexedFiles != 2 {
		t.Errorf("IndexedFiles = %d, want 2", st.IndexedFiles)
	}
	if len(st.Errors) != 0 {
		t.Errorf("Errors = %v, want none", st.Errors)
	}
	if st.InProgress {
		t.Error("InProgress = true, want false in final snapshot")
	}
}

func TestIndexDirectoryWithStatus_PartialErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok.go"), "package a\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(dir, "bad.go"), "package a\n\nfunc B() {}\n")

	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	// Embedder errors only for the file whose content contains "func B".
	emb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		for _, in := range inputs {
			if strings.Contains(in, "func B") {
				return EmbedResult{}, errTestEmbed
			}
		}
		vecs := make([][]float64, len(inputs))
		for i := range vecs {
			vecs[i] = []float64{1, 0, 0}
		}
		return EmbedResult{Embeddings: vecs, Model: "m", Provider: "p", VectorSpaceID: "p/m"}, nil
	})
	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("p/m"))
	if err != nil {
		t.Fatal(err)
	}

	st, err := idx.IndexDirectoryWithStatus(context.Background(), dir)
	if err == nil {
		t.Fatal("want aggregate error for partial failure")
	}
	if st.TotalFiles != 2 {
		t.Errorf("TotalFiles = %d, want 2", st.TotalFiles)
	}
	if st.IndexedFiles != 1 {
		t.Errorf("IndexedFiles = %d, want 1", st.IndexedFiles)
	}
	if len(st.Errors) != 1 {
		t.Errorf("Errors = %v, want exactly 1", st.Errors)
	}
}

func TestIndexDirectoryPolicyOutcomes(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		t.Run(fmt.Sprint(incremental), func(t *testing.T) {
			idx, store, _ := policyTestIndexer(t, WithSensitiveRedaction(SensitiveOpenAIToken))
			dir := t.TempDir()
			redacted := filepath.Join(dir, "a.md")
			skipped := filepath.Join(dir, "b."+policyTestSecret()+".md")
			writeFile(t, redacted, policyTestSecret())
			writeFile(t, skipped, "45320198"+"7654321"+"5")
			writeFile(t, filepath.Join(dir, "c.md"), "safe text")
			seedPolicySource(t, idx, store, skipped, "old sensitive content")
			opts := []IndexDirOption{WithConcurrency(3)}
			if incremental {
				opts = append(opts, WithIncremental())
			}
			want := []IndexPolicyOutcome{
				{Path: redacted, Action: IndexPolicyRedact, Kinds: []SensitiveKind{SensitiveOpenAIToken}},
				{Path: skipped, Action: IndexPolicySkip, Kinds: []SensitiveKind{SensitivePaymentCard}},
			}
			for range 2 {
				status, err := idx.IndexDirectoryWithStatus(context.Background(), dir, opts...)
				if !IsSafeIndexSkip(err) || status.TotalFiles != 3 || status.IndexedFiles != 2 || status.SkippedFiles != 0 || len(status.Errors) != 0 || status.InProgress {
					t.Fatal("directory lost safe-skip classification or policy counts")
				}
				if !reflect.DeepEqual(status.PolicyOutcomes, want) {
					t.Fatal("directory outcomes missing or not sorted, including nil-error redaction")
				}
				var policy *IndexPolicyError
				if !errors.As(err, &policy) || policy.Outcome.Path != skipped {
					t.Fatal("joined directory error lost typed skip")
				}
				if strings.Contains(err.Error(), policyTestSecret()) {
					t.Fatal("directory policy diagnostic disclosed raw path content")
				}
				requirePolicySourceAbsent(t, store, skipped)
			}
		})
	}
}

func TestIndexDirectoryPolicySoleSource(t *testing.T) {
	idx, store, calls := policyTestIndexer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "source.md")
	writeFile(t, path, policyTestSecret())
	seedPolicySource(t, idx, store, path, "old indexed source")
	status, err := idx.IndexDirectoryWithStatus(context.Background(), dir)
	if !IsSafeIndexSkip(err) || status.TotalFiles != 1 || status.IndexedFiles != 0 || status.SkippedFiles != 0 || len(status.Errors) != 0 || len(status.PolicyOutcomes) != 1 || calls.Load() != 0 {
		t.Fatal("sole skipped source must yield an empty safe corpus with a policy outcome")
	}
	requirePolicySourceAbsent(t, store, path)
}

func TestIndexDirectoryPolicyMixedErrors(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		for _, failureFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("unsafe=%t/first=%t", unsafe, failureFirst), func(t *testing.T) {
				idx, inner, _ := policyTestIndexer(t)
				dir := t.TempDir()
				skipPath, failurePath := filepath.Join(dir, "a.md"), filepath.Join(dir, "z.md")
				if failureFirst {
					skipPath, failurePath = failurePath, skipPath
				}
				writeFile(t, skipPath, policyTestSecret())
				cause := errors.New("ordinary failure")
				if unsafe {
					writeFile(t, failurePath, policyTestSecret())
					cause = errors.New("store rejected " + policyTestSecret())
					idx.store = &policyTestStore{SQLiteStore: inner, clearErr: cause, failPath: failurePath}
				} else {
					writeFile(t, failurePath, "ordinary failure content")
					idx.embedder = EmbedderFunc(func(context.Context, string, []string) (EmbedResult, error) { return EmbedResult{}, cause })
				}
				status, err := idx.IndexDirectoryWithStatus(context.Background(), dir, WithConcurrency(1))
				if IsSafeIndexSkip(err) || !errors.Is(err, cause) || len(status.Errors) != 1 || status.TotalFiles != 2 || status.IndexedFiles != 0 || status.SkippedFiles != 0 {
					t.Fatal("mixed error tree hid an ordinary or unsafe failure")
				}
				var policy *IndexPolicyError
				if !errors.As(err, &policy) || strings.Contains(err.Error(), policyTestSecret()) {
					t.Fatal("mixed error tree lost policy identity or disclosed policy cause")
				}
				wantOutcomes := 1
				if unsafe {
					wantOutcomes = 2
				}
				if len(status.PolicyOutcomes) != wantOutcomes {
					t.Fatal("mixed error status lost policy outcomes")
				}
			})
		}
	}
}

func TestIndexDirectoryPolicyCancellationRetainsDetection(t *testing.T) {
	idx, inner, calls := policyTestIndexer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	writeFile(t, path, policyTestSecret())
	writeFile(t, filepath.Join(dir, "z.md"), "not started")
	seedPolicySource(t, idx, inner, path, "old source")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idx.store = &policyTestStore{SQLiteStore: inner, afterClear: cancel}
	status, err := idx.IndexDirectoryWithStatus(ctx, dir, WithConcurrency(1))
	var policy *IndexPolicyError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &policy) || IsSafeIndexSkip(err) {
		t.Fatal("cancellation erased detection or was misclassified as a safe skip")
	}
	if len(status.PolicyOutcomes) != 1 || status.PolicyOutcomes[0].Unsafe || status.IndexedFiles != 0 || status.SkippedFiles != 1 || len(status.Errors) != 0 || calls.Load() != 0 {
		t.Fatal("cancellation lost policy state or changed cancellation-only skip counts")
	}
	requirePolicySourceAbsent(t, inner, path)
}

func TestIndexDirectoryPolicyPruneFailureRetainsCause(t *testing.T) {
	idx, inner, _ := policyTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "source.md"), policyTestSecret())
	cause := errors.New("prune inventory unavailable")
	idx.store = &policyTestStore{SQLiteStore: inner, listErr: cause}
	status, err := idx.IndexDirectoryWithStatus(context.Background(), dir, WithPruneDeleted())
	if !errors.Is(err, cause) || IsSafeIndexSkip(err) || len(status.Errors) != 1 || len(status.PolicyOutcomes) != 1 {
		t.Fatal("directory aggregation flattened a prune cause or lost its policy outcome")
	}
}
