// Package agenttrace persists agent-run observability as two projections that
// share a run id: a post-run, content-full trace (issue #238) and content-light
// live telemetry spans (issue #239). It owns record schemas, a secured
// JSON/JSONL writer, the live TelemetrySink observer, and the post-run trace
// builder. It owns no path policy: callers supply secured, validated paths.
package agenttrace
