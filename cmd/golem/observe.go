package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/internal/agenttrace"
)

// observ holds resolved, validated observability paths and run-id state. It is
// nil when both -trace and -telemetry are off (the caller checks for nil).
type observ struct {
	trace         bool
	telemetry     bool
	traceDir      string
	telemetryPath string
	clock         func() time.Time

	mu      sync.Mutex
	counter int
}

// newObserv resolves trace/telemetry paths from the per-user data dir (reusing
// dataDirBase + validatePathOutsideWorkspace), validates they fall outside the
// workspace, and creates the trace dir. Returns nil when both are disabled.
func newObserv(getenv func(string) string, root string, trace, telemetry bool, clock func() time.Time) (*observ, error) {
	if !trace && !telemetry {
		return nil, nil
	}
	if clock == nil {
		clock = time.Now
	}
	base, err := dataDirBase(getenv)
	if err != nil {
		return nil, err
	}
	o := &observ{trace: trace, telemetry: telemetry, clock: clock}
	if trace {
		o.traceDir = filepath.Join(base, "golem", "traces")
		if err := validatePathOutsideWorkspace(o.traceDir, root); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(o.traceDir, sessionDirMode); err != nil {
			return nil, fmt.Errorf("golem: create trace dir: %w", err)
		}
	}
	if telemetry {
		o.telemetryPath = filepath.Join(base, "golem", "telemetry.jsonl")
		if err := validatePathOutsideWorkspace(o.telemetryPath, root); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(o.telemetryPath), sessionDirMode); err != nil {
			return nil, fmt.Errorf("golem: create telemetry dir: %w", err)
		}
	}
	return o, nil
}

// nextRunID mints a per-turn id: <unix-millis>-<pid>-<counter>. Deterministic
// given the clock; the counter guarantees uniqueness within a process.
func (o *observ) nextRunID() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.counter++
	return fmt.Sprintf("%d-%d-%d", o.clock().UnixMilli(), os.Getpid(), o.counter)
}

// startSink opens a TelemetrySink for one run, or returns (nil, nil) when
// telemetry is off. The caller defers Close on the returned sink.
func (o *observ) startSink(runID string, started time.Time) (*agenttrace.TelemetrySink, error) {
	if o == nil || !o.telemetry {
		return nil, nil
	}
	return agenttrace.NewTelemetrySink(o.telemetryPath, runID, started, o.clock)
}

// writeTrace writes one content-full trace, retrying with a numeric suffix on a
// run-id path collision (create-exclusive). Best-effort: returns an error the
// caller logs but does not propagate as a turn failure.
// grounding is the already-marshaled #348 report, or nil when the turn produced
// none. It is stored verbatim so this tier keeps no opinion about a payload
// cmd/golem owns.
func (o *observ) writeTrace(runID, startedAt, endedAt string, meta agenttrace.TraceMeta, res agent.Result, status string, partial bool, runErr error, grounding json.RawMessage) error {
	rec := agenttrace.BuildTrace(meta, res, status, partial, runErr)
	rec.RunID = runID
	rec.StartedAt = startedAt
	rec.EndedAt = endedAt
	rec.Grounding = grounding
	nameStartedAt := strings.NewReplacer(":", "-", "/", "-", "\\", "-").Replace(startedAt)
	base := filepath.Join(o.traceDir, nameStartedAt+"-"+runID)
	for i := 0; i < 5; i++ {
		path := base + ".json"
		if i > 0 {
			path = fmt.Sprintf("%s-%d.json", base, i)
		}
		err := agenttrace.WriteTrace(path, rec)
		if err == nil {
			return nil
		}
		// WriteTrace wraps the O_EXCL failure with %w; errors.Is walks that chain
		// (os.IsExist does not), so a real collision falls through to a suffix.
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return fmt.Errorf("golem: trace path collision for run %s", runID)
}

// composeObserver returns the renderer alone when it is the only child, else a
// fan-out observer. The sink remains second, after the renderer.
func composeObserver(rend agent.Observer, sink *agenttrace.TelemetrySink, extras ...agent.Observer) agent.Observer {
	children := make([]agent.Observer, 0, len(extras)+2)
	if rend != nil {
		children = append(children, rend)
	}
	if sink != nil {
		children = append(children, sink)
	}
	for _, child := range extras {
		if child != nil {
			children = append(children, child)
		}
	}
	if len(children) == 1 {
		return children[0]
	}
	return &multiObserver{children: children}
}

// multiObserver fans every callback out to its children in order, propagating
// the first error. It implements the optional ToolResultObserver, Thinking-
// Observer, PressureObserver, and ContextAssemblyObserver extensions, forwarding
// each callback only to children that implement it.
type multiObserver struct{ children []agent.Observer }

var _ agent.RetrievalPresentationObserver = (*multiObserver)(nil)

func (m *multiObserver) OnStep(ctx context.Context, e agent.StepEvent) error {
	for _, c := range m.children {
		if err := c.OnStep(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (m *multiObserver) OnToolCall(ctx context.Context, e agent.ToolCallEvent) error {
	for _, c := range m.children {
		if err := c.OnToolCall(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (m *multiObserver) OnToken(ctx context.Context, e agent.TokenEvent) error {
	for _, c := range m.children {
		if err := c.OnToken(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (m *multiObserver) OnToolResult(ctx context.Context, e agent.ToolResultEvent) error {
	for _, c := range m.children {
		if tro, ok := c.(agent.ToolResultObserver); ok {
			if err := tro.OnToolResult(ctx, e); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *multiObserver) OnThinking(ctx context.Context, e agent.ThinkingEvent) error {
	for _, c := range m.children {
		if to, ok := c.(agent.ThinkingObserver); ok {
			if err := to.OnThinking(ctx, e); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *multiObserver) OnPressure(ctx context.Context, e agent.PressureEvent) error {
	for _, c := range m.children {
		if po, ok := c.(agent.PressureObserver); ok {
			if err := po.OnPressure(ctx, e); err != nil {
				return err
			}
		}
	}
	return nil
}

// OnContextAssembly fans the #331 mixed-assembly trace out to children that
// opted in. The telemetry sink consumes it (an aggregate context_assembly span);
// Golem's renderer does not. The type-switch shape still matters because when
// telemetry is enabled this wrapper IS the CLI's observer (composeObserver
// returns the renderer alone otherwise), so an unimplemented callback here would
// silently swallow the event for every child.
func (m *multiObserver) OnContextAssembly(ctx context.Context, e agent.ContextAssemblyEvent) error {
	for _, c := range m.children {
		if cao, ok := c.(agent.ContextAssemblyObserver); ok {
			if err := cao.OnContextAssembly(ctx, e); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *multiObserver) OnRetrievalPresentation(ctx context.Context, e agent.RetrievalPresentationEvent) error {
	for _, c := range m.children {
		if rpo, ok := c.(agent.RetrievalPresentationObserver); ok {
			if err := rpo.OnRetrievalPresentation(ctx, e); err != nil {
				return err
			}
		}
	}
	return nil
}

// toolSchemaHash returns a stable fnv64a digest of the active tool specs (name,
// description, JSON schema) so a trace records which tool surface the run saw
// without embedding the full schemas (#238: "tool specs or tool schema hash").
// Empty when there are no tools. Hashed in slice order, which Golem fixes.
func toolSchemaHash(tools []agent.Tool) string {
	if len(tools) == 0 {
		return ""
	}
	h := fnv.New64a()
	for _, t := range tools {
		s := t.Spec()
		_, _ = h.Write([]byte(s.Name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(s.Description))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(s.Parameters)
		_, _ = h.Write([]byte{0})
	}
	return fmt.Sprintf("fnv64:%x", h.Sum64())
}

// runStatus derives the trace completeness status from the Run return. agent's
// StopReason defaults to Completed and is meaningless on an error path, so the
// policy layer decides status itself.
func runStatus(runErr error, canceled bool) (status string, partial bool) {
	switch {
	case runErr == nil:
		return "completed", false
	case canceled:
		return "canceled", true
	default:
		return "error", true
	}
}
