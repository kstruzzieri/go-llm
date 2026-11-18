// configio/check_run.go
//
// RunCheck: send what a plan showed and nothing else, through a client guarded
// by a grant for one destination, and file each answer in the evidence log.
package configio

import (
	"cmp"
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/fingerprint/probers"
	"github.com/kstruzzieri/go-llm/internal/evidencesel"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// remoteCheckWorkers is how many checks run at once at a remote endpoint
// through openai-compat; a loopback one runs one at a time, so a local server
// loads one model at a time.
const remoteCheckWorkers = 4

// CheckAnswer is one capability's outcome in a check result.
type CheckAnswer struct {
	Capability string `json:"capability"`       // canonical name
	State      string `json:"state"`            // yes, no or inconclusive
	Detail     string `json:"detail,omitempty"` // a bounded code; "" when none
}

// CheckResult is one planned model's finished check. Code is "" when the check
// answered, else why it did not, and Answers is then empty. Requests counts the
// requests the check sent. Persisted is true only once the answers were
// committed to the run's store. A result never carries provider text.
type CheckResult struct {
	Model     string        `json:"model"`
	Method    string        `json:"method"`
	Code      ErrorCode     `json:"code,omitempty"`
	Requests  int           `json:"requests"`
	Answers   []CheckAnswer `json:"answers,omitempty"`
	Persisted bool          `json:"persisted"`
}

// RunOptions configures RunCheck.
type RunOptions struct {
	// Config is the current effective config, URL overrides applied. The
	// plan's provider, endpoint, api, key, timeout and capability policies are
	// read from it again and must be the ones the plan was sealed with.
	// Required.
	Config *config.Config
	// Store receives the answers. Nil checks and saves nothing.
	Store fingerprint.EvidenceStore
	// Policy grants remote destinations; a loopback one needs no grant.
	Policy provider.DestinationPolicy
	// OnResult receives each checked model's result once its answers are
	// committed. Calls never overlap each other or OnUsage. May be nil.
	OnResult func(CheckResult)
	// OnUsage receives the usage of each completed generation as it completes,
	// before anything is saved. Calls never overlap each other or OnResult. May
	// be nil.
	OnUsage func(model string, u provider.Usage)
	// Transport is the base transport of the check's client; nil uses a copy
	// of the default. Every request still passes the destination guard, and a
	// loopback endpoint gets a clone without its proxy that dials localhost as
	// the loopback addresses. A remote endpoint uses it as is, and RunCheck
	// leaves its idle connections open.
	Transport *http.Transport
}

// RunCheck sends the checks plan showed, and nothing else, and files each
// answer in opts.Store. It never touches a model registry.
//
// Before anything is sent it refuses: a nil context or config, with
// invalid_argument; a plan PlanCheck did not return, one whose displayed
// fields were changed, or one the current config no longer resolves to (its
// provider, endpoint, api, key, timeout or capability policies, or this
// binary's recipes), with plan_stale; and a remote endpoint opts.Policy does
// not grant, with destination_not_granted. With a store it then reads one
// snapshot: an unforced run skips a model whose requested answers have become
// fresh, and no run ever adds a model. A failed read is an error wrapping
// provider.ErrEvidenceUnreadable.
//
// The remaining models are checked at most 4 at once at a remote openai-compat
// endpoint, one at a time at a loopback one, and all at once through Ollama,
// whose checks read metadata. A failed check is its model's result, with a
// code, and never stops another; a 403 or 404 is refused, and nothing is saved
// for it. A usable result's rows are appended in one transaction, filed under
// the first nonempty of: the digest the reply named, the plan's runtime digest
// for the model, the listing digest the snapshot holds, else "". With none of
// the first three a row is name-only, and a reader at a live digest ignores
// it; a model re-pulled between the host's listing and a check whose reply
// names no digest gets its new answer filed under the old digest.
//
// Cancellation stops scheduling. A check that returns after ctx is done is
// neither saved nor reported, and RunCheck returns ctx's error, uncoded.
// Otherwise it returns nil once every check has finished; no worker outlives
// the call. Idle connections of a transport the run made are closed before it
// returns; a host's remote Transport keeps its own.
func RunCheck(ctx context.Context, plan CheckPlan, opts RunOptions) error {
	if ctx == nil {
		return wrapCode(CodeInvalidArgument, errors.New("configio: run check: nil context"))
	}
	if opts.Config == nil {
		return wrapCode(CodeInvalidArgument, errors.New("configio: run check: nil config"))
	}
	st, apiKey, err := verifyPlan(plan, opts.Config)
	if err != nil {
		return err
	}
	in := &st.inputs
	pc := opts.Config.Providers[in.target.provider]
	dest, err := provider.NewDestination(in.target.provider, pc.BaseURL)
	if err != nil { // resolveCheckTarget built it a moment ago
		return wrapCode(CodePlanStale, fmt.Errorf("configio: run check: %w", err))
	}
	manifest, err := provider.NewDestinationManifest(provider.DestinationEdge{Purpose: provider.DestinationPurposeCapabilityProbe, Destination: dest})
	if err != nil {
		return wrapCode(CodePlanStale, fmt.Errorf("configio: run check: %w", err))
	}
	gate := provider.NewDestinationGate()
	if err := gate.Install(opts.Policy, manifest); err != nil {
		return wrapCode(CodeDestinationNotGranted, fmt.Errorf("configio: run check: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	r := &checkRun{
		binding: in.target.binding, provider: in.target.provider, apiKey: apiKey,
		gate: gate, digests: st.runtimeDigests, onResult: opts.OnResult,
	}
	items := in.items
	if opts.Store != nil && len(items) > 0 {
		r.store = opts.Store
		if items, err = r.preflight(ctx, opts.Store, in); err != nil {
			return err
		}
	}
	if len(items) == 0 {
		return nil
	}

	prov, client, closeIdle, err := providerbootstrap.BuildGuardedProvider(in.target.provider, pc, gate, dest, nil, opts.Transport)
	if err != nil { // a transport the guard cannot use, such as a TLS dial hook for loopback
		return wrapCode(CodeInvalidArgument, fmt.Errorf("configio: run check: %w", err))
	}
	defer closeIdle() // after every worker: the run's own connections, never a host transport's
	factory := probers.ProberFactoryInput{APIFormat: pc.APIFormat, OllamaClient: client}
	factory.OpenAICompatProvider, _ = prov.(*openaicompat.Provider)
	if opts.OnUsage != nil {
		factory.UsageObserver = func(model string, u provider.Usage) {
			r.cb.Lock()
			defer r.cb.Unlock()
			opts.OnUsage(model, u)
		}
	}
	mp, err := probers.NewProberForAPIFormat(factory)
	if err != nil {
		return wrapCode(CodePlanStale, fmt.Errorf("configio: run check: %w", err))
	}
	var ok bool
	if r.prober, ok = mp.(fingerprint.CapabilityProber); !ok {
		return wrapCode(CodePlanStale, errors.New("configio: run check: the prober cannot run a check recipe"))
	}

	var g errgroup.Group // no context: one model's failure is its result, not the others' cancellation
	g.SetLimit(checkWorkers(in.target, len(items)))
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error {
			r.check(ctx, it)
			return nil
		})
	}
	_ = g.Wait()
	return ctx.Err()
}

// errPlanStale is every stale plan's error; it says nothing of what changed.
var errPlanStale = errors.New("configio: run check: the plan no longer matches its provider, key or config")

// verifyPlan returns plan's frozen state and the current key, or plan_stale
// when plan is not one PlanCheck returned, its displayed fields were changed,
// or cfg no longer resolves to the inputs it was sealed from.
func verifyPlan(plan CheckPlan, cfg *config.Config) (*planState, string, error) {
	stale := wrapCode(CodePlanStale, errPlanStale)
	st := plan.state
	if st == nil || !shownMatches(plan, &st.inputs) {
		return nil, "", stale
	}
	target, apiKey, err := resolveCheckTarget(cfg, st.inputs.target.provider)
	if err != nil {
		return nil, "", stale
	}
	recipe, ok := checkRecipe(target.binding.Adapter, st.inputs.caps)
	if !ok {
		return nil, "", stale
	}
	cur := planInputs{target: target, models: st.inputs.models, caps: st.inputs.caps, force: st.inputs.force}
	for _, m := range cur.models {
		policy, err := cfg.CapabilityPolicy(target.provider, m)
		if err != nil {
			return nil, "", stale
		}
		cur.policies = append(cur.policies, policy)
	}
	for _, it := range st.inputs.items {
		cur.items = append(cur.items, newPlanItem(target, it.item.Model, cur.caps, recipe))
	}
	if !hmac.Equal([]byte(sealPlan(st.secret, apiKey, &cur)), []byte(st.identity)) {
		return nil, "", stale
	}
	return st, apiKey, nil
}

// shownMatches reports whether plan's displayed fields are the ones in was
// planned with.
func shownMatches(plan CheckPlan, in *planInputs) bool {
	if plan.Provider != in.target.provider || plan.EndpointHost != endpointHost(in.target.binding.Endpoint) ||
		plan.Adapter != in.target.binding.Adapter || plan.Force != in.force || len(plan.Items) != len(in.items) {
		return false
	}
	for i, shown := range plan.Items {
		if !reflect.DeepEqual(shown, in.items[i].item) {
			return false
		}
	}
	return true
}

// checkWorkers is how many of a run's checks may be in flight at once.
func checkWorkers(t checkTarget, items int) int {
	switch {
	case t.binding.Adapter == fingerprint.AdapterOllama:
		return items // metadata reads; the plan's 25-model cap bounds them
	case t.local:
		return 1
	}
	return remoteCheckWorkers
}

// checkRun is one RunCheck call's verified, granted state, shared by its
// workers.
type checkRun struct {
	binding  provider.EvidenceBinding
	provider string
	apiKey   string
	gate     *provider.DestinationGate
	prober   fingerprint.CapabilityProber
	store    fingerprint.EvidenceWriter
	digests  map[string]string            // the plan's runtime digests
	snap     fingerprint.EvidenceSnapshot // the pre-flight snapshot
	onResult func(CheckResult)
	cb       sync.Mutex // serializes the host's callbacks
}

// preflight reads one snapshot of the planned models and returns the items
// still to check: an unforced run drops a model whose requested answers have
// become fresh. It never adds one.
func (r *checkRun) preflight(ctx context.Context, store fingerprint.EvidenceReader, in *planInputs) ([]planItem, error) {
	var spellings []string
	for _, it := range in.items {
		spellings = append(spellings, provider.EvidenceModelNames(r.binding.Adapter, it.item.Model)...)
	}
	slices.Sort(spellings)
	key, now := r.apiKey, time.Now()
	snap, err := store.ReadSnapshot(ctx, r.binding.Endpoint, r.binding.Adapter, &key, "", slices.Compact(spellings), now)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("configio: run check: %w", provider.ErrEvidenceUnreadable)
	}
	r.snap = snap
	if in.force {
		return in.items, nil
	}
	var items []planItem
	for _, it := range in.items {
		m := it.item.Model
		i, _ := slices.BinarySearch(in.models, m) // planned models are among the sorted requested ones
		if !answersFresh(snap, r.binding, m, r.digests[m], in.policies[i], in.caps, now) {
			items = append(items, it)
		}
	}
	return items, nil
}

// checkOutcome is one model's finished check: the result to report and the
// rows that file its answers.
type checkOutcome struct {
	result CheckResult
	rows   []fingerprint.EvidenceRow
}

// check sends one planned model's check, then commits and reports it. A check
// that returns after the caller cancelled is neither saved nor reported.
func (r *checkRun) check(ctx context.Context, it planItem) {
	if ctx.Err() != nil {
		return
	}
	recipe := it.recipes[0] // a planned item has one method
	var res fingerprint.CapabilityProbeResult
	bound, err := r.gate.Bind(ctx, provider.DestinationPurposeCapabilityProbe, r.provider)
	if err == nil {
		res, err = r.prober.ProbeCapabilities(bound, it.item.Model, fingerprint.CapabilityProbeRequest{
			Method: recipe.Method, Capabilities: it.item.Capabilities,
		})
	}
	completedAt := time.Now()
	if ctx.Err() != nil {
		return
	}
	out := r.classify(it.item.Model, recipe, res, err, completedAt)
	r.commit(ctx, &out)
	if ctx.Err() != nil && !out.result.Persisted {
		return
	}
	r.report(out.result)
}

// classify turns a check that returned, the caller still live, into its
// outcome: a coded failure with nothing to save (a 403 or 404 is refused), or
// its answers and the rows that file them (fingerprint.AnswerRows).
func (r *checkRun) classify(model string, recipe fingerprint.ProbeRecipe, res fingerprint.CapabilityProbeResult, err error, completedAt time.Time) checkOutcome {
	out := checkOutcome{result: CheckResult{Model: model, Method: recipe.Method, Requests: max(res.Requests, 0)}}
	if err != nil {
		out.result.Code = checkCode(err)
		return out
	}
	listed, _ := evidencesel.Select(r.snap, r.binding, model, true)
	digest := cmp.Or(res.Digest, r.digests[model], listed)
	for _, a := range res.Answers {
		out.result.Answers = append(out.result.Answers, CheckAnswer{Capability: a.Capability, State: string(a.State), Detail: a.Detail})
	}
	out.rows = fingerprint.AnswerRows(r.binding.Endpoint, r.binding.Adapter, model, digest, recipe, res.Answers, completedAt)
	return out
}

// commit appends out's rows in one transaction and marks its result persisted
// once that commits. Nothing is written without a store or rows, or once the
// caller cancelled.
func (r *checkRun) commit(ctx context.Context, out *checkOutcome) {
	if r.store == nil || len(out.rows) == 0 || ctx.Err() != nil {
		return
	}
	if r.store.AppendCheckResult(ctx, r.binding.Endpoint, r.apiKey, out.result.Model, out.rows, nil) == nil {
		out.result.Persisted = true
	}
}

// report hands res to the host's callback, serialized with every other
// callback.
func (r *checkRun) report(res CheckResult) {
	if r.onResult == nil {
		return
	}
	r.cb.Lock()
	defer r.cb.Unlock()
	r.onResult(res)
}
