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
// Planning mode (-goal "<text>"): a local model authors an AgentFlow plan for
// the goal using read-only tools, Golem compiles and locks it via agentflow
// lock-plan, then stops. The locked .agent/plan.lock.json is the durable output;
// run it with -plan (task execution) separately. Planning is read-only and
// mutually exclusive with -p, -plan, -allow-write, -allow-exec, -rag-db,
// -delegate, -mcp-*, -evidence, and the -approve-plan-edits/-approve-plan-gates
// execution approvals.
package main
