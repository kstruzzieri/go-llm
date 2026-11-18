// Package evidencesel picks, from one evidence snapshot, the digest and rows
// that answer for one model through one binding. The capability explanation
// and check planning both call it, so the two read a model's evidence alike.
// It takes values only: no context, no store, no I/O.
package evidencesel

import (
	"slices"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// Select returns the digest model's evidence is read at and the rows that
// count for it. The digest is that of the listing member named exactly model,
// else of the first member under its other Ollama spelling
// (provider.EvidenceModelNames), else "" (name-only); it is "" when there is no
// listing or credentialResolved is false, since an unresolved credential owns
// no listing. The rows are those at binding's endpoint under any of model's
// spellings, in snapshot order.
func Select(snap fingerprint.EvidenceSnapshot, binding provider.EvidenceBinding, model string, credentialResolved bool) (digest string, rows []fingerprint.EvidenceRow) {
	if credentialResolved {
		digest = ListingDigest(snap.Listing, binding.Adapter, model)
	}
	return digest, IndexRows(snap.Rows, binding, []string{model})[model]
}

// ListingDigest returns the first exact listing member's digest, falling back
// to the first member under another spelling when the exact name is absent.
func ListingDigest(listing *fingerprint.ListingSnapshot, adapter, model string) string {
	if listing == nil {
		return ""
	}
	names := provider.EvidenceModelNames(adapter, model)
	var aliasDigest string
	aliased := false
	for _, m := range listing.Members {
		if m.Model == model {
			return m.Digest
		}
		if !aliased && slices.Contains(names, m.Model) {
			aliasDigest, aliased = m.Digest, true
		}
	}
	return aliasDigest
}

// IndexRows groups the requested models' evidence in one pass over the
// snapshot. Each group includes all of the model's spellings at the bound
// endpoint, through every adapter, in the order the snapshot returned them.
func IndexRows(rows []fingerprint.EvidenceRow, binding provider.EvidenceBinding, models []string) map[string][]fingerprint.EvidenceRow {
	if len(rows) == 0 || len(models) == 0 {
		return nil
	}
	requested := make(map[string][]string, len(models))
	for _, model := range models {
		for _, name := range provider.EvidenceModelNames(binding.Adapter, model) {
			if !slices.Contains(requested[name], model) {
				requested[name] = append(requested[name], model)
			}
		}
	}
	byModel := make(map[string][]fingerprint.EvidenceRow, len(models))
	for _, row := range rows {
		if row.Endpoint == binding.Endpoint {
			for _, model := range requested[row.Model] {
				byModel[model] = append(byModel[model], row)
			}
		}
	}
	return byModel
}
