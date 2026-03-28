package fingerprint

// InferKindFromCapabilities classifies using the explicit capabilities array
// from /api/show (available in Ollama 0.5.x+). This is the strongest signal.
//
// Detection order: embedding wins over completion for dual-capability models,
// since embedding models are rarer and more specific.
func InferKindFromCapabilities(capabilities []string) ModelKind {
	hasEmbedding := false
	hasCompletion := false
	for _, c := range capabilities {
		switch c {
		case "embedding":
			hasEmbedding = true
		case "completion":
			hasCompletion = true
		}
	}
	if hasEmbedding {
		return ModelKindEmbedding
	}
	if hasCompletion {
		return ModelKindChat
	}
	return ModelKindUnknown
}

// embeddingFamilies maps model families known to be embedding-only.
var embeddingFamilies = map[string]bool{
	"bert":       true,
	"nomic-bert": true,
}

// InferKind classifies using family/template heuristics as a fallback
// for older Ollama versions that don't expose capabilities.
func InferKind(family, template string) ModelKind {
	if embeddingFamilies[family] {
		return ModelKindEmbedding
	}
	if template != "" {
		return ModelKindChat
	}
	return ModelKindUnknown
}
