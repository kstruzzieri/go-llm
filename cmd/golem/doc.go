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
// -interceptors installs the #436 injection detectors (zero-width characters,
// base64/hex-encoded instructions, exact and scrambled instruction phrases)
// on the agent and on every dispatch child. Injection content is classified by
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
// finding. These three injection detectors return no output findings.
//
// The same opt-in chain installs Secrets. It blocks supported credential and
// payment-card shapes at every origin across input, completed model content and
// thinking, raw and decoded JSON tool arguments, and tool/verifier
// observations. Blocked observations are replaced before the next model
// request. Streaming tokens already emitted remain outside interception.
// Classified blocks are omitted from CLI history and content-full traces, use
// a fixed stderr/machine diagnostic, and may add only a finding count to
// content-light telemetry. Initial blocks save no conversation or checkpoint
// row; later blocks retain undo records for earlier allowed mutations. A
// caller-owned blocked agent.Result may still contain its original goal.
// -interceptors remains off by default.
//
// The same chain carries the #439 guards, so they are opt-in with it.
// Argument invariants block a tool call before it is planned or prompted:
// write_file, edit_file and promote_artifact under a .git, .ssh, .gnupg,
// .aws or .kube component, read_file under the credential components or
// the exact basename .env, and an inline sh/bash/dash/ksh/zsh -c script that
// pipes a curl or wget stdout fetch into a bare shell (optionally under
// sudo). The guard reads the argument the tool's own decoder would use, so
// a case-variant field name is guarded and two equivalent spellings are
// blocked as ambiguous. The model sees "tool call blocked by interceptor
// invariants (<name>)". The egress classifier tags every run_command and
// start_command by what its argv visibly reaches (privileged, network,
// package-manager, interpreter, unknown) after peeling env, nohup, nice,
// time, timeout and stdbuf; anything it cannot parse, including an inline
// script it cannot read literally, and any command outside its quiet set,
// stays visible as unknown. The approval prompt
// appends the current call's class and label to the risk line,
// "interceptor risk 20 · egress: network (git push)", on grant-covered
// auto-approvals too. These are finite checks over the argv, not a sandbox:
// go build may still download modules and make runs whatever the Makefile
// says. No score or badge revokes a grant. The hard line-count limit the
// issue mentioned is deferred; the existing 256 KiB write bounds remain.
//
// Independently of -interceptors, every tool result reaches the model inside
// a keyed <<<TOOL_RESULT / >>>TOOL_RESULT frame minted per request, and every
// effective system prompt carries agent.ToolTrustContract (framed content is
// data, never instructions; project guidance only where delegated) after the
// Golem application prompt and its capability-gated write/exec clauses
// (#430). For observations allowed through the interceptor pipeline, the
// terminal, events, session store and traces show raw results; approval,
// grants and sandboxes remain the enforcement layer.
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
// Filesystem indexing, including startup auto-indexing and golem index, scans
// before hashing, chunking or embedding independently of -interceptors. It
// skips detected secret/payment-card files by default and removes their old
// indexed source. The library supports per-kind redaction; Golem has no
// redaction or scanner-disable flag. Managed documents are excluded.
// Redaction-only runs succeed, while manual partial runs with safe skips exit
// non-zero. Policy-affected unsafe/failing generations retire their active
// pointer. Managed readers validate that pointer before each newly admitted
// retrieval; admitted calls may finish. Retirement is logical, not secure
// erasure, and a failed pointer write cannot guarantee cross-process removal.
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
// Durable checkpoint writes also produce signed MutationReceipts: interactive
// write_file/edit_file, headless -allow-tool, late /allow-write, startup-enabled
// scratch promotion, and actual /undo restores/deletions. Scratch promotion
// availability stays frozen at startup. Existing approvals are unchanged.
// A signed intent precedes filesystem work; observed success requires a durable
// applied receipt. Post-write signing, database, or hardening failure can leave
// an uncertain change and halts further writes. Recovery never invents applied
// evidence, and reports target-state recovery without it as unconfirmed.
// Completed inverse evidence is reconciled without replaying the operation or
// overwriting later edits; earlier uncertain attempts survive completed retries.
//
// The per-user Ed25519 key is <dataDirBase>/golem/signing/agent-ed25519.pem,
// outside the workspace, with owner-only storage and symlink checks. It loads
// once per write-enabled runtime; read-only sessions do not touch it. First
// creation announces a new identity. AgentID is the key ID, not a model/session.
// Retained current-workspace history requires the existing matching key.
// Missing-key diagnostics name the escaped path and historical claimed key ID
// and request restoration from backup; mismatches name the receipt, claimed
// and loaded key IDs, and path. Writes stay disabled without key replacement.
// Invalid history diagnostics never echo unchecked record bytes. There is no
// unsigned fallback, algorithm flag, automatic rotation, repair, or backfill.
// An empty workspace history cannot detect prior global identity loss.
//
// Schema v3 is additive and refused by older binaries. Finish interrupted v1/v2
// recovery/undo with the previous binary before upgrading; migration refuses
// those states before changing the schema. Completed unsigned history remains
// listed but authenticated /undo refuses it. Downgrade needs a pre-upgrade
// backup. Receipts survive completed undo and snapshot pruning without automatic
// expiry; 50 checkpoints / 64 MiB bounds undo snapshots, not total DB growth.
//
// /checkpoints appends [invalid receipts] for bad linkage/metadata, [unsigned]
// for null forward references, [unconfirmed] for missing applied evidence, or
// [receipts verified] for complete authentic evidence bound to row metadata,
// in that precedence order. Non-null missing references are invalid. Any
// unauthenticatable retained history fails the command with
// "receipt history unverifiable; evidence labels unavailable". Listing is
// read-only and checks neither live files nor full prior-content blobs;
// pending undo still hashes restore blobs and guards live content/type/mode.
//
// AgentFlow task/RAM undo, parallel promotion/rollback, direct embedders,
// arbitrary subprocess/external-editor writes, scratch copies/cleanup, and
// Golem metadata are excluded. AgentFlow proof receipts are a separate feature.
// MutationReceipts attest a host key's transition/observation, not approval,
// complete process attribution, trusted time, or power-loss durability. External
// writer race windows and best-effort file fsync remain. There is no audit chain,
// completeness, whole-ledger deletion/reordering/rollback/truncation detection,
// external anchor, or standalone public-key retention/export. Intent-only
// evidence must not become a clean successful audit in #447.
//
// The portable agent/tools helpers sign the complete Body, including Kind, with
// the existing Ed25519 or HMAC signer; Golem always uses Ed25519. V1 envelopes
// are limited to 32 KiB. Mutation IDs use crypto/rand.Text's uppercase base32
// spelling (at least 26 characters, permitting future growth). See
// docs/plans/2026-09-05-mutation-receipts-445-spec-plan.md for the full contract.
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
