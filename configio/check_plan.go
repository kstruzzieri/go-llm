// configio/check_plan.go
//
// PlanCheck: what an explicit capability check of one provider would send,
// decided offline from the config and one evidence-store read, and sealed so a
// run can tell whether the plan still holds.
package configio

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/evidencesel"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
)

// maxCheckModels is the most distinct models one plan may hold.
const maxCheckModels = 25

// planSealVersion names the encoding sealPlan hashes.
const planSealVersion = "go-llm configio check plan v1"

// errPlanEntropy is returned, before anything is read or sent, when no random
// plan secret can be drawn.
var errPlanEntropy = errors.New("configio: plan check: no entropy for the plan secret")

// CheckOptions configures PlanCheck.
type CheckOptions struct {
	// Config is the effective config the host's providers were built from, its
	// URL overrides applied. Required.
	Config *config.Config
	// Store supplies the key's listing, the endpoint's named models and the
	// stored answers. Nil plans from the config alone, so only authored models
	// can be planned.
	Store fingerprint.EvidenceReader
	// Force plans models whose requested answers are all fresh.
	Force bool
	// RuntimeDigests maps a requested model ID, spelled exactly as requested,
	// to the content digest the host's runtime reports for it. Optional.
	// Freshness is judged at that digest instead of the listing member's. A
	// value must be nonempty and hold no NUL; entries for models not requested
	// are ignored. The plan keeps a copy; it is not part of the identity.
	RuntimeDigests map[string]string
}

// CheckItem is one planned model: what its check asks, and what it may send.
type CheckItem struct {
	Model        string   `json:"model"`
	Capabilities []string `json:"capabilities"` // requested, in canonical order
	Methods      []string `json:"methods"`      // probe methods, in the order they run
	// UsualRequests and MaxRequests count requests the check sends.
	UsualRequests int `json:"usual_requests"`
	MaxRequests   int `json:"max_requests"`
	// MaxOutputTokensPerRequest is the output limit requested of the server for
	// each request: not a limit enforced locally, nor a billing ceiling. Zero
	// when the method generates nothing.
	MaxOutputTokensPerRequest int `json:"max_output_tokens_per_request"`
	// ModelLoads is the model loads the check can predict: 1 for a generating
	// check at a loopback endpoint, else 0. A remote server may load models
	// too.
	ModelLoads int `json:"model_loads"`
	// RequestTimeout bounds each request: the method's timeout, or the
	// provider's when that is shorter. A caller's deadline can cut it further.
	// JSON encodes it as Go's time.Duration does: an integer of nanoseconds.
	RequestTimeout time.Duration `json:"request_timeout"`
	// MaxResponseBytes bounds each decoded response body, framing included.
	MaxResponseBytes int64 `json:"max_response_bytes"`
}

// CheckPlan is what an explicit capability check of one provider will send.
// Its exported fields are a display copy: changing them changes nothing. The
// value PlanCheck returns also holds the inputs it was planned from, frozen and
// never a credential, and its identity. A plan built from its fields or
// decoded from JSON has neither, so a run refuses it as stale.
type CheckPlan struct {
	Provider     string      `json:"provider"`
	EndpointHost string      `json:"endpoint_host"` // host[:port] of the canonical endpoint; never a path
	Adapter      string      `json:"adapter"`
	Force        bool        `json:"force"`
	Items        []CheckItem `json:"items"` // planned models, sorted by ID; empty when none needs a check

	state *planState // a pointer, so printing a plan shows none of it
}

// Identity returns the plan's opaque identity: a hash, under a secret drawn for
// this plan alone, of everything it was planned from, the provider's key
// included. No two plans share one. It is "" for a plan PlanCheck did not
// return.
func (p CheckPlan) Identity() string {
	if p.state == nil {
		return ""
	}
	return p.state.identity
}

// checkTarget is what a check takes from the config about its provider, the
// key aside.
type checkTarget struct {
	provider string
	binding  provider.EvidenceBinding
	local    bool          // the endpoint is a literal loopback address
	timeout  time.Duration // the provider's effective per-request timeout
}

// planItem is one planned model, frozen with the recipe of each of its
// methods.
type planItem struct {
	item    CheckItem
	recipes []fingerprint.ProbeRecipe // parallel to item.Methods
}

// planInputs is everything a plan's identity seals, the key aside.
type planInputs struct {
	target   checkTarget
	models   []string                       // requested: unique, sorted, byte for byte as given
	caps     provider.Capability            // requested
	policies []config.ModelCapabilityPolicy // effective, parallel to models
	items    []planItem                     // planned, in model order
	force    bool
}

// planState is a plan's frozen state. It never holds a credential.
type planState struct {
	inputs         planInputs
	runtimeDigests map[string]string // requested model -> runtime digest; not sealed
	secret         [32]byte
	identity       string
}

// PlanCheck decides, offline, what an explicit check of caps on models at
// providerName would send. It sends nothing: it reads the config and makes at
// most one opts.Store.ReadSnapshot call.
//
// In order, it refuses with invalid_argument: a nil context or config; an
// unknown provider, one whose destination cannot be built (an empty or
// control-character name, an invalid base URL), an unknown api_format, or a
// key still holding an unexpanded ${...}; a zero mask or unknown capability
// bits; no models, or a model ID that is empty, whitespace only, holds a NUL or
// is not valid UTF-8; and an empty or NUL-bearing runtime digest for a
// requested model. IDs are kept byte for byte, exact duplicates dropped and
// the rest sorted; more than 25 is batch_too_large. Then caps must be answered
// by one method of the provider's API (Ollama: any capability, by
// ollama-show; openai-compat: chat and stream by chat-stream, with tool_call by
// chat-stream-tools), and every model's capability policy must be valid, else
// invalid_argument.
//
// It reads one snapshot, at one instant, for every model under both Ollama
// spellings. Every model must be listed for the key or named for the endpoint
// (under either spelling), or authored in the config (exactly), else
// listing_required. A model whose policy has an explicit list is never
// planned, even under force. Any other is planned when a requested capability
// lacks a fresh answer read at its runtime digest, else at its listing
// member's: a probe, observed or provider answer, yes or no alike, within its
// FreshUntil when set. Catalog and type knowledge never count. Force plans it
// regardless. An empty plan is valid.
//
// A failed read is an error wrapping provider.ErrEvidenceUnreadable, never the
// store's text. A context done before or during the read returns its error,
// uncoded. The resolved key seals the plan's identity and is never kept.
func PlanCheck(ctx context.Context, providerName string, models []string, caps provider.Capability, opts CheckOptions) (CheckPlan, error) {
	if ctx == nil {
		return CheckPlan{}, invalidPlan("nil context")
	}
	target, pc, err := resolveCheckTarget(opts.Config, providerName)
	if err != nil {
		return CheckPlan{}, err
	}
	apiKey := pc.APIKey
	if caps == 0 || caps&^provider.CanonicalCaps() != 0 {
		return CheckPlan{}, invalidPlan("capabilities %#x are empty or unknown", uint32(caps))
	}
	if len(models) == 0 {
		return CheckPlan{}, invalidPlan("no models")
	}
	for _, m := range models {
		switch {
		case strings.TrimSpace(m) == "":
			return CheckPlan{}, invalidPlan("empty model ID")
		case strings.ContainsRune(m, 0):
			return CheckPlan{}, invalidPlan("model ID %q holds a NUL", m)
		case !utf8.ValidString(m):
			return CheckPlan{}, invalidPlan("model ID %q is not valid UTF-8", m)
		}
		if d, ok := opts.RuntimeDigests[m]; ok && (d == "" || strings.ContainsRune(d, 0)) {
			return CheckPlan{}, invalidPlan("runtime digest for %q is empty or holds a NUL", m)
		}
	}
	ids := slices.Compact(slices.Sorted(slices.Values(models)))
	if len(ids) > maxCheckModels {
		return CheckPlan{}, wrapCode(CodeBatchTooLarge,
			fmt.Errorf("configio: plan check: %d models; at most %d", len(ids), maxCheckModels))
	}
	recipe, ok := checkRecipe(target.binding.Adapter, caps)
	if !ok {
		return CheckPlan{}, invalidPlan("%s checks cannot answer %s", target.binding.Adapter, caps)
	}
	policies := make([]config.ModelCapabilityPolicy, len(ids))
	for i, m := range ids {
		if policies[i], err = opts.Config.CapabilityPolicy(providerName, m); err != nil {
			return CheckPlan{}, wrapCode(CodeInvalidArgument, fmt.Errorf("configio: plan check: %w", err))
		}
	}
	if err := ctx.Err(); err != nil {
		return CheckPlan{}, err
	}

	now := time.Now()
	var snap fingerprint.EvidenceSnapshot
	if opts.Store != nil {
		var spellings []string
		for _, m := range ids {
			spellings = append(spellings, provider.EvidenceModelNames(target.binding.Adapter, m)...)
		}
		slices.Sort(spellings)
		key := apiKey
		var readErr error
		snap, readErr = opts.Store.ReadSnapshot(ctx, target.binding.Endpoint, target.binding.Adapter, &key, "", slices.Compact(spellings), now)
		if err := ctx.Err(); err != nil {
			return CheckPlan{}, err
		}
		if readErr != nil {
			return CheckPlan{}, fmt.Errorf("configio: plan check: %w", provider.ErrEvidenceUnreadable)
		}
	}

	digests := make(map[string]string)
	var items []planItem
	for i, m := range ids {
		if !policies[i].Authored && !listedOrNamed(snap, target.binding.Adapter, m) {
			return CheckPlan{}, wrapCode(CodeListingRequired,
				fmt.Errorf("configio: plan check: model %q is not listed for this key, named for this endpoint or authored", m))
		}
		digest, hasDigest := opts.RuntimeDigests[m]
		if hasDigest {
			digests[m] = digest
		}
		if policies[i].Explicit != nil {
			continue // an explicit list fixes every capability
		}
		if !opts.Force && answersFresh(snap, target.binding, m, digest, policies[i], caps, now) {
			continue
		}
		items = append(items, newPlanItem(target, m, caps, recipe))
	}

	st := &planState{
		inputs: planInputs{
			target: target, models: ids, caps: caps, policies: policies, items: items, force: opts.Force,
		},
		runtimeDigests: digests,
	}
	// ponytail: crypto/rand.Read does not fail on supported platforms; the
	// check stays so a future failure cannot seal a plan under a zero secret.
	if _, err := rand.Read(st.secret[:]); err != nil {
		return CheckPlan{}, errPlanEntropy
	}
	st.identity = sealPlan(st.secret, apiKey, &st.inputs)

	plan := CheckPlan{
		Provider: target.provider, EndpointHost: provider.EndpointHost(target.binding.Endpoint),
		Adapter: target.binding.Adapter, Force: opts.Force, Items: make([]CheckItem, 0, len(items)), state: st,
	}
	for _, it := range items {
		shown := it.item
		shown.Capabilities, shown.Methods = slices.Clone(shown.Capabilities), slices.Clone(shown.Methods)
		plan.Items = append(plan.Items, shown)
	}
	return plan, nil
}

func invalidPlan(format string, args ...any) error {
	return wrapCode(CodeInvalidArgument, fmt.Errorf("configio: plan check: "+format, args...))
}

// resolveCheckTarget reads providerName's target from cfg, with the one
// provider config it was read from, whose APIKey is the resolved key. Every
// failure is invalid_argument, and none quotes the key or base URL.
func resolveCheckTarget(cfg *config.Config, providerName string) (checkTarget, config.ProviderConfig, error) {
	if cfg == nil {
		return checkTarget{}, config.ProviderConfig{}, invalidPlan("nil config")
	}
	pc, ok := cfg.Providers[providerName]
	if !ok {
		return checkTarget{}, config.ProviderConfig{}, invalidPlan("unknown provider %q", providerName)
	}
	dest, err := provider.NewDestination(providerName, pc.BaseURL)
	if err != nil {
		return checkTarget{}, config.ProviderConfig{}, wrapCode(CodeInvalidArgument, fmt.Errorf("configio: plan check: %w", err))
	}
	binding, err := provider.EvidenceBindingFor(pc.BaseURL, pc.APIFormat)
	if err != nil {
		return checkTarget{}, config.ProviderConfig{}, wrapCode(CodeInvalidArgument, fmt.Errorf("configio: plan check: provider %q: %w", providerName, err))
	}
	if strings.Contains(pc.APIKey, "${") {
		return checkTarget{}, config.ProviderConfig{}, invalidPlan("provider %q: the key holds an unexpanded ${...} reference", providerName)
	}
	return checkTarget{
		provider: providerName, binding: binding, local: dest.IsLocal(), timeout: providerbootstrap.ProviderTimeout(pc),
	}, pc, nil
}

// checkRecipe is the recipe that answers caps through adapter: ollama-show for
// Ollama; chat-stream-tools when openai-compat is asked for tool_call, else
// chat-stream. False when that recipe cannot answer every requested
// capability.
func checkRecipe(adapter string, caps provider.Capability) (fingerprint.ProbeRecipe, bool) {
	method := fingerprint.MethodOllamaShow
	if adapter == fingerprint.AdapterOpenAICompat {
		method = fingerprint.MethodChatStream
		if caps.Has(provider.CapToolCall) {
			method = fingerprint.MethodChatStreamTools
		}
	}
	recipe, ok := fingerprint.RecipeFor(method)
	if !ok || recipe.Adapter != adapter {
		return fingerprint.ProbeRecipe{}, false
	}
	for _, name := range caps.Names() {
		if !slices.Contains(recipe.Capabilities, name) {
			return fingerprint.ProbeRecipe{}, false
		}
	}
	return recipe, true
}

// newPlanItem is model's check of caps by recipe, with the limits t enforces.
func newPlanItem(t checkTarget, model string, caps provider.Capability, recipe fingerprint.ProbeRecipe) planItem {
	recipe.Capabilities = slices.Clone(recipe.Capabilities)
	loads := 0
	if recipe.MaxOutputTokensPerRequest > 0 && t.local {
		loads = 1 // a local server may load the model to generate
	}
	return planItem{
		item: CheckItem{
			Model: model, Capabilities: caps.Names(), Methods: []string{recipe.Method},
			UsualRequests: recipe.UsualRequests, MaxRequests: recipe.MaxRequests,
			MaxOutputTokensPerRequest: recipe.MaxOutputTokensPerRequest, ModelLoads: loads,
			RequestTimeout: min(recipe.RequestTimeout, t.timeout), MaxResponseBytes: recipe.MaxResponseBytes,
		},
		recipes: []fingerprint.ProbeRecipe{recipe},
	}
}

// listedOrNamed reports whether snap lists model for the key, or names it for
// the endpoint, under any of its spellings through adapter.
func listedOrNamed(snap fingerprint.EvidenceSnapshot, adapter, model string) bool {
	names := provider.EvidenceModelNames(adapter, model)
	if snap.Listing != nil && slices.ContainsFunc(snap.Listing.Members, func(m fingerprint.ListingMember) bool {
		return slices.Contains(names, m.Model)
	}) {
		return true
	}
	return slices.ContainsFunc(snap.Named, func(n fingerprint.NamedModel) bool { return slices.Contains(names, n.Model) })
}

// answersFresh reports whether every capability in caps has a fresh answer
// for model through binding, read at runtimeDigest when nonempty, else at the
// listing member's digest.
func answersFresh(snap fingerprint.EvidenceSnapshot, binding provider.EvidenceBinding, model, runtimeDigest string, policy config.ModelCapabilityPolicy, caps provider.Capability, now time.Time) bool {
	digest, rows := evidencesel.Select(snap, binding, model, true)
	if runtimeDigest != "" {
		digest = runtimeDigest
	}
	facts := provider.ExplainEvidence(provider.EvidenceInputs{
		Adapter: binding.Adapter, Digest: digest, Now: now,
		Explicit: policy.Explicit, TypeCaps: policy.TypeCaps, TypeNames: policy.TypeNames,
		Rows: rows,
	})
	requested := caps.Names()
	fresh := 0
	for _, f := range facts {
		if !slices.Contains(requested, f.Capability) {
			continue
		}
		if !factFresh(f, now) {
			return false
		}
		fresh++
	}
	return fresh == len(requested)
}

// factFresh reports whether f was decided by a stored definitive answer that
// is fresh at now, as fingerprint.EvidenceRow.Fresh judges the row.
func factFresh(f provider.CapabilityFact, now time.Time) bool {
	switch fingerprint.EvidenceSource(f.Source) {
	case fingerprint.EvidenceSourceProbe, fingerprint.EvidenceSourceObserved, fingerprint.EvidenceSourceProvider:
	default:
		return false
	}
	definitive := f.State == string(fingerprint.CapProbeYes) || f.State == string(fingerprint.CapProbeNo)
	return definitive && (f.FreshUntil.IsZero() || !f.FreshUntil.Before(now))
}

// sealPlan is a plan's identity: HMAC-SHA256 under secret of a versioned
// encoding of apiKey and in, hex encoded. Every string and list is
// length-prefixed, so distinct inputs never encode alike. The key is hashed and
// never kept. It is the one place the identity is computed: to re-verify a
// plan, seal current inputs under the plan's secret and compare.
func sealPlan(secret [32]byte, apiKey string, in *planInputs) string {
	w := sealWriter{hmac.New(sha256.New, secret[:])}
	w.str(planSealVersion)
	w.str(in.target.provider)
	w.str(in.target.binding.Endpoint)
	w.str(in.target.binding.Adapter)
	w.str(apiKey)
	w.flag(in.target.local)
	w.num(int64(in.target.timeout))
	w.strs(in.models)
	w.num(int64(in.caps))
	w.num(int64(len(in.policies)))
	for _, p := range in.policies {
		w.flag(p.Authored)
		w.strs(p.Explicit)
		w.strs(p.Derived)
		w.strs(p.TypeNames)
		w.num(int64(p.TypeCaps))
	}
	w.num(int64(len(in.items)))
	for _, it := range in.items {
		c := it.item
		w.str(c.Model)
		w.strs(c.Capabilities)
		w.strs(c.Methods)
		for _, n := range []int64{int64(c.UsualRequests), int64(c.MaxRequests), int64(c.MaxOutputTokensPerRequest),
			int64(c.ModelLoads), int64(c.RequestTimeout), c.MaxResponseBytes} {
			w.num(n)
		}
		w.num(int64(len(it.recipes)))
		for _, r := range it.recipes {
			w.str(r.Method)
			w.str(r.Adapter)
			w.num(int64(r.Version))
			w.strs(r.Capabilities)
			for _, n := range []int64{int64(r.UsualRequests), int64(r.MaxRequests), int64(r.MaxOutputTokensPerRequest),
				int64(r.RequestTimeout), r.MaxResponseBytes} {
				w.num(n)
			}
			w.str(r.Message)
			w.str(r.ToolName)
			w.str(r.ToolDescription)
			w.str(r.ToolParameters)
			w.flag(r.RequiredToolChoiceFirst)
		}
	}
	w.flag(in.force)
	return hex.EncodeToString(w.h.Sum(nil))
}

// sealWriter writes length-prefixed values into a hash.
type sealWriter struct{ h hash.Hash }

func (w sealWriter) num(v int64) { w.h.Write(binary.BigEndian.AppendUint64(nil, uint64(v))) }

func (w sealWriter) str(s string) {
	w.num(int64(len(s)))
	w.h.Write([]byte(s))
}

func (w sealWriter) strs(ss []string) {
	w.num(int64(len(ss)))
	for _, s := range ss {
		w.str(s)
	}
}

func (w sealWriter) flag(b bool) {
	if b {
		w.num(1)
	} else {
		w.num(0)
	}
}
