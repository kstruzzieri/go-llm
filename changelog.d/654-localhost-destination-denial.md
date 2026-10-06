### Fixed — a `localhost` that resolves off-host is a destination denial (#654)

- The destination guard already refused to dial a `localhost` base URL that
  resolves to any non-loopback address (for example a tampered hosts file),
  but its error was untyped, so callers read it as an outage: provider
  bootstrap logged a warning and started without that provider's model list,
  and the MCP server's Ollama health probe reported the backend unavailable.
  The refusal now matches `provider.ErrDestinationDenied`, so when a provider
  the run uses names `localhost`, Golem (the REPL, `-p`, `golem index`,
  `golem source`, `golem models`) and `go-llm-mcp` stop at startup instead, as
  for any other destination denial. Headless Golem reports it as
  `provider_unavailable` (exit 1).
- `configview` diagnostic subjects (`selector_type_conflict`, `chain_invalid`)
  are still cut at 64 bytes but no longer split a multi-byte UTF-8 character,
  which JSON output rendered as U+FFFD. They appear in `golem models -json`,
  the MCP configview resource and `golem ops`.
