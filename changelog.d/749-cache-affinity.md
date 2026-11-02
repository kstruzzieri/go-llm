### Changed — openai-compat sends each host only the cache key it documents (#749)

openai-compat clients no longer send `x-opencode-session` to every host. Each
client picks a cache-affinity mode from its base URL: an `https` URL on
`api.openai.com` sends the body field `prompt_cache_key`, one on `openrouter.ai`
sends the `x-session-id` header, one on `opencode.ai` keeps sending
`x-opencode-session` with the raw session id, and every other host (local
llama.cpp, Abacus, `http` URLs) gets nothing. The OpenAI and OpenRouter values
are an HMAC-SHA256 of the session id under a random per-process secret, so they
change on restart and are not the thread id or the workspace hash. A request
with no session id (`--no-session`, `golem -p`) sends nothing in any mode.

Set the mode explicitly with `"cache_affinity"` on an openai-compat provider in
`models.json` (`none`, `openai`, `openrouter` or `opencode`; anything else, or
the field on a non-openai-compat provider, fails to load), with
`openaicompat.WithCacheAffinity`, or with `llm-bench -cache-affinity`. Code that
builds `openaicompat.NewClient` directly against an OpenCode mirror on another
host, or over `http`, must now pass `WithCacheAffinity("opencode")` to keep the
header OpenCode requires.
