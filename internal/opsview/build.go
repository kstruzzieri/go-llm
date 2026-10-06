package opsview

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsbackend"
)

// maxBackendOnly bounds unconfigured models per backend (the collector's
// tracking cap).
const maxBackendOnly = 256

// view is one backend's observations plus what Build derives from them once
// per backend, so no model rescans the ring.
type view struct {
	o     opsbackend.BackendObservation
	known bool // the provider is in the observations at all
	ring  ring
	// confirmed holds names fresh samples prove canonical: residency and
	// backend-only claims are about now.
	confirmed map[string]bool
	// identified holds names samples of any age prove canonical: statistics
	// and loads report their own staleness, so an aged proof still names
	// the right rows.
	identified map[string]bool
}

// Build projects the inputs into the v1 snapshot.
func Build(in Input) Snapshot {
	b := builder{in: in}
	s := Snapshot{
		Version: 1, Mode: in.Mode, IntervalMs: in.Interval.Milliseconds(),
		GeneratedAt: deref(stamp(in.Now)), TimingUncertain: in.Observations.TimingUncertain,
		Config: b.config(), Backends: []Backend{}, Models: []Model{}, Attention: []Attention{},
		GeneratedMono: in.NowMono,
	}
	usedBy := b.usedBy()
	configured := map[string]bool{}
	for _, sel := range in.Configured {
		configured[sel] = true
	}
	views := map[string]view{}
	for _, o := range in.Observations.Backends {
		v := b.view(o)
		views[o.Provider] = v
		only, overflow := b.backendOnly(v, configured)
		bv := b.backend(o)
		bv.Observation.BackendOnlyOverflow = overflow
		s.Backends = append(s.Backends, bv)
		for _, name := range only {
			s.Models = append(s.Models, b.model("backend:"+o.Provider+"/"+name, o.Provider, name, false, v, nil))
		}
	}
	for _, sel := range in.Configured {
		prov, name, _ := strings.Cut(sel, "/")
		s.Models = append(s.Models, b.model(sel, prov, name, true, views[prov], usedBy[sel]))
	}
	slices.SortFunc(s.Backends, func(x, y Backend) int { return cmp.Compare(x.ID, y.ID) })
	slices.SortFunc(s.Models, func(x, y Model) int { return cmp.Compare(x.ID, y.ID) })
	s.Attention = b.attention()
	return s
}

func (b builder) view(o opsbackend.BackendObservation) view {
	v := view{o: o, known: true, confirmed: confirmedBy(o, b.fresh), identified: confirmedBy(o, anyAge)}
	if o.Rows != nil {
		v.ring = newRing(o.Rows.Value)
	}
	return v
}

func anyAge(time.Duration) bool { return true }

func (b builder) config() ConfigView {
	c := ConfigView{Source: b.in.Config.Origin.Source, Ready: b.in.Config.Ready, Revision: strPtr(b.in.Revision), Diagnostics: []Diagnostic{}}
	for _, d := range b.in.Config.Diagnostics {
		c.Diagnostics = append(c.Diagnostics, Diagnostic{Code: d.Code, Subject: d.Subject})
	}
	return c
}

// usedBy derives membership from each binding's chain; position decides
// fallback, never the binding's resolution-dependent flag.
func (b builder) usedBy() map[string][]UsedBy {
	out := map[string][]UsedBy{}
	for _, rb := range b.in.Config.Bindings {
		for i, sel := range rb.Chain {
			out[sel] = append(out[sel], UsedBy{UseCase: rb.UseCase, Role: rb.Role, IsFallback: i > 0})
		}
	}
	return out
}

// observed reports whether golem ops currently reads this backend at all.
// Cached samples never outrank this: a backend re-identified as unsupported
// projects nothing it read before.
func observed(o opsbackend.BackendObservation) bool {
	return o.Hosting == opsbackend.HostingLocal && o.Kind != opsbackend.KindNone
}

func sourceName(o opsbackend.BackendObservation) string {
	switch {
	case o.Kind == opsbackend.KindOllama || o.PS != nil:
		return "ollama"
	case o.Kind == opsbackend.KindLlamaSwap || o.Running != nil || o.Rows != nil:
		return "llama-swap"
	}
	return "endpoint"
}

func (b builder) backend(o opsbackend.BackendObservation) Backend {
	out := Backend{
		ID: o.Provider, Endpoint: strPtr(o.Endpoint), Hosting: string(o.Hosting),
		Runtime:     Runtime{Kind: string(o.Kind), Version: strPtr(o.Version), Support: o.Support},
		Observation: Observation{Since: stamp(o.Since), Periods: o.Periods, Gaps: o.Gaps, Refused: o.Refused, ModelsOverflow: o.ModelsOverflow},
		Surfaces:    []SurfaceView{},
	}
	for _, s := range o.Surfaces {
		out.Surfaces = append(out.Surfaces, SurfaceView{Name: s.Name, LastSuccess: stamp(s.LastSuccess), LastError: strPtr(string(s.LastError))})
	}
	src := sourceName(o)
	switch {
	case o.Hosting == opsbackend.HostingRemote:
		out.Reachability = Reachability{State: StateNotObserved, Code: strPtr(ReasonRemote), Envelope: Envelope{Source: "none"}}
	case o.Support == opsbackend.SupportInvalidConfig:
		out.Reachability = Reachability{State: StateNotObserved, Code: strPtr(opsbackend.SupportInvalidConfig), Envelope: Envelope{Source: "none"}}
	case o.Reachable == nil:
		out.Reachability = Reachability{State: StateUnknown, Code: strPtr(ReasonNoSample), Envelope: Envelope{Source: src}}
	case o.Reachable.Value:
		env := b.envelope(src, o.Reachable.At, o.Reachable.Mono)
		if env.Stale {
			out.Reachability = Reachability{State: StateUnknown, Code: strPtr(ReasonStale), Envelope: env}
		} else {
			out.Reachability = Reachability{State: ReachOK, Envelope: env}
		}
	default:
		env := b.envelope(src, o.Reachable.At, o.Reachable.Mono)
		env.Stale = !b.failureFresh(o.ReachRetry)
		if env.Stale {
			out.Reachability = Reachability{State: StateUnknown, Code: strPtr(ReasonStale), Envelope: env}
			break
		}
		retry := max(0, (o.ReachRetry - b.in.NowMono).Milliseconds())
		out.Reachability = Reachability{State: ReachUnreachable, Code: strPtr(string(o.ReachCode)), RetryInMs: &retry, Envelope: env}
	}
	return out
}

// confirmedBy returns names proven to be canonical local llama-swap IDs by
// samples current says may speak: /running names, plus models on 2xx rows
// when a listing can exclude peers. Without a usable listing, rows confirm
// nothing.
func confirmedBy(o opsbackend.BackendObservation, current func(mono time.Duration) bool) map[string]bool {
	c := map[string]bool{}
	if o.Running != nil && current(o.Running.Mono) {
		for _, m := range o.Running.Value {
			c[m.Model] = true
		}
	}
	if o.Listed == nil || !current(o.Listed.Mono) || o.Rows == nil || !current(o.Rows.Mono) {
		return c
	}
	peers := map[string]bool{}
	for _, l := range o.Listed.Value {
		if l.Peer {
			peers[l.ID] = true
		}
	}
	for _, r := range o.Rows.Value {
		if r.Status >= 200 && r.Status <= 299 && !peers[r.Model] {
			c[r.Model] = true
		}
	}
	return c
}

// backendOnly returns unconfigured models with proven identity, sorted and
// capped, plus the overflow count.
func (b builder) backendOnly(v view, configured map[string]bool) ([]string, int) {
	o := v.o
	if !observed(o) {
		return nil, 0
	}
	seen := maps.Clone(v.confirmed) // shared with residency: never write through it
	if o.PS != nil && b.fresh(o.PS.Mono) {
		for _, m := range o.PS.Value {
			seen[m.Name] = true
		}
	}
	var out []string
	for name := range seen {
		if configured[o.Provider+"/"+name] || (o.Kind == opsbackend.KindOllama && configuredOllama(o.Provider, name, configured)) {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	if len(out) > maxBackendOnly {
		return out[:maxBackendOnly], len(out) - maxBackendOnly
	}
	return out, 0
}

func configuredOllama(provider, psName string, configured map[string]bool) bool {
	want := opsbackend.NormalizeOllama(psName)
	if want == "" {
		return false // a blank name matches nothing
	}
	for sel := range configured {
		prov, name, _ := strings.Cut(sel, "/")
		if prov == provider && opsbackend.NormalizeOllama(name) == want {
			return true
		}
	}
	return false
}

func memoryFor(o opsbackend.BackendObservation, name string) (opsbackend.ModelMemory, bool) {
	if name == "" {
		return opsbackend.ModelMemory{}, false // a blank name owns no memory
	}
	for _, m := range o.Models {
		if m.Model == name {
			return m, true
		}
	}
	return opsbackend.ModelMemory{}, false
}

func (b builder) model(id, prov, name string, configured bool, v view, used []UsedBy) Model {
	if used == nil {
		used = []UsedBy{}
	}
	m := Model{ID: id, Provider: prov, BackendModel: name, Configured: configured, UsedBy: used, Stats: []Stats{}}
	if !v.known {
		reason := ReasonUnknownProvider
		m.Residency = Residency{State: StateUnknown, Reason: &reason, Envelope: Envelope{Source: "none"}}
		m.Activity = Activity{State: StateUnknown, Reason: &reason, Envelope: Envelope{Source: "none"}}
		return m
	}
	o := v.o
	m.Residency = b.residency(v, name)
	m.Activity = b.activity(v, name)
	if !observed(o) {
		return m
	}
	switch {
	case o.Kind == opsbackend.KindOllama:
		for _, w := range windows {
			m.Stats = append(m.Stats, noHistoryStats(w))
		}
	case o.Running != nil || o.Rows != nil:
		for _, w := range windows {
			if v.identified[name] {
				m.Stats = append(m.Stats, b.windowStats(name, o.Rows, v.ring, w))
			} else {
				m.Stats = append(m.Stats, unconfirmedStats(w))
			}
		}
	}
	if b.in.Mode != ModeOnce {
		m.Loads = b.loads(v, name)
	}
	return m
}

// unconfirmedStats is one window for a name no sample proved canonical. An
// alias's requests are recorded under its canonical ID, so counting rows by
// the alias would claim a false measured zero.
func unconfirmedStats(w window) Stats {
	s := unavailableStats(w, ReasonUnconfirmed)
	s.Envelope = Envelope{Source: "llama-swap /api/metrics"}
	s.Coverage = Coverage{State: CoverageUnknown, Reason: ReasonUnconfirmed}
	return s
}

func (b builder) residency(v view, name string) Residency {
	o := v.o
	switch {
	case o.Hosting == opsbackend.HostingRemote:
		return Residency{State: StateNotObserved, Reason: strPtr(ReasonRemote), Envelope: Envelope{Source: "none"}}
	case !observed(o):
		return Residency{State: StateNotObserved, Reason: strPtr(o.Support), Envelope: Envelope{Source: "none"}}
	case o.Running != nil:
		return b.llamaSwapResidency(v, name)
	case o.PS != nil:
		return b.ollamaResidency(o, name)
	}
	reason := ReasonNoSample
	if o.ReachCode != "" {
		reason = string(o.ReachCode)
	}
	return Residency{State: StateUnknown, Reason: &reason, Envelope: Envelope{Source: sourceName(o)}}
}

func (b builder) llamaSwapResidency(v view, name string) Residency {
	s := v.o.Running
	env := b.envelope("llama-swap /running", s.At, s.Mono)
	state := ""
	for _, rm := range s.Value {
		if rm.Model == name {
			if st := opsbackend.ResidencyOf(rm.State); st != StateUnloaded {
				state = st
			}
		}
	}
	r := Residency{Envelope: env}
	switch {
	case state != "":
		r.State = state
	case v.confirmed[name]:
		r.State = StateUnloaded
	default:
		r.State, r.Reason = StateUnknown, strPtr(ReasonUnconfirmed)
	}
	if mem, ok := memoryFor(v.o, name); ok && r.State == StateLoaded {
		r.FirstObserved = stamp(mem.FirstObserved)
	}
	if env.Stale {
		last := r.State
		r = Residency{State: StateUnknown, Reason: strPtr(ReasonStale), LastState: &last, Envelope: env}
	}
	return r
}

// ollamaResidency reads presence in /api/ps as loaded and absence as unknown:
// Ollama can serve a copied name from another name's runner, so absence is
// not proof of unload (spec §4.4).
func (b builder) ollamaResidency(o opsbackend.BackendObservation, name string) Residency {
	s := o.PS
	env := b.envelope("ollama /api/ps", s.At, s.Mono)
	want := opsbackend.NormalizeOllama(name)
	r := Residency{State: StateUnknown, Reason: strPtr(ReasonOllamaAbsent), Envelope: env}
	for _, pm := range s.Value {
		if want != "" && (opsbackend.NormalizeOllama(pm.Name) == want || opsbackend.NormalizeOllama(pm.Model) == want) {
			r = Residency{State: StateLoaded, ExpiresAt: stamp(pm.ExpiresAt), Envelope: env}
			if mem, ok := memoryFor(o, want); ok {
				r.FirstObserved = stamp(mem.FirstObserved)
			}
		}
	}
	if env.Stale {
		last := r.State
		r = Residency{State: StateUnknown, Reason: strPtr(ReasonStale), LastState: &last, Envelope: env}
	}
	return r
}

func (b builder) activity(v view, name string) Activity {
	o := v.o
	switch {
	case o.Hosting == opsbackend.HostingRemote:
		return Activity{State: StateNotObserved, Reason: strPtr(ReasonRemote), Envelope: Envelope{Source: "none"}}
	case !observed(o):
		return Activity{State: StateNotObserved, Reason: strPtr(ReasonNotObserved), Envelope: Envelope{Source: "none"}}
	case o.Kind == opsbackend.KindOllama:
		a := Activity{State: StateUnknown, Reason: strPtr(ReasonOllamaNoInflight), Envelope: Envelope{Source: "ollama /api/ps"}}
		if o.PS != nil {
			a.Envelope = b.envelope("ollama /api/ps", o.PS.At, o.PS.Mono)
		}
		return a
	case o.Running == nil && o.Rows == nil:
		return Activity{State: StateUnknown, Reason: strPtr(ReasonNoSample), Envelope: Envelope{Source: sourceName(o)}}
	}
	a := Activity{State: StateUnknown, Reason: strPtr(ReasonNoInflight), Envelope: Envelope{Source: "llama-swap /api/metrics"}}
	if o.Rows != nil {
		a.Envelope = b.envelope("llama-swap /api/metrics", o.Rows.At, o.Rows.Mono)
		// The newest completion of any status, read through the per-model
		// index. A future-dated row (clock step or hostile peer) is skipped,
		// or year 9999 would read as the last completion forever.
		var newest time.Time
		found := false
		for _, i := range v.ring.byModel[name] {
			t := v.ring.rows[i].Timestamp
			if !b.future(t) && (!found || t.After(newest)) {
				newest, found = t, true
			}
		}
		if found {
			a.LastCompletedAt = stamp(newest)
		}
	}
	return a
}

// loads reports observed load history for a model on an observed backend.
// An untracked model has a measured zero only when residency has been
// sampled, the tracking cap never overflowed, and, on llama-swap, a sample
// proved the name canonical (an alias loads under its canonical ID);
// otherwise the count was not measured and loads is omitted.
func (b builder) loads(v view, name string) *Loads {
	o := v.o
	key, named := name, v.identified[name]
	if o.Kind == opsbackend.KindOllama {
		key, named = opsbackend.NormalizeOllama(name), true
	}
	mem, tracked := memoryFor(o, key)
	if !tracked && (o.Periods == 0 || o.ModelsOverflow > 0 || !named) {
		return nil
	}
	l := &Loads{SampleMs: b.in.Interval.Milliseconds(), Since: stamp(o.Since), Transitions: []TransitionView{}}
	if tracked {
		l.Count, l.Since, l.Gaps = mem.Loads, stamp(mem.Since), mem.Gaps
		for _, t := range mem.Transitions {
			l.Transitions = append(l.Transitions, TransitionView{To: t.To, At: deref(stamp(t.At))})
		}
	}
	return l
}
