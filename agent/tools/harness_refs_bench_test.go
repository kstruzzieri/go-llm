package tools

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// benchWorkspace opens the benchmark root: GO_LLM_HARNESS_BENCH_ROOT, or the
// repository root. Default seams: the real ancestors are scanned.
func benchWorkspace(b *testing.B) *Workspace {
	b.Helper()
	root := os.Getenv("GO_LLM_HARNESS_BENCH_ROOT")
	if root == "" {
		root = "../.."
	}
	w, err := NewWorkspace(root)
	if err != nil {
		b.Fatal(err)
	}
	return w
}

// BenchmarkHarnessReferences times one uncached reference-set build (#784,
// Goal 7: under 50 ms per Plan).
func BenchmarkHarnessReferences(b *testing.B) {
	w := benchWorkspace(b)
	ctx := context.Background()
	var refs *harnessRefs
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		refs = w.harnessReferences(ctx)
	}
	b.StopTimer()
	if !refs.complete {
		b.Fatalf("reference set incomplete: %s", refs.cause)
	}
	b.ReportMetric(float64(refs.entries), "entries")
}

// BenchmarkHarnessPromotionCheck times a 50-path promotion check, which
// builds one fresh reference set for the batch.
func BenchmarkHarnessPromotionCheck(b *testing.B) {
	w := benchWorkspace(b)
	ctx := context.Background()
	paths := make([]string, 50)
	for i := range paths {
		paths[i] = fmt.Sprintf("bench_out/file%02d.txt", i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := w.CheckHarnessPromotion(ctx, paths); err != nil {
			b.Fatal(err)
		}
	}
}
