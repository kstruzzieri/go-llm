// Package openaicompat implements provider.Provider against OpenAI-compatible
// local model servers (LM Studio, llama.cpp --api, vLLM, text-generation-inference,
// and similar). It speaks the OpenAI REST/SSE wire format:
//
//	POST /v1/chat/completions   chat (sync + streaming)
//	POST /v1/completions        raw generation + FIM via "suffix"
//	POST /v1/embeddings         vector embeddings
//	GET  /v1/models             model discovery
//
// The package is the inbound client-side complement to the repo's outbound
// compat/ package (which exposes go-llm AS an OpenAI server). The two have
// no shared types by design: compat/ depends on provider/, so importing it
// from a provider sub-package would invert the layering. Wire shapes are
// re-declared here with only the fields this client actually consumes.
//
// Intended wiring: callers that construct providers from config.ProviderConfig
// should map APIFormat == "openai-compat" to this backend. Pass a per-instance
// name via WithProviderName when more than one OpenAI-compat server is
// registered (e.g. "lmstudio-laptop" alongside "vllm-workstation"), matching
// the OllamaProvider naming convention.
//
// Capability semantics: OpenAI-compat servers vary widely in what they
// actually implement. The constructor advertises a permissive default
// (CapChat|CapGenerate|CapStream|CapEmbed|CapToolCall); users should carve
// down per model via config.ModelConfig.Capabilities for backends missing
// a specific endpoint (e.g. removing "generate" for a server without
// /v1/completions). Native FIM detection (CapInsert) is intentionally NOT
// in the default set because OpenAI-compat servers don't expose template
// metadata equivalent to Ollama's /api/show — opt in explicitly when known.
package openaicompat
