// Package projectcontext discovers and reads durable project-context files such
// as AGENTS.md so a consumer can inject stable workspace guidance into prompts.
// It is intentionally narrow: discovery, safe reads, and ordering only — not a
// prompt framework. The library is consumer-agnostic; the consumer supplies the
// workspace root and an optional global directory.
package projectcontext
