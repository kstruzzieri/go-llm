package fingerprint

import (
	"context"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	store, err := NewStore(ctx, db)
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}
	return store
}

func testProfile() Profile {
	return Profile{
		BackendID:                 "http://localhost:11434",
		ModelName:                 "qwen2.5:7b",
		ModelDigest:               "abc123",
		ModelKind:                 ModelKindChat,
		Capabilities:              []string{"completion", "tools"},
		IncompleteCapabilities:    []string{},
		KindSource:                "capabilities",
		ProfileVersion:            CurrentProfileVersion,
		TestedAt:                  time.Now().Truncate(time.Millisecond),
		EffectiveContext:          4096,
		ToolCallingRate:           0.85,
		InstructionScore:          0.92,
		GenerationTokensPerSecond: 42.5,
		PromptLatency:             150 * time.Millisecond,
		ColdStartLatency:          3 * time.Second,
		EmbeddingDim:              0,
		EmbeddingCoherence:        -1,
		EmbeddingLatency:          0,
		PeakMemoryMB:              4096,
		GPULayersUsed:             33,
	}
}

func TestStore_SaveAndGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	want := testProfile()
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := store.Get(ctx, want.BackendID, want.ModelName)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}

	// Compare all fields.
	if got.BackendID != want.BackendID {
		t.Errorf("BackendID = %q, want %q", got.BackendID, want.BackendID)
	}
	if got.ModelName != want.ModelName {
		t.Errorf("ModelName = %q, want %q", got.ModelName, want.ModelName)
	}
	if got.ModelDigest != want.ModelDigest {
		t.Errorf("ModelDigest = %q, want %q", got.ModelDigest, want.ModelDigest)
	}
	if got.ModelKind != want.ModelKind {
		t.Errorf("ModelKind = %q, want %q", got.ModelKind, want.ModelKind)
	}
	if len(got.Capabilities) != len(want.Capabilities) {
		t.Errorf("Capabilities len = %d, want %d", len(got.Capabilities), len(want.Capabilities))
	} else {
		for i := range want.Capabilities {
			if got.Capabilities[i] != want.Capabilities[i] {
				t.Errorf("Capabilities[%d] = %q, want %q", i, got.Capabilities[i], want.Capabilities[i])
			}
		}
	}
	if len(got.IncompleteCapabilities) != 0 {
		t.Errorf("IncompleteCapabilities len = %d, want 0", len(got.IncompleteCapabilities))
	}
	if got.KindSource != want.KindSource {
		t.Errorf("KindSource = %q, want %q", got.KindSource, want.KindSource)
	}
	if got.ProfileVersion != want.ProfileVersion {
		t.Errorf("ProfileVersion = %d, want %d", got.ProfileVersion, want.ProfileVersion)
	}
	if !got.TestedAt.Equal(want.TestedAt) {
		t.Errorf("TestedAt = %v, want %v", got.TestedAt, want.TestedAt)
	}
	if got.EffectiveContext != want.EffectiveContext {
		t.Errorf("EffectiveContext = %d, want %d", got.EffectiveContext, want.EffectiveContext)
	}
	if got.ToolCallingRate != want.ToolCallingRate {
		t.Errorf("ToolCallingRate = %f, want %f", got.ToolCallingRate, want.ToolCallingRate)
	}
	if got.InstructionScore != want.InstructionScore {
		t.Errorf("InstructionScore = %f, want %f", got.InstructionScore, want.InstructionScore)
	}
	if got.GenerationTokensPerSecond != want.GenerationTokensPerSecond {
		t.Errorf("GenerationTokensPerSecond = %f, want %f", got.GenerationTokensPerSecond, want.GenerationTokensPerSecond)
	}
	if got.PromptLatency != want.PromptLatency {
		t.Errorf("PromptLatency = %v, want %v", got.PromptLatency, want.PromptLatency)
	}
	if got.ColdStartLatency != want.ColdStartLatency {
		t.Errorf("ColdStartLatency = %v, want %v", got.ColdStartLatency, want.ColdStartLatency)
	}
	if got.EmbeddingDim != want.EmbeddingDim {
		t.Errorf("EmbeddingDim = %d, want %d", got.EmbeddingDim, want.EmbeddingDim)
	}
	if got.EmbeddingCoherence != want.EmbeddingCoherence {
		t.Errorf("EmbeddingCoherence = %f, want %f", got.EmbeddingCoherence, want.EmbeddingCoherence)
	}
	if got.EmbeddingLatency != want.EmbeddingLatency {
		t.Errorf("EmbeddingLatency = %v, want %v", got.EmbeddingLatency, want.EmbeddingLatency)
	}
	if got.PeakMemoryMB != want.PeakMemoryMB {
		t.Errorf("PeakMemoryMB = %d, want %d", got.PeakMemoryMB, want.PeakMemoryMB)
	}
	if got.GPULayersUsed != want.GPULayersUsed {
		t.Errorf("GPULayersUsed = %d, want %d", got.GPULayersUsed, want.GPULayersUsed)
	}
}

func TestStore_Get_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, err := store.Get(ctx, "http://localhost:11434", "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestStore_Save_Upsert(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("first Save() error: %v", err)
	}

	// Update fields and save again.
	p.ModelDigest = "def456"
	p.GenerationTokensPerSecond = 55.0
	p.ToolCallingRate = 0.95
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("second Save() error: %v", err)
	}

	got, err := store.Get(ctx, p.BackendID, p.ModelName)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got.ModelDigest != "def456" {
		t.Errorf("ModelDigest = %q, want %q", got.ModelDigest, "def456")
	}
	if got.GenerationTokensPerSecond != 55.0 {
		t.Errorf("GenerationTokensPerSecond = %f, want 55.0", got.GenerationTokensPerSecond)
	}
	if got.ToolCallingRate != 0.95 {
		t.Errorf("ToolCallingRate = %f, want 0.95", got.ToolCallingRate)
	}
}

func TestStore_NeedsFingerprint_NoRow(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	needs, err := store.NeedsFingerprint(ctx, "http://localhost:11434", "qwen2.5:7b", "abc123")
	if err != nil {
		t.Fatalf("NeedsFingerprint() error: %v", err)
	}
	if !needs {
		t.Error("NeedsFingerprint() = false, want true (no existing profile)")
	}
}

func TestStore_NeedsFingerprint_MatchingDigest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	needs, err := store.NeedsFingerprint(ctx, p.BackendID, p.ModelName, p.ModelDigest)
	if err != nil {
		t.Fatalf("NeedsFingerprint() error: %v", err)
	}
	if needs {
		t.Error("NeedsFingerprint() = true, want false (matching digest, current version)")
	}
}

func TestStore_NeedsFingerprint_ChangedDigest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	needs, err := store.NeedsFingerprint(ctx, p.BackendID, p.ModelName, "new-digest-xyz")
	if err != nil {
		t.Fatalf("NeedsFingerprint() error: %v", err)
	}
	if !needs {
		t.Error("NeedsFingerprint() = false, want true (digest changed)")
	}
}

func TestStore_NeedsFingerprint_VersionUpgrade(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	p.ProfileVersion = CurrentProfileVersion - 1
	if CurrentProfileVersion <= 1 {
		// For testing: manually insert a row with version 0 to simulate old version.
		p.ProfileVersion = 0
	}
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	needs, err := store.NeedsFingerprint(ctx, p.BackendID, p.ModelName, p.ModelDigest)
	if err != nil {
		t.Fatalf("NeedsFingerprint() error: %v", err)
	}
	if !needs {
		t.Error("NeedsFingerprint() = false, want true (profile version below current)")
	}
}

func TestStore_SaveFailure_BackoffRespected(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	backendID := "http://localhost:11434"
	modelName := "qwen2.5:7b"
	digest := "abc123"

	if err := store.SaveFailure(ctx, backendID, modelName, digest, "connection refused"); err != nil {
		t.Fatalf("SaveFailure() error: %v", err)
	}

	needs, err := store.NeedsFingerprint(ctx, backendID, modelName, digest)
	if err != nil {
		t.Fatalf("NeedsFingerprint() error: %v", err)
	}
	if needs {
		t.Error("NeedsFingerprint() = true, want false (in backoff window)")
	}
}

func TestStore_SaveFailure_StaleDigestCleared(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	backendID := "http://localhost:11434"
	modelName := "qwen2.5:7b"

	// Save failure with old digest.
	if err := store.SaveFailure(ctx, backendID, modelName, "old-digest", "model not found"); err != nil {
		t.Fatalf("SaveFailure() error: %v", err)
	}

	// Check with new digest — stale failure should be cleared.
	needs, err := store.NeedsFingerprint(ctx, backendID, modelName, "new-digest")
	if err != nil {
		t.Fatalf("NeedsFingerprint() error: %v", err)
	}
	if !needs {
		t.Error("NeedsFingerprint() = false, want true (stale failure with different digest)")
	}

	// Verify failure row was actually deleted.
	_, err = store.GetFailure(ctx, backendID, modelName)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFailure() error = %v, want ErrNotFound (stale failure should be deleted)", err)
	}
}

func TestStore_SaveFailure_IncrementCount(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	backendID := "http://localhost:11434"
	modelName := "qwen2.5:7b"
	digest := "abc123"

	if err := store.SaveFailure(ctx, backendID, modelName, digest, "error 1"); err != nil {
		t.Fatalf("first SaveFailure() error: %v", err)
	}
	if err := store.SaveFailure(ctx, backendID, modelName, digest, "error 2"); err != nil {
		t.Fatalf("second SaveFailure() error: %v", err)
	}
	if err := store.SaveFailure(ctx, backendID, modelName, digest, "error 3"); err != nil {
		t.Fatalf("third SaveFailure() error: %v", err)
	}

	fi, err := store.GetFailure(ctx, backendID, modelName)
	if err != nil {
		t.Fatalf("GetFailure() error: %v", err)
	}
	if fi.AttemptCount != 3 {
		t.Errorf("AttemptCount = %d, want 3", fi.AttemptCount)
	}
	if fi.LastError != "error 3" {
		t.Errorf("LastError = %q, want %q", fi.LastError, "error 3")
	}
}

func TestStore_GetFailure_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, err := store.GetFailure(ctx, "http://localhost:11434", "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFailure() error = %v, want ErrNotFound", err)
	}
}

func TestStore_GetFailure_Active(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	backendID := "http://localhost:11434"
	modelName := "qwen2.5:7b"
	digest := "abc123"

	if err := store.SaveFailure(ctx, backendID, modelName, digest, "timeout"); err != nil {
		t.Fatalf("SaveFailure() error: %v", err)
	}

	fi, err := store.GetFailure(ctx, backendID, modelName)
	if err != nil {
		t.Fatalf("GetFailure() error: %v", err)
	}

	if fi.ModelDigest != digest {
		t.Errorf("ModelDigest = %q, want %q", fi.ModelDigest, digest)
	}
	if fi.LastError != "timeout" {
		t.Errorf("LastError = %q, want %q", fi.LastError, "timeout")
	}
	if fi.AttemptCount != 1 {
		t.Errorf("AttemptCount = %d, want 1", fi.AttemptCount)
	}

	// RetryAfter should be ~1 hour from now (first attempt: 1h * 2^0 = 1h).
	expectedRetry := fi.AttemptedAt.Add(1 * time.Hour)
	if !fi.RetryAfter.Equal(expectedRetry) {
		t.Errorf("RetryAfter = %v, want %v", fi.RetryAfter, expectedRetry)
	}

	// RetryAfter should be in the future.
	if fi.RetryAfter.Before(time.Now()) {
		t.Error("RetryAfter is in the past, expected it to be in the future")
	}
}

func TestStore_NeedsFingerprint_IncompleteProfile_NoBackoff(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	p.IncompleteCapabilities = []string{"embedding"}
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	needs, err := store.NeedsFingerprint(ctx, p.BackendID, p.ModelName, p.ModelDigest)
	if err != nil {
		t.Fatalf("NeedsFingerprint() error: %v", err)
	}
	if !needs {
		t.Error("NeedsFingerprint() = false, want true (incomplete profile with no backoff)")
	}
}

func TestStore_NeedsFingerprint_IncompleteProfile_ActiveBackoff(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	p := testProfile()
	p.IncompleteCapabilities = []string{"embedding"}
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Also record a failure with matching digest — creates active backoff.
	if err := store.SaveFailure(ctx, p.BackendID, p.ModelName, p.ModelDigest, "embed failed"); err != nil {
		t.Fatalf("SaveFailure() error: %v", err)
	}

	needs, err := store.NeedsFingerprint(ctx, p.BackendID, p.ModelName, p.ModelDigest)
	if err != nil {
		t.Fatalf("NeedsFingerprint() error: %v", err)
	}
	if needs {
		t.Error("NeedsFingerprint() = true, want false (incomplete profile with active backoff)")
	}
}

func TestStore_Save_Incomplete_PreservesFailure(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	backendID := "http://localhost:11434"
	modelName := "qwen2.5:7b"
	digest := "abc123"

	// Record a failure first.
	if err := store.SaveFailure(ctx, backendID, modelName, digest, "embed timeout"); err != nil {
		t.Fatalf("SaveFailure() error: %v", err)
	}

	// Save an incomplete profile.
	p := testProfile()
	p.IncompleteCapabilities = []string{"embedding"}
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Failure should still be present.
	fi, err := store.GetFailure(ctx, backendID, modelName)
	if err != nil {
		t.Fatalf("GetFailure() error: %v (expected failure to be preserved)", err)
	}
	if fi.LastError != "embed timeout" {
		t.Errorf("LastError = %q, want %q", fi.LastError, "embed timeout")
	}
}

func TestStore_Save_ClearsFailure(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	backendID := "http://localhost:11434"
	modelName := "qwen2.5:7b"
	digest := "abc123"

	// Record a failure first.
	if err := store.SaveFailure(ctx, backendID, modelName, digest, "transient error"); err != nil {
		t.Fatalf("SaveFailure() error: %v", err)
	}

	// Save a complete profile (empty IncompleteCapabilities).
	p := testProfile()
	p.IncompleteCapabilities = []string{} // explicitly complete
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Failure should be gone.
	_, err := store.GetFailure(ctx, backendID, modelName)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFailure() error = %v, want ErrNotFound (failure should be cleared)", err)
	}
}

func TestStore_BackendScoping(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Save same model name on two different backends.
	p1 := testProfile()
	p1.BackendID = "http://localhost:11434"
	p1.ModelName = "qwen2.5:7b"
	p1.GenerationTokensPerSecond = 42.0

	p2 := testProfile()
	p2.BackendID = "http://gpu-server:11434"
	p2.ModelName = "qwen2.5:7b"
	p2.GenerationTokensPerSecond = 85.0

	if err := store.Save(ctx, p1); err != nil {
		t.Fatalf("Save p1 error: %v", err)
	}
	if err := store.Save(ctx, p2); err != nil {
		t.Fatalf("Save p2 error: %v", err)
	}

	got1, err := store.Get(ctx, p1.BackendID, p1.ModelName)
	if err != nil {
		t.Fatalf("Get p1 error: %v", err)
	}
	got2, err := store.Get(ctx, p2.BackendID, p2.ModelName)
	if err != nil {
		t.Fatalf("Get p2 error: %v", err)
	}

	if got1.GenerationTokensPerSecond != 42.0 {
		t.Errorf("backend1 GenerationTokensPerSecond = %f, want 42.0", got1.GenerationTokensPerSecond)
	}
	if got2.GenerationTokensPerSecond != 85.0 {
		t.Errorf("backend2 GenerationTokensPerSecond = %f, want 85.0", got2.GenerationTokensPerSecond)
	}

	// Failures are also scoped independently.
	if err := store.SaveFailure(ctx, p1.BackendID, p1.ModelName, p1.ModelDigest, "local error"); err != nil {
		t.Fatalf("SaveFailure p1 error: %v", err)
	}

	fi1, err := store.GetFailure(ctx, p1.BackendID, p1.ModelName)
	if err != nil {
		t.Fatalf("GetFailure p1 error: %v", err)
	}
	if fi1.LastError != "local error" {
		t.Errorf("backend1 LastError = %q, want %q", fi1.LastError, "local error")
	}

	// Backend 2 should have no failure.
	_, err = store.GetFailure(ctx, p2.BackendID, p2.ModelName)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFailure p2 error = %v, want ErrNotFound", err)
	}
}
