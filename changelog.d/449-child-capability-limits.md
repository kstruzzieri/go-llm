### Security — Child capability and budget limits (#449)

Nested agent runs now intersect their input, generation and configured step
capacities with the active parent. An unset parent input ceiling resolves to
8,192, and smaller parent generation caps may shorten child summaries, including
those using a separately configured larger model route.

Finite token allowances now reserve the assembled prompt estimate plus a fixed
generation cap before inference and account for concurrent and repeated child
runs together. Requests that do not fit stop before calling the model. Unknown
or failed usage retains its reservation. A reported prompt above the estimate is
charged without stopping; output beyond the generation cap stops the run before
its returned tool calls execute. This is estimated admission accounting, not an
exact provider billing guarantee.
Exhaustion during callbacks or a tool batch also blocks subsequent invocations,
including queued parallel tools, while preserving cancellation errors after
verification callbacks. Stopped batches retain only observed tool calls, even
when provider call IDs are empty or duplicated.

Result.DescendantUsage reports descendant usage separately while Result.Usage
and Golem's existing output retain their local-step meaning. Returned run scopes
reject late child admission and publish immutable usage snapshots.

Dispatch retains the canonical read-only tool registry, caller-guard composition
and one-subtree-per-child boundary; scoped retrieval remains separate. Golem's
parent total remains unbounded: this change adds no new finite aggregate Golem
spend pool.
