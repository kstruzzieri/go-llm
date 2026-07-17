# Task 4 report

Implemented `RetrievalScope`, scoped dense/scored retrieval, managed-file result
freshness, and optional `rag_search` collection/tag scope.

## RED

`rtk go test ./rag ./mcp -run 'Scoped|FreshnessWithoutList|RAGSearch.*Scope|DefaultResponseRemains' -count=1`
failed as expected before implementation: `RetrieveScoped` and
`RetrieveScoredScoped` were undefined and `RetrievalScope` was absent.

## Verification

- Focused: 8 passed (`Scoped|FreshnessWithoutList|RAGSearch.*Scope|DefaultResponseRemains`).
- Uncached: 1003 passed (`rtk go test ./rag ./mcp -count=1 -timeout 60s`).
- Race: 173 passed (`rtk go test -race ./rag ./mcp -run 'Retrieve|RAGSearch|Managed' -count=1 -timeout 60s`).
- `rtk git diff --check` clean.

## Review

Scope validation happens before embedding/search; non-empty scope rejects custom
stores. Scoped raw searches request global candidates before filtering and top-k.
Only final valid managed-file results receive cloned freshness metadata; file reads
are deduplicated by origin and retrieval performs no managed registry writes.
