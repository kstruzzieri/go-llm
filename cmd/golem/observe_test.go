package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

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
	rend := newRenderer(io.Discard, false, 4, nil)
	if got := composeObserver(rend, nil); got != agent.Observer(rend) {
		t.Fatalf("nil sink should return renderer unchanged")
	}
}
