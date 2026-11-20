package provider

import "github.com/kstruzzieri/go-llm/fingerprint"

// OllamaListingFacts returns the capability answers an Ollama model listing
// reports: its capabilities array and the insert decision its template implies.
// It delegates to fingerprint.OllamaCapabilityFacts, which the native prober
// shares.
func OllamaListingFacts(info ModelInfo) map[string]fingerprint.CapProbeState {
	return fingerprint.OllamaCapabilityFacts(info.Capabilities, info.Template)
}

// ListingFactMasks returns the capability bits a set of answers says yes to and
// the bits it answers at all. Names outside the canonical vocabulary are ignored.
func ListingFactMasks(facts map[string]fingerprint.CapProbeState) (caps, known Capability) {
	for name, state := range facts {
		bit, ok := canonicalCapability(name)
		if !ok || (state != fingerprint.CapProbeYes && state != fingerprint.CapProbeNo) {
			continue
		}
		known |= bit
		if state == fingerprint.CapProbeYes {
			caps |= bit
		}
	}
	return caps, known
}
