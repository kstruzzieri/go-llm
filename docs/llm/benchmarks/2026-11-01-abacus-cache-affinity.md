# abacus-cache-affinity — 2026-11-01

Task 3.2 of the Golem prompt-caching and cost plan: does sending
`prompt_cache_key` (go-llm's `cache_affinity: "openai"`) raise how much of each
request Abacus RouteLLM serves from its prompt cache on `/v1/chat/completions`?
Abacus documents the field only for `/v1/responses`, so go-llm infers `none`
for `routellm.abacus.ai` until this was measured.

**Result: no statistically significant benefit detected in this sample.**
Abacus caches a repeated prefix on its own, with or without the key, and
accepted the field on every request. The keyed arm cached somewhat more
(84.8% against 80.3%), but this sample is too small to tell that gap from
chance, and it does not show that the key makes no difference either. go-llm
keeps inferring `none` for Abacus provisionally; `"cache_affinity": "openai"`
is accepted there.

## Provenance
- **Commit**: develop `0758c97d` (the A/B below calls the endpoint directly; see "Why not `llm-bench -cache-probe`")
- **Endpoint**: `https://routellm.abacus.ai/v1/chat/completions`, non-streaming
- **Model under test**: `gpt-5-mini` (the spec's one OpenAI-family model on Abacus)
- **Design**: 16 runs, `none` and `openai` interleaved (8 each). Each run starts
  from a fresh random nonce at the head of the system prompt, so no run inherits
  another's cache. Step 1 sends a ~3.7k-token prefix; each later step appends
  one ~790-token tool-result-sized turn, as the probe does, up to step 8
  (~9.2k tokens). The `openai` arm sends one random 32-hex `prompt_cache_key`
  per run, the same shape go-llm sends.
- **Counters**: the share is `cache_read_input_tokens / (raw_input_tokens + cache_read_input_tokens)`
  from Abacus's non-streaming `usage` (see "Usage counters" for why not `prompt_tokens`).

## Results (steps 2-8; step 1 is always cold)

| Run | Arm | Steps 2-8 cached / prompt | Share | Zero-cache steps |
|---:|---|---:|---:|---|
| 1 | `none` | 41,600 / 47,915 | 86.8% | - |
| 2 | `openai` | 41,728 / 47,929 | 87.1% | - |
| 3 | `none` | 32,896 / 47,915 | 68.7% | 2, 3 |
| 4 | `openai` | 41,600 / 47,915 | 86.8% | - |
| 5 | `none` | 41,600 / 47,908 | 86.8% | - |
| 6 | `openai` | 41,728 / 47,929 | 87.1% | - |
| 7 | `none` | 41,600 / 47,908 | 86.8% | - |
| 8 | `openai` | 41,728 / 47,929 | 87.1% | - |
| 9 | `none` | 33,408 / 47,922 | 69.7% | 8 |
| 10 | `openai` | 33,280 / 47,915 | 69.5% | 8 |
| 11 | `none` | 41,728 / 47,936 | 87.0% | - |
| 12 | `openai` | 41,728 / 47,929 | 87.1% | - |
| 13 | `none` | 33,408 / 47,922 | 69.7% | 8 |
| 14 | `openai` | 41,728 / 47,936 | 87.0% | - |
| 15 | `none` | 41,728 / 47,936 | 87.0% | - |
| 16 | `openai` | 41,728 / 47,929 | 87.1% | - |

Pooled share: `none` 80.3%, `openai` 84.8%. Zero-cache steps: `none` 4 of 56,
`openai` 1 of 56 (Fisher exact p = 0.36; runs with any miss, 3 of 8 against
1 of 8, p = 0.57).

Neither test is significant, and with five misses in all, this sample cannot
show whether the key helps. Three of the five misses fall on step 8, the
largest request, and they hit both arms (`none` runs 9 and 13, `openai` run
10). The one early miss (run 3, steps 2-3) is a `none` run. Every hit, in both
arms, covers all or nearly all of the previous request in 128-token blocks.

## Usage counters

Abacus does not report usage the way go-llm's `openai-compat` client reads it:

- **Non-streaming** responses carry `input_tokens`, `output_tokens`,
  `raw_input_tokens` and `cache_read_input_tokens`, with no `prompt_tokens` or
  `prompt_tokens_details`. go-llm reads 0 prompt tokens and no cache counter.
- **Streaming** responses carry `prompt_tokens` and
  `prompt_tokens_details.cached_tokens`, but `prompt_tokens` is billing-weighted:
  a 5,090-token request with 4,992 cached reports `prompt_tokens: 596`
  (98 uncached plus 4,992 at about a tenth). Golem streams and asks for usage,
  so on Abacus its footer can show more cached tokens than total tokens, and
  per-token cost math would discount cached tokens twice.
- `max_tokens` is not honored on `gpt-5-mini` (a 1-token cap returned 77 output
  tokens, 64 of them reasoning).

## Why not `llm-bench -cache-probe`

The probe was run first, as the plan says, with `-candidate-base-url
https://routellm.abacus.ai -models openai-compat/gpt-5-mini -cache-affinity none`.
It completed all 12 requests and reported 0 prompt tokens and `n/a` cached on
every row, because the probe's requests are non-streaming (above). The A/B
above reads Abacus's own fields instead. Rerun the probe once go-llm parses
Abacus usage.

## A/B script

Run once per arm with `ABACUS_API_KEY` set: `python3 -I abacus_ab.py gpt-5-mini <none|openai> 8`.

```python
import json, os, secrets, sys, urllib.request
model, arm, steps = sys.argv[1], sys.argv[2], int(sys.argv[3])
key = os.environ["ABACUS_API_KEY"]
nonce = secrets.token_hex(8)
pck = secrets.token_hex(16)
base = " ".join(f"Rule {i}: keep answers short and cite file paths; item {i*13}." for i in range(220))
msgs = [{"role": "system", "content": f"run {nonce}. {base}"}, {"role": "user", "content": "Read the files and summarize."}]
print(f"arm={arm} model={model} nonce={nonce}")
print("step full cached share")
for step in range(1, steps + 1):
    body = {"model": model, "messages": msgs, "max_tokens": 16}
    if arm == "openai":
        body["prompt_cache_key"] = pck
    req = urllib.request.Request("https://routellm.abacus.ai/v1/chat/completions", json.dumps(body).encode(),
                                 {"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
    u = json.load(urllib.request.urlopen(req, timeout=120)).get("usage", {})
    cached = u.get("cache_read_input_tokens", 0)
    full = u.get("raw_input_tokens", 0) + cached
    print(f"{step} {full} {cached} {cached/full:.3f}", flush=True)
    chunk = " ".join(f"file_{step}.go line {j}: func handler{j}() error {{ return nil }}" for j in range(45))
    msgs = msgs + [{"role": "assistant", "content": f"Reading file_{step}.go."}, {"role": "user", "content": f"Tool result for file_{step}.go:\n{chunk}"}]
```
