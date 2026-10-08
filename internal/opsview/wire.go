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

const stampLayout = "2006-01-02T15:04:05.000Z07:00"

// stamp renders UTC RFC3339 with milliseconds. Zero time is null, and so is a
// time whose UTC year falls outside 0000-9999, which RFC3339 cannot write (a
// metrics row timestamp or an Ollama expires_at can carry either).
func stamp(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	if y := t.Year(); y < 0 || y > 9999 {
		return nil
	}
	s := t.Format(stampLayout)
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
	if b.in.Mode == ModeOnce {
		return true
	}
	return b.in.NowMono-mono <= 3*b.in.Interval
}

// failureFresh reports whether a failure reading is still current: it is
// until the retry its failure scheduled has had time to answer and be
// published (spec §5.4). Ticks run backends serially, so the first tick at or
// after retry can start up to one interval plus one running tick
// (opsbackend.TickTimeout) later; that tick can itself take a full
// TickTimeout, its requests bounded by opsbackend.RequestTimeout, and watch
// and serve publish only when it ends. One second of grace absorbs jitter.
// Without this, reachability would flip between unreachable and unknown
// between backoff retries.
func (b builder) failureFresh(retry time.Duration) bool {
	if b.in.Mode == ModeOnce {
		return true
	}
	return b.in.NowMono <= retry+b.in.Interval+2*opsbackend.TickTimeout+time.Second
}

func (b builder) envelope(source string, at time.Time, mono time.Duration) Envelope {
	age := (b.in.NowMono - mono).Milliseconds()
	if age < 0 {
		age = 0
	}
	return Envelope{Source: source, ObservedAt: stamp(at), Stale: !b.fresh(mono), AgeMs: &age}
}
