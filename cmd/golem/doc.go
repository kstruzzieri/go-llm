// Command golem is a minimal read-only terminal coding agent. It wires the
// shared provider/router bootstrap and the read-only file tools into an
// agent.Orchestrator and drives it from a line-based REPL. v1 never mutates
// the project: only read, search, glob, and list tools are exposed.
package main
