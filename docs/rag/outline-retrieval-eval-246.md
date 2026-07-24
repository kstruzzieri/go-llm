# Outline retrieval evaluation (#246)

## Recommendation: iterate

Do not replace `resident_exact` with `outline_then_content` yet. The outline
selector reduced the final ranking set from 1,401 chunks to 50 and improved
recall/support coverage from 0.5 to 0.8, but it missed every `content_only`
support, reduced MRR from 1.0 to 0.8 and citation/source accuracy at K=10 from
1.0 to 0.33. It was also slower and allocated more than `resident_exact`.
Neither mode dominates the other, so this is an evidence-based `iterate`
result, not a threshold decision.

Keep #291 resident-exact retrieval ahead of any outline planning in the
production sequence. The bounded semantic+keyword control merits another
experiment: compared with the outline selector it had higher recall and lower
latency/allocations, but it traded away source precision and remained slower
and heavier than resident-exact. Existing hierarchical retrieval is downstream
of resident-exact retrieval; it hydrates M candidates before grouping and
therefore is not an outline-pruning substitute. This result makes no production
retrieval change.

## Reproduction

The measured evaluator commit was
`26c37071af6ed201de1f24a36e4ed75459b7fa6d`. The exact staged implementation
tree (including the CLI and writer used for this run) was
`c6a2470b2c89d34ba4d7fee25a51533ff5b1f378`.

Environment:

```text
go version go1.26.1 darwin/arm64
Darwin 25.5.0 arm64
```

Commands:

```sh
rtk go version
rtk uname -mrs
rtk go run ./cmd/rag-eval \
  -experiment outline \
  -dimensions 768 \
  -candidate-m 50 \
  -warm-runs 5 \
  -out /private/tmp/go-llm-246-outline-report.json
```

The run used one identical file-backed SQLite fixture for all modes: 1,401
chunks, 138 sources, 20 golden queries, 768-dimensional vectors, final K values
5 and 10, candidate M=50, and five measured samples per query. The raw report
is `/private/tmp/go-llm-246-outline-report.json`, with SHA-256
`9cc895e456211ab4efe1a853d0badce3b4ab1eafd54b55713eb8c7ff9daedaab`.
Its schema is `rag-outline-eval/v1` and its conclusion was `iterate`. Tables
below round display values; the raw JSON retains exact measurements.

## Modes

| Mode | Retrieval path |
| --- | --- |
| `full_corpus_search_multi` | Mutable `SearchMulti`; exact scoring and content loading across all 1,401 chunks. |
| `resident_exact` | #291 immutable resident snapshot; exact scoring across all chunks and final-result hydration. |
| `bounded_semantic_keyword_union` | Semantic top-50 plus non-zero FTS5 keyword top-50, stable-identity union, final scoring, then hydration. |
| `outline_then_content` | Metadata-only deterministic top-50 selection, final scoring, then hydration. |
| `hierarchical` | Resident-exact top-50 retrieval and hydration, followed by the existing hierarchical post-retrieval policy. |

## Results

Quality and context:

| Mode | Recall@5 | Recall@10 | MRR@5 | MRR@10 | Support coverage@5 | Support coverage@10 | Citation/source accuracy@5 | Citation/source accuracy@10 | Context tokens@5 | Context tokens@10 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `full_corpus_search_multi` | 0.5 | 0.5 | 1 | 1 | 0.5 | 0.5 | 1 | 1 | 217.4 | 442.65 |
| `resident_exact` | 0.5 | 0.5 | 1 | 1 | 0.5 | 0.5 | 1 | 1 | 217.4 | 442.65 |
| `bounded_semantic_keyword_union` | 1 | 1 | 1 | 1 | 1 | 1 | 0.52 | 0.26 | 203.1 | 423.9 |
| `outline_then_content` | 0.8 | 0.8 | 0.8 | 0.8 | 0.8 | 0.8 | 0.45 | 0.33 | 194.6 | 397.45 |
| `hierarchical` | 0.5 | 0.5 | 1 | 1 | 0.5 | 0.5 | 1 | 1 | 217.4 | 442.65 |

Measured cost and work:

| Mode | P50 ms | P95 ms | Allocated bytes | Allocations | Candidates inspected | Ranked candidates | Hydrated content chunks | Deterministic | Planning tokens |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | :---: | :---: |
| `full_corpus_search_multi` | 9.262 | 10.512 | 21,316,214 | 71,821 | 1401 | 1401 | 1401 | true | null |
| `resident_exact` | 1.673 | 1.742 | 168,823 | 520 | 1401 | 1401 | 10 | true | null |
| `bounded_semantic_keyword_union` | 3.129 | 3.470 | 737,906 | 9,136 | 1401 | 50 | 10 | true | null |
| `outline_then_content` | 3.511 | 4.055 | 2,642,701 | 24,555 | 1401 | 50 | 10 | true | null |
| `hierarchical` | 1.848 | 1.954 | 459,310 | 3,045 | 1401 | 50 | 50 | true | null |

`candidates_inspected` counts unique candidate records consulted before final
hydration: snapshot/planning modes consult lean rows, while full-corpus mode
materializes full rows. `ranked_candidates` counts the final scoring or
selection set. `hydrated_content_chunks` counts every full-content row loaded,
including hierarchical candidates later discarded. All five modes still
inspect the whole corpus; bounded and outline modes reduce downstream ranking
work, not the scan.

### Per-category K=10

Full-corpus, resident-exact, and hierarchical results are identical within
each category, so `resident_exact` represents all three below.

| Mode | Category | Recall | MRR | Support coverage | Citation/source accuracy | Context tokens |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| `resident_exact` | `content_only` | 0.5 | 1 | 0.5 | 1 | 450.75 |
| `resident_exact` | `direct_symbol` | 0.5 | 1 | 0.5 | 1 | 443.25 |
| `resident_exact` | `distributed_support` | 0.5 | 1 | 0.5 | 1 | 433.5 |
| `resident_exact` | `outline_summary` | 0.5 | 1 | 0.5 | 1 | 442.75 |
| `resident_exact` | `path_and_pairing` | 0.5 | 1 | 0.5 | 1 | 443 |
| `bounded_semantic_keyword_union` | `content_only` | 1 | 1 | 1 | 0.275 | 438.5 |
| `bounded_semantic_keyword_union` | `direct_symbol` | 1 | 1 | 1 | 0.25 | 422 |
| `bounded_semantic_keyword_union` | `distributed_support` | 1 | 1 | 1 | 0.225 | 421.25 |
| `bounded_semantic_keyword_union` | `outline_summary` | 1 | 1 | 1 | 0.275 | 414.25 |
| `bounded_semantic_keyword_union` | `path_and_pairing` | 1 | 1 | 1 | 0.275 | 423.5 |
| `outline_then_content` | `content_only` | 0 | 0 | 0 | 0 | 396.75 |
| `outline_then_content` | `direct_symbol` | 1 | 1 | 1 | 0.2 | 408.75 |
| `outline_then_content` | `distributed_support` | 1 | 1 | 1 | 0.25 | 346.5 |
| `outline_then_content` | `outline_summary` | 1 | 1 | 1 | 0.2 | 396.5 |
| `outline_then_content` | `path_and_pairing` | 1 | 1 | 1 | 1 | 438.75 |

The category split exposes the intended ceiling: metadata-only pruning works
when symbols, paths, pairing hints, headings, comments, or docstrings carry the
cue, but it cannot recover `content_only` evidence that is deliberately absent
from the outline. Distributed supports span separate `cmd/` and `pkg/` paths;
this fixture measures retrieval of both sources, not multi-hop answer synthesis.

## Limits

- The fixture is generated and file-backed, with deterministic synthetic
  embeddings. It controls retrieval comparisons but does not model a live
  repository's query distribution or semantic embedding errors.
- The SQLite snapshot and adapters are warm. This single-process run does not
  measure cold startup, concurrency, persistent-memory pressure, or a
  long-lived production cache.
- Allocation deltas come from process-wide Go runtime counters, and latency is
  specific to the machine above. Use them for this within-run comparison, not
  as portable service-level targets.
- No generator or planner ran. Answer quality and planning prompt tokens are
  therefore unobservable; `planning_tokens` is `null`, and retrieval metrics
  must not be presented as end-to-end answer quality.
- Hierarchical mode already uses resident-exact retrieval upstream and
  hydrates its 50 candidates before hierarchy. Its measurements cannot support
  a claim that hierarchy performs outline-first pruning.
- No success threshold was chosen. The recommendation follows the observed
  quality/cost trade-off against `resident_exact`.
