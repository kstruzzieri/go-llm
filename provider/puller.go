package provider

import "context"

// ModelPuller is an optional capability for providers that can download or
// install models on demand (e.g. Ollama's registry pull). Providers whose
// models are file-managed — llama.cpp/openai-compat GGUF files served by
// llama-server — do not implement it; callers must treat a failed type
// assertion as "pull unsupported for this backend".
type ModelPuller interface {
	// PullModel downloads the named model. fn receives progress updates;
	// pass nil to ignore progress.
	PullModel(ctx context.Context, name string, fn func(status string, completed, total int64)) error
}
