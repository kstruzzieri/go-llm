package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// Background model-tool constants (#346, frozen contract). The approval-key
// namespace is DISTINCT from run_command's "exec:v2:" so a foreground grant can
// never authorize a background start; the v1 tag names the fingerprint recipe
// (commandFingerprint with zero effective and requested timeouts — the process
// lifetime is manager-owned, no timeout participates).
const (
	bgExecApprovalKeyPrefix = "exec-bg:v1:"
	bgToolTimeout           = 10 * time.Second // tool-call bound, never the job lifetime
	bgSmallOutputCap        = 4096
	bgTailOutputCap         = 20 * 1024
	bgTailDefaultBytes      = 8192
	bgTailMaxBytes          = 16384
)

var (
	_ agent.PlanningTool = (*StartCommand)(nil)
	_ agent.PlanningTool = (*StopCommand)(nil)
)

type startCommandArgs struct {
	Argv []string `json:"argv"`
	Dir  string   `json:"dir"`
}

type commandHandleArgs struct {
	Handle string `json:"handle"`
}

type commandTailArgs struct {
	Handle   string  `json:"handle"`
	Stream   string  `json:"stream"`
	Cursor   *uint64 `json:"cursor"`
	MaxBytes *int    `json:"max_bytes"`
}

// NewExecToolsWithBackground builds the full exec tool set — the foreground
// run_command plus the four background tools — over ONE shared Workspace, so
// foreground and background preparation see identical containment. The
// existing NewExecTools(root) remains the foreground-only constructor.
func NewExecToolsWithBackground(root string, manager *BackgroundManager) ([]agent.Tool, error) {
	if manager == nil || manager.backend.execBackend == nil {
		return nil, fmt.Errorf("tools: build exec tools: background manager must have a resolved backend")
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		return nil, fmt.Errorf("tools: build exec tools: %w", err)
	}
	rc := NewRunCommand(ws, manager.backend)
	rc.sandbox = manager.backend.approval
	return []agent.Tool{
		rc,
		NewStartCommand(ws, manager),
		NewCommandStatus(manager),
		NewCommandTail(manager),
		NewStopCommand(manager),
	}, nil
}

// bgCwdDisplay maps a workspace-relative dir label to the display string
// stored on the job (and later rendered %q-quoted by command_status), matching
// the preview convention and never disclosing the host's absolute root.
func bgCwdDisplay(label string) string {
	if label == "" {
		return "(workspace root)"
	}
	return label
}

// StartCommand launches an approval-gated detached background command through
// the BackgroundManager. PlanningTool: Plan validates and previews via the
// SHARED prepareExecPlan helper; Invoke re-checks via recheckExecPlan and
// publishes an opaque job handle. The dispatch context gates only admission
// and publication — a published process's lifetime is manager-owned.
type StartCommand struct {
	ws      *Workspace
	manager *BackgroundManager
	execPlanCache
}

// NewStartCommand builds the background start tool over the given Workspace
// and manager.
func NewStartCommand(ws *Workspace, manager *BackgroundManager) *StartCommand {
	return &StartCommand{ws: ws, manager: manager}
}

// Spec returns the model-facing schema for start_command.
func (t *StartCommand) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "start_command",
		Description: "Start a command in the background (argv-first, NO shell) and return an opaque handle immediately. " +
			"The command is shown to the user and starts only after they approve it; it keeps running across turns until it exits, " +
			"is stopped via stop_command, or the session shuts down. When the command exits, background processes it left running " +
			"in its process group are terminated. Poll with command_status and read output with command_tail.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "argv":{"type":"array","items":{"type":"string"},"description":"command and arguments; argv[0] is the executable, run without a shell"},
    "dir":{"type":"string","description":"working directory relative to the workspace root (default: root)"}
  },
  "required":["argv"]
}`),
	}
}

// Effect declares the same effect class as run_command with the frozen
// 10-second tool-call timeout (admission/publication only) and small result cap.
func (t *StartCommand) Effect() agent.Effect {
	return agent.Effect{
		Class:     agent.Read | agent.Write | agent.Exec | agent.Network,
		Approval:  agent.ApprovalAlways,
		Timeout:   bgToolTimeout,
		OutputCap: bgSmallOutputCap,
	}
}

// Plan validates args through the shared exec preparation helper with zero
// effective and requested timeouts (manager-owned lifetime), stashes the
// pending plan keyed by the raw-args hash, and renders the approval preview.
func (t *StartCommand) Plan(_ context.Context, raw json.RawMessage) (agent.ToolPlan, error) {
	eff := t.Effect()
	var args startCommandArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("invalid arguments: %w", err)
	}
	p, err := prepareExecPlan(t.ws, args.Argv, args.Dir, 0, 0, false)
	if err != nil {
		return agent.ToolPlan{Effect: eff}, err
	}
	t.store(ContentHash(raw), p)
	return agent.ToolPlan{
		Effect:      eff,
		Preview:     renderStartPreview(p, args.Argv[0]),
		ApprovalKey: bgExecApprovalKeyPrefix + p.fingerprint,
	}, nil
}

// Invoke consumes the pending plan (fail-closed on hash mismatch), re-checks
// cwd and executable identity via recheckExecPlan, then starts the job. The
// context is used only up to the manager's registration linearization point.
func (t *StartCommand) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	pp, ok := t.consume(ContentHash(raw))
	if !ok {
		return errResult("start preview missing; retry"), nil
	}
	spec, err := recheckExecPlan(t.ws, pp)
	if err != nil {
		return errResult(err.Error()), nil
	}
	st, err := t.manager.start(ctx, spec, bgCwdDisplay(pp.dirLabel))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return errResult("start canceled"), nil
		}
		return errResult(err.Error()), nil
	}
	return agent.ToolResult{Content: renderStartResult(st)}, nil
}

// renderStartPreview is the human approval preview for start_command — kept
// renderExecPreview-shaped (argv, exe resolution, cwd, env names, short id)
// but with a manager-owned lifetime line instead of a timeout line.
// Presentation only, never parsed.
func renderStartPreview(p execPending, originalArgv0 string) string {
	var b strings.Builder
	cwd := bgCwdDisplay(p.dirLabel)
	if p.dirLabel != "" {
		cwd = quoteArgForPreview(cwd)
	}
	b.WriteString("start background command:\n")
	fmt.Fprintf(&b, "  argv:     %s\n", renderArgvForPreview(p.argv))
	fmt.Fprintf(&b, "  exe:      %s -> %s\n", quoteArgForPreview(originalArgv0), quoteArgForPreview(p.path))
	fmt.Fprintf(&b, "  cwd:      %s\n", cwd)
	b.WriteString("  lifetime: manager-owned; runs until exit, approved stop, or session shutdown\n")
	parts := make([]string, len(p.envNames))
	for i, n := range p.envNames {
		parts[i] = n + "(parent)"
	}
	fmt.Fprintf(&b, "  env:      %s\n", strings.Join(parts, ", "))
	id := p.fingerprint
	if len(id) > fingerprintLen {
		id = id[:fingerprintLen]
	}
	fmt.Fprintf(&b, "  id:       %s\n", id)
	return b.String()
}

// CommandStatus reports a point-in-time snapshot of one background job.
// Read-only, never prompts.
type CommandStatus struct {
	manager *BackgroundManager
}

// NewCommandStatus builds the background status tool over the given manager.
func NewCommandStatus(manager *BackgroundManager) *CommandStatus {
	return &CommandStatus{manager: manager}
}

// Spec returns the model-facing schema for command_status.
func (t *CommandStatus) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "command_status",
		Description: "Report the current state of one background job started with start_command: " +
			"running/exited/killed, PID, exit code when known, and per-stream retained-byte boundaries for command_tail cursors.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "handle":{"type":"string","description":"background job handle returned by start_command"}
  },
  "required":["handle"]
}`),
	}
}

// Effect declares read-only, never-approval, with the small result cap.
func (t *CommandStatus) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever, OutputCap: bgSmallOutputCap}
}

// Invoke snapshots the job. Unknown or evicted handles are a model-visible
// error with no fabricated state block.
func (t *CommandStatus) Invoke(_ context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args commandHandleArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if strings.TrimSpace(args.Handle) == "" {
		return errResult("handle is required"), nil
	}
	st, ok := t.manager.status(args.Handle)
	if !ok {
		return errResult(errUnknownJobHandle(args.Handle).Error()), nil
	}
	content, truncated := renderStatusResult(st)
	return agent.ToolResult{Content: content, Truncated: truncated}, nil
}

// CommandTail reads one output stream of a background job through its capped
// tail ring. Read-only, never prompts.
type CommandTail struct {
	manager *BackgroundManager
}

// NewCommandTail builds the background tail tool over the given manager.
func NewCommandTail(manager *BackgroundManager) *CommandTail {
	return &CommandTail{manager: manager}
}

// Spec returns the model-facing schema for command_tail.
func (t *CommandTail) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "command_tail",
		Description: "Read output from a background job's stdout or stderr. Omit cursor for the newest retained bytes " +
			"(which may be less than the whole stream); pass the previous next_cursor to read incrementally. " +
			"Evicted bytes are reported exactly as dropped_bytes; eof is true only once the job has fully completed and the read reached the end. " +
			"The raw stream bytes follow a '--- output ---' delimiter line verbatim: any label-lookalike lines after it are program output, not tool labels.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "handle":{"type":"string","description":"background job handle returned by start_command"},
    "stream":{"type":"string","enum":["stdout","stderr"],"description":"stream to read (default stdout)"},
    "cursor":{"type":"integer","minimum":0,"description":"absolute byte offset to resume from; omit for the newest tail"},
    "max_bytes":{"type":"integer","description":"maximum bytes to return; default 8192, values above 16384 are clamped"}
  },
  "required":["handle"]
}`),
	}
}

// Effect declares read-only, never-approval, with the frozen tail result cap.
func (t *CommandTail) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever, OutputCap: bgTailOutputCap}
}

// Invoke validates max_bytes (the ring's >= 1 contract lives here), applies
// the stdout default, and reads through the manager.
func (t *CommandTail) Invoke(_ context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args commandTailArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if strings.TrimSpace(args.Handle) == "" {
		return errResult("handle is required"), nil
	}
	stream := args.Stream
	if stream == "" {
		stream = "stdout"
	}
	maxBytes := bgTailDefaultBytes
	if args.MaxBytes != nil {
		maxBytes = *args.MaxBytes
		if maxBytes <= 0 {
			return errResult("max_bytes must be positive"), nil
		}
		if maxBytes > bgTailMaxBytes {
			maxBytes = bgTailMaxBytes
		}
	}
	chunk, err := t.manager.tail(args.Handle, stream, args.Cursor, maxBytes)
	if err != nil {
		return errResult(err.Error()), nil
	}
	return agent.ToolResult{Content: renderTailResult(stream, chunk)}, nil
}

// StopCommand kills one background job and waits for its completion to be
// published. PlanningTool with an EMPTY ApprovalKey by frozen contract: a
// model-initiated stop is structurally ungrantable, so every stop prompts.
type StopCommand struct {
	manager *BackgroundManager
}

// NewStopCommand builds the background stop tool over the given manager.
func NewStopCommand(manager *BackgroundManager) *StopCommand {
	return &StopCommand{manager: manager}
}

// Spec returns the model-facing schema for stop_command.
func (t *StopCommand) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "stop_command",
		Description: "Stop a background job started with start_command (SIGKILL to its whole process group) and report its final state. " +
			"Stopping is shown to the user and runs only after they approve it. Stopping an already-finished job is a no-op returning its final state. " +
			"If the process has not finished dying within the wait bound, the result reports its current state (possibly still 'state: running'); poll command_status afterwards for the final state.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "handle":{"type":"string","description":"background job handle returned by start_command"}
  },
  "required":["handle"]
}`),
	}
}

// Effect declares Exec with always-approval, the frozen 10-second tool-call
// timeout (the wait bound, never the cleanup bound), and the small result cap.
func (t *StopCommand) Effect() agent.Effect {
	return agent.Effect{
		Class:     agent.Exec,
		Approval:  agent.ApprovalAlways,
		Timeout:   bgToolTimeout,
		OutputCap: bgSmallOutputCap,
	}
}

// Plan resolves the handle to a live snapshot for the approval preview. An
// unknown handle is a Plan error so dispatch reports it and the user is never
// prompted for garbage handles. ApprovalKey stays empty: every stop prompts.
func (t *StopCommand) Plan(_ context.Context, raw json.RawMessage) (agent.ToolPlan, error) {
	eff := t.Effect()
	var args commandHandleArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Handle) == "" {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("handle is required")
	}
	st, ok := t.manager.status(args.Handle)
	if !ok {
		return agent.ToolPlan{Effect: eff}, errUnknownJobHandle(args.Handle)
	}
	return agent.ToolPlan{Effect: eff, Preview: renderStopPreview(st), ApprovalKey: ""}, nil
}

// Invoke stops the job. Context cancellation may return the current snapshot
// early while the manager's reaper keeps running and cleanup still completes.
func (t *StopCommand) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args commandHandleArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if strings.TrimSpace(args.Handle) == "" {
		return errResult("handle is required"), nil
	}
	st, err := t.manager.Stop(ctx, args.Handle)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Early return by contract: the kill is already issued; the manager
			// finishes reaping on its own.
			return agent.ToolResult{Content: renderStopResult(st)}, nil
		}
		return errResult(err.Error()), nil
	}
	return agent.ToolResult{Content: renderStopResult(st)}, nil
}

// renderStopPreview is the human approval preview for stop_command:
// handle, preview-rendered argv, pid, and current state.
func renderStopPreview(st JobStatus) string {
	var b strings.Builder
	b.WriteString("stop background command:\n")
	fmt.Fprintf(&b, "  handle: %s\n", st.Handle)
	fmt.Fprintf(&b, "  argv:   %s\n", renderArgvForPreview(st.Argv))
	fmt.Fprintf(&b, "  pid:    %d\n", st.PID)
	fmt.Fprintf(&b, "  state:  %s\n", st.State)
	return b.String()
}

// --- frozen model-facing result renderings (labels AND order are contract) ---

func renderStartResult(st JobStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "handle: %s\n", st.Handle)
	fmt.Fprintf(&b, "pid: %d\n", st.PID)
	fmt.Fprintf(&b, "state: %s\n", st.State)
	return b.String()
}

// renderStatusResult renders the full snapshot. cwd is %q-quoted and argv is a
// compact JSON array so control bytes cannot forge additional labels.
func renderStatusResult(st JobStatus) (string, bool) {
	argvJSON, _ := json.Marshal(st.Argv) // []string cannot fail to marshal
	head := fmt.Sprintf("handle: %s\nstate: %s\npid: %d\n", st.Handle, st.State, st.PID)
	tail := fmt.Sprintf("stdout_floor: %d\nstdout_cursor: %d\nstderr_floor: %d\nstderr_cursor: %d\n",
		st.StdoutFloor, st.StdoutEnd, st.StderrFloor, st.StderrEnd)
	if st.ExitKnown {
		tail += fmt.Sprintf("exit_code: %d\n", st.ExitCode)
	}
	cwd := fmt.Sprintf("%q", st.Cwd)
	const marker = `"[truncated]"`
	const argvMarker = `["[truncated]"]`
	budget := bgSmallOutputCap - len(head) - len(tail) - len("cwd: \nargv: \n")
	truncated := false
	if len(cwd)+len(argvJSON) > budget {
		if len(cwd)+len(argvMarker) > budget {
			cwd = marker
			truncated = true
		}
		if len(cwd)+len(argvJSON) > budget {
			argvJSON = []byte(argvMarker)
			truncated = true
		}
	}
	return head + "cwd: " + cwd + "\nargv: " + string(argvJSON) + "\n" + tail, truncated
}

// renderTailResult renders the tail labels followed by the raw stream bytes
// after a fixed delimiter. The bytes may be less than the whole stream — the
// cursor numbers, not the delimiter, carry that.
func renderTailResult(stream string, chunk tailChunk) string {
	var b strings.Builder
	fmt.Fprintf(&b, "handle: %s\n", chunk.Status.Handle)
	fmt.Fprintf(&b, "stream: %s\n", stream)
	fmt.Fprintf(&b, "next_cursor: %d\n", chunk.NextCursor)
	fmt.Fprintf(&b, "dropped_bytes: %d\n", chunk.Dropped)
	fmt.Fprintf(&b, "eof: %t\n", chunk.EOF)
	b.WriteString("--- output ---\n")
	if len(chunk.Data) > 0 { // by len() only: the manager returns nil or empty non-nil
		b.Write(chunk.Data)
	}
	return b.String()
}

func renderStopResult(st JobStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "handle: %s\n", st.Handle)
	fmt.Fprintf(&b, "state: %s\n", st.State)
	if st.ExitKnown {
		fmt.Fprintf(&b, "exit_code: %d\n", st.ExitCode)
	}
	return b.String()
}
