package agent

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/provider"
)

// Hook names the pipeline stage a finding or block came from (#436).
type Hook uint8

const (
	// HookInput inspects content entering State: the initial input at step 0,
	// every tool observation at ingress, and verifier output.
	HookInput Hook = iota + 1
	// HookOutput inspects the collected model response (content, thinking,
	// tool calls) before it is recorded, on the success and the
	// provider-error path.
	HookOutput
	// HookToolCall inspects one tool call before Plan and approval.
	HookToolCall
)

func (h Hook) String() string {
	switch h {
	case HookInput:
		return "input"
	case HookOutput:
		return "output"
	case HookToolCall:
		return "tool_call"
	default:
		return "unknown"
	}
}

// Verdict is an interceptor's disposition for one finding. The pipeline takes
// the maximum across a hook's findings.
type Verdict uint8

const (
	// VerdictAllow is the zero value: nothing to report.
	VerdictAllow Verdict = iota
	// VerdictTag annotates context and telemetry without stopping anything.
	VerdictTag
	// VerdictBlock prevents the guarded action. Recoverable where a synthetic
	// observation makes sense (tool call, tool result); terminal for the
	// initial input and model output.
	VerdictBlock
	// VerdictAbort halts the run from any hook (#438 canary compromise).
	VerdictAbort
)

func (v Verdict) String() string {
	switch v {
	case VerdictAllow:
		return "allow"
	case VerdictTag:
		return "tag"
	case VerdictBlock:
		return "block"
	case VerdictAbort:
		return "abort"
	default:
		return "unknown"
	}
}

// Origin classifies the provenance of inspected content (spec D4). The loop
// assigns it; interceptors read it. Unknown is the zero value, the default
// for tools that declare nothing, and what every invalid value normalizes to;
// detectors treat it like foreign.
type Origin uint8

const (
	OriginUnknown Origin = iota
	OriginUser
	OriginSystem
	OriginModel
	OriginWorkspace
	OriginForeign
)

func (o Origin) String() string {
	switch o {
	case OriginUnknown:
		return "unknown"
	case OriginUser:
		return "user"
	case OriginSystem:
		return "system"
	case OriginModel:
		return "model"
	case OriginWorkspace:
		return "workspace"
	case OriginForeign:
		return "foreign"
	default:
		return "unknown"
	}
}

// normalizeOrigin maps any value outside the known enum to OriginUnknown so
// an invalid provenance can never fail open into tag-level handling.
func normalizeOrigin(o Origin) Origin {
	if o > OriginForeign {
		return OriginUnknown
	}
	return o
}

// TargetKind says what a finding is about (spec D2). The pipeline validates
// it per hook; an invalid target becomes TargetNone.
type TargetKind uint8

const (
	TargetNone TargetKind = iota
	TargetSystem
	TargetSummary
	TargetMessage
	TargetOutputContent
	TargetOutputToolCall
	TargetToolCall
)

func (k TargetKind) String() string {
	switch k {
	case TargetNone:
		return "none"
	case TargetSystem:
		return "system"
	case TargetSummary:
		return "summary"
	case TargetMessage:
		return "message"
	case TargetOutputContent:
		return "output_content"
	case TargetOutputToolCall:
		return "output_tool_call"
	case TargetToolCall:
		return "tool_call"
	default:
		return "unknown"
	}
}

// OriginTool is the static provenance declaration of a Tool. ToolResult.Origin
// overrides it per invocation. A tool that declares neither is OriginUnknown.
type OriginTool interface {
	Origin() Origin
}

// InspectedAlternative is one ContextSet alternative of an observation.
type InspectedAlternative struct {
	Group, Alternative int
	Content            string
}

// InspectedMessage is one State message as presented to InspectInput.
// StateIndex is the message's index in State.Messages (for a tool result at
// ingress, the index it is about to occupy).
type InspectedMessage struct {
	StateIndex   int
	Role         string // "user" | "assistant" | "tool"
	Origin       Origin
	ToolName     string
	ToolCallID   string
	Content      string
	Alternatives []InspectedAlternative
}

// InputInspection is either the initial state at step 0 (System, Summary,
// history and goal) or exactly one tool observation at ingress.
type InputInspection struct {
	Step     int
	System   string // step 0 only; TargetSystem, OriginSystem
	Summary  string // step 0 only; TargetSummary, OriginModel
	Messages []InspectedMessage
}

// OutputInspection carries the collected model response for one step. On the
// provider-error path it carries the partial content and thinking with no
// tool calls (fragments are dropped, as before #436).
type OutputInspection struct {
	Step      int
	Content   string
	Thinking  string
	ToolCalls []provider.ToolCall
}

// ToolCallInspection carries one tool call before Plan and approval. Call is
// a clone; Effect is the tool's static, normalized effect.
type ToolCallInspection struct {
	Step   int
	Call   provider.ToolCall
	Effect Effect
}

// Finding is one structured observation. The pipeline normalizes every
// finding: Interceptor, Hook and Step are stamped; a blank Rule becomes
// "unspecified"; an unknown Verdict clamps to Abort; Risk is clamped to
// [0, 100]; Detail is flattened to one line, capped at 256 bytes, and is
// telemetry only; Target is validated for the hook (see TargetKind) and an
// invalid target becomes TargetNone with StateIndex/Group/Alternative -1 and
// an empty ToolCallID; Origin comes from the target when there is one and is
// otherwise the interceptor's value normalized.
type Finding struct {
	Interceptor string
	Rule        string
	Verdict     Verdict
	Risk        int
	Detail      string
	Origin      Origin
	Hook        Hook
	Step        int
	Target      TargetKind
	StateIndex  int
	ToolCallID  string
	Group       int
	Alternative int
}

// RiskReport is the cumulative per-run score (saturating sum of Finding.Risk)
// and every finding that contributed, in pipeline order.
type RiskReport struct {
	Score    int
	Findings []Finding
}

// Interceptor is the deterministic middleware seam (#436). Hooks run on the
// run's goroutine, in registration order, and must not block or spawn
// goroutines. Every value an interceptor receives is a private copy; nothing
// it does to the copy changes what the loop uses. One instance may serve
// several concurrent runs (dispatch children, parallel consumers): keep no
// per-run mutable state, or implement RunScopedInterceptor. A returned error
// hard-aborts the run after the remaining interceptors have run.
type Interceptor interface {
	Name() string
	InspectInput(ctx context.Context, in InputInspection) ([]Finding, error)
	InspectOutput(ctx context.Context, out OutputInspection) ([]Finding, error)
	InspectToolCall(ctx context.Context, call ToolCallInspection) ([]Finding, error)
}

// RunScope is the immutable projection of a run that ForRun sees.
type RunScope struct {
	System string
}

// RunScopedInterceptor is optionally implemented by an installed interceptor
// that needs per-run state (spec D11; #438's per-turn nonce). ForRun returns
// the instance used for this run (non-nil, same Name) and a system-prompt
// addendum the pipeline appends to the run's System ("" appends nothing).
type RunScopedInterceptor interface {
	ForRun(ctx context.Context, scope RunScope) (Interceptor, string, error)
}

// WithInterceptors installs interceptors in order; multiple options append.
// Run fails fast on a nil entry, an empty name, or a duplicate name. A typed
// nil pointer satisfies the interface and panics on first use; install only
// real values, as WithVerifier's callers do.
func WithInterceptors(ic ...Interceptor) Option {
	return func(o *Orchestrator) { o.interceptors = append(o.interceptors, ic...) }
}

func validateInterceptors(ic []Interceptor) error {
	seen := make(map[string]struct{}, len(ic))
	for i, it := range ic {
		if it == nil {
			return fmt.Errorf("agent: nil interceptor at index %d", i)
		}
		name := it.Name()
		if name == "" {
			return fmt.Errorf("agent: interceptor at index %d has an empty name", i)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("agent: duplicate interceptor name %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// resolveInterceptors validates the installed chain and replaces every
// RunScopedInterceptor with its per-run instance, returning the concatenated
// system addenda in chain order.
func resolveInterceptors(ctx context.Context, chain []Interceptor, scope RunScope) ([]Interceptor, string, error) {
	if err := validateInterceptors(chain); err != nil {
		return nil, "", err
	}
	if len(chain) == 0 {
		return nil, "", nil
	}
	out := make([]Interceptor, len(chain))
	addenda := ""
	for i, ic := range chain {
		out[i] = ic
		rs, ok := ic.(RunScopedInterceptor)
		if !ok {
			continue
		}
		scoped, addendum, err := rs.ForRun(ctx, scope)
		if err != nil {
			return nil, "", fmt.Errorf("agent: interceptor %s for run: %w", ic.Name(), err)
		}
		if scoped == nil {
			return nil, "", fmt.Errorf("agent: interceptor %s returned a nil run instance", ic.Name())
		}
		if scoped.Name() != ic.Name() {
			return nil, "", fmt.Errorf("agent: interceptor %s returned a run instance named %q", ic.Name(), scoped.Name())
		}
		out[i] = scoped
		addenda += addendum
	}
	return out, addenda, nil
}

// BlockedError is returned by Run, with the partial Result, when a hook
// blocks terminally or aborts. Findings holds the blocking hook invocation's
// findings; the cause is the first finding at the maximum verdict. When
// interceptors also errored, Run returns errors.Join(blocked, errs...), so
// errors.As still finds the BlockedError.
type BlockedError struct {
	Hook     Hook
	Step     int
	Findings []Finding
}

// cause returns the first finding whose verdict equals the maximum verdict
// among findings; a zero Finding when there are none.
func cause(findings []Finding) Finding {
	max := VerdictAllow
	for _, f := range findings {
		if f.Verdict > max {
			max = f.Verdict
		}
	}
	for _, f := range findings {
		if f.Verdict == max {
			return f
		}
	}
	return Finding{}
}

func (e *BlockedError) Error() string {
	c := cause(e.Findings)
	if c.Interceptor == "" {
		return fmt.Sprintf("agent: %s blocked by interceptor", e.Hook)
	}
	return fmt.Sprintf("agent: %s blocked by interceptor %s (%s)", e.Hook, c.Interceptor, c.Rule)
}

// RiskApprover is optionally implemented by an Approver that wants the run's
// cumulative RiskReport with each approval (spec D2). Dispatch prefers it
// over KeyedApprover and Approver. The report is a snapshot.
type RiskApprover interface {
	ApproveWithRisk(ctx context.Context, call provider.ToolCall, preview, approvalKey string, risk RiskReport) (ApprovalDecision, error)
}

// InterceptionEvent reports one hook invocation that produced findings, BEFORE
// any block takes effect. Risk is the cumulative snapshot after this hook.
type InterceptionEvent struct {
	Step       int
	Hook       Hook
	Verdict    Verdict
	Findings   []Finding
	Risk       RiskReport
	ToolCallID string
}

// InterceptionObserver is an OPTIONAL extension of Observer. A returned error
// aborts Run after the hook completes, like the other observer callbacks.
type InterceptionObserver interface {
	OnInterception(ctx context.Context, e InterceptionEvent) error
}

// interceptorRun is the per-run pipeline state (filled in by Task 2).
type interceptorRun struct {
	chain []Interceptor
	risk  RiskReport
}

func (r *interceptorRun) result() *RiskReport {
	if len(r.risk.Findings) == 0 {
		return nil
	}
	s := RiskReport{Score: r.risk.Score, Findings: append([]Finding(nil), r.risk.Findings...)}
	return &s
}
