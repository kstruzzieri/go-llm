package agent

import (
	"context"
	"sync"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/conversation"
)

// Exposure records whether outside content has entered a conversation. It is
// the conversation package's record, aliased here because agent imports it.
type Exposure = conversation.Exposure

type exposureKey struct{}

// exposureState is the one record a run tree shares; mu guards exp.
type exposureState struct {
	mu  sync.Mutex
	exp Exposure
}

// exposureRun is one run's node over the shared state. It is a struct so
// per-run fields can join it later; today it only points at the state.
type exposureRun struct {
	state *exposureState
}

// newExposureRun joins the parent run's tracker found in ctx, or starts a
// fresh one, then folds in req's prior state. Exposure only grows: a nested
// run adds to its parent's record and never clears it.
func newExposureRun(ctx context.Context, req Request) (context.Context, *exposureRun) {
	r := &exposureRun{}
	if parent := exposureFrom(ctx); parent != nil {
		r.state = parent.state
	} else {
		r.state = &exposureState{}
	}
	switch p := req.PriorExposure; {
	case p != nil && p.Recorded:
		r.merge(*p)
	case p != nil || len(req.History) > 0 || req.HistorySummary != "":
		r.merge(Exposure{Unknown: true, First: "history"})
	}
	return context.WithValue(ctx, exposureKey{}, r), r
}

// exposureFrom returns the run's tracker, or nil outside a run.
func exposureFrom(ctx context.Context) *exposureRun {
	r, _ := ctx.Value(exposureKey{}).(*exposureRun)
	return r
}

func (r *exposureRun) merge(e Exposure) {
	s := r.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if (e.Foreign || e.Unknown) && s.exp.First == "" {
		s.exp.First = boundExposureFirst(e.First)
	}
	s.exp.Foreign = s.exp.Foreign || e.Foreign
	s.exp.Unknown = s.exp.Unknown || e.Unknown
}

// admit records a tool result of the given origin: OriginForeign sets
// Foreign, OriginUnknown sets Unknown, any other origin changes nothing.
func (r *exposureRun) admit(origin Origin, toolName string) {
	if r == nil {
		return
	}
	switch origin {
	case OriginForeign:
		r.merge(Exposure{Foreign: true, First: "tool:" + toolName})
	case OriginUnknown:
		r.merge(Exposure{Unknown: true, First: "tool:" + toolName})
	}
}

// admitForeign records foreign content that is not a tool result, labelled
// by first (for example "consult").
func (r *exposureRun) admitForeign(first string) {
	if r == nil {
		return
	}
	r.merge(Exposure{Foreign: true, First: first})
}

// snapshot returns a copy of the record. Recorded is always true: a run
// that tracked the conversation has a record, even a clean one.
func (r *exposureRun) snapshot() Exposure {
	if r == nil {
		return Exposure{}
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	e := r.state.exp
	e.Recorded = true
	return e
}

// boundExposureFirst (applied in merge) cuts s to MaxExposureFirstBytes at a UTF-8 boundary.
func boundExposureFirst(s string) string {
	if len(s) <= conversation.MaxExposureFirstBytes {
		return s
	}
	n := conversation.MaxExposureFirstBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
