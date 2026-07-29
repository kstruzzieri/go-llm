# Outline retrieval evaluation (#246)

## Recommendation: iterate; do not ship outline retrieval

Do not replace `resident_exact` with `outline_then_content`. The outline
selector reduced the downstream ranking set from 1,401 chunks to 50 and raised
recall/support coverage from 0.6 to 0.8, but it missed every `content_only`
support, reduced MRR from 1.0 to 0.8, and reduced K=10 source-path precision
from 0.84 to 0.33. It was also slower and allocated more than
`resident_exact`. These are mixed retrieval trade-offs, so the one-off evidence
recommendation is `iterate`, not ship.

Keep #291 resident-exact retrieval ahead of any outline planning. The bounded
semantic+keyword control recovered every expected support, but had K=10
source-path precision of 0.255 and remained slower and heavier than resident
exact. Hierarchical retrieval is downstream of resident exact: it ranks all
1,401 chunks, hydrates 50, and then post-inspects those 50. Its deliberate
`MaxGroups=1` regression remains visible on `distributed_support`: resident
exact retrieves both expected supports (recall/coverage 1.0), while hierarchy
keeps one (0.5).

`source_path_precision` is the fraction of returned chunks whose source path
is expected. It is only a retrieval proxy. No generator ran, so generated
answer quality and actual citation correctness remain unmeasured. This evidence
therefore references #246 rather than closing its acceptance gap.

## Reproduction

The evaluator and fixture logic used for the measured run is preserved in commit
`76e53ffadfaac9d03256776f988bf9b62d21f0db`; later commits only renamed the
outline sample-count flag from `-warm-runs` to `-samples` and refactored fixture
constants without changing fixture output, so the numbers below still reproduce.

Environment:

```text
go version go1.26.1 darwin/arm64
macOS 26.5.2 (25F84); Darwin 25.5.0 arm64
Apple M3 Max (16 cores); 128 GB RAM
```

Commands:

```sh
go version
sw_vers
uname -mrs
system_profiler SPHardwareDataType
go run ./cmd/rag-eval \
  -experiment outline \
  -dimensions 768 \
  -candidate-m 50 \
  -samples 5 \
  -out /private/tmp/go-llm-246-outline-report.json
```

The run used one file-backed SQLite fixture for all modes: 1,401 chunks, 138
sources, 20 golden queries, 768-dimensional vectors, final K values 5 and 10,
candidate M=50, and five measured samples per query. The uncommitted raw report
is `/private/tmp/go-llm-246-outline-report.json` (224,535 bytes), with SHA-256:

```text
771f790bbf436991fc2f11deb6d2055467481f243f96166bd0b082e21e9237bf
```

Its schema is `rag-outline-eval/v1`. Raw JSON contains measurements only; it
does not contain a conclusion or recommendation.

## Modes

| Mode | Retrieval path |
| --- | --- |
| `full_corpus_search_multi` | Mutable `SearchMulti`; scores and loads content for all 1,401 chunks. |
| `resident_exact` | #291 immutable resident snapshot; scores all 1,401 chunks and hydrates the final 10. |
| `bounded_semantic_keyword_union` | Semantic top-50 plus non-zero FTS5 keyword top-50, stable-identity union, final scoring, then hydration. |
| `outline_then_content` | Precomputed metadata-only token sets, deterministic top-50 selection, final scoring, then hydration. |
| `hierarchical` | Resident exact scores all 1,401 and hydrates 50; hierarchy then post-inspects those 50 with `MaxGroups=1`. |

## Results

Rounded quality and context values:

| Mode | Recall@5 | Recall@10 | MRR@5 | MRR@10 | Support coverage@5 | Support coverage@10 | Source-path precision@5 | Source-path precision@10 | Context tokens@5 | Context tokens@10 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `full_corpus_search_multi` | 0.6 | 0.6 | 1 | 1 | 0.6 | 0.6 | 0.88 | 0.84 | 214.75 | 440.25 |
| `resident_exact` | 0.6 | 0.6 | 1 | 1 | 0.6 | 0.6 | 0.88 | 0.84 | 214.75 | 440.25 |
| `bounded_semantic_keyword_union` | 1 | 1 | 1 | 1 | 1 | 1 | 0.51 | 0.255 | 203.15 | 423.95 |
| `outline_then_content` | 0.8 | 0.8 | 0.8 | 0.8 | 0.8 | 0.8 | 0.45 | 0.33 | 194.6 | 397.45 |
| `hierarchical` | 0.5 | 0.5 | 1 | 1 | 0.5 | 0.5 | 1 | 1 | 184.25 | 365.2 |

Rounded cost and work values:

| Mode | P50 ms | P95 ms | Allocated bytes | Allocations | Candidates inspected | Ranked candidates | Hydrated chunks | Post-retrieval inspected | Deterministic | Planning tokens |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | :---: | :---: |
| `full_corpus_search_multi` | 9.181 | 9.955 | 21,316,324 | 71,821 | 1401 | 1401 | 1401 | 0 | true | null |
| `resident_exact` | 1.663 | 1.697 | 168,878 | 524 | 1401 | 1401 | 10 | 0 | true | null |
| `bounded_semantic_keyword_union` | 3.198 | 3.526 | 738,469 | 9,135 | 1401 | 50 | 10 | 0 | true | null |
| `outline_then_content` | 2.063 | 2.481 | 701,651 | 13,106 | 1401 | 50 | 10 | 0 | true | null |
| `hierarchical` | 1.947 | 2.077 | 457,791 | 3,041 | 1401 | 1401 | 50 | 50 | true | null |

`candidates_inspected` counts unique corpus records consulted before hydration.
`ranked_candidates` counts the upstream scoring set. `hydrated_content_chunks`
counts every full-content row loaded. `post_retrieval_candidates_inspected`
counts a separate downstream selection stage and is non-zero only for
hierarchy. Bounded and outline reduce downstream work but still inspect all
1,401 lean records.

### Exact summary JSON

This committed excerpt preserves exact aggregate values so the temporary raw
artifact is not required for review. The quality and work counters (`recall`,
`mrr`, coverage, `source_path_precision`, `*candidates*`, `hydrated_content_chunks`)
are deterministic and reproduce exactly. `latency_ms`, `allocated_bytes`, and
`allocation_count` are machine-specific single-run measurements: the digits below
are the exact captured values, not reproducible targets, and will differ on other
hosts (see Limits).

```json
{
  "modes": [
    {
      "name": "full_corpus_search_multi",
      "summary": {
        "recall_at_5": 0.6,
        "recall_at_10": 0.6,
        "mrr_at_5": 1,
        "mrr_at_10": 1,
        "expected_support_coverage_at_5": 0.6,
        "expected_support_coverage_at_10": 0.6,
        "source_path_precision_at_5": 0.8799999999999997,
        "source_path_precision_at_10": 0.8399999999999999,
        "final_context_tokens_at_5": 214.75,
        "final_context_tokens_at_10": 440.25,
        "planning_tokens": null,
        "latency_ms": {"count": 100, "p50": 9.1810625, "p95": 9.954915249999999},
        "allocated_bytes": 21316324.08,
        "allocation_count": 71821.42,
        "candidates_inspected": 1401,
        "ranked_candidates": 1401,
        "hydrated_content_chunks": 1401,
        "post_retrieval_candidates_inspected": 0,
        "deterministic_ordering": true
      }
    },
    {
      "name": "resident_exact",
      "summary": {
        "recall_at_5": 0.6,
        "recall_at_10": 0.6,
        "mrr_at_5": 1,
        "mrr_at_10": 1,
        "expected_support_coverage_at_5": 0.6,
        "expected_support_coverage_at_10": 0.6,
        "source_path_precision_at_5": 0.8799999999999997,
        "source_path_precision_at_10": 0.8399999999999999,
        "final_context_tokens_at_5": 214.75,
        "final_context_tokens_at_10": 440.25,
        "planning_tokens": null,
        "latency_ms": {"count": 100, "p50": 1.6629795, "p95": 1.6973118500000002},
        "allocated_bytes": 168878.08,
        "allocation_count": 523.91,
        "candidates_inspected": 1401,
        "ranked_candidates": 1401,
        "hydrated_content_chunks": 10,
        "post_retrieval_candidates_inspected": 0,
        "deterministic_ordering": true
      }
    },
    {
      "name": "bounded_semantic_keyword_union",
      "summary": {
        "recall_at_5": 1,
        "recall_at_10": 1,
        "mrr_at_5": 1,
        "mrr_at_10": 1,
        "expected_support_coverage_at_5": 1,
        "expected_support_coverage_at_10": 1,
        "source_path_precision_at_5": 0.51,
        "source_path_precision_at_10": 0.255,
        "final_context_tokens_at_5": 203.15,
        "final_context_tokens_at_10": 423.95,
        "planning_tokens": null,
        "latency_ms": {"count": 100, "p50": 3.1982505000000003, "p95": 3.5257274},
        "allocated_bytes": 738468.8,
        "allocation_count": 9135.4,
        "candidates_inspected": 1401,
        "ranked_candidates": 50,
        "hydrated_content_chunks": 10,
        "post_retrieval_candidates_inspected": 0,
        "deterministic_ordering": true
      }
    },
    {
      "name": "outline_then_content",
      "summary": {
        "recall_at_5": 0.8,
        "recall_at_10": 0.8,
        "mrr_at_5": 0.8,
        "mrr_at_10": 0.8,
        "expected_support_coverage_at_5": 0.8,
        "expected_support_coverage_at_10": 0.8,
        "source_path_precision_at_5": 0.45000000000000007,
        "source_path_precision_at_10": 0.33000000000000007,
        "final_context_tokens_at_5": 194.6,
        "final_context_tokens_at_10": 397.45,
        "planning_tokens": null,
        "latency_ms": {"count": 100, "p50": 2.0625, "p95": 2.4813044999999994},
        "allocated_bytes": 701650.72,
        "allocation_count": 13105.87,
        "candidates_inspected": 1401,
        "ranked_candidates": 50,
        "hydrated_content_chunks": 10,
        "post_retrieval_candidates_inspected": 0,
        "deterministic_ordering": true
      }
    },
    {
      "name": "hierarchical",
      "summary": {
        "recall_at_5": 0.5,
        "recall_at_10": 0.5,
        "mrr_at_5": 1,
        "mrr_at_10": 1,
        "expected_support_coverage_at_5": 0.5,
        "expected_support_coverage_at_10": 0.5,
        "source_path_precision_at_5": 1,
        "source_path_precision_at_10": 1,
        "final_context_tokens_at_5": 184.25,
        "final_context_tokens_at_10": 365.2,
        "planning_tokens": null,
        "latency_ms": {"count": 100, "p50": 1.947396, "p95": 2.0767854},
        "allocated_bytes": 457791.04,
        "allocation_count": 3040.54,
        "candidates_inspected": 1401,
        "ranked_candidates": 1401,
        "hydrated_content_chunks": 50,
        "post_retrieval_candidates_inspected": 50,
        "deterministic_ordering": true
      }
    }
  ]
}
```

### Per-category K=10

| Mode | Category | Recall | MRR | Support coverage | Source-path precision | Context tokens |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| `full_corpus_search_multi` | `content_only` | 0.5 | 1 | 0.5 | 1 | 450.75 |
| `full_corpus_search_multi` | `direct_symbol` | 0.5 | 1 | 0.5 | 1 | 443.25 |
| `full_corpus_search_multi` | `distributed_support` | 1 | 1 | 1 | 0.2 | 421.5 |
| `full_corpus_search_multi` | `outline_summary` | 0.5 | 1 | 0.5 | 1 | 442.75 |
| `full_corpus_search_multi` | `path_and_pairing` | 0.5 | 1 | 0.5 | 1 | 443 |
| `resident_exact` | `content_only` | 0.5 | 1 | 0.5 | 1 | 450.75 |
| `resident_exact` | `direct_symbol` | 0.5 | 1 | 0.5 | 1 | 443.25 |
| `resident_exact` | `distributed_support` | 1 | 1 | 1 | 0.2 | 421.5 |
| `resident_exact` | `outline_summary` | 0.5 | 1 | 0.5 | 1 | 442.75 |
| `resident_exact` | `path_and_pairing` | 0.5 | 1 | 0.5 | 1 | 443 |
| `bounded_semantic_keyword_union` | `content_only` | 1 | 1 | 1 | 0.275 | 438.5 |
| `bounded_semantic_keyword_union` | `direct_symbol` | 1 | 1 | 1 | 0.25 | 422 |
| `bounded_semantic_keyword_union` | `distributed_support` | 1 | 1 | 1 | 0.2 | 421.5 |
| `bounded_semantic_keyword_union` | `outline_summary` | 1 | 1 | 1 | 0.275 | 414.25 |
| `bounded_semantic_keyword_union` | `path_and_pairing` | 1 | 1 | 1 | 0.275 | 423.5 |
| `outline_then_content` | `content_only` | 0 | 0 | 0 | 0 | 396.75 |
| `outline_then_content` | `direct_symbol` | 1 | 1 | 1 | 0.2 | 408.75 |
| `outline_then_content` | `distributed_support` | 1 | 1 | 1 | 0.25 | 346.5 |
| `outline_then_content` | `outline_summary` | 1 | 1 | 1 | 0.2 | 396.5 |
| `outline_then_content` | `path_and_pairing` | 1 | 1 | 1 | 1 | 438.75 |
| `hierarchical` | `content_only` | 0.5 | 1 | 0.5 | 1 | 450.75 |
| `hierarchical` | `direct_symbol` | 0.5 | 1 | 0.5 | 1 | 443.25 |
| `hierarchical` | `distributed_support` | 0.5 | 1 | 0.5 | 1 | 46.25 |
| `hierarchical` | `outline_summary` | 0.5 | 1 | 0.5 | 1 | 442.75 |
| `hierarchical` | `path_and_pairing` | 0.5 | 1 | 0.5 | 1 | 443 |

The category split exposes two intended ceilings. Metadata-only pruning cannot
recover `content_only` evidence absent from the outline. Separately, the neutral
distributed query context lets flat resident/full retrieval recover both
supports across `cmd/` and `pkg/`, while hierarchy's single-group limit drops
one. Retrieval of support paths is not multi-hop answer synthesis.

## Limits

- The fixture is generated and file-backed, with deterministic synthetic
  embeddings. It controls retrieval comparisons but does not model a live
  repository's query distribution or semantic embedding errors.
- Every mode is warmed once before measurement. Allowed outline token sets are
  also precomputed before measurement; their construction cost and retained
  heap are excluded.
- This single-process run does not measure cold startup, concurrency,
  persistent-memory pressure, or a long-lived production cache.
- Allocation deltas use process-wide Go runtime counters, and latency is
  machine-specific. Use them only for this within-run comparison.
- No generator or planner ran. Generated answer quality, actual citation
  quality, and planning prompt tokens are unobservable; `planning_tokens` is
  `null`.
- Hierarchy uses resident-exact retrieval upstream, ranks all 1,401 chunks, and
  hydrates 50 before post-retrieval grouping. It is not outline-first pruning.
- No success threshold was chosen. The committed `iterate` recommendation is
  a human interpretation of these measurements and is intentionally absent
  from raw JSON.
