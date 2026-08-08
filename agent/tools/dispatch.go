package tools

import (
	"context"
	"encoding/json"
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
	// DefaultDispatchCallsPerRun bounds aggregate fan-out when installed in a parent Budget.
	DefaultDispatchCallsPerRun = 4

	maxDispatchTasks             = 4
	maxDispatchTaskBytes         = 8 * 1024
	defaultDispatchMaxSteps      = 6
	defaultDispatchTotalTokens   = 32 * 1024
	defaultDispatchOutputReserve = 1024
	defaultDispatchSummary       = 8 * 1024
	defaultDispatchResult        = 64 * 1024
	defaultDispatchTimeout       = 5 * time.Minute
)

const dispatchSystemPrompt = "You are a read-only exploration subagent. Investigate only the assigned task using the available read and retrieval tools. Do not write or edit files, run commands, call external tools, submit plans, or dispatch children. Return a concise evidence-backed summary and do not claim actions you did not perform."

// DispatchLimits bounds every child and one dispatch invocation. Zero fields
// select conservative defaults; negative fields are rejected.
type DispatchLimits struct {
	MaxSteps        int
	Budget          agent.Budget
	MaxTasks        int
	MaxConcurrent   int
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
	for i, name := range order {
		tool, ok := found[name]
		if !ok {
			if i < 4 {
				return nil, fmt.Errorf("tools: dispatch: required child tool %q is missing", name)
			}
			continue
		}
		tools = append(tools, tool)
	}
	return &Dispatch{caller: caller, ctxMgr: ctxMgr, tools: tools, limits: limits}, nil
}

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

func (d *Dispatch) Effect() agent.Effect {
	return agent.Effect{
		Class: agent.Read | agent.Network, Approval: agent.ApprovalNever,
		Timeout: d.limits.Timeout, OutputCap: d.limits.MaxResultBytes,
	}
}

func (d *Dispatch) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, err
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
	sem := make(chan struct{}, d.limits.MaxConcurrent)
	var wg sync.WaitGroup
	for i, task := range args.Tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if ctx.Err() == nil {
				envelope.Results[i] = d.runChild(ctx, task)
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, err
	}
	failed := false
	truncated := false
	for i := range envelope.Results {
		result := &envelope.Results[i]
		var cut bool
		result.Summary, cut = truncateUTF8(result.Summary, d.limits.MaxSummaryBytes)
		if cut {
			result.Truncated = true
			truncated = true
		}
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

func (d *Dispatch) runChild(ctx context.Context, task string) dispatchResult {
	caller := &modelRecordingCaller{next: d.caller}
	result, err := agent.New(caller, d.ctxMgr).Run(ctx, agent.Request{
		Goal: task, System: dispatchSystemPrompt, Tools: d.tools,
		MaxSteps: d.limits.MaxSteps, Budget: d.limits.Budget,
	}, nil)
	out := dispatchResult{Summary: result.Answer, StopReason: result.StopReason.String(), Model: caller.model()}
	if err != nil {
		out.StopReason = "error"
		out.Error = err.Error()
		if out.Model == "" {
			out.Error += "; child model identity unavailable"
		}
		return out
	}
	if out.Model == "" {
		out.Error = "child model identity unavailable"
	}
	return out
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

func (c *modelRecordingCaller) model() string {
	if c.actual == (provider.ModelKey{}) {
		return ""
	}
	return c.actual.String()
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

		var longest *string
		var owner *dispatchResult
		for i := range envelope.Results {
			result := &envelope.Results[i]
			for _, field := range []*string{&result.Summary, &result.Error, &result.Model} {
				if longest == nil || len(*field) > len(*longest) {
					longest, owner = field, result
				}
			}
		}
		if longest == nil || *longest == "" {
			return nil, truncated, fmt.Errorf("tools: dispatch: result metadata exceeds %d-byte cap", limit)
		}
		keep := len(*longest) - (len(content) - limit)
		if keep >= len(*longest) {
			keep = len(*longest) - 1
		}
		if keep < 0 {
			keep = 0
		}
		*longest, _ = truncateUTF8(*longest, keep)
		owner.Truncated = true
		truncated = true
	}
}

func truncateUTF8(value string, limit int) (string, bool) {
	valid := strings.ToValidUTF8(value, "�")
	if len(valid) <= limit {
		return valid, valid != value
	}
	end := limit
	for end > 0 && !utf8.RuneStart(valid[end]) {
		end--
	}
	return valid[:end], true
}

func normalizeDispatchLimits(limits DispatchLimits) (DispatchLimits, error) {
	if limits.MaxSteps < 0 || limits.MaxTasks < 0 || limits.MaxConcurrent < 0 || limits.MaxSummaryBytes < 0 || limits.MaxResultBytes < 0 || limits.Timeout < 0 ||
		limits.Budget.InputCeiling < 0 || limits.Budget.OutputReserve < 0 || limits.Budget.TotalTokens < 0 {
		return DispatchLimits{}, fmt.Errorf("tools: dispatch: limits must not be negative")
	}
	if limits.MaxSteps == 0 {
		limits.MaxSteps = defaultDispatchMaxSteps
	}
	if limits.MaxTasks == 0 {
		limits.MaxTasks = maxDispatchTasks
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
	if limits.MaxTasks > maxDispatchTasks {
		return DispatchLimits{}, fmt.Errorf("tools: dispatch: max tasks %d exceeds hard cap %d", limits.MaxTasks, maxDispatchTasks)
	}
	if limits.MaxConcurrent > limits.MaxTasks {
		return DispatchLimits{}, fmt.Errorf("tools: dispatch: max concurrent children %d exceeds max tasks %d", limits.MaxConcurrent, limits.MaxTasks)
	}
	if limits.Budget.OutputReserve >= limits.Budget.InputCeiling {
		return DispatchLimits{}, fmt.Errorf("tools: dispatch: output reserve %d must be smaller than input ceiling %d", limits.Budget.OutputReserve, limits.Budget.InputCeiling)
	}
	return limits, nil
}
