// Command golem is a terminal coding agent. It wires the shared
// provider/router bootstrap and file tools into an agent.Orchestrator and
// drives it from a line-based REPL. The workspace is read-only by default:
// -allow-write and -allow-exec opt into approval-gated mutation and command
// execution, and retrieve can be enabled for a prebuilt RAG index. Session,
// memory, and agent-memory features write per-user databases outside the
// workspace. For thinking-capable models, -think off|on|low|medium|high
// drives reasoning behavior and captured thinking renders dim above the
// answer (a no-op with a notice when the model does not support thinking).
//
// The openai-compat backend URL can be overridden with -base-url (or
// GO_LLM_BASE_URL); otherwise, when the configured loopback URL does not
// serve the agent model, startup scans 127.0.0.1:8080-8090 for it
// (-no-probe disables the scan).
//
// The source subcommand manages ad-hoc documents in the workspace index over
// the managed-document registry:
//
//	golem source add <path>              ingest a local UTF-8 file
//	golem source add -text -name <name>  ingest stdin as a named text document
//	golem source list [-json]            list managed sources with state and freshness
//	golem source rm <id>                 delete a managed source and its chunks
//	golem source reindex <id>            re-read and re-embed a managed source
//
// Mutations acquire the workspace index writer lease and publish a new
// immutable index generation; the active index is never modified in place.
//
// A workspace may declare a post-write verification command in .golem.json at
// its root, read only under -allow-write and only from that exact path (no
// ancestor search):
//
//	{"verify": {"argv": ["go", "build", "./..."], "dir": ".", "timeout_seconds": 60}}
//
// Structured argv only: there is no shell, and the file cannot supply an
// environment, an output limit, or a sandbox policy. After any tool-call batch
// that successfully ran write_file or edit_file, the command runs once and its
// bounded result is appended to that batch's last successful write, so the
// model sees a break in the same turn. A failure never fails the run.
//
// It runs once per mutating BATCH, not once per turn, so a turn that edits in
// several steps pays for it several times: prefer a fast check, a build over a
// full test suite. Only the first few kilobytes reach the model, stderr first.
//
// Because a repository-supplied argv is still arbitrary host execution, the
// resolved command and cwd are approved once per session at first use, under
// their own grant namespace: a verification grant can never authorize
// run_command or start_command, or the reverse. The command is resolved and
// frozen at startup, so editing .golem.json mid-session changes nothing until
// the next run; a malformed file warns and disables verification rather than
// failing the session.
//
// Verification runs on the host with no isolation, so a verifier that writes
// (a formatter, a codegen step) produces changes the checkpoint journal did
// not capture and /undo will not restore. Prefer a read-only check.
//
// Planning mode (-goal "<text>"): a local model authors an AgentFlow plan for
// the goal using read-only tools, Golem compiles and locks it via agentflow
// lock-plan, then stops. The locked .agent/plan.lock.json is the durable output;
// run it with -plan (task execution) separately. Planning is read-only and
// mutually exclusive with -p, -plan, -allow-write, -allow-exec, -rag-db,
// -delegate, -dispatch, -mcp-*, -evidence, and the
// -approve-plan-edits/-approve-plan-gates execution approvals.
package main
