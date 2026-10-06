package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsview"
)

// The display strings below are shared by the table and (PR B) the web
// board. They interpolate backend- and config-sourced text verbatim; every
// renderer must escape the result (opsCell for the terminal).

// fmtAge renders a millisecond age compactly.
func fmtAge(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// agedMs is the envelope's age advanced by renderer elapsed time.
func agedMs(e opsview.Envelope, elapsed time.Duration) *int64 {
	if e.AgeMs == nil {
		return nil
	}
	v := *e.AgeMs + elapsed.Milliseconds()
	return &v
}

// opsPart is one displayable fact: a state word, detail, and source age.
type opsPart struct {
	State  string
	Detail string
	AgeMs  *int64
}

func opsReason(r *string) string {
	if r == nil {
		return ""
	}
	return opsview.Label(*r)
}

func residencyPart(m opsview.Model, generatedAt time.Time, elapsed time.Duration) opsPart {
	r := m.Residency
	var details []string
	if t := opsReason(r.Reason); t != "" {
		details = append(details, t)
	}
	if r.LastState != nil {
		details = append(details, "last: "+*r.LastState)
	}
	if r.FirstObserved != nil {
		if t, err := time.Parse(time.RFC3339, *r.FirstObserved); err == nil {
			details = append(details, "first observed "+fmtAge(generatedAt.Sub(t).Milliseconds()+elapsed.Milliseconds())+" ago")
		}
	}
	if r.ExpiresAt != nil {
		details = append(details, "expires "+*r.ExpiresAt)
	}
	details = append(details, r.Source)
	return opsPart{State: r.State, Detail: strings.Join(details, "; "), AgeMs: agedMs(r.Envelope, elapsed)}
}

func activityPart(m opsview.Model, elapsed time.Duration) opsPart {
	a := m.Activity
	detail := opsReason(a.Reason)
	if a.LastCompletedAt != nil {
		detail += "; last completed " + *a.LastCompletedAt
	}
	return opsPart{State: a.State, Detail: detail, AgeMs: agedMs(a.Envelope, elapsed)}
}

func usedByText(m opsview.Model) string {
	if !m.Configured {
		return "not in models.json"
	}
	var parts []string
	for _, u := range m.UsedBy {
		s := u.UseCase
		if u.IsFallback {
			s += " (fallback)"
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

// loadsText summarizes observed loads; "-" when none are reported (once
// mode or an unobserved backend).
func loadsText(m opsview.Model) string {
	l := m.Loads
	if l == nil {
		return "-"
	}
	text := fmt.Sprintf("%d observed loads (sampled every %ds", l.Count, l.SampleMs/1000)
	if l.Gaps > 0 {
		text += fmt.Sprintf(", %d gaps", l.Gaps)
	}
	text += ")"
	if n := len(l.Transitions); n > 0 {
		last := l.Transitions[n-1]
		text += "; last " + last.To + " at " + last.At
	}
	return text
}

// statsText summarizes one window. It reads no coverage bounds: they are
// null beside retained rows when a stamp falls outside years 0..9999.
func statsText(m opsview.Model, window string) string {
	for _, s := range m.Stats {
		if s.Window != window {
			continue
		}
		if s.Calls == nil || s.Errors == nil {
			return "n/a (" + opsview.Label(s.Coverage.Reason) + ")"
		}
		text := fmt.Sprintf("%d calls, %d errors", *s.Calls, *s.Errors)
		if s.DecodeTPS.P50 != nil {
			text += fmt.Sprintf("; decode p50 %.1f tok/s (n=%d)", *s.DecodeTPS.P50, s.DecodeTPS.N)
		} else if s.DecodeTPS.N > 0 {
			text += fmt.Sprintf("; decode n=%d (%s)", s.DecodeTPS.N, opsReason(s.DecodeTPS.Reason))
		}
		text += "; retained history " + s.Coverage.State
		if s.Stale {
			text += " (stale)"
		}
		return text
	}
	return "n/a"
}

func backendText(b opsview.Backend, elapsed time.Duration) string {
	rt := b.Runtime.Kind
	if b.Runtime.Version != nil {
		rt += " " + *b.Runtime.Version
	}
	text := rt + " " + b.Runtime.Support + "; " + b.Reachability.State
	if b.Reachability.Code != nil {
		text += " (" + opsview.Label(*b.Reachability.Code) + ")"
	}
	if age := agedMs(b.Reachability.Envelope, elapsed); age != nil {
		text += ", checked " + fmtAge(*age) + " ago"
	}
	if r := b.Reachability.RetryInMs; r != nil {
		// retry_in_ms sits at 0 until the collector's retry is published
		// (up to 13 s later), so a due retry never reads "in 0s".
		if left := *r - elapsed.Milliseconds(); left > 0 {
			text += ", next check in " + fmtAge(left+999) // round up
		} else {
			text += ", retry due"
		}
	}
	if b.Observation.Since != nil {
		text += "; observed since " + *b.Observation.Since
	}
	text += fmt.Sprintf(", %d gaps", b.Observation.Gaps)
	return text
}

const opsCoverageNote = "stats are llama-swap's retained history (all clients); activity is unknown because llama-swap v235 publishes no reliable in-flight snapshot"
