### Removed — Legacy destination flag syntax (#501)

**Breaking change in v0.4.0:** `golem` and `go-llm-mcp` no longer accept
`-allow-destination "provider=URL"`. Update scripts to use the repeatable
canonical form, for example `-allow-destination "openai/https://api.openai.com"`.
Rejected legacy values report the canonical grammar without echoing the value
or credentials. Canonical destination normalization and admission policy are
unchanged; equals signs in canonical provider names and URL paths remain valid.
