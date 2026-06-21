// Command golem is a minimal read-only terminal coding agent. It wires the
// shared provider/router bootstrap and the read-only file tools into an
// agent.Orchestrator and drives it from a line-based REPL. v1 never mutates
// the project: file inspection tools are always exposed, and retrieve can be
// enabled for a prebuilt RAG index.
package main
