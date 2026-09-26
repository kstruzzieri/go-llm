### Security — Child capability and budget limits (#449)

Nested agent runs now intersect their input, generation and configured step
capacities with the active parent. An unset parent input ceiling resolves to
8,192, and smaller parent generation caps may shorten child summaries, including
those using a separately configured larger model route.

Finite token allowances now reserve the assembled prompt estimate plus a fixed
generation cap before inference and account for concurrent and repeated child
runs together. Requests that do not fit stop before calling the model. Unknown
or failed usage retains its reservation; reported overruns stop before executing
returned tool calls. This is estimated admission accounting, not an exact
provider billing guarantee.

Result.DescendantUsage reports descendant usage separately while Result.Usage
and Golem's existing output retain their local-step meaning. Returned run scopes
reject late child admission and publish immutable usage snapshots.

Dispatch retains the canonical read-only tool registry, caller-guard composition
and one-subtree-per-child boundary; scoped retrieval remains separate. Golem's
parent total remains unbounded: this change adds no new finite aggregate Golem
spend pool.
