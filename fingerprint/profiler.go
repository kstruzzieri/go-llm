package fingerprint

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/singleflight"
)

// ModelProber abstracts model kind detection and performance probing.
type ModelProber interface {
	DetectKind(ctx context.Context, model string) (*KindDetection, error)
	ProbeChat(ctx context.Context, model string, opts interface{}) (*ChatMetrics, error)
	ProbeEmbedding(ctx context.Context, model string) (*EmbeddingMetrics, error)
}

// Profiler orchestrates model fingerprinting: detection, probing, caching,
// failure tracking, and version upgrades. Concurrent requests for the same
// model+digest are deduplicated via singleflight.
type Profiler struct {
	store  Store
	prober ModelProber
	sfg    singleflight.Group
}

// NewProfiler creates a Profiler backed by the given store and prober.
func NewProfiler(store Store, prober ModelProber) *Profiler {
	return &Profiler{
		store:  store,
		prober: prober,
	}
}

// EnsureProfile returns a fingerprint profile for the given model,
// using a cached profile if valid or running probes if needed.
//
// The modelDigest should come from /api/show and is used to detect
// when a model has been updated and needs re-profiling.
//
// Returns a partial profile (with IncompleteCapabilities set) if some
// probes succeed and others fail. Returns an error only if all probes
// fail or detection fails entirely.
func (p *Profiler) EnsureProfile(ctx context.Context, backendID, modelName, modelDigest string) (*Profile, error) {
	key := backendID + "\x00" + modelName + "\x00" + modelDigest

	v, err, _ := p.sfg.Do(key, func() (interface{}, error) {
		return p.ensureProfileInner(ctx, backendID, modelName, modelDigest)
	})
	if err != nil {
		return nil, err
	}
	profile := v.(*Profile)
	return profile, nil
}

func (p *Profiler) ensureProfileInner(ctx context.Context, backendID, modelName, modelDigest string) (*Profile, error) {
	needs, err := p.store.NeedsFingerprint(ctx, backendID, modelName, modelDigest)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: check needs: %w", err)
	}

	if !needs {
		// Try to return cached profile.
		profile, err := p.store.Get(ctx, backendID, modelName)
		if err == nil {
			// Verify digest matches — don't return stale profiles.
			if profile.ModelDigest == modelDigest {
				return profile, nil
			}
			// Stale digest — check if in backoff for the current digest.
			return nil, p.checkBackoff(ctx, backendID, modelName)
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("fingerprint: get cached: %w", err)
		}
		// Not found — check failure backoff.
		failure, fErr := p.store.GetFailure(ctx, backendID, modelName)
		if fErr == nil && failure != nil {
			if time.Now().Before(failure.RetryAfter) {
				return nil, &BackoffError{
					RetryAfter: failure.RetryAfter,
					LastError:  failure.LastError,
				}
			}
		} else if fErr != nil && !errors.Is(fErr, ErrNotFound) {
			return nil, fmt.Errorf("fingerprint: get failure: %w", fErr)
		}
		// Fall through to profiling (race condition path: NeedsFingerprint
		// returned false but Get returned not-found).
	}

	// Check for existing partial profile we can build on.
	var base *Profile
	existing, err := p.store.Get(ctx, backendID, modelName)
	if err == nil && existing.ModelDigest == modelDigest && len(existing.IncompleteCapabilities) > 0 {
		// Check backoff for partial retry.
		failure, fErr := p.store.GetFailure(ctx, backendID, modelName)
		if fErr == nil && failure != nil && time.Now().Before(failure.RetryAfter) {
			// In backoff but have partial profile — return partial.
			return existing, nil
		}
		// Only use as a merge base if the profile version is current.
		// A stale-version partial needs a full re-probe, not a
		// selective retry that would stamp old metrics as current.
		if existing.ProfileVersion >= CurrentProfileVersion {
			base = existing
		}
	}

	// Detect model kind.
	detection, err := p.prober.DetectKind(ctx, modelName)
	if err != nil {
		_ = p.store.SaveFailure(ctx, backendID, modelName, modelDigest, err.Error())
		return nil, fmt.Errorf("fingerprint: detect kind: %w", err)
	}

	// Determine which probes to run.
	probeChat, probeEmbed := p.selectProbes(detection, base)

	profile := p.buildBaseProfile(backendID, modelName, modelDigest, detection)
	if base != nil {
		// Carry forward successful metrics from base.
		p.mergeBase(profile, base)
	}

	var chatErr, embedErr error

	// Run probes in fixed order: chat first, then embedding.
	if probeChat {
		metrics, err := p.prober.ProbeChat(ctx, modelName, nil)
		if err != nil {
			chatErr = err
		} else {
			profile.GenerationTokensPerSecond = metrics.TokensPerSecond
			profile.PromptLatency = metrics.PromptLatency
			if metrics.ColdStartLatency > 0 {
				profile.ColdStartLatency = metrics.ColdStartLatency
			}
		}
	}

	if probeEmbed {
		metrics, err := p.prober.ProbeEmbedding(ctx, modelName)
		if err != nil {
			embedErr = err
		} else {
			profile.EmbeddingDim = metrics.Dim
			profile.EmbeddingLatency = metrics.Latency
			if metrics.ColdStartLatency > 0 && profile.ColdStartLatency == 0 {
				profile.ColdStartLatency = metrics.ColdStartLatency
			}
		}
	}

	// Handle results.
	bothFailed := (probeChat && chatErr != nil) && (probeEmbed && embedErr != nil)
	onlyChat := probeChat && !probeEmbed
	onlyEmbed := probeEmbed && !probeChat
	singleFailed := (onlyChat && chatErr != nil) || (onlyEmbed && embedErr != nil)

	if bothFailed || singleFailed {
		errMsg := p.firstError(chatErr, embedErr)
		_ = p.store.SaveFailure(ctx, backendID, modelName, modelDigest, errMsg)
		return nil, fmt.Errorf("fingerprint: probe %q: %s", modelName, errMsg)
	}

	// Determine incomplete capabilities.
	var incomplete []string
	if probeChat && chatErr != nil {
		incomplete = append(incomplete, "completion")
	}
	if probeEmbed && embedErr != nil {
		incomplete = append(incomplete, "embedding")
	}
	profile.IncompleteCapabilities = incomplete

	// Set sentinel values for untested fields.
	if !probeChat || chatErr != nil {
		if profile.GenerationTokensPerSecond == 0 {
			profile.GenerationTokensPerSecond = -1
		}
		if profile.ToolCallingRate == 0 {
			profile.ToolCallingRate = -1
		}
		if profile.InstructionScore == 0 {
			profile.InstructionScore = -1
		}
	}
	if !probeEmbed || embedErr != nil {
		if profile.EmbeddingCoherence == 0 {
			profile.EmbeddingCoherence = -1
		}
	}

	if err := p.store.Save(ctx, *profile); err != nil {
		return nil, fmt.Errorf("fingerprint: save profile: %w", err)
	}

	// If partial, also record failure for the incomplete part.
	if len(incomplete) > 0 {
		errMsg := p.firstError(chatErr, embedErr)
		_ = p.store.SaveFailure(ctx, backendID, modelName, modelDigest, errMsg)
	}

	return profile, nil
}

// selectProbes determines which probes to run based on detection and any
// existing base profile.
func (p *Profiler) selectProbes(det *KindDetection, base *Profile) (chat, embed bool) {
	if base != nil && len(base.IncompleteCapabilities) > 0 &&
		base.ProfileVersion >= CurrentProfileVersion {
		// Only retry incomplete capabilities when the profile version is
		// current. If the version is stale, re-run all probes so that the
		// new version's metrics are collected for every capability.
		for _, cap := range base.IncompleteCapabilities {
			switch cap {
			case "completion":
				chat = true
			case "embedding":
				embed = true
			}
		}
		return chat, embed
	}

	// Dual-capability only when capabilities were explicitly detected.
	if det.Source == "capabilities" {
		hasCap := make(map[string]bool)
		for _, c := range det.Capabilities {
			hasCap[c] = true
		}
		chat = hasCap["completion"] || hasCap["tools"]
		embed = hasCap["embedding"]
		return chat, embed
	}

	// Heuristic or probe: single-capability only.
	switch det.Kind {
	case ModelKindChat:
		return true, false
	case ModelKindEmbedding:
		return false, true
	default:
		return true, false // default to chat probe for unknown
	}
}

// buildBaseProfile creates a new profile with identity and detection info.
func (p *Profiler) buildBaseProfile(backendID, modelName, modelDigest string, det *KindDetection) *Profile {
	return &Profile{
		BackendID:                 backendID,
		ModelName:                 modelName,
		ModelDigest:               modelDigest,
		ModelKind:                 det.Kind,
		Capabilities:              det.Capabilities,
		KindSource:                det.Source,
		ProfileVersion:            CurrentProfileVersion,
		TestedAt:                  time.Now(),
		GenerationTokensPerSecond: -1,
		ToolCallingRate:           -1,
		InstructionScore:          -1,
		EmbeddingCoherence:        -1,
	}
}

// mergeBase carries forward successful metrics from a partial base profile.
func (p *Profiler) mergeBase(dst, src *Profile) {
	if src.GenerationTokensPerSecond > 0 {
		dst.GenerationTokensPerSecond = src.GenerationTokensPerSecond
	}
	if src.PromptLatency > 0 {
		dst.PromptLatency = src.PromptLatency
	}
	if src.ColdStartLatency > 0 {
		dst.ColdStartLatency = src.ColdStartLatency
	}
	if src.EmbeddingDim > 0 {
		dst.EmbeddingDim = src.EmbeddingDim
	}
	if src.EmbeddingLatency > 0 {
		dst.EmbeddingLatency = src.EmbeddingLatency
	}
	if src.EmbeddingCoherence > 0 {
		dst.EmbeddingCoherence = src.EmbeddingCoherence
	}
	if src.ToolCallingRate >= 0 {
		dst.ToolCallingRate = src.ToolCallingRate
	}
	if src.InstructionScore >= 0 {
		dst.InstructionScore = src.InstructionScore
	}
}

// checkBackoff returns a BackoffError if the model is in an active backoff window.
func (p *Profiler) checkBackoff(ctx context.Context, backendID, modelName string) error {
	failure, err := p.store.GetFailure(ctx, backendID, modelName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("fingerprint: stale profile with no failure record")
		}
		return fmt.Errorf("fingerprint: get failure: %w", err)
	}
	if time.Now().Before(failure.RetryAfter) {
		return &BackoffError{
			RetryAfter: failure.RetryAfter,
			LastError:  failure.LastError,
		}
	}
	return fmt.Errorf("fingerprint: stale profile, backoff expired")
}

// firstError returns the error message from the first non-nil error.
func (p *Profiler) firstError(errs ...error) string {
	for _, e := range errs {
		if e != nil {
			return e.Error()
		}
	}
	return "unknown error"
}
