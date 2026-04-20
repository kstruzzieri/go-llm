package compat

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// ModelsListResponse is the OpenAI-shape response for GET /v1/models.
type ModelsListResponse struct {
	Object string        `json:"object"` // always "list"
	Data   []ModelObject `json:"data"`
}

// ModelObject is the OpenAI-shape model descriptor, extended with go-llm
// specific fields under the x_ prefix. Standard OpenAI SDKs ignore unknown
// fields, so these extensions are safe for mixed clients.
//
// Canonical entries (real provider/model pairs) populate Capabilities,
// ContextWindow, Quality, Resources, and Warm. Alias entries populate
// AliasFor only; clients should follow x_alias_for to look up the
// canonical entry for capability details.
type ModelObject struct {
	ID      string `json:"id"` // canonical provider/model selector, or alias name
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	Capabilities  []string           `json:"x_capabilities,omitempty"`
	ContextWindow int                `json:"x_context_window,omitempty"`
	Quality       string             `json:"x_quality,omitempty"`
	Resources     *ModelResourcesExt `json:"x_resources,omitempty"`
	Warm          bool               `json:"x_warm,omitempty"`
	AliasFor      string             `json:"x_alias_for,omitempty"`
}

// ModelResourcesExt reports resource requirements for a canonical model. All
// sizes are in gigabytes. Fields are omitted when zero; Resources itself is
// set to nil by handleModels when every field is zero so omitempty suppresses
// the whole object.
type ModelResourcesExt struct {
	RAMRequired     float64 `json:"ram_required_gb,omitempty"`
	RAMRecommended  float64 `json:"ram_recommended_gb,omitempty"`
	VRAMRecommended float64 `json:"vram_recommended_gb,omitempty"`
}

// handleModels implements GET /v1/models. It enumerates every model the
// server can route to (canonical provider/model pairs) plus every configured
// alias, and emits them in OpenAI's list-of-models shape with go-llm x_*
// extensions. Warm entries sort first so clients that enumerate models see
// the fastest ready-to-serve options before cold ones.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		writeError(w, http.StatusServiceUnavailable, "no_registry", "server has no model registry")
		return
	}

	profiles, err := s.registry.All(r.Context())
	if err != nil {
		// ModelRegistry.All does not wrap the underlying cancellation with %w
		// when every provider fails, so errors.Is(err, context.Canceled) on
		// the aggregate "all N providers failed" string returns false. Detect
		// cancellation directly via the request context and route through
		// writeCompatError so it maps to 499 (client closed) or 504 (deadline)
		// instead of a misleading 502. For genuine registry failures, keep the
		// specific registry_error code and log the cause so operators can see
		// why the list endpoint is flapping.
		if ctxErr := r.Context().Err(); ctxErr != nil {
			writeCompatError(w, ctxErr)
			return
		}
		log.Printf("compat: /v1/models registry error: %v", err)
		writeError(w, http.StatusBadGateway, "registry_error", err.Error())
		return
	}

	// Pre-size generously (canonical + aliases) and ensure the slice is
	// non-nil so an empty registry serializes as "data": [] not "data": null.
	out := make([]ModelObject, 0, len(profiles)+len(s.aliases))

	// Warmth lookup keyed by ModelKey. A model is warm when its entry
	// reports Loaded == true.
	warm := make(map[provider.ModelKey]bool)
	if s.router != nil {
		for _, wm := range s.router.WarmthSnapshot() {
			if wm.Info.Loaded {
				warm[wm.Key] = true
			}
		}
	}

	// Track canonical IDs and UpdatedAt so alias entries can reuse the
	// target's UpdatedAt instead of falling back to zero. We also keep a
	// secondary index keyed by bare model name so unqualified alias targets
	// (e.g. "gpt-3.5-turbo" -> "qwen3:8b") can still find a canonical
	// timestamp. Ambiguous bare names — the same model served by more than one
	// provider — are dropped below so we do not arbitrarily pick one
	// provider's UpdatedAt.
	canonicalUpdatedAt := make(map[string]time.Time, len(profiles))
	bareCount := make(map[string]int, len(profiles))
	bareUpdatedAt := make(map[string]time.Time, len(profiles))

	for _, p := range profiles {
		id := p.Key.String()
		canonicalUpdatedAt[id] = p.UpdatedAt
		bareCount[p.Key.Model]++
		bareUpdatedAt[p.Key.Model] = p.UpdatedAt

		obj := ModelObject{
			ID:            id,
			Object:        "model",
			Created:       p.UpdatedAt.Unix(),
			OwnedBy:       p.Provider,
			Capabilities:  splitCapabilities(p.Caps),
			ContextWindow: p.ContextWindow,
			Quality:       p.Quality.String(),
			Warm:          warm[p.Key],
		}
		if p.Resources.RAMRequired != 0 || p.Resources.RAMRecommended != 0 || p.Resources.VRAMRecommended != 0 {
			obj.Resources = &ModelResourcesExt{
				RAMRequired:     p.Resources.RAMRequired,
				RAMRecommended:  p.Resources.RAMRecommended,
				VRAMRecommended: p.Resources.VRAMRecommended,
			}
		}
		out = append(out, obj)
	}

	// Drop ambiguous bare-name entries: when a bare model name is advertised
	// by more than one provider, we cannot pick a single canonical UpdatedAt
	// without silently privileging one provider over another. Deleting the
	// key here makes unqualified alias Created fall back to 0, which is more
	// honest than an arbitrary choice.
	for model, n := range bareCount {
		if n > 1 {
			delete(bareUpdatedAt, model)
		}
	}

	// Deterministic alias ordering: Go map iteration is randomized, so sort
	// alias keys alphabetically before emitting.
	aliasKeys := make([]string, 0, len(s.aliases))
	for k := range s.aliases {
		aliasKeys = append(aliasKeys, k)
	}
	sort.Strings(aliasKeys)

	for _, aliasKey := range aliasKeys {
		target := s.aliases[aliasKey]
		// Normalize the target the same way resolveModel does: parse the
		// "provider/model" form when present; carry the bare model name
		// through otherwise. parseKey does not synthesize a provider for
		// unqualified inputs, so we do not invent one here either.
		tk := parseKey(target)
		var aliasFor string
		if tk.Provider != "" {
			aliasFor = tk.String()
		} else {
			aliasFor = tk.Model
		}

		// Use the canonical target's UpdatedAt when we can resolve it; fall
		// back to 0 otherwise. Disambiguation rules:
		//   1. Qualified target (provider/model): look up the canonical entry
		//      directly and use its UpdatedAt.
		//   2. Unqualified target (bare model) with exactly one canonical
		//      provider advertising that model: use that canonical's
		//      UpdatedAt.
		//   3. Unqualified target whose bare name is advertised by multiple
		//      providers: Created stays 0. Picking one provider's timestamp
		//      arbitrarily would mislead clients that follow x_alias_for to a
		//      different provider's canonical entry.
		var created int64
		if ts, ok := canonicalUpdatedAt[aliasFor]; ok {
			created = ts.Unix()
		} else if tk.Provider == "" {
			if ts, ok := bareUpdatedAt[tk.Model]; ok {
				created = ts.Unix()
			}
		}

		out = append(out, ModelObject{
			ID:       aliasKey,
			Object:   "model",
			Created:  created,
			OwnedBy:  "alias",
			AliasFor: aliasFor,
		})
	}

	sortModelsWarmFirst(out)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ModelsListResponse{Object: "list", Data: out})
}

// sortModelsWarmFirst orders entries so warm models come before cold ones and
// IDs ascend within each group. Uses a stable sort so any future refactor that
// introduces tied keys (e.g. multiple alias entries sharing an ID shape)
// preserves input order rather than flipping entries between calls. Extracted
// as a named helper so the ordering policy can be unit-tested without threading
// warmth through the router.
func sortModelsWarmFirst(models []ModelObject) {
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].Warm != models[j].Warm {
			return models[i].Warm
		}
		return models[i].ID < models[j].ID
	})
}
