package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/internal/agenttrace"
)

// schemaStubTool is an agent.Tool whose full spec (name/desc/params) varies, for
// toolSchemaHash tests. (tools_test.go's stubTool exposes only a name.)
type schemaStubTool struct{ name, desc, params string }

func (s schemaStubTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: s.name, Description: s.desc, Parameters: json.RawMessage(s.params)}
}
func (schemaStubTool) Effect() agent.Effect { return agent.Effect{Class: agent.Read} }
func (schemaStubTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

func TestToolSchemaHash(t *testing.T) {
	if toolSchemaHash(nil) != "" {
		t.Fatal("nil tools -> want empty hash")
	}
	a := []agent.Tool{schemaStubTool{"read", "r", `{"type":"object"}`}}
	b := []agent.Tool{schemaStubTool{"read", "r", `{"type":"object"}`}}
	c := []agent.Tool{schemaStubTool{"read", "r", `{"type":"string"}`}}
	if toolSchemaHash(a) != toolSchemaHash(b) {
		t.Fatal("identical specs must hash equal")
	}
	if toolSchemaHash(a) == toolSchemaHash(c) {
		t.Fatal("differing schema must change the hash")
	}
	if toolSchemaHash(a) == "" {
		t.Fatal("non-empty tools -> non-empty hash")
	}
}

func TestNewObserv_DisabledReturnsNil(t *testing.T) {
	o, err := newObserv(os.Getenv, t.TempDir(), false, false, time.Now)
	if err != nil {
		t.Fatalf("newObserv: %v", err)
	}
	if o != nil {
		t.Fatalf("both disabled -> want nil observ, got %+v", o)
	}
}

func TestNewObserv_ResolvesPathsOutsideWorkspace(t *testing.T) {
	base := t.TempDir()
	root := t.TempDir() // workspace root, distinct from data base
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return base
		}
		return ""
	}
	o, err := newObserv(getenv, root, true, true, time.Now)
	if err != nil {
		t.Fatalf("newObserv: %v", err)
	}
	if !strings.HasPrefix(o.traceDir, filepath.Join(base, "golem", "traces")) {
		t.Fatalf("traceDir = %q", o.traceDir)
	}
	if o.telemetryPath != filepath.Join(base, "golem", "telemetry.jsonl") {
		t.Fatalf("telemetryPath = %q", o.telemetryPath)
	}
	if _, err := os.Stat(o.traceDir); err != nil {
		t.Fatalf("traceDir not created: %v", err)
	}
}

func TestObserv_WriteTraceCollisionRetry(t *testing.T) {
	base := t.TempDir()
	root := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return base
		}
		return ""
	}
	o, err := newObserv(getenv, root, true, false, func() time.Time { return time.Unix(1719600000, 0) })
	if err != nil {
		t.Fatalf("newObserv: %v", err)
	}
	runID := "fixed-run"
	startedAt := "2026-06-28T00-00-00Z"
	// Pre-create the primary trace path so the first WriteTrace collides; the
	// retry must fall through to a "-1.json" suffix and leave the seed untouched.
	primary := filepath.Join(o.traceDir, startedAt+"-"+runID+".json")
	if werr := os.WriteFile(primary, []byte("preexisting"), 0o600); werr != nil {
		t.Fatalf("seed: %v", werr)
	}
	res := agent.Result{Steps: []agent.StepRecord{{Index: 0}}, StopReason: agent.Completed}
	if werr := o.writeTrace(runID, startedAt, startedAt, agenttrace.TraceMeta{Goal: "g"}, res, "completed", false, nil); werr != nil {
		t.Fatalf("writeTrace: %v", werr)
	}
	suffixed := filepath.Join(o.traceDir, startedAt+"-"+runID+"-1.json")
	if _, serr := os.Stat(suffixed); serr != nil {
		t.Fatalf("expected suffixed trace %q: %v", suffixed, serr)
	}
	if b, _ := os.ReadFile(primary); string(b) != "preexisting" {
		t.Fatalf("primary trace was clobbered: %q", b)
	}
}

func TestObserv_WriteTraceSanitizesStartedAtFilename(t *testing.T) {
	base := t.TempDir()
	root := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return base
		}
		return ""
	}
	o, err := newObserv(getenv, root, true, false, time.Now)
	if err != nil {
		t.Fatalf("newObserv: %v", err)
	}
	startedAt := "2026-06-29T10:20:30.123456789Z"
	res := agent.Result{StopReason: agent.Completed}
	if err := o.writeTrace("run1", startedAt, startedAt, agenttrace.TraceMeta{Goal: "g"}, res, "completed", false, nil); err != nil {
		t.Fatalf("writeTrace: %v", err)
	}

	entries, err := os.ReadDir(o.traceDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("trace files = %d, want 1", len(entries))
	}
	if strings.Contains(entries[0].Name(), ":") {
		t.Fatalf("trace filename contains colon: %q", entries[0].Name())
	}
	raw, err := os.ReadFile(filepath.Join(o.traceDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var rec agenttrace.TraceRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if rec.StartedAt != startedAt {
		t.Fatalf("started_at = %q, want %q", rec.StartedAt, startedAt)
	}
}

func TestObserv_NextRunIDUnique(t *testing.T) {
	o := &observ{clock: func() time.Time { return time.Unix(1719600000, 123_000_000) }}
	a, b := o.nextRunID(), o.nextRunID()
	if a == b {
		t.Fatalf("run ids not unique: %q == %q", a, b)
	}
}

func TestRunStatus(t *testing.T) {
	if s, p := runStatus(nil, false); s != "completed" || p {
		t.Fatalf("nil err -> %q/%v", s, p)
	}
	if s, p := runStatus(context.Canceled, true); s != "canceled" || !p {
		t.Fatalf("canceled -> %q/%v", s, p)
	}
	if s, p := runStatus(context.DeadlineExceeded, false); s != "error" || !p {
		t.Fatalf("error -> %q/%v", s, p)
	}
}

func TestComposeObserver(t *testing.T) {
	rend := newRenderer(io.Discard, false, 4, nil, false)
	if got := composeObserver(rend, nil); got != agent.Observer(rend) {
		t.Fatalf("nil sink should return renderer unchanged")
	}
}

type pressureChild struct{ got []agent.PressureEvent }

func (p *pressureChild) OnStep(context.Context, agent.StepEvent) error         { return nil }
func (p *pressureChild) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }
func (p *pressureChild) OnToken(context.Context, agent.TokenEvent) error       { return nil }
func (p *pressureChild) OnPressure(_ context.Context, e agent.PressureEvent) error {
	p.got = append(p.got, e)
	return nil
}

type nonPressureObs struct{}

func (nonPressureObs) OnStep(context.Context, agent.StepEvent) error         { return nil }
func (nonPressureObs) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }
func (nonPressureObs) OnToken(context.Context, agent.TokenEvent) error       { return nil }

func TestMultiObserverOnPressureFanout(t *testing.T) {
	child := &pressureChild{}
	plain := nonPressureObs{} // does NOT implement PressureObserver
	m := &multiObserver{children: []agent.Observer{plain, child}}
	e := agent.PressureEvent{Step: 3, Pressure: agent.Pressure{Level: agent.LevelWarn}}
	if err := m.OnPressure(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(child.got) != 1 || child.got[0].Step != 3 {
		t.Fatalf("PressureObserver child not reached: %+v", child.got)
	}
}

func TestObserv_TraceAndTelemetryShareRunID(t *testing.T) {
	base := t.TempDir()
	root := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return base
		}
		return ""
	}
	o, err := newObserv(getenv, root, true, true, func() time.Time { return time.Unix(1719600000, 0) })
	if err != nil {
		t.Fatalf("newObserv: %v", err)
	}
	runID := o.nextRunID()
	started := o.clock()

	sink, err := o.startSink(runID, started)
	if err != nil {
		t.Fatalf("startSink: %v", err)
	}
	res := agent.Result{Steps: []agent.StepRecord{{Index: 0}}, StopReason: agent.Completed}
	_ = sink.Finish(res, "completed")
	_ = sink.Close()

	if err := o.writeTrace(runID,
		started.UTC().Format(time.RFC3339Nano), started.UTC().Format(time.RFC3339Nano),
		agenttrace.TraceMeta{Goal: "g"}, res, "completed", false, nil); err != nil {
		t.Fatalf("writeTrace: %v", err)
	}

	tel, _ := os.ReadFile(o.telemetryPath)
	if !strings.Contains(string(tel), runID) {
		t.Fatalf("telemetry missing run id %q:\n%s", runID, tel)
	}
	entries, _ := os.ReadDir(o.traceDir)
	if len(entries) != 1 {
		t.Fatalf("trace files = %d, want 1", len(entries))
	}
	if !strings.Contains(entries[0].Name(), runID) {
		t.Fatalf("trace file %q missing run id %q", entries[0].Name(), runID)
	}
}
