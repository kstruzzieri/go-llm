### Added — Recipe slash commands (#353)

Golem discovers user `.recipe.json` prompt bundles from the standard
`go-llm/commands` config directory for the REPL. `/recipes` lists commands and
`/recipes reload` refreshes the catalog; help and unknown-command suggestions
include valid recipes. Positional arguments use literal quoting and bounded
shared template expansion. Expanded user goals retain ordinary approvals,
ingress checks, persistence, and cancellation. Advisory model hints apply for
one invocation without changing the permanent session model. Completes epic #344.
