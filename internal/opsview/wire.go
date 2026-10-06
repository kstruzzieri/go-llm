package opsview

import (
	"time"

	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/internal/opsbackend"
)

// Input is everything Build needs. Configured is explicit because configview
// unions config and inventory selectors; Revision because configview does not
// project it. NowMono drives every age and freshness decision; wall clocks
// are never subtracted.
type Input struct {
	Config       configview.Snapshot
	Configured   []string
	Revision     string
	Observations opsbackend.Observations
	Now          time.Time
	NowMono      time.Duration
	Mode         Mode
	Interval     time.Duration
}

type builder struct{ in Input }

const (
	stampLayout = "2006-01-02T15:04:05.000Z07:00"
	// retryGrace is the request timeout (2 s) plus one second: a failure
	// reading stays fresh until its scheduled retry has had time to answer.
	retryGrace = 3 * time.Second
)

// stamp renders UTC RFC3339 with milliseconds; zero time is null.
func stamp(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(stampLayout)
	return &s
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// fresh reports whether a successful reading taken at mono is recent enough:
// within three intervals. A one-shot run's readings come from the run itself.
func (b builder) fresh(mono time.Duration) bool {
	if b.in.Mode == ModeOnce || b.in.Interval <= 0 {
		return true
	}
	return b.in.NowMono-mono <= 3*b.in.Interval
}

// failureFresh reports whether a failure reading is still current: it is
// until its next scheduled retry has had time to answer (spec §5.4). Without
// this, reachability would flip between unreachable and unknown between
// backoff retries.
func (b builder) failureFresh(retry time.Duration) bool {
	if b.in.Mode == ModeOnce || b.in.Interval <= 0 {
		return true
	}
	return b.in.NowMono <= retry+retryGrace
}

func (b builder) envelope(source string, at time.Time, mono time.Duration) Envelope {
	age := (b.in.NowMono - mono).Milliseconds()
	if age < 0 {
		age = 0
	}
	return Envelope{Source: source, ObservedAt: stamp(at), Stale: !b.fresh(mono), AgeMs: &age}
}
