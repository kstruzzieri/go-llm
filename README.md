<p align="center">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/brand/golem-lockup-on-dark.svg">
  <img alt="Golem agent logo" src="assets/brand/golem-lockup-on-light.svg" width="260">
</picture>
</p>


# go-llm

A local-first LLM toolkit and terminal coding agent for Go. Run models through **[llama.cpp](https://github.com/ggml-org/llama.cpp)** — the recommended, primary backend for best local performance, via its OpenAI-compatible server — or through [Ollama](https://ollama.com). go-llm provides the plumbing for model management, routing, RAG-powered retrieval, MCP integration, and domain-specific analysis — local-first by default, no cloud account required — with optional bring-your-own-key access to hosted OpenAI-compatible APIs (see [Use a hosted API](#use-a-hosted-api-bring-your-own-key)).

Use it directly in a terminal through **Golem**, the bundled local coding agent ([full guide](docs/golem.md)); expose it as a standalone [MCP server](#mcp-server); or embed the Go packages in your own application ([library reference](docs/library.md)). Pure Go with minimal dependencies (no CGo).

**Current release: v0.3.0** (2026-09-19) — [release notes](CHANGELOG.md#030---2026-09-18) · [binaries](https://github.com/kstruzzieri/go-llm/releases/tag/v0.3.0). Upgrading from v0.2.0? Read the [consumer upgrade notes](CHANGELOG.md#changed--v030-consumer-upgrade-notes-560) first.

## Contents

- [What's included](#whats-included)
- [Requirements](#requirements) · [Installation](#installation)
- [Terminal Quick Start](#terminal-quick-start) — full guide in [docs/golem.md](docs/golem.md)
- [Use as a Go library](#use-as-a-go-library) — full reference in [docs/library.md](docs/library.md)
- [Local model backends](#local-model-backends) · [Use a hosted API](#use-a-hosted-api-bring-your-own-key) — full detail in [docs/backends.md](docs/backends.md)
- [MCP Server](#mcp-server)
- [Roadmap](#roadmap)
- [Dependencies](#dependencies) · [Testing](#testing) · [License](#license)

### What's included

- **Model backends** — `openai-compat` provider (llama.cpp / vLLM / LM Studio) and a native Ollama REST client: chat, completions, embeddings, model management, tool calling, streaming
- **Golem terminal agent** — read-only by default; approval-gated write/exec with scoped session grants, background jobs, project-context trust, signed mutation receipts, `/consult` to an external subscription CLI, and destination admission that shows every remote endpoint before the first outbound byte
- **Zero-trust agent runtime** — tool-observation fencing, deterministic injection and secret detectors, deny-default exec sandboxes (macOS Seatbelt, Linux Bubblewrap) that fail closed rather than fall back to the host, signed agent-memory provenance, and an offline audit verifier
- **RAG pipeline** — code-aware chunking, SQLite vector store with hybrid search, concurrent `.gitignore`-aware indexing, context-building retrieval
- **FIM completion** — Fill-in-the-Middle for IDE inline suggestions with context window management
- **Model config and routing** — `models.json` roles and fallback chains; use-case-aware `provider.Router` with circuit breakers and slot-aware admission
- **MCP** — standalone server (stdio, HTTP/2) and a client that mounts external MCP servers' tools into the agent
- **Also** — Parquet export, analysis helpers (code review, ML metrics, trading), reusable recipe bundles

Package-by-package map: [docs/library.md#packages](docs/library.md#packages).

## Requirements

- Go 1.25+
- A local model backend (choose one or run both side by side):
  - **llama.cpp** (recommended) — `llama-server` exposing its OpenAI-compatible API
  - **Ollama** — running locally (default: `http://localhost:11434`)

## Installation

Install the terminal tools:

```bash
go install github.com/kstruzzieri/go-llm/cmd/golem@latest
go install github.com/kstruzzieri/go-llm/cmd/go-llm-mcp@latest
```

Or build from a local checkout:

```bash
go build -o bin/golem ./cmd/golem
go build -o bin/go-llm-mcp ./cmd/go-llm-mcp
```

Use `go get` when embedding go-llm as a library:

```bash
go get github.com/kstruzzieri/go-llm
```


Prebuilt `golem` and `go-llm-mcp` binaries for each release are on the [releases page](https://github.com/kstruzzieri/go-llm/releases).

## Terminal Quick Start

Start your configured model backend first. The checked-in `models.json` defaults to a llama.cpp-compatible server at `http://127.0.0.1:8080`; see [Local model backends](#local-model-backends).

```bash
golem -root /path/to/project
```

Golem starts read-only: it can inspect files, search the workspace, route through the configured `agent` model chain, and keep a persistent per-workspace session. `/help` lists every command. Opt in to project mutation explicitly:

```bash
golem -root /path/to/project -allow-write              # apply write/edit calls after approval
golem -root /path/to/project -allow-write -allow-exec  # also run shell commands after approval
golem -root /path/to/project -p "Summarize this repo"  # one-shot, no REPL; final answer on stdout
```

The full guide is **[docs/golem.md](docs/golem.md)**: [workspace RAG index](docs/golem.md#workspace-rag-index), [REPL commands](docs/golem.md#repl-commands), [mutation receipts and offline audit](docs/golem.md#mutation-receipts-and-offline-audit), [`/think`](docs/golem.md#thinking-mode-think) and [`/model`](docs/golem.md#switching-models-model), [Git context](docs/golem.md#git-context), [interceptors and secret detection](docs/golem.md#interceptors-and-secret-detection), [project-context trust](docs/golem.md#project-context-trust), [scripting / one-shot mode](docs/golem.md#scripting--one-shot-mode), and [external MCP catalog trust](docs/golem.md#external-mcp-catalog-trust). `/consult` is documented in [docs/consult.md](docs/consult.md), recipes in [docs/recipes.md](docs/recipes.md), agent memory in [docs/memory.md](docs/memory.md).

## Use as a Go library

Everything is available as plain Go packages — chat, streaming, tool calling,
embeddings, RAG indexing and retrieval, FIM completion, model configuration,
Parquet export, and the analysis helpers. The full API walkthrough with
runnable examples lives in **[docs/library.md](docs/library.md)**. The
30-second version:

```go
package main

import (
    "context"
    "fmt"

    "github.com/kstruzzieri/go-llm/ollama"
)

func main() {
    client := ollama.NewClient()
    resp, err := client.Chat(context.Background(), ollama.ChatRequest{
        Model:    "gemma4:31b",
        Messages: []ollama.ChatMessage{{Role: "user", Content: "hello"}},
    })
    if err != nil {
        panic(err)
    }
    fmt.Println(resp.Message.Content)
}
```


## Local model backends

go-llm selects a backend per provider in `models.json` via the `api_format` field, and routes between providers with `provider.Router`:

| `api_format` | Speaks to | Notes |
|---|---|---|
| `openai-compat` | llama.cpp `llama-server`, [llama-swap](https://github.com/mostlygeek/llama-swap), vLLM, LM Studio, any OpenAI `/v1` server | **Recommended** for best local performance. `base_url` is the server root — go-llm appends `/v1`. The shipped `models.json` targets llama-swap on `127.0.0.1:8080`. |
| `ollama` (default when omitted) | Ollama's native REST API, `http://localhost:11434` | Fully supported alternative; pre-existing configs load unchanged. |

Setup for each — llama-swap, pinned `llama-server` processes, Ollama, and the `slot_discovery` opt-in — is in **[docs/backends.md](docs/backends.md#local-model-backends)**. For a first-run walkthrough including model downloads, see [docs/GETTING_STARTED.md](docs/GETTING_STARTED.md).

## Use a hosted API (bring your own key)

No local GPU? Point an `openai-compat` provider at any hosted OpenAI-compatible endpoint with your own key. Keep the secret out of the file with `"api_key": "${OPENAI_API_KEY}"`: go-llm expands the reference at load time and fails fast if the variable is unset. Before the first outbound byte, Golem shows a manifest of every remote endpoint the config can reach and asks for consent — loopback auto-admits, anything remote waits for a yes; scripts pre-admit exact destinations with `-allow-destination "provider/https://host"`. Compatibility table, mixing providers, and fallback chains: [docs/backends.md](docs/backends.md#use-a-hosted-api-bring-your-own-key).

## MCP Server

Expose go-llm over the [Model Context Protocol](https://modelcontextprotocol.io/) to Claude Desktop, IDE extensions, or any MCP client.

```bash
go build -o go-llm-mcp ./cmd/go-llm-mcp/
./go-llm-mcp --transport stdio                       # Claude Desktop, IDE integration
./go-llm-mcp --transport http --addr 127.0.0.1:8080  # HTTP/2, local development
```

Tools cover chat, generation, code completion, embeddings, RAG, model management, and analysis, routed through `provider.Router`; opt-in agent-memory tools register with `--agent-memory-db`. Remote destinations are denied unless pre-admitted with `-allow-destination` — the standalone server never prompts. Claude Desktop configuration, TLS, embedding the server in your own binary, and the routing and admission details: [docs/GETTING_STARTED.md](docs/GETTING_STARTED.md#mcp-server).

## Roadmap

| Release | Scope | Tracking |
|---|---|---|
| **v0.3.0** (current) | Zero-trust agent foundation — observation fencing, injection and secret detectors, signed mutation receipts and agent memory, scoped dispatch children, offline audit verifier — plus the Golem session surface (`/model`, `/think`, `/compact`, `/consult`, headless `-p`, Git context) and recipe bundles | [CHANGELOG](CHANGELOG.md#030---2026-09-18) |
| **v0.4.0** | Codex `/consult` adapter (#546), capability-attenuated child tool registries (#449), ANSI sanitization of streamed output (#433), Workspace write/delete hardening (#552), migration and CAS fixes | [milestone](https://github.com/kstruzzieri/go-llm/milestone/1) |
| **v0.5.0** | Quarantined ingestion for foreign content (#434), injection-aware retrieval tagging (#435), adversarial injection corpus for `llm-bench` (#452), default-on interceptors (#517) | [milestone](https://github.com/kstruzzieri/go-llm/milestone/2) |
| Later | Hosted-native transports (Anthropic Messages, Gemini, OpenAI Responses), agentic RAG orchestration, in-band routing transparency in MCP responses, evidence-governed feedback, vision inputs, ANN search | [open issues](https://github.com/kstruzzieri/go-llm/issues) |

Security work is coordinated under epic [#429](https://github.com/kstruzzieri/go-llm/issues/429).

## Dependencies

Minimal by design:

- `modernc.org/sqlite` — pure Go SQLite driver (no CGo)
- `golang.org/x/sync` — concurrency primitives (bounded worker pools for indexing)
- `golang.org/x/net` — h2c HTTP/2 cleartext transport (only imported by `mcp/`)
- `golang.org/x/term` — VT100 line editor for the Golem REPL prompt (only imported by `cmd/golem/`)
- `golang.org/x/sys` — build-tagged platform helpers (Linux PTY test support in `cmd/golem/`, Windows directory fsync in `profiles/`)
- `github.com/modelcontextprotocol/go-sdk` — official MCP Go SDK (imported by `mcp/`, `mcpclient/`, and `cmd/llm-bench/`)
- `github.com/parquet-go/parquet-go` — Parquet file writer (only imported by `rag/parquet/`)
- `github.com/santhosh-tekuri/jsonschema/v6` — JSON Schema validator (only imported by `cmd/llm-bench/`)

## Testing

```bash
go test ./...
```

The Docker-backed local CI — lint, hardening contracts, `go test -race`, compile smoke — runs as a pre-push hook once enabled with `scripts/setup-local-ci`; see [docs/local-ci.md](docs/local-ci.md).

## License

Licensed under the [Apache License, Version 2.0](LICENSE). See [`NOTICE`](NOTICE)
for attribution.
