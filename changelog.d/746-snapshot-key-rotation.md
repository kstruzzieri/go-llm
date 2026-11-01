### Security — Golem's project and Git snapshot fence key now rotates when exposed (#746)

Golem frames approved project guidance and the Git snapshot in the system
prompt (#430) under one key per snapshot. Until now that key was never checked
against run text, so text that quoted it (a file the model read, command
output, the model's own reply) could forge a frame the model would read as
approved project guidance or repository state for the rest of the session.
The key now rotates the way tool-result keys do since #745: when a run's text,
including the conversation history a goal replays, that history's summary and
advice staged with `/consult`, contains it, the system prompt is re-keyed
before the next request, and later goals keep the new key,
so the session never returns to a key that has been shown. Without an
exposure the system prompt stays byte-identical across requests and goals, so
the prompt cache is unaffected; a rotation misses it for one request. AgentFlow
planner runs and task steps rotate the key within each run only.

Library callers can do the same for their own system-prompt frames:
`agent.Request.SystemKeys` names the fence keys `System` frames untrusted text
under (12 or more characters of `A-Z` and `2-7`; anything else is refused
before the first model call), `agent.Result.SystemKeys` reports the keys
`System` ended under (runtime-only, never serialized into traces), and
`golem.Turn.SystemKeys` forwards them through the Golem runtime. The
containment inventory and mutation ledger record the new checks (M-075 to
M-080).
