// provider/evidence_binding.go
//
// The registry's read path over the capability evidence log (#734). Answers are
// keyed by canonical endpoint and adapter, not provider name, so a rename keeps
// them and two providers of one name at different endpoints never share them.
package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// EvidenceBinding says which log rows answer for a provider: its endpoint in
// canonical form and the API it is reached through.
type EvidenceBinding struct{ Endpoint, Adapter string }

// EvidenceBindingFor returns the binding of a provider's base URL and api_format.
// An empty format is ollama, as in config. The endpoint is canonical because the
// store keys and hashes the endpoint exactly as given. Errors never contain the
// base URL.
func EvidenceBindingFor(baseURL, apiFormat string) (EvidenceBinding, error) {
	adapter := fingerprint.AdapterOllama
	switch apiFormat {
	case "", fingerprint.AdapterOllama:
	case fingerprint.AdapterOpenAICompat:
		adapter = fingerprint.AdapterOpenAICompat
	default:
		return EvidenceBinding{}, fmt.Errorf("provider: evidence binding: unknown api format %q", apiFormat)
	}
	endpoint, _, err := CanonicalEndpoint(baseURL)
	if err != nil {
		return EvidenceBinding{}, fmt.Errorf("provider: evidence binding: %w", err)
	}
	return EvidenceBinding{Endpoint: endpoint, Adapter: adapter}, nil
}

// WithEvidenceLog makes the registry read capabilities from the evidence log and
// ignore capability_probes, and keeps cached profiles fresh across processes that
// share the store. bind maps a provider name to its binding; a provider it does
// not resolve has no evidence, with no fallback to the old table. bind must do no
// I/O and be safe for concurrent use. Without this option the registry behaves as
// if the log did not exist. On-demand resolution (ResolveToolCall's probe path)
// still reads and writes capability_probes.
func WithEvidenceLog(store fingerprint.EvidenceStore, bind func(provider string) (EvidenceBinding, bool)) ModelRegistryOption {
	return func(r *ModelRegistry) {
		r.evidenceStore, r.evidenceBind = store, bind
	}
}

// syncGeneration drops every cached profile when the log has been written since
// the last check, by this process or another, and fences builds in flight as the
// policy setters do. A read error keeps the cache; the next boundary tries again.
// It runs at routing boundaries, not in Lookup.
func (r *ModelRegistry) syncGeneration(ctx context.Context) {
	if r == nil || r.evidenceStore == nil {
		return
	}
	generation, err := r.evidenceStore.Generation(ctx)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation == r.syncedGeneration {
		return
	}
	r.syncedGeneration = generation
	clear(r.profiles)
	r.overrideVersion++
}

// evidenceView is one read of the log for a provider's models, evaluated at one
// instant. A nil view means no log is attached.
type evidenceView struct {
	adapter    string
	now        time.Time
	generation int64
	rows       map[string][]fingerprint.EvidenceRow // by model
	unbound    bool                                 // the binder does not resolve the provider
	failed     bool                                 // the read failed, so the rows say nothing
}

// readEvidence reads the log for models in one snapshot, without account reads:
// capability evidence belongs to the endpoint and adapter, not to a key. A failed
// read degrades to no evidence, like the old table's, and the view remembers it.
func (r *ModelRegistry) readEvidence(ctx context.Context, provider string, models []string) *evidenceView {
	if r.evidenceStore == nil {
		return nil
	}
	view := &evidenceView{now: r.now()}
	binding, ok := EvidenceBinding{}, false
	if r.evidenceBind != nil {
		binding, ok = r.evidenceBind(provider)
	}
	if !ok {
		view.unbound = true
		return view
	}
	snap, err := r.evidenceStore.ReadSnapshot(ctx, binding.Endpoint, binding.Adapter, nil, "", models, view.now)
	if err != nil {
		view.failed = true
		return view
	}
	view.adapter, view.generation = binding.Adapter, snap.Generation
	view.rows = make(map[string][]fingerprint.EvidenceRow, len(models))
	for _, row := range snap.Rows {
		view.rows[row.Model] = append(view.rows[row.Model], row)
	}
	return view
}

// evidenceUnchanged reports whether a profile built from view may be published:
// the generation has not moved since the snapshot. It reads the store, so call it
// outside r.mu. A view with no rows to go stale passes; one whose read failed, or
// whose generation cannot be read now, does not, because a profile cached without
// the log's answer would stay until some later write.
func (r *ModelRegistry) evidenceUnchanged(ctx context.Context, view *evidenceView) bool {
	if view == nil || view.unbound {
		return true
	}
	if view.failed {
		return false
	}
	generation, err := r.evidenceStore.Generation(ctx)
	return err == nil && generation == view.generation
}

func (v *evidenceView) readings(model, digest string) map[string]fingerprint.CapabilityReading {
	return fingerprint.ReadCapabilities(v.rows[model], v.adapter, digest, v.now)
}

// caps reduces a model's readings at digest to the capabilities the log says yes
// to and those it has a definitive answer for.
func (v *evidenceView) caps(model, digest string) (yes, known Capability) {
	for name, reading := range v.readings(model, digest) {
		bit, ok := canonicalCapability(name)
		answer := readingAnswer(reading)
		if !ok || answer == nil {
			continue
		}
		known |= bit
		if answer.State == fingerprint.CapProbeYes {
			yes |= bit
		}
	}
	return yes, known
}

// deadline is the next instant the model's readings at digest change with no
// write, or zero when there is none or no view.
func (v *evidenceView) deadline(model, digest string) time.Time {
	if v == nil {
		return time.Time{}
	}
	return fingerprint.NextReadingTransition(v.rows[model], v.adapter, digest, v.now)
}

// readingAnswer is the answer a reading gives when its sources disagree: the newer
// direct answer, else what the provider reported, else nil. #736's precedence core
// replaces it.
func readingAnswer(c fingerprint.CapabilityReading) *fingerprint.EvidenceRow {
	switch {
	case c.Probe != nil && (c.Observed == nil || !c.Observed.ObservedAt.After(c.Probe.ObservedAt)):
		return c.Probe
	case c.Observed != nil:
		return c.Observed
	}
	return c.Provider
}

// runtimeDigest is the identity evidence is read under: the runtime's content
// digest, empty when name-only. Unlike capProbeDigest it has no fallback, because
// the log stores a name-only row under an empty digest.
func runtimeDigest(info *ModelInfo) string {
	if info == nil {
		return ""
	}
	return info.Digest
}

// explainToolCallFromLog is ExplainToolCall's provenance on the log path, from the
// log's tool_call reading. The vocabulary stays ToolCallExplanation's: a direct
// answer or an attempt is "probe" and what the provider reported is "runtime". #736
// reworks it.
func (r *ModelRegistry) explainToolCallFromLog(ctx context.Context, key ModelKey, runtimeInfo *ModelInfo) (ToolCallExplanation, error) {
	log := r.readEvidence(ctx, key.Provider, []string{key.Model})
	digest := runtimeDigest(runtimeInfo)
	yes, known := log.caps(key.Model, digest)
	facts := r.projectListedModel(ctx, key, *runtimeInfo, yes, known, r.snapshotListedProjectionPolicy())
	if err := ctx.Err(); err != nil {
		return ToolCallExplanation{}, err
	}

	exp := ToolCallExplanation{Caps: facts.Caps, Has: facts.Caps.Has(CapToolCall), Source: "unknown"}
	reading := log.readings(key.Model, digest)[capabilityToolCall]
	answer := readingAnswer(reading)
	attempt := reading.Attempt
	switch {
	case answer != nil:
		exp.State, exp.TestedAt, exp.Valid = answer.State, answer.ObservedAt, true
	case attempt != nil:
		exp.State, exp.TestedAt = attempt.State, attempt.ObservedAt
		exp.Valid = attempt.ExpiresAt.IsZero() || !log.now.After(attempt.ExpiresAt)
	}

	direct := answer != nil && answer.Source != fingerprint.EvidenceSourceProvider
	switch {
	case exp.Has:
		static := r.catalogProfileFor(ParseModelName(key.Model), runtimeInfo)
		switch {
		case static != nil && static.Caps.Has(CapToolCall):
			exp.Source = "catalog"
		case direct && answer.State == fingerprint.CapProbeYes:
			exp.Source = "probe"
		default:
			exp.Source = "runtime"
		}
	case direct, answer == nil && attempt != nil:
		exp.Source = "probe"
	case answer != nil:
		exp.Source = "runtime"
	}
	return exp, nil
}
