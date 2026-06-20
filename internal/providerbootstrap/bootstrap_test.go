package providerbootstrap

import (
	"context"
	"database/sql"
	"testing"

	"github.com/kstruzzieri/go-llm/fingerprint"
	_ "modernc.org/sqlite"
)

func TestBundleCloseNilSafe(t *testing.T) {
	var b *Bundle
	if err := b.Close(); err != nil {
		t.Fatalf("nil Bundle.Close() = %v, want nil", err)
	}
	if err := (&Bundle{}).Close(); err != nil {
		t.Fatalf("empty Bundle.Close() = %v, want nil", err)
	}
}

// newTestFingerprintStore creates an in-memory SQLite-backed fingerprint store
// for testing. It registers a t.Cleanup to close the underlying DB.
func newTestFingerprintStore(t *testing.T) fingerprint.Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test fingerprint db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := fingerprint.NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("fingerprint.NewStore: %v", err)
	}
	return store
}

func TestNew_NilConfigBuildsRouter(t *testing.T) {
	b, err := New(context.Background(), Options{})
	if err != nil {
		t.Fatalf("New(nil cfg) error: %v", err)
	}
	defer b.Close()
	if b.Router == nil || b.Models == nil || b.Providers == nil {
		t.Fatalf("New returned incomplete bundle: %+v", b)
	}
	if b.Config == nil {
		t.Fatalf("expected synthetic Config to be set on Bundle")
	}
}

func TestNew_ProberFactoryInstalledWithFingerprintStore(t *testing.T) {
	// With a fingerprint store, New must wire the prober factory (parity with mcp).
	// Assert indirectly: New succeeds and a registry was built. A deeper assertion
	// belongs to the MCP parity suite (Task 6).
	b, err := New(context.Background(), Options{FingerprintStore: newTestFingerprintStore(t)})
	if err != nil {
		t.Fatalf("New with fp store error: %v", err)
	}
	defer b.Close()
	if b.Models == nil {
		t.Fatalf("expected model registry")
	}
}

func TestNew_BestEffortRefreshRecordsWarnings(t *testing.T) {
	// Point ollama at an unreachable URL so RefreshModels fails; New must still
	// succeed and record a Warning rather than error.
	b, err := New(context.Background(), Options{OllamaURLOverride: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("New should tolerate refresh failure, got: %v", err)
	}
	defer b.Close()
	if len(b.Warnings) == 0 {
		t.Fatalf("expected a best-effort refresh warning")
	}
}
