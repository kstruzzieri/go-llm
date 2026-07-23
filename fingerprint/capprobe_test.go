package fingerprint

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCapProbe_SaveGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	in := CapProbe{
		BackendID: "http://localhost:8080", ModelName: "byo-model",
		Capability: "tool_call", State: CapProbeYes,
		ModelDigest: "sha256:abc", ProbeVersion: CurrentToolProbeVersion,
		TestedAt: now,
	}
	if err := s.SaveCapProbe(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.GetCapProbe(ctx, in.BackendID, in.ModelName, "tool_call")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != CapProbeYes || got.ModelDigest != "sha256:abc" ||
		got.ProbeVersion != CurrentToolProbeVersion || !got.TestedAt.Equal(now) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.ExpiresAt.IsZero() {
		t.Fatalf("yes row must not expire, got %v", got.ExpiresAt)
	}
}

func TestCapProbe_GetMissingReturnsErrNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetCapProbe(context.Background(), "b", "m", "tool_call")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCapProbe_SaveUpsertsOnSameKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := CapProbe{
		BackendID: "b", ModelName: "m", Capability: "tool_call",
		State: CapProbeInconclusive, ModelDigest: "d1",
		ProbeVersion: CurrentToolProbeVersion, TestedAt: time.Now(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	if err := s.SaveCapProbe(ctx, base); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	base.State = CapProbeYes
	base.ExpiresAt = time.Time{}
	if err := s.SaveCapProbe(ctx, base); err != nil {
		t.Fatalf("save 2 (upsert): %v", err)
	}
	got, err := s.GetCapProbe(ctx, "b", "m", "tool_call")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != CapProbeYes || !got.ExpiresAt.IsZero() {
		t.Fatalf("upsert did not replace: %+v", got)
	}
}

func TestCapProbe_SaveStaleWriteGuard(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	newer := CapProbe{
		BackendID: "b", ModelName: "m", Capability: "tool_call",
		State: CapProbeYes, ModelDigest: "new",
		ProbeVersion: CurrentToolProbeVersion, TestedAt: now,
	}
	if err := s.SaveCapProbe(ctx, newer); err != nil {
		t.Fatalf("save newer: %v", err)
	}
	stale := newer
	stale.State = CapProbeNo
	stale.ModelDigest = "old"
	stale.TestedAt = now.Add(-time.Hour)
	if err := s.SaveCapProbe(ctx, stale); err != nil {
		t.Fatalf("save stale: %v", err)
	}
	got, err := s.GetCapProbe(ctx, "b", "m", "tool_call")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != CapProbeYes || got.ModelDigest != "new" || !got.TestedAt.Equal(now) {
		t.Fatalf("stale probe overwrote newer row: %+v", got)
	}
}

func TestCapProbe_DeleteCapProbes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	in := CapProbe{BackendID: "b", ModelName: "m", Capability: "tool_call",
		State: CapProbeNo, ModelDigest: "d", ProbeVersion: 1, TestedAt: time.Now()}
	if err := s.SaveCapProbe(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.DeleteCapProbes(ctx, "b", "m"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetCapProbe(ctx, "b", "m", "tool_call"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
}

// Valid centralizes the freshness contract: digest match, version match, expiry.
func TestCapProbe_Valid(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		probe  CapProbe
		digest string
		want   bool
	}{
		{"fresh yes", CapProbe{State: CapProbeYes, ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion, TestedAt: now}, "d", true},
		{"digest mismatch", CapProbe{State: CapProbeYes, ModelDigest: "old", ProbeVersion: CurrentToolProbeVersion}, "new", false},
		{"version mismatch", CapProbe{State: CapProbeYes, ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion + 1}, "d", false},
		{"expired inconclusive", CapProbe{State: CapProbeInconclusive, ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion, ExpiresAt: now.Add(-time.Minute)}, "d", false},
		{"unexpired inconclusive", CapProbe{State: CapProbeInconclusive, ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion, ExpiresAt: now.Add(time.Hour)}, "d", true},
		{"no-expiry no", CapProbe{State: CapProbeNo, ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion}, "d", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.probe.Valid(tt.digest, now); got != tt.want {
				t.Fatalf("Valid(%q) = %v, want %v", tt.digest, got, tt.want)
			}
		})
	}
}

func TestMigrationV2_UpgradesExistingV1DB(t *testing.T) {
	// Build a genuine v1-only database: v1 tables plus schema version 1
	// recorded exactly as runMigrations records it, and no v2 table.
	db := openTestDB(t)
	if _, err := db.Exec(`CREATE TABLE fingerprint_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create version table: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin v1 tx: %v", err)
	}
	if err := migrateV1(tx); err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO fingerprint_schema_version (version, description, applied_at) VALUES (?, ?, ?)`,
		migrations[0].version, migrations[0].description, time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("record v1: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit v1: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'capability_probes'`).Scan(&n); err != nil {
		t.Fatalf("check pre-upgrade schema: %v", err)
	}
	if n != 0 {
		t.Fatal("capability_probes must not exist in v1 DB")
	}

	// Opening the store must apply v2 on top of the existing v1 DB.
	s, err := NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("upgrade store: %v", err)
	}
	want := migrations[len(migrations)-1].version
	if v, err := currentSchemaVersion(db); err != nil || v != want {
		t.Fatalf("want schema version %d, got %d (err %v)", want, v, err)
	}
	in := CapProbe{BackendID: "b", ModelName: "m", Capability: "tool_call",
		State: CapProbeYes, ModelDigest: "d", ProbeVersion: 1, TestedAt: time.Now()}
	if err := s.SaveCapProbe(context.Background(), in); err != nil {
		t.Fatalf("save after upgrade: %v", err)
	}

	// Reopen must be idempotent.
	if _, err := NewStore(context.Background(), db); err != nil {
		t.Fatalf("reopen store: %v", err)
	}
}
