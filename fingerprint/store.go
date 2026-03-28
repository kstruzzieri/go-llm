package fingerprint

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// maxBackoff is the ceiling for exponential failure backoff.
const maxBackoff = 24 * time.Hour

// baseBackoff is the initial backoff duration after the first failure.
const baseBackoff = 1 * time.Hour

// Store defines fingerprint persistence operations.
type Store interface {
	Get(ctx context.Context, backendID, modelName string) (*Profile, error)
	GetFailure(ctx context.Context, backendID, modelName string) (*FailureInfo, error)
	Save(ctx context.Context, profile Profile) error
	NeedsFingerprint(ctx context.Context, backendID, modelName, currentDigest string) (bool, error)
	SaveFailure(ctx context.Context, backendID, modelName, modelDigest, errMsg string) error
}

// SQLiteStore is a fingerprint Store backed by SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewStore creates a fingerprint store on the given database, running
// migrations if needed.
func NewStore(ctx context.Context, db *sql.DB) (*SQLiteStore, error) {
	if err := runMigrations(db); err != nil {
		return nil, fmt.Errorf("fingerprint: initialize store: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// Get retrieves a profile by backend ID and model name.
// Returns ErrNotFound if no profile exists.
func (s *SQLiteStore) Get(ctx context.Context, backendID, modelName string) (*Profile, error) {
	var p Profile
	var capsJSON, incompleteJSON string
	var testedAtMs int64
	var promptNs, coldStartNs, embLatNs int64

	err := s.db.QueryRowContext(ctx,
		`SELECT backend_id, model_name, model_digest, model_kind,
			capabilities, incomplete_capabilities, kind_source,
			profile_version, tested_at,
			effective_context, tool_calling_rate, instruction_score,
			generation_tokens_per_sec, prompt_latency_ns, cold_start_latency_ns,
			embedding_dim, embedding_coherence, embedding_latency_ns,
			peak_memory_mb, gpu_layers_used
		FROM fingerprint_profiles
		WHERE backend_id = ? AND model_name = ?`,
		backendID, modelName,
	).Scan(
		&p.BackendID, &p.ModelName, &p.ModelDigest, &p.ModelKind,
		&capsJSON, &incompleteJSON, &p.KindSource,
		&p.ProfileVersion, &testedAtMs,
		&p.EffectiveContext, &p.ToolCallingRate, &p.InstructionScore,
		&p.GenerationTokensPerSecond, &promptNs, &coldStartNs,
		&p.EmbeddingDim, &p.EmbeddingCoherence, &embLatNs,
		&p.PeakMemoryMB, &p.GPULayersUsed,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("fingerprint: get %q/%q: %w", backendID, modelName, err)
	}

	if err := json.Unmarshal([]byte(capsJSON), &p.Capabilities); err != nil {
		return nil, fmt.Errorf("fingerprint: get %q/%q: unmarshal capabilities: %w", backendID, modelName, err)
	}
	if err := json.Unmarshal([]byte(incompleteJSON), &p.IncompleteCapabilities); err != nil {
		return nil, fmt.Errorf("fingerprint: get %q/%q: unmarshal incomplete_capabilities: %w", backendID, modelName, err)
	}

	p.TestedAt = time.UnixMilli(testedAtMs)
	p.PromptLatency = time.Duration(promptNs)
	p.ColdStartLatency = time.Duration(coldStartNs)
	p.EmbeddingLatency = time.Duration(embLatNs)

	return &p, nil
}

// GetFailure retrieves a failure record by backend ID and model name.
// The returned FailureInfo includes a computed RetryAfter timestamp.
// Returns ErrNotFound if no failure exists.
func (s *SQLiteStore) GetFailure(ctx context.Context, backendID, modelName string) (*FailureInfo, error) {
	var fi FailureInfo
	var attemptedAtMs int64

	err := s.db.QueryRowContext(ctx,
		`SELECT model_digest, last_error, attempted_at, attempt_count
		FROM fingerprint_failures
		WHERE backend_id = ? AND model_name = ?`,
		backendID, modelName,
	).Scan(&fi.ModelDigest, &fi.LastError, &attemptedAtMs, &fi.AttemptCount)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("fingerprint: get failure %q/%q: %w", backendID, modelName, err)
	}

	fi.AttemptedAt = time.UnixMilli(attemptedAtMs)
	fi.RetryAfter = computeRetryAfter(fi.AttemptedAt, fi.AttemptCount)

	return &fi, nil
}

// Save persists a profile using upsert semantics. For complete profiles
// (empty IncompleteCapabilities), any failure record for the same model is deleted.
//
// The upsert only overwrites an existing row when the incoming profile is
// at least as recent (by tested_at). This prevents a slow-finishing probe
// for an old digest from overwriting a newer profile during concurrent
// mixed-digest profiling.
func (s *SQLiteStore) Save(ctx context.Context, profile Profile) error {
	capsJSON, err := json.Marshal(profile.Capabilities)
	if err != nil {
		return fmt.Errorf("fingerprint: save: marshal capabilities: %w", err)
	}
	incompleteJSON, err := json.Marshal(profile.IncompleteCapabilities)
	if err != nil {
		return fmt.Errorf("fingerprint: save: marshal incomplete_capabilities: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("fingerprint: save: begin tx: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO fingerprint_profiles (
			backend_id, model_name, model_digest, model_kind,
			capabilities, incomplete_capabilities, kind_source,
			profile_version, tested_at,
			effective_context, tool_calling_rate, instruction_score,
			generation_tokens_per_sec, prompt_latency_ns, cold_start_latency_ns,
			embedding_dim, embedding_coherence, embedding_latency_ns,
			peak_memory_mb, gpu_layers_used
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(backend_id, model_name) DO UPDATE SET
			model_digest              = excluded.model_digest,
			model_kind                = excluded.model_kind,
			capabilities              = excluded.capabilities,
			incomplete_capabilities   = excluded.incomplete_capabilities,
			kind_source               = excluded.kind_source,
			profile_version           = excluded.profile_version,
			tested_at                 = excluded.tested_at,
			effective_context         = excluded.effective_context,
			tool_calling_rate         = excluded.tool_calling_rate,
			instruction_score         = excluded.instruction_score,
			generation_tokens_per_sec = excluded.generation_tokens_per_sec,
			prompt_latency_ns         = excluded.prompt_latency_ns,
			cold_start_latency_ns     = excluded.cold_start_latency_ns,
			embedding_dim             = excluded.embedding_dim,
			embedding_coherence       = excluded.embedding_coherence,
			embedding_latency_ns      = excluded.embedding_latency_ns,
			peak_memory_mb            = excluded.peak_memory_mb,
			gpu_layers_used           = excluded.gpu_layers_used
		WHERE excluded.tested_at >= fingerprint_profiles.tested_at`,
		profile.BackendID, profile.ModelName, profile.ModelDigest, string(profile.ModelKind),
		string(capsJSON), string(incompleteJSON), profile.KindSource,
		profile.ProfileVersion, profile.TestedAt.UnixMilli(),
		profile.EffectiveContext, profile.ToolCallingRate, profile.InstructionScore,
		profile.GenerationTokensPerSecond, int64(profile.PromptLatency), int64(profile.ColdStartLatency),
		profile.EmbeddingDim, profile.EmbeddingCoherence, int64(profile.EmbeddingLatency),
		profile.PeakMemoryMB, profile.GPULayersUsed,
	)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("fingerprint: save %q/%q: %w", profile.BackendID, profile.ModelName, err)
	}

	// Complete profiles clear any associated failure row.
	if len(profile.IncompleteCapabilities) == 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM fingerprint_failures WHERE backend_id = ? AND model_name = ?`,
			profile.BackendID, profile.ModelName,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("fingerprint: save: clear failure %q/%q: %w", profile.BackendID, profile.ModelName, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("fingerprint: save: commit %q/%q: %w", profile.BackendID, profile.ModelName, err)
	}

	return nil
}

// NeedsFingerprint determines whether a model requires (re-)fingerprinting.
//
// Returns true when:
//   - no profile exists
//   - model digest has changed
//   - profile version is below CurrentProfileVersion
//   - profile has incomplete capabilities with no active backoff
//
// Returns false when the model is in a backoff window after a recent failure.
func (s *SQLiteStore) NeedsFingerprint(ctx context.Context, backendID, modelName, currentDigest string) (bool, error) {
	// Check for recent failure.
	var failDigest string
	var failAttemptedMs int64
	var failCount int
	err := s.db.QueryRowContext(ctx,
		`SELECT model_digest, attempted_at, attempt_count
		FROM fingerprint_failures
		WHERE backend_id = ? AND model_name = ?`,
		backendID, modelName,
	).Scan(&failDigest, &failAttemptedMs, &failCount)

	hasFailure := err == nil
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("fingerprint: needs fingerprint: query failure %q/%q: %w", backendID, modelName, err)
	}

	if hasFailure {
		if failDigest != currentDigest {
			// Stale failure — digest changed, delete it and proceed.
			if _, err := s.db.ExecContext(ctx,
				`DELETE FROM fingerprint_failures WHERE backend_id = ? AND model_name = ?`,
				backendID, modelName,
			); err != nil {
				return false, fmt.Errorf("fingerprint: needs fingerprint: delete stale failure %q/%q: %w", backendID, modelName, err)
			}
			hasFailure = false
		} else {
			// Same digest — check if still in backoff window.
			retryAfter := computeRetryAfter(time.UnixMilli(failAttemptedMs), failCount)
			if time.Now().Before(retryAfter) {
				return false, nil
			}
			// Backoff expired — fall through to check profile.
		}
	}

	// Check for existing profile.
	var profileDigest string
	var profileVersion int
	var incompleteJSON string
	err = s.db.QueryRowContext(ctx,
		`SELECT model_digest, profile_version, incomplete_capabilities
		FROM fingerprint_profiles
		WHERE backend_id = ? AND model_name = ?`,
		backendID, modelName,
	).Scan(&profileDigest, &profileVersion, &incompleteJSON)

	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("fingerprint: needs fingerprint: query profile %q/%q: %w", backendID, modelName, err)
	}

	// Digest changed — needs re-fingerprint.
	if profileDigest != currentDigest {
		// Also delete any stale failure for the old digest.
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM fingerprint_failures WHERE backend_id = ? AND model_name = ?`,
			backendID, modelName,
		); err != nil {
			return false, fmt.Errorf("fingerprint: needs fingerprint: delete stale failure %q/%q: %w", backendID, modelName, err)
		}
		return true, nil
	}

	// Version upgrade needed.
	if profileVersion < CurrentProfileVersion {
		return true, nil
	}

	// Check incomplete capabilities.
	var incomplete []string
	if err := json.Unmarshal([]byte(incompleteJSON), &incomplete); err != nil {
		return false, fmt.Errorf("fingerprint: needs fingerprint: unmarshal incomplete %q/%q: %w", backendID, modelName, err)
	}
	if len(incomplete) > 0 {
		// Incomplete profile — need fingerprint only if not in active backoff.
		if hasFailure {
			// We already checked above: if we're here, backoff has expired
			// or digest didn't match. But let's be explicit.
			retryAfter := computeRetryAfter(time.UnixMilli(failAttemptedMs), failCount)
			if time.Now().Before(retryAfter) {
				return false, nil
			}
		}
		return true, nil
	}

	return false, nil
}

// SaveFailure records or increments a fingerprint failure for backoff tracking.
// When the digest changes, attempt_count resets to 1 so a new model version
// starts with a fresh backoff window instead of inheriting stale retry history.
func (s *SQLiteStore) SaveFailure(ctx context.Context, backendID, modelName, modelDigest, errMsg string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO fingerprint_failures (backend_id, model_name, model_digest, last_error, attempted_at, attempt_count)
		VALUES (?, ?, ?, ?, ?, 1)
		ON CONFLICT(backend_id, model_name) DO UPDATE SET
			model_digest  = excluded.model_digest,
			last_error    = excluded.last_error,
			attempted_at  = excluded.attempted_at,
			attempt_count = CASE
				WHEN fingerprint_failures.model_digest = excluded.model_digest
				THEN attempt_count + 1
				ELSE 1
			END`,
		backendID, modelName, modelDigest, errMsg, now,
	)
	if err != nil {
		return fmt.Errorf("fingerprint: save failure %q/%q: %w", backendID, modelName, err)
	}
	return nil
}

// computeRetryAfter calculates the retry timestamp using exponential backoff:
// min(1h * 2^(count-1), 24h) from the attempted_at time.
func computeRetryAfter(attemptedAt time.Time, attemptCount int) time.Time {
	backoff := baseBackoff
	for i := 1; i < attemptCount; i++ {
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
			break
		}
	}
	return attemptedAt.Add(backoff)
}
