package main

import (
	"context"
	"errors"
	"testing"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

type recSink struct{ n int }

func (r *recSink) Record(agenttools.MutationRecord) { r.n++ }

func TestAgentflowJournal_SuccessRecords(t *testing.T) {
	rf := func(_ context.Context, step, attempt, path string) error { return nil }
	_, cancel := context.WithCancel(context.Background())
	j := newAgentflowJournal(rf, cancel)
	j.setStep("P1", "A1")
	j.Record(agenttools.MutationRecord{Path: "src/a.go"})
	if j.fatalErr() != nil {
		t.Fatalf("no fatal expected: %v", j.fatalErr())
	}
}

func TestAgentflowJournal_FailureLatchesAndCancels(t *testing.T) {
	rf := func(_ context.Context, step, attempt, path string) error { return errors.New("agentflow down") }
	ctx, cancel := context.WithCancel(context.Background())
	j := newAgentflowJournal(rf, cancel)
	j.setStep("P1", "A1")
	j.Record(agenttools.MutationRecord{Path: "src/a.go"})
	if j.fatalErr() == nil {
		t.Fatal("record failure must latch a fatal error")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("record failure must cancel the run context")
	}
}

func TestCompositeJournal_FansOut(t *testing.T) {
	a, b := &recSink{}, &recSink{}
	c := compositeJournal{sinks: []agenttools.Journal{a, b}}
	c.Record(agenttools.MutationRecord{Path: "x"})
	if a.n != 1 || b.n != 1 {
		t.Fatalf("fan-out failed: a=%d b=%d", a.n, b.n)
	}
}
