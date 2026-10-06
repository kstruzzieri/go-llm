package opsview

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsbackend"
)

const (
	modelErrorWindow = 10 * time.Minute
	// maxModelErrors bounds model_errors items per snapshot; one overflow
	// item counts the rest.
	maxModelErrors = 256
)

// telemetryCodes are the surface errors from a backend that answered but
// whose telemetry is unusable. Unreachable and timeout are reachability's.
var telemetryCodes = []opsbackend.Code{
	opsbackend.CodeUnauthorized, opsbackend.CodeDenied, opsbackend.CodeMalformed, opsbackend.CodeTooLarge, opsbackend.CodeHTTPStatus,
}

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

// attention applies the spec §5.4 rules. No rule has a reading of when its
// condition began (each reading is re-stamped by the next tick or retry, and
// a since taken from one reorders the list every cycle), so since is always
// null and the order is severity, subject, reason, text: total, so a
// renderer comparing the top item sees a change only when the item changed.
func (b builder) attention() []Attention {
	out := []Attention{}
	add := func(sev, reason, subject, text string) {
		out = append(out, Attention{Severity: sev, Reason: reason, Subject: subject, Text: text})
	}
	used := b.usedProviders()
	remote := 0
	var errs []Attention
	unlisted := 0 // erroring models never itemised (name over MaxNameLen)
	for _, o := range b.in.Observations.Backends {
		if o.Refused > 0 {
			add(SevSerious, AttnRefused, o.Provider, fmt.Sprintf("golem ops refused %d of its own requests to %s; this is a console bug", o.Refused, o.Provider))
		}
		if o.Hosting == opsbackend.HostingRemote {
			remote++
			continue
		}
		switch o.Support {
		case opsbackend.SupportInvalidConfig, opsbackend.SupportUnsupported, opsbackend.SupportUnrecognized:
			if used[o.Provider] {
				add(SevWarning, AttnBackendUnsupported, o.Provider, fmt.Sprintf("%s is not observed: %s", o.Provider, Label(o.Support)))
			}
			continue
		}
		// A failure stays current until its scheduled retry, so a down
		// backend raises one steady alert instead of flipping to "stale"
		// between retries (spec §5.4).
		down := o.Reachable != nil && !o.Reachable.Value && b.failureFresh(o.ReachRetry)
		switch {
		case down && used[o.Provider]:
			what := "is unreachable"
			if o.ReachCode == opsbackend.CodeTimeout {
				what = "did not answer in time"
			}
			// Readings taken before the failure stay fresh for their own
			// window, so its models need not read unknown yet.
			add(SevCritical, AttnBackendUnreachable, o.Provider, o.Provider+" "+what+"; its readings age out")
		case down:
		case o.Reachable != nil && !o.Reachable.Value:
			// The failure outlived its retry window: the console missed its
			// own schedule, so the backend is no longer known down.
			add(SevWarning, AttnTelemetryStale, o.Provider, fmt.Sprintf("%s last check failed (%s); next check overdue, so its facts read unknown", o.Provider, Label(string(o.ReachCode))))
		default:
			if facts := b.staleFacts(o); len(facts) > 0 {
				add(SevWarning, AttnTelemetryStale, o.Provider, fmt.Sprintf("%s has no fresh reading of %s", o.Provider, strings.Join(facts, ", ")))
			}
		}
		// Surface errors from before an outage say nothing during it. One
		// item per code: a wrong api_key fails every surface at once.
		for _, code := range telemetryCodes {
			var names []string
			for _, s := range o.Surfaces {
				if s.LastError == code {
					names = append(names, s.Name)
				}
			}
			if len(names) > 0 && !down {
				slices.Sort(names)
				add(SevWarning, AttnTelemetryUnavailable, o.Provider, fmt.Sprintf("%s telemetry unavailable on %s: %s", o.Provider, strings.Join(names, ", "), Label(string(code))))
			}
		}
		items, long := b.modelErrors(o)
		errs = append(errs, items...)
		unlisted += long
	}
	slices.SortFunc(errs, attentionOrder)
	if n := len(errs) - maxModelErrors; n > 0 {
		errs = errs[:maxModelErrors]
		unlisted += n
	}
	if unlisted > 0 {
		add(SevSerious, AttnModelErrors, "", fmt.Sprintf("%d unlisted models returned non-2xx responses in the last 10 minutes (the list holds at most %d models, none named over %d bytes)", unlisted, maxModelErrors, opsbackend.MaxNameLen))
	}
	out = append(out, errs...)
	// configview reports a missing models.json as Ready false plus a
	// config_missing diagnostic, so the diagnostics alone say it once.
	for _, d := range b.in.Config.Diagnostics {
		add(SevWarning, AttnConfigProblem, d.Subject, strings.TrimSpace("models.json problem: "+d.Code+" "+d.Subject))
	}
	if n := len(b.in.Observations.Backends); n > 0 && remote == n {
		add(SevInfo, AttnCoverageNote, "", "every configured provider is remote, so nothing is observed")
	}
	slices.SortFunc(out, attentionOrder)
	return out
}

func attentionOrder(x, y Attention) int {
	return cmp.Or(
		cmp.Compare(severityRank[x.Severity], severityRank[y.Severity]),
		cmp.Compare(x.Subject, y.Subject),
		cmp.Compare(x.Reason, y.Reason),
		cmp.Compare(x.Text, y.Text),
	)
}

// staleFacts names each successful reading that has aged out. Each fact is
// judged on its own reading, so fresh metrics cannot mask stale residency.
func (b builder) staleFacts(o opsbackend.BackendObservation) []string {
	var out []string
	if o.Running != nil && !b.fresh(o.Running.Mono) {
		out = append(out, "residency (/running)")
	}
	if o.PS != nil && !b.fresh(o.PS.Mono) {
		out = append(out, "residency (/api/ps)")
	}
	if o.Rows != nil && !b.fresh(o.Rows.Mono) {
		out = append(out, "statistics (/api/metrics)")
	}
	if o.Reachable != nil && o.Reachable.Value && !b.okCurrent(o) {
		out = append(out, "reachability")
	}
	return out
}

// okCurrent reports whether a positive reachability reading still speaks for
// now. Every attempt the collector makes re-stamps reachability unless the
// ops allowlist or the destination gate stopped it, so while every surface
// the backend's kind reads backs off (a 401 from the start) it is next
// contacted at its earliest pending attempt, and the reading stands until
// that attempt can have been published: the bound a failure keeps until its
// retry. A surface due now (NextAttempt 0) would have re-stamped it, so an
// aged reading beside one means the console missed its schedule. The
// collector publishes only the surfaces the current kind reads.
func (b builder) okCurrent(o opsbackend.BackendObservation) bool {
	if b.fresh(o.Reachable.Mono) {
		return true
	}
	var next time.Duration
	for _, s := range o.Surfaces {
		switch {
		case s.LastError == opsbackend.CodeDenied || s.LastError == opsbackend.CodeRefused:
		case s.NextAttempt == 0:
			return false
		case next == 0 || s.NextAttempt < next:
			next = s.NextAttempt
		}
	}
	return next > 0 && b.failureFresh(next)
}

// modelErrors reports a lower bound: rows are retained history only, so
// absence never implies healthy. A future-dated row is skipped, or it would
// sit in the window forever. A model named over opsbackend.MaxNameLen bytes
// is not itemised (decode does not bound names, and Subject and Text carry
// them into -json): it is returned as a count for the overflow item. The
// caller sorts and caps the items.
func (b builder) modelErrors(o opsbackend.BackendObservation) ([]Attention, int) {
	if o.Rows == nil || !b.fresh(o.Rows.Mono) {
		return nil, 0
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
	var out []Attention
	long := 0
	for model, a := range per {
		switch {
		case a.n < 2:
		case len(model) > opsbackend.MaxNameLen:
			long++
		default:
			out = append(out, Attention{Severity: SevSerious, Reason: AttnModelErrors, Subject: o.Provider + "/" + model,
				Text: fmt.Sprintf("at least %d retained requests to %s on %s returned non-2xx in the last 10 min (newest at %s)", a.n, model, o.Provider, deref(stamp(a.newest)))})
		}
	}
	return out, long
}
