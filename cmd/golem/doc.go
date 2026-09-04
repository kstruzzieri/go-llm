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
// serve the active model route, startup scans 127.0.0.1:8080-8090 for it
// (-no-probe disables the scan).
//
// -interceptors installs the #436 detector chain (zero-width characters,
// base64/hex-encoded instructions, exact and scrambled instruction phrases)
// on the agent and on every dispatch child. Content is classified by
// provenance: workspace files, command output, memory records, plan
// diagnostics and model-origin tool results are tagged (a fixed trailer tells
// the model the content is data, and the run's risk score grows), while an
// injection in a foreign result (an MCP tool) is replaced by a fixed blocked
// marker before the model sees it. Interactive tool-call and plan-lock prompts
// show "interceptor risk 30"; the verifier approval prompt cannot show it.
// Successful REPL and -p stderr footers append " · risk 30". The
// non-interactive -approve-plan-lock path is unchanged. A dispatch child's
// score stays in that child's existing risk_score envelope field rather than
// aggregating into the parent report. The -trace record carries every parent
// finding. The default detectors return no output findings; streamed model
// tokens therefore remain unchanged. Off by default until the trailers'
// effect on answer quality is measured.
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
// its root, read only when writes are enabled (at startup with -allow-write or
// later with /allow-write) and only from that exact path (no ancestor search):
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
// frozen when writes are enabled, so editing .golem.json after that point
// changes nothing until the next run; a malformed file warns and disables
// verification rather than failing the session.
//
// Verification runs on the host with no isolation, so a verifier that writes
// (a formatter, a codegen step) produces changes the checkpoint journal did
// not capture and /undo will not restore. Prefer a read-only check.
//
// -grounding is a SEPARATE, unrelated check: it verifies the ANSWER, not the
// workspace. The .golem.json "verify" command above runs a workspace command
// after a write; -grounding asks a model whether the final answer's claims are
// supported by the retrieval evidence that reached the answering prompt. They
// share no configuration, no approval, and no naming.
//
// When a completed turn used retrieve and its answering prompt carried
// retrieval attribution, Golem runs the claim-support judge over exactly that
// evidence and prints one dim line:
//
//	grounding · partial · 3/4 claims · 5 evidence · 1.2s · 850 tok
//
// It works on both retrieval modes. Verification is fail-open and never
// changes the answer, the agent result, the session, the recorded run status,
// or the exit code: a routing failure, malformed verify output, or the 60s
// ceiling prints one line naming a stable reason (timeout, judge_failed) and
// nothing else. Ctrl-C during the judge cancels grounding alone; the answer is
// already yours. A turn that ran no retrieve is silent, while a turn that
// retrieved but whose answering prompt carried no evidence says so
// (no_final_evidence) rather than passing for a turn that never retrieved.
//
// Evidence the CLI cannot reconstruct exactly is never judged. A retrieved
// chunk that was capped, or an identity that resolves to two different chunks,
// reports evidence_incomplete and makes no model call, because a verdict over
// a silently reduced evidence subset would report claims as unsupported that
// the model had support for.
//
// The two verifier stages route by their own extract/verify side-task
// use-cases at background priority, so grounding never displaces the primary
// agent model; with no verify/extract, analysis, or chat default configured,
// startup warns once and the flag has no effect. Verifier tokens are reported
// only on the grounding line and in the trace, never in the run's own usage
// footer or in telemetry.
//
// What the verdict MEANS is narrower than "is this answer correct". It measures
// only whether each claim is supported by the retrieval evidence in that
// prompt. An answer that correctly explains a retrieved function using ordinary
// language or standard-library knowledge will have those claims marked
// unsupported, because that knowledge was not in the evidence. A "partial" is
// therefore a prompt for a human to look, not a finding that the answer is
// wrong.
//
// It also costs: two sequential model calls on every retrieval-backed turn, so
// an answer that took a few seconds can take noticeably longer to ground. A
// notice is printed while the check runs.
//
// The verifier reads retrieved workspace content, which is untrusted. The judge
// prompt fences evidence with a per-request key and instructs the model to
// treat everything inside as data, and evidence ids are echoed rather than
// accepted from the model, so a hostile chunk cannot invent a citation. It can
// still argue: the per-claim status, reason, and contradicted values come back
// from the model, so a corpus containing instructions aimed at the judge can
// influence a verdict. Treat "supported" as evidence of grounding, not as a
// security boundary over a corpus you do not trust.
//
// -trace additionally persists the complete report - every claim, its verdict,
// the evidence it cites, and any missing-evidence queries - under the trace's
// "grounding" key. That trace is content-full and already carries workspace
// text; telemetry receives no grounding field at all. Task and planning modes
// ignore -grounding with a warning, since neither runs an answer turn.
//
// Planning mode (-goal "<text>"): a model authors an AgentFlow plan for
// the goal using read-only tools, Golem compiles and locks it via agentflow
// lock-plan, then stops. The locked .agent/plan.lock.json is the durable output;
// run it with -plan (task execution) separately. Planning is read-only and
// mutually exclusive with -p, -plan, -allow-write, -allow-exec, -rag-db,
// -delegate, -dispatch, -mcp-*, -evidence, and the
// -approve-plan-edits/-approve-plan-gates execution approvals.
//
// Planning mode routes through the "planning" use case (#476), not "agent":
// a models.json authoring defaults.planning sends plan authoring to that
// role, and one that does not degrades in order through reasoning, analysis,
// and agent before falling back to model recommendation. The planning route
// is the process's single active route -- it is what destination admission
// consents, tool preflight proves, the input ceiling is sized from, and the
// orchestrator caller routes -- so goal mode performs no discovery, refresh,
// probe, or inference for the inactive agent, embedding, or summarize
// routes. Because the fallbacks can select a role on a different provider
// than agent, a remote planning route is subject to the same
// -allow-destination consent as every other remote destination.
package main
