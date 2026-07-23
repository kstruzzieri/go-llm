# Packed float32 SQLite embeddings (#293)

New SQLite indexes persist embeddings as a versioned packed float32 BLOB. The
public embedder, `VectorStore`, incremental-index, and export APIs still use
`[]float64`; precision and encoding remain private to SQLite persistence.

## Binary format

Each row contains a 16-byte envelope followed by `dimension * 4` payload bytes:

| Offset | Bytes | Meaning |
|---:|---:|---|
| 0 | 4 | ASCII magic `GLLV` |
| 4 | 1 | format version (`1`) |
| 5 | 1 | element type (`1` = IEEE-754 float32) |
| 6 | 1 | byte order (`1` = little-endian) |
| 7 | 1 | reserved; must be zero |
| 8 | 4 | unsigned little-endian dimension |
| 12 | 4 | unsigned little-endian payload byte length |
| 16 | `4 * dimension` | little-endian float32 elements |

The decoder requires an exact payload length, a dimension in `1..1,048,576`,
a known version/type/order, a zero reserved byte, and finite elements. Encoding
rejects non-finite float64 input and values that overflow during float32
conversion. A 4,096-dimensional vector therefore occupies exactly 16,400
bytes: 16,384 payload bytes plus the bounded 16-byte envelope.

## Compatibility and migration policy

The envelope is self-identifying, so this change does not add an in-place data
migration or rewrite existing rows. Fresh schemas declare `embedding BLOB` and
all store writes bind packed bytes. SQLite's dynamic typing lets the centralized
decoder continue reading a homogeneous legacy JSON-text corpus through dense,
hybrid, immutable-snapshot, incremental-reuse, and export paths.

The policy is deliberately non-destructive:

- A writable library store that already contains legacy JSON or mixed formats
  remains readable but refuses embedding writes with rebuild/migration guidance.
- A corpus containing both JSON and packed rows is reported as `mixed`; scoring
  rejects it before ranking instead of skipping rows or combining dimensions.
- An explicit Golem `-rag-db` must be packed. Legacy or mixed files are opened
  immutable, diagnosed, and refused without migrations, WAL creation, or file
  changes. The user must deliberately rebuild the DB or remove `-rag-db` to use
  the private auto index.
- A Golem-owned legacy or mixed auto index fails active-generation validation.
  The writer keeps the active files untouched, builds a fresh packed staging
  generation from source, validates it, and changes visibility only through
  the #292 active-pointer publication step.

`StoreStats` preserves the existing totals and database-byte estimate while
adding `EmbeddingFormat` and `EmbeddingBytes`. It reports `packed-f32-v1`,
`legacy-json-f64`, `mixed`, or `empty`, plus the decoded dimension and actual
sum of embedding value bytes.

## Validation cost and write-block lifetime

- Opening a writable store and every `Stats` call scan and validate all
  embedding rows (`O(chunks × dimension)`). Read-only opens defer validation
  to the first snapshot load. Golem pays the scan during generation
  validation and again when the gated retriever opens; it is a per-open cost,
  never a per-query cost.
- Each write transaction re-inspects only the corpus's first and last rows as
  a best-effort guard against foreign-format rows appended by an older writer
  after open; the complete scan runs at open time.
- The write block computed at open persists for the lifetime of the handle: a
  legacy or mixed corpus emptied through deletes accepts writes again only
  after reopening the store.

## Retrieval quality

The committed `internal/rageval` fixture and baseline reproduce unchanged with
packed persistence. A separate permanent differential test compares packed and
legacy JSON stores across 64 deterministic 4,096-dimensional vectors:

- dense and hybrid top-10 IDs and ordering are identical;
- hybrid fused rank scores are exact;
- `Distance == 1 - Score` remains exact;
- maximum observed semantic-score delta is
  `7.601331022955016e-10`, below the test gate of `1e-6`.

## File-backed measurements

Measured on the same Apple M3 Max host class and the same deterministic
1,401-chunk × 4,096-dimension corpus used by #289 and #291. Five samples ran in
one process with one operation per sample; medians are shown. “Cold” remains a
new immutable connection plus its first snapshot-backed query with the OS file
cache left warm.

```sh
GO_LLM_RAG_FILE_BENCH=1 \
GO_LLM_RAG_FILE_BENCH_DIMS=4096 \
go test ./rag/ -run '^$' -bench '^BenchmarkFileBackedRAG$' \
  -benchmem -benchtime=1x -count=5
```

| Measure | #291 JSON | #293 packed float32 | Change |
|---|---:|---:|---:|
| SQLite DB bytes | 115,122,176 | 24,014,848 | 79.14% smaller (4.79×) |
| dense cold open + query | 1,021.116 ms | 43.313 ms | 23.58× faster |
| dense cold allocated bytes | 570,894,568 | 144,239,680 | 74.73% lower (3.96×) |
| hybrid cold open + query | 1,032.126 ms | 46.676 ms | 22.11× faster |
| dense warm query | 6.604 ms | 6.498 ms | within warm-path variation |
| hybrid warm query | 7.640 ms | 7.484 ms | within warm-path variation |
| resident normalized vector bytes | 45,907,968 | 45,907,968 | unchanged |

The packed corpus contains 22,976,400 embedding bytes including envelopes; the
remaining roughly 1.0 MiB is SQLite tables, indexes, metadata, and page slack.
Cold allocation still includes the intentionally retained contiguous normalized
float64 snapshot. Warm allocation and scoring behavior remain the #291 path;
the improvement is confined to persistence size and one-time snapshot load.
