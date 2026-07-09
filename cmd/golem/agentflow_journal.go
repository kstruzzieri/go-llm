package main

import (
	"context"
	"sync"
	"time"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// recordFileChangeFunc records one applied edit with agentflow. Injected so the
// journal is testable without a real client.
type recordFileChangeFunc func(ctx context.Context, step, attempt, path string) error

// agentflowJournal calls record-file-change for each applied edit. A failure is
// fatal to the run: it latches the error and cancels the run context so the model
// cannot continue after an unreceipted edit (which would fail drift silently).
type agentflowJournal struct {
	record recordFileChangeFunc
	cancel context.CancelFunc

	mu      sync.Mutex
	step    string
	attempt string
	fatal   error
}

func newAgentflowJournal(record recordFileChangeFunc, cancel context.CancelFunc) *agentflowJournal {
	return &agentflowJournal{record: record, cancel: cancel}
}

// setStep binds subsequent Record calls to a claimed step + attempt.
func (j *agentflowJournal) setStep(step, attempt string) {
	j.mu.Lock()
	j.step, j.attempt = step, attempt
	j.mu.Unlock()
}

func (j *agentflowJournal) Record(rec agenttools.MutationRecord) {
	j.mu.Lock()
	step, attempt := j.step, j.attempt
	j.mu.Unlock()

	// Use a short standalone context so recording is not itself cancelled by the
	// run context we may be about to cancel.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := j.record(ctx, step, attempt, rec.Path); err != nil {
		j.mu.Lock()
		if j.fatal == nil {
			j.fatal = err
		}
		j.mu.Unlock()
		j.cancel() // abort the run; the driver reads fatalErr() and surfaces it
	}
}

func (j *agentflowJournal) fatalErr() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.fatal
}

// compositeJournal fans one mutation out to several sinks (the RAM /undo journal
// and the agentflow journal).
type compositeJournal struct{ sinks []agenttools.Journal }

func (c compositeJournal) Record(rec agenttools.MutationRecord) {
	for _, s := range c.sinks {
		s.Record(rec)
	}
}
