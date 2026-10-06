package opsview

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsbackend"
)

const modelErrorWindow = 10 * time.Minute

func (b builder) usedProviders() map[string]bool {
	out := map[string]bool{}
	for _, rb := range b.in.Config.Bindings {
		for _, sel := range rb.Chain {
			prov, _, _ := strings.Cut(sel, "/")
			out[prov] = true
		}
	}
	return out
}

// attention applies the spec §5.4 rules. The order is total (severity, since,
// subject, then reason and text), so a renderer comparing the top item sees
// a change only when the item changed.
func (b builder) attention() []Attention {
	out := []Attention{}
	add := func(sev, reason, subject string, since time.Time, text string) {
		out = append(out, Attention{Severity: sev, Reason: reason, Subject: subject, Since: stamp(since), Text: text})
	}
	used := b.usedProviders()
	remote := 0
	for _, o := range b.in.Observations.Backends {
		if o.Refused > 0 {
			add(SevSerious, AttnRefused, o.Provider, time.Time{}, fmt.Sprintf("golem ops refused %d of its own requests to %s; this is a console bug", o.Refused, o.Provider))
		}
		if o.Hosting == opsbackend.HostingRemote {
			remote++
			continue
		}
		switch o.Support {
		case opsbackend.SupportInvalidConfig, opsbackend.SupportUnsupported, opsbackend.SupportUnrecognized:
			if used[o.Provider] {
				add(SevWarning, AttnBackendUnsupported, o.Provider, time.Time{}, fmt.Sprintf("%s is not observed: %s", o.Provider, Label(o.Support)))
			}
			continue
		}
		// A failure stays current until its scheduled retry, so a down
		// backend raises one steady alert instead of flipping to "stale"
		// between retries (spec §5.4).
		down := o.Reachable != nil && !o.Reachable.Value && b.failureFresh(o.ReachRetry)
		if down && used[o.Provider] {
			add(SevCritical, AttnBackendUnreachable, o.Provider, o.Reachable.At, fmt.Sprintf("%s is unreachable (%s); every model on it is unavailable", o.Provider, o.ReachCode))
		}
		if !down && b.positiveStale(o) {
			add(SevWarning, AttnTelemetryStale, o.Provider, time.Time{}, fmt.Sprintf("%s has no fresh reading; its facts read unknown", o.Provider))
		}
		for _, s := range o.Surfaces {
			switch s.LastError {
			case opsbackend.CodeUnauthorized, opsbackend.CodeDenied, opsbackend.CodeMalformed, opsbackend.CodeTooLarge, opsbackend.CodeHTTPStatus:
				add(SevWarning, AttnTelemetryUnavailable, o.Provider, time.Time{}, fmt.Sprintf("%s %s telemetry unavailable: %s", o.Provider, s.Name, s.LastError))
			}
		}
		b.modelErrors(o, add)
	}
	// configview reports a missing models.json as Ready false plus a
	// config_missing diagnostic, so the diagnostics alone say it once.
	for _, d := range b.in.Config.Diagnostics {
		add(SevWarning, AttnConfigProblem, d.Subject, time.Time{}, strings.TrimSpace("models.json problem: "+d.Code+" "+d.Subject))
	}
	if n := len(b.in.Observations.Backends); n > 0 && remote == n {
		add(SevInfo, AttnCoverageNote, "", time.Time{}, "every configured provider is remote, so nothing is observed")
	}
	slices.SortFunc(out, func(x, y Attention) int {
		return cmp.Or(
			cmp.Compare(severityRank[x.Severity], severityRank[y.Severity]),
			cmp.Compare(deref(x.Since), deref(y.Since)),
			cmp.Compare(x.Subject, y.Subject),
			cmp.Compare(x.Reason, y.Reason),
			cmp.Compare(x.Text, y.Text),
		)
	})
	return out
}

// positiveStale reports a successful reading that has aged out: residency,
// statistics or reachability. Each fact is checked on its own, so fresh
// metrics cannot mask stale residency.
func (b builder) positiveStale(o opsbackend.BackendObservation) bool {
	return (o.Running != nil && !b.fresh(o.Running.Mono)) ||
		(o.PS != nil && !b.fresh(o.PS.Mono)) ||
		(o.Rows != nil && !b.fresh(o.Rows.Mono)) ||
		(o.Reachable != nil && o.Reachable.Value && !b.fresh(o.Reachable.Mono))
}

// modelErrors reports a lower bound: rows are retained history only, so
// absence never implies healthy. A future-dated row is skipped, or it would
// sit in the window forever.
func (b builder) modelErrors(o opsbackend.BackendObservation, add func(string, string, string, time.Time, string)) {
	if o.Rows == nil || !b.fresh(o.Rows.Mono) {
		return
	}
	type agg struct {
		n      int
		newest time.Time
	}
	per := map[string]*agg{}
	start := b.in.Now.Add(-modelErrorWindow)
	for _, r := range o.Rows.Value {
		if r.Status >= 200 && r.Status <= 299 || r.Timestamp.Before(start) || b.future(r.Timestamp) {
			continue
		}
		a := per[r.Model]
		if a == nil {
			a = &agg{}
			per[r.Model] = a
		}
		a.n++
		if r.Timestamp.After(a.newest) {
			a.newest = r.Timestamp
		}
	}
	for _, model := range slices.Sorted(maps.Keys(per)) {
		a := per[model]
		if a.n >= 2 {
			add(SevSerious, AttnModelErrors, model, a.newest, fmt.Sprintf("at least %d retained requests to %s on %s returned non-2xx in the last 10 min (newest at %s)", a.n, model, o.Provider, deref(stamp(a.newest))))
		}
	}
}
