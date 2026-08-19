package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/fingerprint"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/mcpclient"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
	"github.com/kstruzzieri/go-llm/rag"
)

type flags struct {
	version             bool // -version: print build identity and exit
	configPath          string
	root                string
	prompt              string
	promptSet           bool // -p was passed (distinguishes an explicit empty prompt)
	ollamaURL           string
	baseURL             string
	baseURLSet          bool // -base-url was passed (distinguishes an explicit empty value)
	noProbe             bool
	noCapProbe          bool
	ragDB               string
	maxSteps            int
	inputCeiling        int
	outputReserve       int
	noColor             bool
	noEditor            bool
	noSession           bool
	fresh               bool
	sessionID           string
	allowWrite          bool
	allowExec           bool
	delegate            bool
	delegateRole        string
	dispatch            bool
	dispatchRole        string
	mcpStdio            stringSliceFlag
	mcpHTTP             stringSliceFlag
	noRag               bool
	noAutoIndex         bool
	progressive         bool
	noProjectContext    bool
	noCompress          bool
	noMemory            bool
	agentMemory         bool
	trace               bool
	telemetry           bool
	pressureWarn        int
	feedback            bool
	feedbackDB          string
	think               string
	planPath            string
	planWorkers         int
	planWorkersSet      bool
	approveEdits        bool
	approveGates        bool
	agentflowSrc        string
	agentflowStatus     bool
	agentflowResume     bool
	jsonOutput          bool
	evidencePath        string
	reviewManifest      string
	taskBriefPath       string // -task-brief: explicit Agentflow task brief for external -plan input
	taskBriefSet        bool
	workflowHandoffPath string // -workflow-handoff: exact route approved and materialized by planning mode
	workflowHandoffSet  bool
	workflowProfile     string // optional Agentflow-selected profile override
	workflowReason      string // required rationale paired with workflowProfile
	wfProfileSet        bool
	wfReasonSet         bool
	goal                string // AgentFlow planning mode goal (-goal)
	goalSet             bool   // -goal was passed (distinguishes an explicit empty goal)
	approvePlanLock     bool   // -approve-plan-lock: non-interactive planning-mode lock approval
}

func parseFlags(args []string) (flags, error) {
	var f flags
	fs := flag.NewFlagSet("golem", flag.ContinueOnError)
	fs.BoolVar(&f.version, "version", false, "print golem build version and exit")
	fs.StringVar(&f.configPath, "config", "", "path to models.json (default: auto-discover)")
	fs.StringVar(&f.root, "root", ".", "workspace root the tools are scoped to")
	fs.StringVar(&f.ollamaURL, "ollama-url", "", "override Ollama base URL")
	fs.StringVar(&f.baseURL, "base-url", "", "override the openai-compat backend base URL for the primary agent model (server root, without /v1); used exactly as given, disables discovery")
	fs.BoolVar(&f.noProbe, "no-probe", false, "disable openai-compat backend port discovery; explicit and configured URLs are still used as resolved")
	fs.BoolVar(&f.noCapProbe, "no-cap-probe", false, "disable the active tool-capability probe for undeclared models (catalog and explicit capabilities still apply)")
	fs.StringVar(&f.prompt, "p", "", "one-shot mode: run a single agent turn with this prompt and exit; only the final answer goes to stdout (implies -no-session -no-compress -no-memory; approval-gated tools are unavailable, so -allow-write/-allow-exec are ignored)")
	fs.StringVar(&f.ragDB, "rag-db", "", "path to a prebuilt RAG SQLite DB to enable the retrieve tool")
	fs.IntVar(&f.maxSteps, "max-steps", 0, "max agent steps per prompt (0 => default 16)")
	fs.IntVar(&f.inputCeiling, "input-ceiling", 0, "token input ceiling (0 => derive from model context)")
	fs.IntVar(&f.outputReserve, "output-reserve", 0, "token output reserve")
	fs.BoolVar(&f.noColor, "no-color", false, "disable ANSI styling (automatic for non-terminal output, NO_COLOR, or TERM=dumb)")
	fs.BoolVar(&f.noEditor, "no-editor", false, "disable TTY line editing and history; read plain lines from stdin")
	fs.BoolVar(&f.noSession, "no-session", false, "disable persistent session memory")
	fs.BoolVar(&f.fresh, "fresh", false, "start a new persistent session instead of resuming this workspace")
	fs.BoolVar(&f.allowWrite, "allow-write", false, "enable approval-gated write_file/edit_file tools")
	fs.BoolVar(&f.allowExec, "allow-exec", false, "enable the approval-gated run_command exec tool")
	fs.BoolVar(&f.delegate, "delegate", false, "enable the delegate_code tool (route a scoped codegen sub-task to a specialist model)")
	fs.StringVar(&f.delegateRole, "delegate-role", "coding", "model role the delegate_code tool routes to")
	fs.BoolVar(&f.dispatch, "dispatch", false, "enable the dispatch tool (bounded read-only exploration tasks run sequentially in child agents)")
	fs.StringVar(&f.dispatchRole, "dispatch-role", "", "model role dispatch child agents route to (default: the primary agent chain, so children never force a model swap)")
	fs.Var(&f.mcpStdio, "mcp-stdio", "attach an MCP server over stdio: \"[alias=]command args...\" (repeatable; use `env KEY=val cmd` for env vars)")
	fs.Var(&f.mcpHTTP, "mcp-http", "attach an MCP server over streamable HTTP: \"[alias=]https://endpoint\" (repeatable)")
	fs.BoolVar(&f.noRag, "no-rag", false, "disable the retrieve tool entirely (ignore any auto index)")
	fs.BoolVar(&f.noAutoIndex, "no-auto-index", false, "disable startup auto-index refresh; existing auto indexes may still be used")
	fs.BoolVar(&f.progressive, "progressive", false, "generate and retrieve opt-in L0/L1 progressive source summaries; enable mixed context assembly")
	fs.BoolVar(&f.noProjectContext, "no-project-context", false, "do not load AGENTS.md project-context files into the system prompt")
	fs.BoolVar(&f.noCompress, "no-compress", false, "disable post-turn conversation compression into a durable summary")
	fs.BoolVar(&f.noMemory, "no-memory", false, "disable explicit local memories (/remember, /memories, memory_search)")
	fs.BoolVar(&f.agentMemory, "agent-memory", false, "enable agent-authored memory records (agent_memory_* tools, /records; requires sessions)")
	fs.StringVar(&f.sessionID, "session", "", "explicit session id to resume or create (default: per-workspace)")
	// -trace persists a full per-run trace (may contain workspace/user content; for replay/eval).
	// -telemetry appends content-light run metrics only (timings, route, usage; no prompt or output).
	fs.BoolVar(&f.trace, "trace", false, "persist a content-full run trace per turn (outside the workspace; may contain workspace/user/memory content)")
	fs.BoolVar(&f.telemetry, "telemetry", false, "append content-light run telemetry (timings, route, usage; no prompt/output)")
	fs.IntVar(&f.pressureWarn, "pressure-warn", 75, "context-pressure warn threshold percent 1-100 (0 disables the warning line)")
	fs.BoolVar(&f.feedback, "feedback", false, "enable local behavioral feedback collection and retrieval ranking")
	fs.StringVar(&f.feedbackDB, "feedback-db", "", "override the behavioral feedback DB path (default: per-workspace under the data dir)")
	fs.StringVar(&f.think, "think", "", "reasoning control for the agent model: off, on, low, medium, high (default: model decides); no-op with a notice when the model does not support thinking")
	fs.StringVar(&f.planPath, "plan", "", "AgentFlow task mode: path to a plan document (JSON) to lock and execute; requires both -approve-plan-edits and -approve-plan-gates; mutually exclusive with -p, -allow-write/-allow-exec, -rag-db, -delegate, -dispatch, and -mcp-*")
	fs.IntVar(&f.planWorkers, "plan-workers", 1, "AgentFlow task mode: maximum workers for the initial parallel plan cohort (positive; requires -plan)")
	fs.BoolVar(&f.approveEdits, "approve-plan-edits", false, "required in task mode: auto-approve step-scoped write/edit (still bounded by the step-scope and .agent guards)")
	fs.BoolVar(&f.approveGates, "approve-plan-gates", false, "required in task mode: auto-run plan-declared validation gates")
	fs.StringVar(&f.agentflowSrc, "agentflow-src", "", "run 'python3 -m agentflow' with PYTHONPATH=<checkout>/src instead of the agentflow binary")
	fs.BoolVar(&f.agentflowStatus, "agentflow-status", false, "inspect the current Agentflow next action without mutation")
	fs.BoolVar(&f.agentflowResume, "agentflow-resume", false, "resume an existing Agentflow run serially; requires -plan and both plan approvals")
	fs.BoolVar(&f.jsonOutput, "json", false, "with -agentflow-status, relay Agentflow next-action JSON verbatim")
	fs.StringVar(&f.evidencePath, "evidence", "", "optional evidence sidecar JSON object/array recorded before lock in task mode")
	fs.StringVar(&f.reviewManifest, "review-manifest", "", "task mode: Agentflow review manifest to record and execute as finding-linked amendments; requires -plan")
	fs.StringVar(&f.taskBriefPath, "task-brief", "", "task mode: explicit Agentflow task brief JSON for an externally supplied plan (default: conservative exact-fact projection)")
	fs.StringVar(&f.workflowHandoffPath, "workflow-handoff", "", "task mode: approved workflow recommendation already materialized by planning mode")
	fs.StringVar(&f.workflowProfile, "workflow-profile", "", "Agentflow modes: explicitly select a workflow profile; requires -workflow-reason")
	fs.StringVar(&f.workflowReason, "workflow-reason", "", "Agentflow modes: non-empty rationale paired with -workflow-profile")
	fs.StringVar(&f.goal, "goal", "", "AgentFlow planning mode: author and preview a traceable plan, require approval to lock it, then stop")
	fs.BoolVar(&f.approvePlanLock, "approve-plan-lock", false, "planning mode: print the plan preview and approve the lock without prompting (non-interactive -goal)")
	if err := fs.Parse(args); err != nil {
		return flags{}, err
	}
	if fs.NArg() > 0 {
		return flags{}, fmt.Errorf("golem: unexpected positional arguments %q; every option must be a -flag", fs.Args())
	}
	f.think = strings.ToLower(f.think)
	switch f.think {
	case "", "off", "on", "low", "medium", "high":
	default:
		return flags{}, fmt.Errorf("golem: invalid -think %q (want off, on, low, medium, or high)", f.think)
	}
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "p":
			f.promptSet = true
		case "plan-workers":
			f.planWorkersSet = true
		case "base-url":
			f.baseURLSet = true
		case "goal":
			f.goalSet = true
		case "task-brief":
			f.taskBriefSet = true
		case "workflow-handoff":
			f.workflowHandoffSet = true
		case "workflow-profile":
			f.wfProfileSet = true
		case "workflow-reason":
			f.wfReasonSet = true
		}
	})
	return f, nil
}

// shouldStartAutoIndex reports whether default REPL startup should run the
// background auto-index job. Flags only: auto-path and embed-chain errors are
// handled at the wiring site.
func shouldStartAutoIndex(f flags) bool {
	return !f.promptSet && !f.goalSet && !f.noAutoIndex && !f.noRag && f.ragDB == ""
}

// autoIndexEnabled is the wiring gate for the background auto-index job:
// the flag decision plus the two startup resolution errors (auto index path,
// embedding chain) that force the immediate enableRetrieve path instead.
func autoIndexEnabled(f flags, autoErr, embChainErr error) bool {
	return shouldStartAutoIndex(f) && autoErr == nil && embChainErr == nil
}

// validateFlags rejects flag values flag.Parse cannot police.
func validateFlags(f flags) error {
	if f.agentflowStatus && f.agentflowResume {
		return fmt.Errorf("golem: -agentflow-status and -agentflow-resume are mutually exclusive")
	}
	if f.jsonOutput && !f.agentflowStatus {
		return fmt.Errorf("golem: -json is valid only with -agentflow-status")
	}
	if f.agentflowStatus || f.agentflowResume {
		mode := "-agentflow-status"
		if f.agentflowResume {
			mode = "-agentflow-resume"
		}
		if f.agentflowStatus && (f.planPath != "" || f.planWorkersSet || f.approveEdits || f.approveGates || f.taskBriefPath != "" || f.workflowHandoffPath != "") {
			return fmt.Errorf("golem: %s is read-only and does not accept task execution flags", mode)
		}
		if f.agentflowResume && f.planPath == "" {
			return fmt.Errorf("golem: %s requires -plan", mode)
		}
		if f.agentflowResume && f.planWorkers != 1 {
			return fmt.Errorf("golem: %s requires -plan-workers=1", mode)
		}
		if f.promptSet || f.goalSet || f.reviewManifest != "" || f.evidencePath != "" ||
			f.wfProfileSet || f.wfReasonSet || f.workflowProfile != "" || f.workflowReason != "" ||
			f.ragDB != "" || f.delegate || f.dispatch || len(f.mcpStdio) > 0 || len(f.mcpHTTP) > 0 ||
			f.allowWrite || f.allowExec || f.approvePlanLock {
			return fmt.Errorf("golem: %s cannot be combined with planning, setup, review, or ambient tool flags", mode)
		}
	}
	if f.promptSet && strings.TrimSpace(f.prompt) == "" {
		return fmt.Errorf("golem: -p requires a non-empty prompt")
	}
	if f.promptSet && (f.fresh || f.sessionID != "") {
		return fmt.Errorf("golem: -p (one-shot) is incompatible with -session and -fresh")
	}
	if f.fresh && f.sessionID != "" {
		return fmt.Errorf("golem: -fresh and -session are mutually exclusive")
	}
	if f.noRag && f.ragDB != "" {
		return fmt.Errorf("golem: -no-rag and -rag-db are mutually exclusive")
	}
	if f.dispatchRole != "" && !f.dispatch {
		return fmt.Errorf("golem: -dispatch-role requires -dispatch")
	}
	if f.pressureWarn < 0 || f.pressureWarn > 100 {
		return fmt.Errorf("golem: -pressure-warn must be between 0 and 100")
	}
	if f.feedbackDB != "" && !f.feedback {
		return fmt.Errorf("golem: -feedback-db requires -feedback")
	}
	if f.planWorkers < 0 || (f.planWorkersSet && f.planWorkers < 1) {
		return fmt.Errorf("golem: -plan-workers must be positive")
	}
	if f.planPath == "" && (f.planWorkersSet || f.planWorkers > 1) {
		return fmt.Errorf("golem: -plan-workers is valid only with -plan (task mode)")
	}
	if f.reviewManifest != "" && f.planPath == "" {
		return fmt.Errorf("golem: -review-manifest is valid only with -plan (task mode)")
	}
	if f.taskBriefSet && strings.TrimSpace(f.taskBriefPath) == "" {
		return fmt.Errorf("golem: -task-brief requires a non-empty path")
	}
	if f.taskBriefPath != "" && f.planPath == "" {
		return fmt.Errorf("golem: -task-brief is valid only with -plan (task mode)")
	}
	if f.workflowHandoffSet && strings.TrimSpace(f.workflowHandoffPath) == "" {
		return fmt.Errorf("golem: -workflow-handoff requires a non-empty path")
	}
	if f.workflowHandoffPath != "" && f.planPath == "" {
		return fmt.Errorf("golem: -workflow-handoff is valid only with -plan (task mode)")
	}
	profileSet := f.wfProfileSet || f.workflowProfile != ""
	reasonSet := f.wfReasonSet || f.workflowReason != ""
	if profileSet != reasonSet ||
		(profileSet && (strings.TrimSpace(f.workflowProfile) == "" || strings.TrimSpace(f.workflowReason) == "")) {
		return fmt.Errorf("golem: -workflow-profile and a non-empty -workflow-reason must be supplied together")
	}
	if (profileSet || reasonSet) && f.planPath == "" && !f.goalSet {
		return fmt.Errorf("golem: -workflow-profile/-workflow-reason apply only to -goal or -plan Agentflow modes")
	}
	if f.workflowHandoffPath != "" && (profileSet || reasonSet) {
		return fmt.Errorf("golem: -workflow-handoff already binds the approved route; do not pass -workflow-profile/-workflow-reason")
	}
	if f.planPath != "" && f.promptSet {
		return fmt.Errorf("golem: -plan (task mode) is incompatible with -p (one-shot)")
	}
	if f.planPath != "" && (f.allowWrite || f.allowExec) {
		return fmt.Errorf("golem: -plan (task mode) uses -approve-plan-edits/-approve-plan-gates; do not pass -allow-write/-allow-exec")
	}
	if f.planPath != "" && f.ragDB != "" {
		return fmt.Errorf("golem: -plan (task mode) does not use RAG; do not pass -rag-db")
	}
	if f.planPath != "" && f.delegate {
		return fmt.Errorf("golem: -plan (task mode) does not attach delegate_code; proof-mode tools are built from the locked plan")
	}
	if f.planPath != "" && f.dispatch {
		return fmt.Errorf("golem: -plan (task mode) does not attach dispatch; proof-mode tools are built from the locked plan")
	}
	if f.planPath != "" && (len(f.mcpStdio) > 0 || len(f.mcpHTTP) > 0) {
		return fmt.Errorf("golem: -plan (task mode) does not attach MCP tools; proof-mode tools are built from the locked plan")
	}
	if f.planPath != "" && (!f.approveEdits || !f.approveGates) {
		return fmt.Errorf("golem: -plan (task mode) requires both -approve-plan-edits and -approve-plan-gates")
	}
	if f.goalSet && strings.TrimSpace(f.goal) == "" {
		return fmt.Errorf("golem: -goal requires a non-empty goal")
	}
	if f.goalSet && f.planPath != "" {
		return fmt.Errorf("golem: -goal (planning mode) is incompatible with -plan (task execution)")
	}
	if f.goalSet && f.promptSet {
		return fmt.Errorf("golem: -goal (planning mode) is incompatible with -p (one-shot)")
	}
	if f.goalSet && (f.allowWrite || f.allowExec) {
		return fmt.Errorf("golem: -goal (planning mode) is read-only; do not pass -allow-write/-allow-exec")
	}
	if f.goalSet && f.ragDB != "" {
		return fmt.Errorf("golem: -goal (planning mode) does not use RAG; do not pass -rag-db")
	}
	if f.goalSet && f.delegate {
		return fmt.Errorf("golem: -goal (planning mode) does not attach delegate_code")
	}
	if f.goalSet && f.dispatch {
		return fmt.Errorf("golem: -goal (planning mode) does not attach dispatch")
	}
	if f.goalSet && (len(f.mcpStdio) > 0 || len(f.mcpHTTP) > 0) {
		return fmt.Errorf("golem: -goal (planning mode) does not attach MCP tools")
	}
	if f.goalSet && f.evidencePath != "" {
		return fmt.Errorf("golem: -goal (planning mode) authors no evidence; do not pass -evidence")
	}
	if f.goalSet && (f.approveEdits || f.approveGates) {
		return fmt.Errorf("golem: -goal (planning mode) does not execute; -approve-plan-edits/-approve-plan-gates apply to -plan")
	}
	if f.approvePlanLock && !f.goalSet {
		return fmt.Errorf("golem: -approve-plan-lock applies to -goal (planning mode) only")
	}
	return nil
}

// applyTaskMode forces the same non-interactive, ambient-state-free defaults for
// AgentFlow task mode (-plan) that one-shot mode forces for -p, plus disabling
// RAG/auto-index: task mode drives a locked plan with a step-scoped toolset and
// must not open sessions, memory, compression, RAG, or the auto-index worker.
func applyTaskMode(f flags) (flags, []string) {
	if f.planPath == "" {
		return f, nil
	}
	var warns []string
	if f.sessionID != "" || f.fresh {
		warns = append(warns, "task mode: -session/-fresh ignored (task mode does not persist a session)")
	}
	if f.feedback {
		warns = append(warns, "task mode: -feedback ignored (task mode does not use RAG or feedback ranking)")
	}
	if f.trace || f.telemetry {
		warns = append(warns, "task mode: -trace/-telemetry are not wired for the task run; the AgentFlow proof pack is the durable record")
	}
	f.noSession = true
	f.noCompress = true
	f.noMemory = true
	f.agentMemory = false
	f.noAutoIndex = true
	f.noRag = true
	return f, warns
}

// applyGoalMode forces the same non-interactive, ambient-state-free defaults for
// AgentFlow planning mode (-goal) that task mode forces for -plan: the planner
// runs read-only with no session, memory, compression, RAG, auto-index, or
// feedback. Project-context (-no-project-context) is deliberately left untouched:
// AGENTS.md instructions are planning input, not ambient conversational state.
func applyGoalMode(f flags) (flags, []string) {
	if !f.goalSet {
		return f, nil
	}
	var warns []string
	if f.sessionID != "" || f.fresh {
		warns = append(warns, "planning mode: -session/-fresh ignored (planning mode does not persist a session)")
	}
	if f.feedback {
		warns = append(warns, "planning mode: -feedback ignored (planning mode does not use RAG or feedback ranking)")
	}
	if f.trace || f.telemetry {
		warns = append(warns, "planning mode: -trace/-telemetry are not wired for the planner run; the locked plan is the durable record")
	}
	f.noSession = true
	f.noCompress = true
	f.noMemory = true
	f.agentMemory = false
	f.noAutoIndex = true
	f.noRag = true
	f.feedback = false
	f.feedbackDB = ""
	return f, warns
}

// applyOneShotMode forces the scripting-safe defaults -p implies: no session
// persistence, no post-turn compression, no memory DB, and no approval-gated
// tools (there is no interactive approver to answer the prompt, so
// -allow-write/-allow-exec are dropped with a warning instead of dangling
// unanswerable approvals).
func applyOneShotMode(f flags) (flags, []string) {
	if !f.promptSet {
		return f, nil
	}
	var warns []string
	f.noSession = true
	f.noCompress = true
	f.noMemory = true
	if f.allowWrite || f.allowExec {
		warns = append(warns, "one-shot: -allow-write/-allow-exec ignored (approval prompts need the REPL); write/exec tools unavailable")
		f.allowWrite = false
		f.allowExec = false
	}
	return f, warns
}

type startupInfo struct {
	workspace          string
	agentflowState     bool
	backendLine        string
	useRecommend       bool
	bootstrapWarns     []error
	preflightWarns     []string
	retrieveLine       string
	retrieveOmitted    bool
	retrieveRequested  bool // -rag-db, -no-rag, or auto suppress the generic no-index notice
	thinkLine          string
	inputCeilingLine   string
	sessionLine        string
	projectContextLine string
	memoryLine         string
	agentMemoryLine    string
	mcpLine            string
	delegateLine       string
	dispatchLine       string
}

// startupNotices renders the human-facing startup lines (written to stderr).
func startupNotices(info startupInfo) []string {
	var out []string
	out = append(out, "workspace: "+info.workspace)
	if info.agentflowState {
		out = append(out, "agentflow state detected: inspect with -agentflow-status; resume with -agentflow-resume -plan <plan>")
	}
	if info.backendLine != "" {
		out = append(out, info.backendLine)
	}
	if info.sessionLine != "" {
		out = append(out, info.sessionLine)
	}
	if info.memoryLine != "" {
		out = append(out, info.memoryLine)
	}
	if info.agentMemoryLine != "" {
		out = append(out, info.agentMemoryLine)
	}
	if info.mcpLine != "" {
		out = append(out, info.mcpLine)
	}
	if info.delegateLine != "" {
		out = append(out, info.delegateLine)
	}
	if info.dispatchLine != "" {
		out = append(out, info.dispatchLine)
	}
	if info.projectContextLine != "" {
		out = append(out, info.projectContextLine)
	}
	if info.retrieveLine != "" {
		out = append(out, info.retrieveLine)
	}
	if info.useRecommend {
		out = append(out, "no defaults.agent configured; using model recommendation (run will route to the recommended model)")
	}
	if info.thinkLine != "" {
		out = append(out, info.thinkLine)
	}
	if info.inputCeilingLine != "" {
		out = append(out, info.inputCeilingLine)
	}
	for _, w := range info.bootstrapWarns {
		out = append(out, "warning: "+w.Error())
	}
	for _, w := range info.preflightWarns {
		out = append(out, "warning: "+w)
	}
	// The generic no-index notice is only meaningful when retrieve was NOT
	// requested. When -rag-db was given but failed, a specific "retrieve
	// disabled: ..." warning above already explains why.
	if info.retrieveOmitted && !info.retrieveRequested {
		out = append(out, "retrieve unavailable: no RAG index configured; using file/search tools")
	}
	return out
}

// newOrchestratorFactory returns the session's orchestrator constructor. The
// session builds one per agentflow parallel worker on top of the startup
// orchestrator, and every one must see the same context policy: -progressive
// drives BOTH the Retrieve renderer and #331 mixed context assembly, from one
// flag.
//
// It takes flags rather than a bool so the production call site cannot pass a
// value other than the one -progressive parsed into.
//
// With -dispatch it also installs the per-run dispatch invocation cap.
func newOrchestratorFactory(caller agent.ModelCaller, f flags) func() *agent.Orchestrator {
	var opts []agent.Option
	if f.dispatch {
		// #282 coordination: cap dispatch INVOCATIONS per run so the parent
		// cannot bypass child-local limits by re-dispatching fresh children;
		// with the library's per-call task cap this bounds total children per
		// run. The option fails fast on a Run whose tool set omits the named
		// tool — sound here because -dispatch is rejected in -plan/-goal and
		// agentflow modes, so every Run through this factory carries the
		// dispatch tool.
		opts = append(opts, agent.WithToolInvocationLimit(agent.ToolInvocationLimit{
			Tool: agenttools.DispatchToolName,
			Max:  agenttools.DefaultDispatchCallsPerRun,
		}))
	}
	return func() *agent.Orchestrator {
		return agent.New(caller, agent.ContextManager{Mixed: f.progressive}, opts...)
	}
}

func shouldShowAgentflowHint(f flags) bool {
	return !f.promptSet && f.planPath == "" && !f.goalSet && !f.agentflowStatus && !f.agentflowResume
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		// -h / -help: the flag package already printed usage to stderr; help
		// is not a failure, so exit cleanly without a spurious "golem:" line.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		var statusErr *agentflowStatusExit
		if errors.As(err, &statusErr) {
			os.Exit(statusErr.ExitCode())
		}
		// runIndex/runOneShot already rendered their own output; just exit non-zero.
		if errors.Is(err, errIndexFailed) || errors.Is(err, errOneShotFailed) || errors.Is(err, errAgentflowTaskFailed) {
			os.Exit(1)
		}
		_, _ = fmt.Fprintf(os.Stderr, "golem: %v\n", err)
		os.Exit(1)
	}
}

func colorEnabled(out *os.File, noColor bool) bool {
	return colorPermitted(noColor) && realTermOps{}.IsTerminal(int(out.Fd()))
}

// colorPermitted holds the flag/environment gates separately from the TTY
// probe so their truth table is testable without a terminal.
func colorPermitted(noColor bool) bool {
	return !noColor && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

// runHooks exposes only the process lifecycle boundaries that package tests
// must control or observe deterministically. A zero value is the production
// path; it neither replaces composition nor coordinates shutdown.
type runHooks struct {
	openFeedback        func(context.Context, string, string, func(string)) (*feedbackService, error)
	startAutoIndex      func() func()
	afterAutoIndexStart func(lineSourceMode, agent.Tool, *feedbackService) error
	closed              func(string)
}

func formatFeedbackReport(report feedbackReport) string {
	reasons := make([]string, 0, len(report.reasons))
	for reason, count := range report.reasons {
		if count != 0 {
			reasons = append(reasons, fmt.Sprintf("%s:%d", reason, count))
		}
	}
	if report.attempted == 0 && report.completed == 0 && report.dropped == 0 && len(reasons) == 0 && report.presentationDuplicates == 0 && report.presentationJoinMisses == 0 {
		return ""
	}
	sort.Strings(reasons)
	dropReasons := "none"
	if len(reasons) > 0 {
		dropReasons = strings.Join(reasons, ",")
	}
	return fmt.Sprintf("behavioral feedback: attempted=%d completed=%d dropped=%d drop_reasons=%s duplicates=%d join_misses=%d",
		report.attempted, report.completed, report.dropped, dropReasons, report.presentationDuplicates, report.presentationJoinMisses)
}

func run(args []string, stdin *os.File, stdout, stderr *os.File, testHooks ...runHooks) error {
	var hooks runHooks
	if len(testHooks) > 0 {
		hooks = testHooks[0]
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "index":
			return runIndex(context.Background(), args[1:], stdout, stderr)
		case "models":
			return runModels(context.Background(), args[1:], stdout, stderr)
		default:
			return fmt.Errorf("unknown command %q (did you mean \"index\" or \"models\"?)", args[0])
		}
	}

	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	if f.version {
		_, _ = fmt.Fprintln(stdout, versionString())
		return nil
	}
	if err := validateFlags(f); err != nil {
		return err
	}
	f, taskWarns := applyTaskMode(f)
	f, goalWarns := applyGoalMode(f)
	taskWarns = append(taskWarns, goalWarns...)
	f, oneShotWarns := applyOneShotMode(f)
	oneShotWarns = append(taskWarns, oneShotWarns...)

	root, err := filepath.Abs(f.root)
	if err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}

	ctx := context.Background()
	if f.agentflowStatus {
		return runAgentflowStatus(ctx, stdout, root, f.agentflowSrc, f.jsonOutput)
	}

	cfg, err := loadConfig(f.configPath)
	if err != nil {
		return err
	}

	backendRes, err := resolveBackend(ctx, cfg, backendResolveOpts{
		flagBaseURL: f.baseURL,
		flagSet:     f.baseURLSet,
		noProbe:     f.noProbe,
		lookupEnv:   os.LookupEnv,
		prober:      openaicompat.DiscoverBaseURL,
	})
	if err != nil {
		return err // explicit-override validation error: fatal, matches validateFlags semantics
	}

	var capStore fingerprint.CapProbeStore
	var profileStore fingerprint.Store
	capStoreWarn := ""
	if !f.noCapProbe || f.inputCeiling <= 0 {
		capHandle, warning := openCapProbeStore(ctx, os.Getenv, root)
		if capHandle != nil {
			profileStore = capHandle.profiles
			if !f.noCapProbe {
				capStore = capHandle.store
			}
			defer func() { _ = capHandle.db.Close() }()
		}
		capStoreWarn = warning
	}

	bundle, err := providerbootstrap.New(ctx, providerbootstrap.Options{
		Config:                          cfg, // nil => default Ollama provider
		OllamaURLOverride:               f.ollamaURL,
		OpenAICompatURLOverrideProvider: backendRes.providerKey,
		OpenAICompatURLOverride:         backendRes.baseURL,
		FingerprintProfileStore:         profileStore,
		CapabilityProbeStore:            capStore, // nil when -no-cap-probe or open fully failed
	})
	if err != nil {
		return fmt.Errorf("bootstrap providers: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	plan, err := resolveAgentChain(bundle.Config)
	if err != nil {
		return err
	}

	resolveEndpoint := newPreflightEndpointResolver(bundle.Config, f.ollamaURL, backendRes.providerKey, backendRes.diagSource())
	var resolver toolCallResolver
	if !f.noCapProbe && capStore != nil {
		resolver = bundle.Models // concrete *provider.ModelRegistry; always non-nil after bootstrap
	}
	warns, err := preflightToolCapable(ctx, bundle.Models, plan.chain, resolveEndpoint, resolver)
	warns = append(backendRes.warns, warns...)
	if err != nil {
		if len(backendRes.warns) > 0 {
			return fmt.Errorf("%s\n%w", strings.Join(backendRes.warns, "\n"), err)
		}
		return err
	}
	warns = append(warns, oneShotWarns...)
	if capStoreWarn != "" {
		warns = append(warns, capStoreWarn)
	}

	thinkOpts, thinkLine := resolveThinkOptions(ctx, bundle.Models, plan.chain, f.think)
	inputCeiling := resolveInputCeiling(ctx, bundle.Models, plan.chain, f.inputCeiling, f.outputReserve, resolver != nil)

	autoDBPath, autoWorkspaceID, autoErr := indexDBPathForWorkspace(os.Getenv, root)
	if autoErr != nil && !f.noRag && f.ragDB == "" {
		warns = append(warns, "retrieve auto-index disabled: "+autoErr.Error())
	}
	feedbackOpener := hooks.openFeedback
	if feedbackOpener == nil {
		feedbackOpener = openFeedbackService
	}
	feedbackSvc, feedbackWarning := openConfiguredFeedback(ctx, f.feedback && !f.noRag, root, f.feedbackDB, os.Getenv,
		func(line string) { _, _ = fmt.Fprintln(stderr, line) }, feedbackOpener)
	if feedbackWarning != "" {
		warns = append(warns, feedbackWarning)
	}
	defer func() {
		report, closeErr := feedbackSvc.close()
		if line := formatFeedbackReport(report); line != "" {
			_, _ = fmt.Fprintln(stderr, line)
		}
		if closeErr != nil {
			_, _ = fmt.Fprintln(stderr, "behavioral feedback close failed: "+closeErr.Error())
		}
		if hooks.closed != nil {
			hooks.closed("feedback")
		}
	}()
	feedbackWeighter := feedbackSvc.behavioralWeighter()
	embChain, embChainErr := embeddingChain(bundle.Config)
	var retrieve agent.Tool
	retrieveLine := ""
	retrieveRequested := f.ragDB != "" || f.noRag
	var ready *readyRetrieve
	if autoIndexEnabled(f, autoErr, embChainErr) {
		// Install a valid prior immutable generation before the background
		// refresh starts. The wrapper remains warming only on a true first run.
		ready = newReadyRetrieve(warmingRetrieveMessage)
		defer func() {
			if closeErr := ready.close(); closeErr != nil {
				_, _ = fmt.Fprintln(stderr, closeErr)
			}
			if hooks.closed != nil {
				hooks.closed("retrieval")
			}
		}()
		retrieve = ready
		rr := enableRetrieve(ctx, bundle.Config, bundle.Router, retrieveOpts{
			autoDBPath:  autoDBPath,
			workspaceID: autoWorkspaceID, weighter: feedbackWeighter, progressive: f.progressive,
		})
		warns = append(warns, rr.warns...)
		if rr.reader != nil {
			ready.install(rr.reader, rr.line)
			retrieveLine = rr.line + "; refresh in background"
		} else {
			retrieveLine = "retrieve: auto-index warming in background"
		}
		// Defensive only: the non-nil wrapper already makes retrieveOmitted
		// false, which is what actually suppresses the generic no-index notice.
		retrieveRequested = true
	} else {
		rr := enableRetrieve(ctx, bundle.Config, bundle.Router, retrieveOpts{
			noRag:       f.noRag,
			ragDB:       f.ragDB,
			autoDBPath:  autoDBPath,
			workspaceID: autoWorkspaceID,
			weighter:    feedbackWeighter,
			progressive: f.progressive,
		})
		if rr.reader != nil {
			// Route the explicit generation through the same readyRetrieve
			// wrapper the auto path uses: its Invoke admits calls through
			// reader.inflight under lock, so close() drains in-flight
			// retrievals before the store and feedback service shut down.
			// A raw rr.tool would bypass admission and let shutdown close
			// both under an active retrieval. Local on purpose: the outer
			// `ready` gates background-refresh wiring, which an explicit
			// -rag-db run must never start.
			wrapped := newReadyRetrieve(warmingRetrieveMessage)
			wrapped.install(rr.reader, rr.line)
			retrieve = wrapped
			defer func() {
				if closeErr := wrapped.close(); closeErr != nil {
					_, _ = fmt.Fprintln(stderr, closeErr)
				}
				if hooks.closed != nil {
					hooks.closed("retrieval")
				}
			}()
		}
		warns = append(warns, rr.warns...)
		retrieveLine = rr.line
		retrieveRequested = retrieveRequested || rr.suppressNotice
	}
	tools, err := buildTools(root, nil)
	if err != nil {
		return err
	}
	// Everything appended after this point (retrieve, dispatch, memory, write,
	// exec, delegate, MCP) lands in tools[readToolCount:], which is what the
	// consumer runtime receives as EXTRA tools — it rebuilds the base file
	// tools from its own workspace. Reorderings must keep the file tools first.
	readToolCount := len(tools)
	if retrieve != nil {
		tools = append(tools, retrieve)
	}
	retrieveOmitted := retrieve == nil

	// Dispatch is built here, before memory/write/exec/delegate/MCP are
	// appended, so the child-visible set is exactly the read-only tools above.
	dispatchLine := ""
	if f.dispatch {
		dchain, derr := resolveDispatchChain(bundle.Config, f.dispatchRole, plan.chain)
		if derr != nil {
			return derr
		}
		childCeiling := inputCeiling.ceiling
		if f.dispatchRole != "" {
			// The run-level preflight and input ceiling above cover plan.chain
			// only. An explicit dispatch chain needs its own tool-capability
			// gate (children always carry the file tools, so a chain without
			// tool_call would otherwise fail only at invocation time) and its
			// own context-derived ceiling.
			dwarns, perr := preflightToolCapable(ctx, bundle.Models, dchain, resolveEndpoint, resolver)
			warns = append(warns, dwarns...)
			if perr != nil {
				return perr
			}
			childCeiling = resolveInputCeiling(ctx, bundle.Models, dchain, f.inputCeiling, f.outputReserve, resolver != nil).ceiling
		}
		caller := newRouterChainCallerFor(bundle.Router, dchain, dispatchUseCase)
		dpt, derr := newDispatchTool(caller, f.progressive, agent.Budget{InputCeiling: childCeiling, OutputReserve: f.outputReserve}, tools)
		if derr != nil {
			return derr
		}
		tools = append(tools, dpt)
		head := "model recommendation"
		if len(dchain) > 0 {
			head = dchain[0]
		}
		dispatchLine = fmt.Sprintf("dispatch: enabled -> %s", head)
	}

	wantAgentMemory, agentMemoryWarn := agentMemoryRequest(f.agentMemory, f.noSession)
	if agentMemoryWarn != "" {
		warns = append(warns, agentMemoryWarn)
	}
	mrt := openMemoryRuntime(ctx, os.Getenv, root, !f.noMemory, wantAgentMemory)
	warns = append(warns, mrt.warns...)
	memoryEnabled := mrt.user != nil
	agentMemoryEnabled := mrt.records != nil
	if memoryEnabled {
		tools = append(tools, agenttools.MemorySearch{S: mrt.user, WorkspaceID: workspaceID(root), Limit: 8})
	}
	defer func() {
		if mrt.db != nil {
			_ = mrt.db.Close()
		}
	}()

	var journal *mutationJournal
	if f.allowWrite {
		wt, j, werr := buildWriteTools(root)
		if werr != nil {
			return werr
		}
		tools = append(tools, wt...)
		journal = j
	}

	if f.allowExec {
		et, eerr := buildExecTools(root)
		if eerr != nil {
			return eerr
		}
		tools = append(tools, et...)
	}

	delegateLine := ""
	if f.delegate {
		dt, dchain, derr := buildDelegateTool(bundle.Config, bundle.Router, f.delegateRole, nil)
		if derr != nil {
			return derr
		}
		tools = append(tools, dt)
		delegateLine = fmt.Sprintf("delegate: enabled -> %s", dchain[0])
	}

	var mcpManager *mcpclient.Manager
	mcpAttached := false
	mcpLine := ""
	if servers, perr := parseMCPServers(f.mcpStdio, f.mcpHTTP); perr != nil {
		return perr // fatal: bad flag config / explicit duplicate alias
	} else if len(servers) > 0 {
		mgr, mcpWarns, cerr := mcpclient.Connect(ctx, mcpClientImpl(), servers)
		if cerr != nil {
			return cerr // fatal: invalid / duplicate alias
		}
		for _, w := range mcpWarns {
			warns = append(warns, "mcp: "+w.Error())
		}
		mcpTools := mgr.Tools()
		tools = append(tools, mcpTools...)
		mcpManager = mgr
		mcpAttached = len(mcpTools) > 0
		// Positive confirmation so a silently-failed server attach is visible:
		// attached-tool count against the configured-server count (failures and
		// skipped tools appear as the "mcp: ..." warnings above).
		mcpLine = fmt.Sprintf("mcp: attached %d tool(s) from %d configured server(s)", len(mcpTools), len(servers))
	}
	defer func() {
		if mcpManager != nil {
			_ = mcpManager.Close()
		}
	}()

	baseSystem := buildSystemPrompt(f.allowWrite, f.allowExec)
	baseSystem += delegateSystemFragment(f.delegate, f.allowWrite)
	baseSystem += dispatchSystemFragment(f.dispatch)
	baseSystem += memorySystemFragment(memoryEnabled)
	projectContextLine := ""
	projectContextBlock := ""
	if !f.noProjectContext {
		if block, n, perr := loadProjectContext(ctx, root, os.Getenv); perr != nil {
			warns = append(warns, "project context disabled: "+perr.Error())
		} else if block != "" {
			baseSystem = baseSystem + "\n\n" + block
			projectContextBlock = block
			projectContextLine = fmt.Sprintf("project context: loaded %d file(s)", n)
		}
	}

	var sessn *session
	var sessionLine string
	if !f.noSession {
		switch dbPath, derr := sessionDBPathForWorkspace(os.Getenv, root); {
		case derr != nil:
			warns = append(warns, "session disabled: "+derr.Error())
		default:
			if id, ierr := resolveSessionID(sessionIDOpts{fresh: f.fresh, explicit: f.sessionID, root: root}); ierr != nil {
				warns = append(warns, "session disabled: "+ierr.Error())
			} else if s, info, oerr := openSession(ctx, dbPath, id); oerr != nil {
				warns = append(warns, "session disabled: "+oerr.Error())
			} else {
				sessn = s
				sessionLine = info.line()
			}
		}
	}
	defer func() { _ = sessn.Close() }() // nil-safe

	// Appended after the session block (not beside memorySystemFragment) so the
	// framing can reflect whether the session actually opened: without one,
	// create/promote deterministically error, so the model must not be told to
	// use them. baseSystem is consumed only at replSession construction below;
	// this fragment now trails the project-context block in the composed prompt.
	baseSystem += agentMemorySystemFragment(agentMemoryEnabled, sessn != nil)

	if agentMemoryEnabled {
		tools = appendAgentMemoryTools(tools, mrt.records, mrt.dbPath, workspaceID(root), sessn)
	}

	memoryLine := ""
	if memoryEnabled {
		memoryLine = "memory: enabled"
	}
	agentMemoryLine := agentMemoryNotice(agentMemoryEnabled, sessn != nil)
	for _, line := range startupNotices(startupInfo{
		workspace:          root,
		agentflowState:     shouldShowAgentflowHint(f) && agentflowStateDetected(root),
		backendLine:        backendRes.notice,
		useRecommend:       plan.useRecommend,
		bootstrapWarns:     bundle.Warnings,
		preflightWarns:     warns,
		retrieveLine:       retrieveLine,
		retrieveOmitted:    retrieveOmitted,
		retrieveRequested:  retrieveRequested,
		thinkLine:          thinkLine,
		inputCeilingLine:   inputCeiling.line(),
		sessionLine:        sessionLine,
		projectContextLine: projectContextLine,
		memoryLine:         memoryLine,
		agentMemoryLine:    agentMemoryLine,
		mcpLine:            mcpLine,
		delegateLine:       delegateLine,
		dispatchLine:       dispatchLine,
	}) {
		_, _ = fmt.Fprintln(stderr, line)
	}

	summarizeChain, err := resolveSummarizeChain(bundle.Config)
	if err != nil {
		return err
	}
	var sourceSummarizer rag.SourceSummaryGenerator
	if f.progressive {
		if len(summarizeChain) == 0 {
			_, _ = fmt.Fprintln(stderr, "golem: warning: "+progressiveNoChainWarning(true))
		}
		sourceSummarizer = routerSourceSummaryGenerator(bundle.Router, summarizeChain)
	}

	newOrchestrator := newOrchestratorFactory(newRouterChainCaller(bundle.Router, plan.chain), f)
	orch := newOrchestrator()

	obsv, err := newObserv(os.Getenv, root, f.trace, f.telemetry, time.Now)
	if err != nil {
		return fmt.Errorf("golem: observability setup: %w", err)
	}

	budget := agent.Budget{InputCeiling: inputCeiling.ceiling, OutputReserve: f.outputReserve}
	if f.pressureWarn > 0 {
		// The agent package owns the band layout (single source of truth for the
		// monotonic clamp + defaults); golem only supplies the warn fraction.
		budget.Pressure = agent.PressureThresholdsForWarn(float64(f.pressureWarn) / 100)
	}
	summarizer := agent.NewRouterSummarizer(bundle.Router, summarizeChain)
	runtime, err := golemruntime.New(ctx, golemruntime.Options{
		Root:     root,
		System:   baseSystem,
		Tools:    tools[readToolCount:],
		MaxSteps: f.maxSteps,
		Budget:   budget,
		// The REPL line reader accepts lines up to 1 MiB; keep the runtime's
		// message bound in lockstep so a pasted log or diff is not rejected.
		MaxMessageBytes: maxGoalBytes,
		ModelOptions:    thinkOpts,
		Summarizer:      summarizer,
		// The CLI is the trusted host: -trace records include model reasoning.
		RetainReasoning:    true,
		DisableCompression: f.noCompress,
		OnWarning: func(err error) {
			_, _ = fmt.Fprintf(stderr, "warning: %v\n", err)
		},
		Orchestrator: orch,
	})
	if err != nil {
		return err
	}
	defer func() {
		_ = runtime.Close()
		if hooks.closed != nil {
			hooks.closed("runtime")
		}
	}()

	renderOut := stdout
	if f.promptSet {
		renderOut = stderr
	}
	sess := &replSession{
		orch:                orch,
		runtime:             runtime,
		newOrchestrator:     newOrchestrator,
		tools:               tools,
		baseSystem:          baseSystem,
		projectContextBlock: projectContextBlock,
		maxSteps:            f.maxSteps,
		budget:              budget,
		color:               colorEnabled(renderOut, f.noColor),
		retrieveOmitted:     retrieveOmitted,
		session:             sessn,
		journal:             journal,
		grants:              newApprovalGrants(),
		allowWrite:          f.allowWrite,
		allowExec:           f.allowExec,
		mcpAttached:         mcpAttached,
		memory:              mrt.user,
		memoryDBPath:        mrt.dbPath,
		records:             mrt.records,
		workspaceID:         workspaceID(root),
		obs:                 obsv,
		feedback:            feedbackSvc,
		pressureWarn:        f.pressureWarn > 0,
		mixed:               f.progressive,
		modelOptions:        thinkOpts,
	}
	if sess.maxSteps == 0 {
		sess.maxSteps = 16 // mirror agent defaultMaxSteps so the footer's k/max is accurate
	}

	interrupts := make(chan struct{}, 1)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, interruptSignals()...)
	defer signal.Stop(sigCh)

	// Interactive REPL routes Ctrl-C through replControl: mid-turn cancels the
	// turn; a first idle Ctrl-C hints and a second quits. It also reprints the
	// prompt under async auto-index notices so the prompt is never buried. The
	// one-shot (-p) path keeps the plain cancel-the-turn wiring and stderr
	// notices.
	replCtx := ctx
	notice := func(line string) { _, _ = fmt.Fprintln(stderr, line) }
	onInterrupt := func() {
		select {
		case interrupts <- struct{}{}:
		default:
		}
	}
	if !f.promptSet {
		var cancelREPL context.CancelFunc
		replCtx, cancelREPL = context.WithCancel(ctx)
		defer cancelREPL()
		ctrl := newReplControl(stdout, stderr, interrupts, cancelREPL)
		sess.control = ctrl
		if feedbackSvc != nil {
			feedbackSvc.warn.set(ctrl.notice)
		}
		notice = ctrl.notice
		onInterrupt = ctrl.interrupt
	}
	go func() {
		for range sigCh {
			onInterrupt()
		}
	}()

	// startAutoIndex is nil unless auto-indexing is enabled. It is a closure so
	// the REPL branch can start it after binding the idle display and stop it
	// before the source closes; every other mode never starts it at all.
	var startAutoIndex func() func()
	if ready != nil {
		startAutoIndex = func() func() {
			// Launched after startup construction succeeds so async notices never
			// interleave the synchronous startup block, and setup errors do not race
			// the background job against deferred provider cleanup.
			autoCtx, cancelAuto := context.WithCancel(ctx)
			autoDone := make(chan struct{})
			go func() {
				defer close(autoDone)
				runAutoIndex(autoCtx, autoIndexJob{
					root:        root,
					dbPath:      autoDBPath,
					workspaceID: autoWorkspaceID,
					cfg:         bundle.Config,
					router:      bundle.Router,
					embedder: newChainEmbedder(func(rc context.Context, rreq provider.RoutingRequest) (embedExecutor, error) {
						return bundle.Router.Route(rc, rreq)
					}, embChain),
					embChain:    embChain,
					weighter:    feedbackWeighter,
					progressive: f.progressive,
					summarize:   sourceSummarizer,
					ready:       ready,
					notice:      notice,
				})
			}()
			return func() {
				cancelAuto()
				<-autoDone
			}
		}
	}
	if hooks.startAutoIndex != nil {
		startAutoIndex = hooks.startAutoIndex
	}

	// Final dispatch. A line source is created only where an interactive read
	// can actually happen, so the modes that never read stdin open no reader.
	//
	// Auto-indexing is orthogonal to input and every branch must start it when
	// enabled: shouldStartAutoIndex excludes only -p and -goal, so -plan wants
	// the background refresh without wanting a reader. Only the REPL has to
	// bind the idle display first, which is why it starts the job itself rather
	// than here. Keeping the guarded call in all three branches makes that
	// invariant structural -- a branch that forgets it leaves the retrieve
	// wrapper on its warming message for the whole run while the startup banner
	// has already announced a refresh that never starts.
	switch lineSourceModeFor(f) {
	case sourceAnswerOnly:
		// Interactive -goal reads only the plan-lock approval: a source, but
		// no idle display and no history.
		if startAutoIndex != nil {
			stopAutoIndex := startAutoIndex()
			defer stopAutoIndex()
			if hooks.afterAutoIndexStart != nil {
				if err := hooks.afterAutoIndexStart(sourceAnswerOnly, retrieve, feedbackSvc); err != nil {
					return err
				}
			}
		}
		return withLineSource(newInput(inputConfig{
			Stdin:       stdin,
			Stdout:      stdout,
			Stderr:      stderr,
			NoEditor:    f.noEditor,
			Getenv:      os.Getenv,
			Root:        root,
			OnInterrupt: onInterrupt,
		}), func(src lineSource) error {
			return runAgentflowAuthor(ctx, src, stdout, stderr, interrupts, sess, f, root)
		})
	case sourceNone:
		if startAutoIndex != nil {
			stopAutoIndex := startAutoIndex()
			defer stopAutoIndex()
			if hooks.afterAutoIndexStart != nil {
				if err := hooks.afterAutoIndexStart(sourceNone, retrieve, feedbackSvc); err != nil {
					return err
				}
			}
		}
		if f.goalSet {
			return runAgentflowAuthor(ctx, nil, stdout, stderr, interrupts, sess, f, root)
		}
		if f.planPath != "" {
			return runAgentflowTask(ctx, stdout, stderr, interrupts, sess, f, root)
		}
		return runOneShot(ctx, stdout, stderr, interrupts, sess, f.prompt)
	}

	// /edit is wired regardless of -no-editor: the flag disables the inline
	// line editor, not external composition. Availability is still gated on
	// real terminals at dispatch time.
	base, derr := dataDirBase(os.Getenv)
	if derr != nil {
		// Reported, not discarded. Leaving goalEditor nil rendered "/edit
		// requires an interactive terminal", which sends a user with a HOME-less
		// environment hunting a terminal problem that does not exist.
		_, _ = fmt.Fprintf(stderr, "golem: /edit unavailable: %v\n", derr)
	} else {
		sess.goalEditor = &ttyGoalEditor{
			stdin: stdin, stdout: stdout, stderr: stderr,
			getenv:  os.Getenv,
			ops:     realTermOps{},
			run:     runEditorProcess,
			dataDir: filepath.Join(base, "golem"),
			root:    root,
		}
	}
	return withLineSource(newInput(inputConfig{
		Stdin:      stdin,
		Stdout:     stdout,
		Stderr:     stderr,
		NoEditor:   f.noEditor,
		UseHistory: true, // only the default REPL reads goals worth recalling
		Getenv:     os.Getenv,
		Root:       root,
		// Raw mode disables ISIG, so the editor sees Ctrl-C as an in-band
		// byte; it delivers the event to the same policy owner the SIGINT
		// handler uses, keeping one interrupt taxonomy for both modes.
		OnInterrupt: onInterrupt,
	}), func(src lineSource) error {
		// Bound before the auto-index goroutine can emit a notice, so no
		// asynchronous message is ever rendered through the default display
		// while a source exists.
		if sess.control != nil {
			sess.control.setIdleDisplay(src.IdleDisplay)
			// Unbound after the notice producer is joined and before the
			// source closes, so the Ctrl-C window between runREPL returning
			// and signal.Stop cannot render through a closed source.
			defer sess.control.setIdleDisplay(nil)
		}
		if startAutoIndex != nil {
			stopAutoIndex := startAutoIndex()
			// Cancels and joins the writer before withLineSource closes the
			// source, so no notice can reach a closed source.
			defer stopAutoIndex()
			if hooks.afterAutoIndexStart != nil {
				if err := hooks.afterAutoIndexStart(sourceREPL, retrieve, feedbackSvc); err != nil {
					return err
				}
			}
		}
		return runREPL(replCtx, src, stdout, interrupts, sess)
	})
}

func interruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
