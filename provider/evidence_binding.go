// provider/evidence_binding.go
//
// The registry's read path over the capability evidence log (#734), and the
// on-demand tool_call resolution that writes it on a log-bound registry (#735),
// with its cooldown and automatic allowance. Answers are
// keyed by canonical endpoint and adapter, not provider name, so a rename keeps
// them and two providers of one name at different endpoints never share them.
package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sync"
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
// if the log did not exist.
//
// On-demand resolution (ResolveToolCall, EnsureToolCallResolved) then reads and
// writes only the log: it never reads or writes capability_probes, never asserts
// fingerprint.ToolCallProber, and spends the registry's automatic allowance (see
// ResolveToolCall). WithCapabilityProber is enough to enable it; a probe store
// is not needed. A lookup never profiles on such a registry, even with a full
// fingerprint.Store and WithFingerprintProberFactory: stored profiles are read
// and validated as without the log, but DetectKind and ProbeChat never run.
//
// The binding of a provider name is fixed for the registry's life; a host that
// re-points a name builds a new registry. Resolution takes the binding before
// it prepares the provider and prober and compares it at both of its log reads,
// which catches a binding that changes one way during a resolution, not a name
// re-pointed and back while the prober is being built.
func WithEvidenceLog(store fingerprint.EvidenceStore, bind func(provider string) (EvidenceBinding, bool)) ModelRegistryOption {
	return func(r *ModelRegistry) {
		r.evidenceStore, r.evidenceBind = store, bind
	}
}

// bindingFor resolves a provider's evidence binding, false when there is none.
func (r *ModelRegistry) bindingFor(provider string) (EvidenceBinding, bool) {
	if r.evidenceBind == nil {
		return EvidenceBinding{}, false
	}
	return r.evidenceBind(provider)
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
	endpoint   string // the binding read under; set whenever the binder resolved the provider
	adapter    string
	now        time.Time
	generation int64
	rows       map[string][]fingerprint.EvidenceRow // by model
	unbound    bool                                 // the binder does not resolve the provider
	failed     bool                                 // the read failed, so the rows say nothing
}

// readEvidence reads the log for models in one snapshot, without account reads:
// capability evidence belongs to the endpoint and adapter, not to a key. A
// successful read adds the registry's kept rows (keepUnsavedEvidence) for the
// binding's endpoint. A failed read degrades to no evidence, kept rows included,
// like the old table's, and the view remembers it.
func (r *ModelRegistry) readEvidence(ctx context.Context, provider string, models []string) *evidenceView {
	if r.evidenceStore == nil {
		return nil
	}
	view := &evidenceView{now: r.now()}
	binding, ok := r.bindingFor(provider)
	if !ok {
		view.unbound = true
		return view
	}
	view.endpoint, view.adapter = binding.Endpoint, binding.Adapter
	snap, err := r.evidenceStore.ReadSnapshot(ctx, binding.Endpoint, binding.Adapter, nil, "", models, view.now)
	if err != nil {
		view.failed = true
		return view
	}
	view.generation = snap.Generation
	view.rows = make(map[string][]fingerprint.EvidenceRow, len(models))
	for _, row := range snap.Rows {
		view.rows[row.Model] = append(view.rows[row.Model], row)
	}
	// Kept rows join as the store's endpoint query would return them: this
	// endpoint, a model asked for, any adapter. The work under RLock is one pass
	// over all kept rows, copying the matching ones by value into the view's own
	// slices, so no view shares storage with r.kept.
	asked := make(map[string]bool, len(models))
	for _, m := range models {
		asked[m] = true
	}
	r.mu.RLock()
	for _, row := range r.kept {
		if row.Endpoint == binding.Endpoint && asked[row.Model] {
			view.rows[row.Model] = append(view.rows[row.Model], row)
		}
	}
	r.mu.RUnlock()
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
		exp.Unsaved = answer.ID < 0
	case attempt != nil:
		exp.State, exp.TestedAt = attempt.State, attempt.ObservedAt
		exp.Valid = attempt.ExpiresAt.IsZero() || !log.now.After(attempt.ExpiresAt)
		exp.Unsaved = attempt.ID < 0
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

// automaticProbeAllowance is how many requests automatic capability checks on a
// log-bound registry may send over the registry's life. It is fixed: there is no
// setting for it, and time does not refill it.
const automaticProbeAllowance = 2

// Sentinels of on-demand resolution on a log-bound registry. Each carries a
// fixed message; match them with errors.Is.
var (
	// ErrAutomaticProbeBudgetExhausted: the automatic allowance cannot fund the
	// check, so nothing was sent or written and no cooldown started. It is never
	// a definitive no. Its bounded code is automatic_probe_budget_exhausted.
	ErrAutomaticProbeBudgetExhausted = errors.New("provider: automatic_probe_budget_exhausted: the automatic capability checks have spent their allowance")
	// ErrEvidenceUnbound: the provider has no evidence binding, so no answer
	// could be read or saved for it.
	ErrEvidenceUnbound = errors.New("provider: the provider has no evidence binding")
	// ErrEvidenceUnreadable: the evidence log could not be read, so no check
	// was started. An answer already merged into a cached profile is still
	// served while the log cannot be read (ResolveToolCall may say yes while
	// ExplainToolCall, which reads fresh, finds none); the cache is checked
	// again at the next boundary at which the log's generation reads.
	ErrEvidenceUnreadable = errors.New("provider: the evidence log could not be read")
)

// Stages of CapabilityProbeProgress.
const (
	probeStageStarted         = "started"
	probeStageFinished        = "finished"
	probeStageCancelled       = "cancelled"
	probeStageBudgetExhausted = "budget_exhausted"
)

// CapabilityProbeProgress reports an automatic generation check on a log-bound
// registry. Stage is "started" (before the first request), "finished" or
// "cancelled" (after the check returns), or "budget_exhausted" (the allowance
// refused the check; reported once per model per registry). Counts are requests
// the check sends: Requests is what it sent (0 before it runs),
// ReservedRequests what it reserved from the allowance, RemainingRequests what
// the allowance holds after the event. EndpointHost is host[:port] only; no
// event carries a credential or an endpoint path. Native metadata checks report
// nothing. RemainingRequests is read when the allowance changes and emitted
// after, so with concurrent checks on distinct keys events can arrive out of
// that order.
type CapabilityProbeProgress struct {
	Key                       ModelKey
	EndpointHost              string
	Method                    string
	Stage                     string
	Requests                  int
	ReservedRequests          int
	RemainingRequests         int
	MaxOutputTokensPerRequest int
}

// WithCapabilityProbeProgress installs a callback for automatic check progress
// on a log-bound registry. Calls are serialized, made outside registry locks,
// and "started" is delivered before the check's first request. fn must not
// call back into the registry's resolution.
func WithCapabilityProbeProgress(fn func(CapabilityProbeProgress)) ModelRegistryOption {
	return func(r *ModelRegistry) {
		r.progress = fn
	}
}

func (r *ModelRegistry) emitProgress(p CapabilityProbeProgress) {
	if r.progress == nil {
		return
	}
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	r.progress(p)
}

// allowanceCovers reports whether the allowance could fund n requests now.
func (r *ModelRegistry) allowanceCovers(n int) bool {
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	return r.budgetRemaining >= n
}

// reserveAllowance takes n requests whole, or none, and returns what is left.
func (r *ModelRegistry) reserveAllowance(n int) (remaining int, ok bool) {
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	if r.budgetRemaining < n {
		return r.budgetRemaining, false
	}
	r.budgetRemaining -= n
	return r.budgetRemaining, true
}

// refundAllowance returns n unsent requests and reports what is left.
func (r *ModelRegistry) refundAllowance(n int) int {
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	r.budgetRemaining += n
	return r.budgetRemaining
}

// refuseCheck reports a check the allowance cannot fund, once per model.
func (r *ModelRegistry) refuseCheck(event CapabilityProbeProgress) error {
	r.budgetMu.Lock()
	first := !r.budgetReported[event.Key]
	r.budgetReported[event.Key] = true
	event.RemainingRequests = r.budgetRemaining
	r.budgetMu.Unlock()
	if first {
		event.Stage = probeStageBudgetExhausted
		r.emitProgress(event)
	}
	return ErrAutomaticProbeBudgetExhausted
}

// probeMethodFor is the method an automatic check uses through adapter.
func probeMethodFor(adapter string) string {
	switch adapter {
	case fingerprint.AdapterOllama:
		return fingerprint.MethodOllamaShow
	case fingerprint.AdapterOpenAICompat:
		return fingerprint.MethodChatStreamTools
	}
	return ""
}

// endpointHost is the host[:port] of a canonical endpoint, never its path.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Host
}

// resolveToolCallFromLog is ResolveToolCall's slow path on a log-bound registry.
// See ResolveToolCall and WithEvidenceLog. Must not be called while holding r.mu.
func (r *ModelRegistry) resolveToolCallFromLog(ctx context.Context, key ModelKey) (fingerprint.CapProbeState, error) {
	// 1. The binding the rows are written under, taken before the provider and
	// prober are prepared.
	binding, ok := r.bindingFor(key.Provider)
	if !ok {
		return "", ErrEvidenceUnbound
	}
	ctx, err := r.bindMetaPurpose(ctx, DestinationPurposeCapabilityProbe, key.Provider)
	if err != nil {
		return "", err
	}
	p, err := r.providers.Resolve(key)
	if err != nil {
		return "", err
	}
	runtimeInfo, err := r.queryRuntime(ctx, key)
	if err != nil {
		return "", err
	}
	spec, err := r.capProber(ctx, key, runtimeInfo, p)
	if err != nil {
		return "", err
	}
	if spec == nil || spec.Prober == nil {
		return "", nil
	}
	// 2. Never ToolCallProber: it certifies a non-streamed request.
	prober, ok := spec.Prober.(fingerprint.CapabilityProber)
	if !ok {
		return "", nil
	}
	recipe, ok := fingerprint.RecipeFor(probeMethodFor(binding.Adapter))
	if !ok {
		return "", nil
	}
	digest := runtimeDigest(runtimeInfo)

	// 3 and 4.
	if state, done, err := r.toolCallFromLog(ctx, key, binding, digest); done {
		return state, err
	}

	generation := recipe.MaxOutputTokensPerRequest > 0
	event := CapabilityProbeProgress{
		Key: key, EndpointHost: endpointHost(binding.Endpoint), Method: recipe.Method,
		MaxOutputTokensPerRequest: recipe.MaxOutputTokensPerRequest,
	}
	// 5. A check that will be refused must not wait in a slot queue.
	if generation && !r.allowanceCovers(recipe.MaxRequests) {
		return "", r.refuseCheck(event)
	}

	// 6. Admission; the deferred release is the backstop for every return.
	r.mu.RLock()
	admitter := r.admitter
	r.mu.RUnlock()
	release := noopRelease
	if admitter != nil {
		release, err = admitter.acquireSlot(ctx, key)
		if err != nil {
			return "", err
		}
	}
	release = sync.OnceFunc(release)
	defer release()
	if state, done, err := r.toolCallFromLog(ctx, key, binding, digest); done {
		return state, err
	}

	// 7. A panic inside the prober spends the whole reservation: a request may
	// already have been sent (the streamed prober counts one, then calls the
	// host's usage observer, which can panic), so nothing is refunded. Once the
	// prober returns, its count is known first; a later panic refunds the rest.
	reserved, unsent := 0, 0
	defer func() {
		if unsent > 0 {
			r.refundAllowance(unsent)
		}
	}()
	if generation {
		remaining, ok := r.reserveAllowance(recipe.MaxRequests)
		if !ok {
			return "", r.refuseCheck(event)
		}
		reserved = recipe.MaxRequests
		event.Stage, event.ReservedRequests, event.RemainingRequests = probeStageStarted, reserved, remaining
		r.emitProgress(event)
	}

	// 8.
	result, err := prober.ProbeCapabilities(ctx, key.Model, fingerprint.CapabilityProbeRequest{
		Method: recipe.Method, Capabilities: []string{capabilityToolCall},
	})
	sent := min(max(result.Requests, 0), reserved)
	unsent = reserved - sent
	completedAt := r.now()
	release()
	if generation {
		event.Stage, event.Requests = probeStageFinished, sent
		if err != nil && ctx.Err() != nil {
			event.Stage = probeStageCancelled
		}
		refund := unsent
		unsent = 0
		event.RemainingRequests = r.refundAllowance(refund)
		r.emitProgress(event)
	}
	if err != nil {
		return "", err
	}

	// 9.
	rows, state, err := probeEvidenceRows(key, binding, digest, recipe, result, completedAt)
	if err != nil {
		return "", err
	}
	// 10. The cached profile is dropped either way (keeping drops them all), so
	// phase 3's Lookup in the same call rebuilds it. A failed append, the
	// caller's cancellation included, keeps the rows.
	if err := r.evidenceStore.AppendEvidence(ctx, rows...); err != nil {
		r.keepUnsavedEvidence(rows)
	} else {
		r.invalidateProfile(key)
	}
	// 11.
	return state, nil
}

// keepUnsavedEvidence keeps, for the registry's life, the rows of a completed
// check whose append failed, so this process still reads what the check found.
// Each row is unchanged but for its ID, which counts up from math.MinInt64: a
// negative ID marks a row that was never stored, a stored row beats a kept one
// on an exact ObservedAt tie, and a later kept row beats an earlier one. Keeping
// the rows, dropping every cached profile and fencing builds in flight happen
// in one critical section with no store I/O, as syncGeneration does for a
// write. The write is not retried; another process sees nothing.
func (r *ModelRegistry) keepUnsavedEvidence(rows []fingerprint.EvidenceRow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range rows {
		// Valid only while kept rows are never dropped; dropping them needs a
		// separate counter, or IDs repeat and a later row can lose a tie.
		row.ID = math.MinInt64 + int64(len(r.kept))
		r.kept = append(r.kept, row)
	}
	clear(r.profiles)
	r.overrideVersion++
}

// toolCallFromLog is one read of the log for key's tool_call at digest. done
// means it settled the resolution: a definitive answer, an attempt still in its
// cooldown, or an error. Reserving and progress are the caller's.
func (r *ModelRegistry) toolCallFromLog(ctx context.Context, key ModelKey, binding EvidenceBinding, digest string) (state fingerprint.CapProbeState, done bool, err error) {
	view := r.readEvidence(ctx, key.Provider, []string{key.Model})
	// readEvidence keeps only a flag for a failed read, so a done caller is
	// checked first.
	if err := ctx.Err(); err != nil {
		return "", true, err
	}
	switch {
	case view.unbound, view.endpoint != binding.Endpoint, view.adapter != binding.Adapter:
		return "", true, ErrEvidenceUnbound
	case view.failed:
		return "", true, ErrEvidenceUnreadable
	}
	reading := view.readings(key.Model, digest)[capabilityToolCall]
	if answer := readingAnswer(reading); answer != nil {
		if answer.State == fingerprint.CapProbeYes {
			// The profile that sent the caller here was built without this row.
			r.invalidateProfile(key)
		}
		return answer.State, true, nil
	}
	// An attempt with no ExpiresAt (ExpiryFor never writes one; another writer
	// might) holds its cooldown indefinitely, as the explanation reads it.
	if a := reading.Attempt; a != nil && (a.ExpiresAt.IsZero() || !view.now.After(a.ExpiresAt)) {
		return fingerprint.CapProbeInconclusive, true, nil
	}
	return "", false, nil
}

// probeEvidenceRows builds one row per answer of a completed check, observed at
// completedAt on the store's millisecond, and returns the tool_call state. A
// result with no valid tool_call answer is an error: the requests it sent stay
// spent, nothing is written and no cooldown starts.
func probeEvidenceRows(key ModelKey, binding EvidenceBinding, digest string, recipe fingerprint.ProbeRecipe, result fingerprint.CapabilityProbeResult, completedAt time.Time) ([]fingerprint.EvidenceRow, fingerprint.CapProbeState, error) {
	observedAt := storeMillis(completedAt)
	var state fingerprint.CapProbeState
	rows := make([]fingerprint.EvidenceRow, 0, len(result.Answers))
	for _, a := range result.Answers {
		row := fingerprint.EvidenceRow{
			KeyVersion: fingerprint.EvidenceKeyVersion, Endpoint: binding.Endpoint, Adapter: binding.Adapter,
			Model: key.Model, Digest: digest, Capability: a.Capability,
			Source: a.Source, Kind: a.Kind, State: a.State,
			Method: recipe.Method, MethodVersion: recipe.Version, Detail: a.Detail,
			ObservedAt: observedAt,
		}
		freshUntil, expiresAt := fingerprint.ExpiryFor(row, a.TTL)
		row.FreshUntil, row.ExpiresAt = storeMillis(freshUntil), storeMillis(expiresAt)
		rows = append(rows, row)
		if a.Capability == capabilityToolCall {
			state = a.State
		}
	}
	if !validCapProbeState(state) {
		return nil, "", fmt.Errorf("provider: probe tool_call %v returned no valid tool_call answer", key)
	}
	return rows, state, nil
}

// storeMillis truncates t to the store's millisecond precision; zero stays zero.
func storeMillis(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return time.UnixMilli(t.UnixMilli())
}
