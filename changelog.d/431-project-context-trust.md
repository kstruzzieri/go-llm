### Added — Project-context trust gate (#431)

Golem now requires exact, session-scoped approval before it injects selected
AGENTS.md-style project guidance. It revalidates the approved snapshot before
each operator goal and each AgentFlow authoring or task invocation, and supports
explicit `-trust-project-context` consent for scripted runs.
