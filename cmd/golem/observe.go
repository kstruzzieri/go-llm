package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
func (o *observ) writeTrace(runID, startedAt, endedAt string, meta agenttrace.TraceMeta, res agent.Result, status string, partial bool, runErr error) error {
	rec := agenttrace.BuildTrace(meta, res, status, partial, runErr)
	rec.RunID = runID
	rec.StartedAt = startedAt
	rec.EndedAt = endedAt
	base := filepath.Join(o.traceDir, startedAt+"-"+runID)
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

// composeObserver returns the renderer alone when sink is nil, else a fan-out
// observer driving both. The sink never errors, so only the renderer can abort.
func composeObserver(rend agent.Observer, sink *agenttrace.TelemetrySink) agent.Observer {
	if sink == nil {
		return rend
	}
	return &multiObserver{children: []agent.Observer{rend, sink}}
}

// multiObserver fans every callback out to its children in order, propagating
// the first error. It implements ToolResultObserver and forwards OnToolResult
// only to children that implement it.
type multiObserver struct{ children []agent.Observer }

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
