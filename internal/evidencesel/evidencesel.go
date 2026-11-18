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
	names := provider.EvidenceModelNames(binding.Adapter, model)
	if snap.Listing != nil && credentialResolved {
		aliased := false
		for _, m := range snap.Listing.Members {
			if m.Model == model {
				digest = m.Digest
				break
			}
			if !aliased && slices.Contains(names, m.Model) {
				digest, aliased = m.Digest, true
			}
		}
	}
	rows = make([]fingerprint.EvidenceRow, 0, len(snap.Rows))
	for _, r := range snap.Rows {
		if r.Endpoint == binding.Endpoint && slices.Contains(names, r.Model) {
			rows = append(rows, r)
		}
	}
	return digest, rows
}
