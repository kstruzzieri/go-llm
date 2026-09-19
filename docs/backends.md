# Model backends

Full reference for configuring where go-llm sends requests: local llama.cpp
(via llama-swap or pinned `llama-server` processes), Ollama, and hosted
OpenAI-compatible APIs with your own key. The [README](../README.md#local-model-backends)
carries the summary; [Getting Started](GETTING_STARTED.md) is the first-run walkthrough.

## Local model backends

go-llm selects a backend per provider in `models.json` via the `api_format` field: `openai-compat` (llama.cpp, vLLM, LM Studio, any OpenAI `/v1` server) or `ollama` (native Ollama REST, the default when omitted). **llama.cpp is the recommended primary backend** for best local performance. The shipped `models.json` points the reference lineup at a single `openai-compat` provider; an `ollama` provider is kept as the supported alternative.

### llama.cpp via llama-swap (recommended)

A single `llama-server` process pins one model in memory, so running the whole lineup that way means one process (and one slice of VRAM) per model. [llama-swap](https://github.com/mostlygeek/llama-swap) is a tiny OpenAI-compatible proxy that fronts all of them on **one** port and starts/stops the right `llama-server` on demand from the requested model name — the same load-on-demand ergonomics as Ollama, with llama.cpp's performance and per-model flag control.

`llama-swap` config (`llama-swap.yaml`) — one entry per model:

```yaml
models:
  "gemma4:31b":
    cmd: llama-server -m /models/gemma4-31b.gguf --port ${PORT} -c 8192 -ngl 99 --jinja
  "qwen3.6:35b-a3b":
    cmd: llama-server -m /models/qwen3.6-35b-a3b.gguf --port ${PORT} -c 8192 -ngl 99 --jinja
  "qwen3-coder-next:latest":
    cmd: llama-server -m /models/qwen3-coder-next.gguf --port ${PORT} -c 8192 -ngl 99 --jinja
  "qwen3.5:9b-mtp":
    cmd: llama-server -m /models/qwen3.5-9b-mtp.gguf --port ${PORT} -c 8192 -ngl 99 --jinja
  "qwen3-embedding:8b":
    cmd: llama-server -m /models/qwen3-embedding-8b.gguf --port ${PORT} -c 8192 -ngl 99 --embeddings
```

Run `llama-swap --config llama-swap.yaml --listen 127.0.0.1:8080`, then point a single `openai-compat` provider at it (`base_url` is the server root — **no** `/v1` suffix; go-llm appends it). This is the shipped `models.json` shape:

```json
{
  "providers": {
    "llamacpp": { "base_url": "http://127.0.0.1:8080", "timeout": "5m", "api_format": "openai-compat", "slot_discovery": true },
    "ollama":   { "base_url": "http://localhost:11434", "timeout": "5m" }
  },
  "models": {
    "general":   { "name": "gemma4:31b", "provider": "llamacpp", "type": "dense" },
    "embedding": { "name": "qwen3-embedding:8b", "provider": "llamacpp", "type": "embedding" }
  }
}
```

The model `name` must match the `llama-swap` model key. Set the provider's `api_key` field only if the proxy requires a Bearer token. Models on a backend that lacks `/v1/completions` can carve their capability set down (e.g. `"capabilities": ["chat", "stream"]`).

`"slot_discovery": true` makes go-llm read the server's `/props` `total_slots` so future slot-aware admission can size concurrency to the backend. It is a per-provider opt-in (the library default is off) and belongs only on `openai-compat` providers backed by llama.cpp's `llama-server` or llama-swap — the shipped `models.json` enables it on the `llamacpp` provider because that config targets llama-swap. Leave it off for backends without `/props` (vLLM, LM Studio): an enabled backend that cannot answer `/props` is treated as having a single slot.

### llama.cpp without a proxy (pinned servers)

You can skip the proxy and run `llama-server` per model on its own port — useful when you want specific models hot at all times or per-model flags a proxy would complicate:

```bash
llama-server -m /path/to/model.gguf --host 127.0.0.1 --port 8091 \
  -c 8192 -ngl 99 --jinja --alias my-model
```

Then declare one `openai-compat` provider per port and point each model at its provider. The Router's circuit breakers and fallback chains route around any server that isn't running.

### Ollama (supported alternative)

```json
{ "providers": { "ollama": { "base_url": "http://localhost:11434", "timeout": "5m" } } }
```

`api_format` defaults to `ollama` when omitted, so pre-existing configs load unchanged. The low-level `ollama.NewClient()` API (used in the examples below) talks to Ollama directly; to target a llama.cpp backend, configure an `openai-compat` provider as above and route through `provider.Router`.

## Use a hosted API (bring your own key)

No local GPU? Point go-llm at any hosted **OpenAI-compatible** endpoint with the
`openai-compat` provider and your own API key. `base_url` is the server **root** —
do **not** include `/v1`; go-llm appends it.

Keep the secret out of the file: set `api_key` to a `${ENV_VAR}` reference and
export the variable. go-llm expands it when the config loads and fails fast if the
variable is unset or empty, so a missing key surfaces as a clear config error
rather than a remote 401. Literal keys still work, but `${ENV_VAR}` is recommended.

```bash
export OPENAI_API_KEY=sk-...
golem -config models.json
```

**Destination admission:** before the first outbound byte, Golem resolves the
config's full network plan and shows a manifest of every remote endpoint it
could reach — deduplicated destinations with each use-case route marked
primary or fallback — and asks for consent. Literal loopback endpoints
(llama.cpp, Ollama on `127.0.0.1`/`localhost`) auto-admit; anything remote
waits for a yes. For scripts and one-shot runs, pre-admit exact destinations
with the repeatable flag:

```bash
golem -p "..." -allow-destination "openai/https://api.openai.com"
```

The standalone MCP server is gated too but never prompts, and it admits per
provider rather than per route — pre-admit each remote provider with the same
`-allow-destination "provider/URL"` form (repeatable); see
[MCP Server](#mcp-server-1).

```json
{
  "providers": {
    "openai": {
      "base_url": "https://api.openai.com",
      "api_format": "openai-compat",
      "api_key": "${OPENAI_API_KEY}"
    }
  },
  "models": {
    "agent":     { "name": "gpt-4o",                 "provider": "openai", "type": "dense", "capabilities": ["chat", "stream", "tool_call"] },
    "embedding": { "name": "text-embedding-3-small", "provider": "openai", "type": "embedding" }
  },
  "defaults": { "chat": "agent", "agent": "agent", "embedding": "embedding" }
}
```

Golem's agent loop routes the **`agent`** role, so set `defaults.agent` to a
chat/stream/**tool-call**-capable model. `golem index` and RAG need an
**embedding**-capable model — set `defaults.embedding` to one (hosted providers
without embeddings can omit it and skip indexing).

### More compatibility examples

Only `base_url` and the model `name` change; go-llm appends `/v1` to each.

| Provider | `base_url` | Notes |
|----------|-----------|-------|
| OpenAI | `https://api.openai.com` | |
| OpenRouter | `https://openrouter.ai/api` | One key → many models (incl. Claude, Llama). The OpenAI SDK base is `…/api/v1`; go-llm adds the `/v1`. |
| Anthropic (OpenAI-compat layer) | `https://api.anthropic.com` | Anthropic's **OpenAI SDK compatibility** endpoint (`…/v1/`), handy for testing/comparison — **not** native Claude support. The native `/v1/messages` API is not supported. |

### Mixing providers and fallbacks

Providers and keys coexist — declare several and let a model fall back across them:

```json
{
  "providers": {
    "openai":     { "base_url": "https://api.openai.com",    "api_format": "openai-compat", "api_key": "${OPENAI_API_KEY}" },
    "openrouter": { "base_url": "https://openrouter.ai/api", "api_format": "openai-compat", "api_key": "${OPENROUTER_API_KEY}" }
  },
  "models": {
    "agent":        { "name": "gpt-4o",                       "provider": "openai",     "type": "dense", "capabilities": ["chat", "stream", "tool_call"], "fallbacks": ["agent-backup"] },
    "agent-backup": { "name": "anthropic/claude-3.5-sonnet",  "provider": "openrouter", "type": "dense", "capabilities": ["chat", "stream", "tool_call"] }
  },
  "defaults": { "agent": "agent" }
}
```

If a hosted backend lacks an endpoint (`/v1/completions`, embeddings, FIM, or
tool calls), set that model's `capabilities` to the endpoints that actually work
so the Router won't send unsupported requests.

For the Golem-specific walkthrough (flags, capability probing costs, verification
runbook), see [Running Golem against a hosted API](docs/GETTING_STARTED.md#running-golem-against-a-hosted-api).
