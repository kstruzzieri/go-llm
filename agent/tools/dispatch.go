package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

const (
	// DispatchToolName is the model-facing name of the read-only child dispatcher.
	DispatchToolName = "dispatch"
	// DefaultDispatchCallsPerRun bounds aggregate fan-out when installed with agent.WithToolInvocationLimit.
	DefaultDispatchCallsPerRun = 4
	// MaxDispatchTasks is the hard cap on tasks in one dispatch invocation.
	MaxDispatchTasks = 4

	maxDispatchTaskBytes         = 8 * 1024
	maxDispatchArgsBytes         = MaxDispatchTasks*maxDispatchTaskBytes*6 + 1024
	defaultDispatchMaxSteps      = 6
	defaultDispatchTotalTokens   = 32 * 1024
	defaultDispatchOutputReserve = 1024
	defaultDispatchSummary       = 8 * 1024
	defaultDispatchResult        = 64 * 1024
	defaultDispatchTimeout       = 5 * time.Minute
	dispatchSummaryTruncated     = "… [summary truncated]"
	dispatchErrorTruncated       = "… [error details truncated]"
)

const dispatchSystemPrompt = "You are a read-only exploration subagent. Investigate only the assigned task using the available read and retrieval tools. Do not write or edit files, run commands, call external tools, submit plans, or dispatch children. Return a concise evidence-backed summary and do not claim actions you did not perform."

// DispatchLimits bounds every child and one dispatch invocation. Zero fields
// select conservative defaults; negative fields are rejected.
type DispatchLimits struct {
	MaxSteps int
	Budget   agent.Budget
	MaxTasks int
	// MaxConcurrent values above one require a ModelCaller, child tools, and a
	// ContextManager (including any custom Compactor or Estimate) that support
	// concurrent use.
	MaxConcurrent int
	// Concurrency, when non-nil, is read once at the start of every valid
	// Invoke. Its result is clamped to [1, MaxConcurrent]. It must be safe
	// for concurrent use and return promptly.
	Concurrency func() int
	// OnChildComplete, when non-nil, is called synchronously after a started
	// child finishes and after its dispatch permits are released. Index is the
	// zero-based input task index and total is the invocation's task count.
	// Calls may overlap and arrive in any order. The callback must be safe for
	// concurrent use, return promptly, and not panic. It is not called for a
	// task that never starts.
	OnChildComplete func(index, total int)
	MaxSummaryBytes int
	MaxResultBytes  int
	Timeout         time.Duration
}

// Dispatch runs bounded read-only child orchestrators.
type Dispatch struct {
	caller agent.ModelCaller
	ctxMgr agent.ContextManager
	tools  []agent.Tool
	limits DispatchLimits
	slots  chan struct{}
}

type dispatchArgs struct {
	Tasks []string `json:"tasks"`
}

type dispatchResult struct {
	Summary    string `json:"summary"`
	StopReason string `json:"stop_reason"`
	Model      string `json:"model"`
	Error      string `json:"error,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type dispatchEnvelope struct {
	Results []dispatchResult `json:"results"`
}

// NewDispatch builds a dispatcher from the parent's existing tools. Only the
// four built-in file readers and an optional retrieve tool cross the boundary;
// every other registered capability is omitted.
func NewDispatch(caller agent.ModelCaller, ctxMgr agent.ContextManager, available []agent.Tool, limits DispatchLimits) (*Dispatch, error) {
	if caller == nil {
		return nil, fmt.Errorf("tools: dispatch: model caller is required")
	}
	var err error
	limits, err = normalizeDispatchLimits(limits)
	if err != nil {
		return nil, err
	}

	order := []string{"read_file", "search", "glob", "list", "retrieve"}
	allowed := make(map[string]bool, len(order))
	for _, name := range order {
		allowed[name] = true
	}
	found := make(map[string]agent.Tool, len(order))
	for i, tool := range available {
		if tool == nil {
			return nil, fmt.Errorf("tools: dispatch: nil available tool at index %d", i)
		}
		name := tool.Spec().Name
		if !allowed[name] {
			continue
		}
		if _, duplicate := found[name]; duplicate {
			return nil, fmt.Errorf("tools: dispatch: duplicate child tool %q", name)
		}
		effect := tool.Effect()
		if effect.Class != agent.Read || effect.Approval != agent.ApprovalNever {
			return nil, fmt.Errorf("tools: dispatch: child tool %q must be read-only and never require approval", name)
		}
		if _, planning := tool.(agent.PlanningTool); planning {
			return nil, fmt.Errorf("tools: dispatch: child tool %q must not plan mutations", name)
		}
		found[name] = tool
	}
	tools := make([]agent.Tool, 0, len(order))
	for _, name := range order {
		tool, ok := found[name]
		if !ok {
			if name != "retrieve" { // retrieve is the only optional child tool
				return nil, fmt.Errorf("tools: dispatch: required child tool %q is missing", name)
			}
			continue
		}
		tools = append(tools, tool)
	}
	return &Dispatch{caller: caller, ctxMgr: ctxMgr, tools: tools, limits: limits, slots: make(chan struct{}, limits.MaxConcurrent)}, nil
}

// invokeConcurrency resolves one invocation's fan-out: the static
// MaxConcurrent, lowered by the Concurrency governor when one is set. The
// clamp to [1, MaxConcurrent] means a misbehaving governor can only waste
// parallelism, never exceed the validated ceiling.
func (d *Dispatch) invokeConcurrency() int {
	n := d.limits.MaxConcurrent
	if d.limits.Concurrency != nil {
		if governed := d.limits.Concurrency(); governed < n {
			n = governed
		}
	}
	if n < 1 {
		return 1
	}
	return n
}

// Spec returns the model-facing dispatch contract.
func (d *Dispatch) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        DispatchToolName,
		Description: "Run one or more bounded, read-only exploration tasks in child agents and return their summaries, stop reasons, and actual models.",
		Parameters: json.RawMessage(fmt.Sprintf(`{
  "type":"object",
  "properties":{
    "tasks":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":%d,"description":"independent read-only investigation tasks"}
  },
  "required":["tasks"]
}`, d.limits.MaxTasks)),
	}
}

// Effect declares bounded read/network access without approval.
func (d *Dispatch) Effect() agent.Effect {
	return agent.Effect{
		Class: agent.Read | agent.Network, Approval: agent.ApprovalNever,
		Timeout: d.limits.Timeout, OutputCap: d.limits.MaxResultBytes,
	}
}

// Invoke runs the requested child tasks and returns an ordered JSON envelope.
func (d *Dispatch) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, err
	}
	if len(raw) > maxDispatchArgsBytes {
		return agent.ToolResult{IsError: true, Content: fmt.Sprintf("dispatch arguments must be at most %d bytes", maxDispatchArgsBytes)}, nil
	}
	var args dispatchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolResult{IsError: true, Content: "invalid arguments: " + err.Error()}, nil
	}
	if len(args.Tasks) == 0 {
		return agent.ToolResult{IsError: true, Content: "dispatch requires at least one task"}, nil
	}
	if len(args.Tasks) > d.limits.MaxTasks {
		return agent.ToolResult{IsError: true, Content: fmt.Sprintf("dispatch accepts at most %d tasks", d.limits.MaxTasks)}, nil
	}
	for i, task := range args.Tasks {
		if strings.TrimSpace(task) == "" {
			return agent.ToolResult{IsError: true, Content: fmt.Sprintf("dispatch task %d is empty", i+1)}, nil
		}
		if len(task) > maxDispatchTaskBytes {
			return agent.ToolResult{IsError: true, Content: fmt.Sprintf("dispatch task %d must be at most %d bytes", i+1, maxDispatchTaskBytes)}, nil
		}
	}

	envelope := dispatchEnvelope{Results: make([]dispatchResult, len(args.Tasks))}
	childErrors := make([]error, len(args.Tasks))
	started := make([]bool, len(args.Tasks))
	// The invocation-local semaphore applies this invocation's governed size
	// without weakening the instance-wide d.slots bound shared by overlapping
	// invocations. Children acquire local then shared, always in that order.
	invokeSlots := make(chan struct{}, d.invokeConcurrency())
	var wg sync.WaitGroup
	for i, task := range args.Tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The nested function scopes both permit releases so they run
			// before OnChildComplete: the callback must never hold a slot.
			startedChild := func() bool {
				select {
				case invokeSlots <- struct{}{}:
				case <-ctx.Done():
					return false
				}
				defer func() { <-invokeSlots }()
				select {
				case d.slots <- struct{}{}:
				case <-ctx.Done():
					return false
				}
				defer func() { <-d.slots }()
				if ctx.Err() != nil {
					return false
				}
				started[i] = true
				envelope.Results[i], childErrors[i] = d.runChild(ctx, task)
				return true
			}()
			if startedChild && d.limits.OnChildComplete != nil {
				d.limits.OnChildComplete(i, len(args.Tasks))
			}
		}()
	}
	wg.Wait()
	// Cancellation is a hard abort: the parent is gone and nothing consumes the
	// envelope. A deadline is this tool's own per-invocation timeout, so already
	// computed child evidence is returned instead of being discarded; a deadline
	// on the PARENT context still hard-aborts one frame up, at the orchestrator's
	// parent-ctx check after Invoke.
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return agent.ToolResult{}, err
	}
	for i, err := range childErrors {
		if err != nil {
			return agent.ToolResult{}, fmt.Errorf("tools: dispatch: child %d: %w", i+1, err)
		}
	}
	for i := range envelope.Results {
		if !started[i] {
			envelope.Results[i] = dispatchResult{
				StopReason: "error",
				Error:      "child not started before dispatch timeout; child model identity unavailable",
			}
		}
	}
	failed := false
	truncated := false
	for i := range envelope.Results {
		result := &envelope.Results[i]
		truncated = truncated || result.Truncated
		if result.Error != "" {
			failed = true
		}
	}
	content, cut, err := marshalDispatchEnvelope(&envelope, d.limits.MaxResultBytes)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: string(content), IsError: failed, Truncated: truncated || cut}, nil
}

func (d *Dispatch) runChild(ctx context.Context, task string) (dispatchResult, error) {
	caller := &modelRecordingCaller{next: d.caller}
	result, err := agent.New(caller, d.ctxMgr).Run(ctx, agent.Request{
		Goal: task, System: dispatchSystemPrompt, Tools: d.tools,
		MaxSteps: d.limits.MaxSteps, Budget: d.limits.Budget,
	}, nil)
	model, modelErr := caller.model(d.limits.MaxResultBytes)
	if modelErr != nil {
		return dispatchResult{}, modelErr
	}
	out := dispatchResult{Summary: result.Answer, StopReason: result.StopReason.String(), Model: model}
	if err != nil {
		out.StopReason = "error"
	}
	if strings.TrimSpace(out.Summary) == "" {
		out.Summary = ""
		// ponytail: the last evidence avoids an extra model call; add bounded
		// finalization only if partial summaries prove insufficient.
		for i := len(result.Messages) - 1; i >= 0; i-- {
			message := result.Messages[i]
			if message.Role == "user" || strings.TrimSpace(message.Content) == "" {
				continue
			}
			label := fmt.Sprintf("Partial result before %s: ", out.StopReason)
			if len(label)+len(dispatchSummaryTruncated) > d.limits.MaxSummaryBytes {
				out.Summary = dispatchSummaryTruncated
				out.Truncated = true
			} else {
				evidence, cut := truncateUTF8WithMarker(strings.TrimSpace(message.Content), d.limits.MaxSummaryBytes-len(label), dispatchSummaryTruncated)
				out.Summary = label + evidence
				out.Truncated = out.Truncated || cut
			}
			break
		}
		if out.Summary == "" && err == nil {
			out.Error = fmt.Sprintf("child produced no summary before %s", out.StopReason)
		}
	}
	if err != nil {
		out.Error = err.Error()
		if strings.TrimSpace(out.Error) == "" {
			out.Error = "child failed without an error message"
		}
		if out.Model == "" {
			const suffix = "; child model identity unavailable"
			var cut bool
			out.Error, cut = truncateUTF8WithMarker(out.Error, d.limits.MaxResultBytes-len(suffix), dispatchErrorTruncated)
			out.Error += suffix
			out.Truncated = out.Truncated || cut
		}
	} else if out.Model == "" {
		if out.Error != "" {
			out.Error += "; child model identity unavailable"
		} else {
			out.Error = "child model identity unavailable"
		}
	}
	var cut bool
	out.Summary, cut = truncateUTF8WithMarker(out.Summary, d.limits.MaxSummaryBytes, dispatchSummaryTruncated)
	out.Truncated = out.Truncated || cut
	out.Error, cut = truncateUTF8WithMarker(out.Error, d.limits.MaxResultBytes, dispatchErrorTruncated)
	out.Truncated = out.Truncated || cut
	out.Model, cut = truncateUTF8(out.Model, d.limits.MaxResultBytes)
	out.Truncated = out.Truncated || cut
	return out, nil
}

type modelRecordingCaller struct {
	next   agent.ModelCaller
	actual provider.ModelKey
}

func (c *modelRecordingCaller) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	result, err := c.next.Chat(ctx, req, onToken)
	if result.RouteOutcome != nil && result.RouteOutcome.ActualModel != (provider.ModelKey{}) {
		c.actual = result.RouteOutcome.ActualModel
	}
	return result, err
}

func (c *modelRecordingCaller) model(limit int) (string, error) {
	if c.actual == (provider.ModelKey{}) {
		return "", nil
	}
	if len(c.actual.Provider) >= limit || len(c.actual.Model) > limit-len(c.actual.Provider)-1 {
		return "", fmt.Errorf("model identity exceeds %d-byte cap", limit)
	}
	return c.actual.String(), nil
}

func marshalDispatchEnvelope(envelope *dispatchEnvelope, limit int) ([]byte, bool, error) {
	truncated := false
	for {
		content, err := json.Marshal(envelope)
		if err != nil {
			return nil, truncated, fmt.Errorf("tools: dispatch: encode result: %w", err)
		}
		if len(content) <= limit {
			return content, truncated, nil
		}

		// Shrink the globally longest shrinkable field, summary or error alike,
		// so one bloated error cannot starve every healthy summary of space.
		var longest *string
		var owner *dispatchResult
		isError := false
		for i := range envelope.Results {
			result := &envelope.Results[i]
			if len(result.Summary) > len(dispatchSummaryTruncated) && (longest == nil || len(result.Summary) > len(*longest)) {
				longest, owner, isError = &result.Summary, result, false
			}
			if len(result.Error) > len(dispatchErrorTruncated) && (longest == nil || len(result.Error) > len(*longest)) {
				longest, owner, isError = &result.Error, result, true
			}
		}
		if longest == nil {
			return nil, truncated, fmt.Errorf("tools: dispatch: result metadata exceeds %d-byte cap", limit)
		}
		keep := len(*longest) - (len(content) - limit)
		if keep >= len(*longest) {
			keep = len(*longest) - 1
		}
		if keep < 0 {
			keep = 0
		}
		if isError {
			*longest, _ = truncateUTF8WithMarker(*longest, keep, dispatchErrorTruncated)
		} else {
			*longest, _ = truncateUTF8WithMarker(*longest, keep, dispatchSummaryTruncated)
		}
		owner.Truncated = true
		truncated = true
	}
}

func truncateUTF8(value string, limit int) (string, bool) {
	truncated := len(value) > limit
	if truncated {
		value = value[:limit]
	}
	valid := strings.ToValidUTF8(value, "�")
	changed := truncated || valid != value
	if len(valid) > limit {
		end := limit
		for end > 0 && !utf8.RuneStart(valid[end]) {
			end--
		}
		valid = valid[:end]
		changed = true
	}
	return strings.Clone(valid), changed
}

func truncateUTF8WithMarker(value string, limit int, marker string) (string, bool) {
	bounded, changed := truncateUTF8(value, limit)
	if !changed {
		return bounded, false
	}
	prefixLimit := limit - len(marker)
	if prefixLimit <= 0 {
		return strings.Clone(marker), true
	}
	prefix, _ := truncateUTF8(value, prefixLimit)
	return prefix + marker, true
}

func normalizeDispatchLimits(limits DispatchLimits) (DispatchLimits, error) {
	for _, field := range []struct {
		name  string
		value int64
	}{
		{"max steps", int64(limits.MaxSteps)},
		{"input ceiling", int64(limits.Budget.InputCeiling)},
		{"output reserve", int64(limits.Budget.OutputReserve)},
		{"total tokens", int64(limits.Budget.TotalTokens)},
		{"max tasks", int64(limits.MaxTasks)},
		{"max concurrent", int64(limits.MaxConcurrent)},
		{"max summary bytes", int64(limits.MaxSummaryBytes)},
		{"max result bytes", int64(limits.MaxResultBytes)},
		{"timeout", int64(limits.Timeout)},
	} {
		if field.value < 0 {
			return DispatchLimits{}, fmt.Errorf("tools: dispatch: %s must not be negative, got %d", field.name, field.value)
		}
	}
	if limits.MaxSteps == 0 {
		limits.MaxSteps = defaultDispatchMaxSteps
	}
	if limits.MaxTasks == 0 {
		limits.MaxTasks = MaxDispatchTasks
	}
	if limits.MaxConcurrent == 0 {
		limits.MaxConcurrent = 1
	}
	if limits.MaxSummaryBytes == 0 {
		limits.MaxSummaryBytes = defaultDispatchSummary
	}
	if limits.MaxResultBytes == 0 {
		limits.MaxResultBytes = defaultDispatchResult
	}
	if limits.Timeout == 0 {
		limits.Timeout = defaultDispatchTimeout
	}
	if limits.Budget.InputCeiling == 0 {
		limits.Budget.InputCeiling = agent.DefaultInputCeiling
	}
	if limits.Budget.OutputReserve == 0 {
		limits.Budget.OutputReserve = defaultDispatchOutputReserve
	}
	if limits.Budget.TotalTokens == 0 {
		limits.Budget.TotalTokens = defaultDispatchTotalTokens
	}
	if limits.MaxTasks > MaxDispatchTasks {
		return DispatchLimits{}, fmt.Errorf("tools: dispatch: max tasks %d exceeds hard cap %d", limits.MaxTasks, MaxDispatchTasks)
	}
	if limits.MaxConcurrent > limits.MaxTasks {
		return DispatchLimits{}, fmt.Errorf("tools: dispatch: max concurrent children %d exceeds max tasks %d", limits.MaxConcurrent, limits.MaxTasks)
	}
	if limits.Budget.OutputReserve >= limits.Budget.InputCeiling {
		return DispatchLimits{}, fmt.Errorf("tools: dispatch: output reserve %d must be smaller than input ceiling %d", limits.Budget.OutputReserve, limits.Budget.InputCeiling)
	}
	if limits.MaxSummaryBytes < len(dispatchSummaryTruncated) {
		return DispatchLimits{}, fmt.Errorf("tools: dispatch: max summary bytes %d cannot hold truncation marker (%d bytes)", limits.MaxSummaryBytes, len(dispatchSummaryTruncated))
	}
	minimum := dispatchEnvelope{Results: make([]dispatchResult, limits.MaxTasks)}
	for i := range minimum.Results {
		minimum.Results[i] = dispatchResult{
			Summary: dispatchSummaryTruncated, StopReason: agent.ToolErrorCapReached.String(),
			Model: "?", Error: dispatchErrorTruncated, Truncated: true,
		}
	}
	encoded, err := json.Marshal(minimum)
	if err != nil {
		return DispatchLimits{}, fmt.Errorf("tools: dispatch: encode minimum result: %w", err)
	}
	if limits.MaxResultBytes < len(encoded) {
		return DispatchLimits{}, fmt.Errorf("tools: dispatch: max result bytes %d cannot hold required metadata (%d bytes)", limits.MaxResultBytes, len(encoded))
	}
	return limits, nil
}
