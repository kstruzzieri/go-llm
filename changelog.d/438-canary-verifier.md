### Security — Per-conversation canary verifier (#438)

With Golem's opt-in `-interceptors` chain, plant an unpredictable canary for
each live conversation activation and abort when its complete ASCII
case-insensitive marker appears outside system instructions. The marker lasts
across ordinary turns and `/clear`, rotates on startup and successful `/new` or
`/resume`, and is burned after a canary abort surfaced by the managed top-level
turn; renewal fails closed before another model or tool call.

A tainted model response aborts before any tool in that response is dispatched;
actions completed earlier in the turn are not rolled back. Detection covers
collected content and thinking, tool-call metadata and raw or decoded arguments,
and the existing non-system input projection. A tool-result match is detected
after the tool ran but before its result enters model context. Inspection runs
after stream emission, so already displayed tokens or stream-JSON deltas cannot
be retracted. A canary discovered only while sealing can override the final
invocation result without rewriting an already emitted Runtime terminal event.

Ordinary trace system metadata omits only the planted fragment; this is not
general redaction of provider errors, unrelated interceptor metadata or external
logging. Detected canary aborts skip content-full traces.

Dispatch children inherit detection without independent canary planting.
Delegate, AgentFlow planner, summarizer and grounding prompts remain outside
the planting guarantee. A nested run's error gains no new parent-abort or
parent-renewal semantics. JSON escape decoding in tool arguments remains covered;
other transformed, encoded, split, Unicode-normalized or cross-message marker
representations remain outside complete-marker matching.
