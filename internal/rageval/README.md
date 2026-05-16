# RAG Evaluation Baseline

This internal package supports issue `#93`: agentic RAG Phase 0 evaluation
fixtures and baseline metrics.

It is intentionally under `internal/` so the harness does not decide the public
package boundary before `#94`.

## Fixtures

`testdata/fixtures.json` contains:

- 12 synthetic code chunks
- 20 golden queries
- 5 required categories with 4 queries each:
  - `single_hop`
  - `cross_file_trace`
  - `compare`
  - `architecture`
  - `code_review`

The fixture uses deterministic synthetic embeddings and does not require
Ollama or any live model.

## Baseline

Regenerate the committed report with:

```sh
go run ./cmd/rag-eval \
  -fixtures internal/rageval/testdata/fixtures.json \
  -out internal/rageval/testdata/baseline.json
```

The report compares:

- `static`: `rag.Retriever.Retrieve`
- `hybrid_search_multi`: `rag.SQLiteStore.SearchMulti`

Metrics include recall@5/10, MRR@5/10, duplicate rate, context precision
proxy, estimated context tokens, and cold/warm latency.

Threshold fields are intentionally `null` until Keith sets owner-approved
values before `#95` starts.
