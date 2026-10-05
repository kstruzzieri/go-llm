package opsbackend

import (
	"context"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

const (
	tickTimeout    = 5 * time.Second
	backoffBase    = 2 * time.Second
	backoffMax     = 30 * time.Second
	maxTracked     = 256
	maxTransitions = 20
	maxSkew        = time.Second
	maxVersionLen  = 64
	maxNameLen     = 512 // longest model name residency memory admits, in bytes
)

// Clock supplies wall time for display and monotonic elapsed time for ages
// and gaps. Production uses SystemClock; tests drive both by hand.
type Clock struct {
	Wall func() time.Time
	Mono func() time.Duration
}

// SystemClock returns the process clock: UTC wall time without a monotonic
// reading, and monotonic time elapsed since the call.
func SystemClock() Clock {
	start := time.Now()
	return Clock{
		Wall: func() time.Time { return time.Now().Round(0).UTC() },
		Mono: func() time.Duration { return time.Since(start) },
	}
}

// BackendSpec is one configured provider as models.json declares it.
type BackendSpec struct {
	Provider  string
	BaseURL   string
	APIFormat string
	APIKey    string
}

// Kind is what a backend was identified as.
type Kind string

// Backend kinds.
const (
	KindUnidentified Kind = "unidentified"
	KindLlamaSwap    Kind = "llama-swap"
	KindOllama       Kind = "ollama"
	KindNone         Kind = "none"
)

// Hosting says whether golem ops may contact a backend at all.
type Hosting string

// Hosting values.
const (
	HostingLocal   Hosting = "local"
	HostingRemote  Hosting = "remote"
	HostingUnknown Hosting = "unknown"
)

// Support values for BackendObservation.Support. The three failure values
// are the matching Code strings, so the vocabulary has one source.
const (
	SupportSupported     = "supported"
	SupportUnknown       = "unknown"
	SupportNotObserved   = "not_observed"
	SupportUnsupported   = string(CodeUnsupportedVersion)
	SupportUnrecognized  = string(CodeUnrecognizedRuntime)
	SupportInvalidConfig = string(CodeInvalidConfiguration)
)

// Sample is one decoded response stamped with console receipt time.
type Sample[T any] struct {
	Value T
	At    time.Time
	Mono  time.Duration
}

// Surface is the last outcome of one endpoint and its schedule.
type Surface struct {
	Name        string
	LastSuccess time.Time     // zero: never succeeded
	LastError   Code          // "": last attempt succeeded or none yet
	NextAttempt time.Duration // monotonic time of the next scheduled attempt; 0: next tick
}

// Transition is one observed residency change.
type Transition struct {
	To string
	At time.Time
}

// ModelMemory is what the collector remembers about one backend model.
type ModelMemory struct {
	Model         string
	FirstObserved time.Time // first observed loaded in the current period; zero when not loaded
	Loads         int       // observed load episodes since Since
	Gaps          int       // observation gaps since Since
	Since         time.Time // first time the model was tracked
	Transitions   []Transition
}

// BackendObservation is everything observed about one provider.
type BackendObservation struct {
	Provider   string
	Endpoint   string // scheme://host[:port] only
	Hosting    Hosting
	Kind       Kind
	Version    string
	Support    string
	Reachable  *Sample[bool]
	ReachCode  Code
	ReachRetry time.Duration // when Reachable is a failure: monotonic time of the next scheduled retry
	Surfaces   []Surface
	Running    *Sample[[]RunningModel]
	Rows       *Sample[[]ActivityRow]
	Listed     *Sample[[]ListedModel]
	PS         *Sample[[]PSModel]
	Since      time.Time // current observation period start
	Periods    int
	Gaps       int
	Refused    int64
	Models     []ModelMemory
	// ModelsOverflow is the most names one residency sample held that memory
	// could not track (over the cap, or longer than maxNameLen bytes). It
	// never decreases, so a model dropped once always reads as untracked.
	ModelsOverflow int
}

// Observations is one detached collector snapshot.
type Observations struct {
	Interval        time.Duration
	TimingUncertain bool // wall and monotonic time diverged on this tick; every earlier reading was dropped
	CollectedAt     time.Time
	CollectedMono   time.Duration
	Backends        []BackendObservation
}

// SkewSince reports how far wall and monotonic time have drifted apart since
// collection. Renderers call it before projecting: monotonic time stops
// during sleep on macOS, so a snapshot rendered after wake would otherwise
// look seconds old.
func (o Observations) SkewSince(c Clock) time.Duration {
	d := c.Wall().Sub(o.CollectedAt) - (c.Mono() - o.CollectedMono)
	if d < 0 {
		return -d
	}
	return d
}

// Dropped returns the snapshot with every reading discarded and timing marked
// uncertain; configuration, classification and memory counters stay.
func (o Observations) Dropped() Observations {
	out := o.clone()
	out.TimingUncertain = true
	for i := range out.Backends {
		b := &out.Backends[i]
		b.Reachable, b.ReachCode, b.ReachRetry = nil, "", 0
		b.Running, b.Rows, b.Listed, b.PS = nil, nil, nil, nil
	}
	return out
}

// Options configures a Collector. Interval is 0 for a one-shot run.
type Options struct {
	Interval time.Duration
	Clock    Clock
}

// Collector polls configured backends. Tick is serialized; every result is a
// deep copy that shares no storage with the collector.
type Collector struct {
	mu        sync.Mutex
	opts      Options
	backends  []*backendState
	ticked    bool
	lastWall  time.Time
	lastMono  time.Duration
	uncertain bool
}

type surfaceState struct {
	failures    int
	next        time.Duration
	lastSuccess time.Time
	lastErr     Code
}

type backendState struct {
	spec     BackendSpec
	dest     provider.Destination
	obs      BackendObservation
	refused  atomic.Int64
	identify *client
	observe  *client
	surfaces map[string]*surfaceState
	order    []string
	prev     map[string]string // model -> live state in the current period; nil before a baseline
	mem      map[string]*ModelMemory
}

// NewCollector validates every spec without I/O. Remote, invalid and
// unknown-format providers are recorded and never contacted.
func NewCollector(specs []BackendSpec, opts Options) *Collector {
	if opts.Clock.Wall == nil || opts.Clock.Mono == nil {
		opts.Clock = SystemClock()
	}
	sorted := slices.Clone(specs)
	slices.SortFunc(sorted, func(a, b BackendSpec) int { return strings.Compare(a.Provider, b.Provider) })
	c := &Collector{opts: opts}
	for _, spec := range sorted {
		c.backends = append(c.backends, newBackendState(spec))
	}
	return c
}

func newBackendState(spec BackendSpec) *backendState {
	b := &backendState{spec: spec, surfaces: map[string]*surfaceState{}, mem: map[string]*ModelMemory{}}
	b.obs = BackendObservation{Provider: spec.Provider, Hosting: HostingUnknown, Kind: KindNone, Support: SupportInvalidConfig}
	d, err := provider.NewDestination(spec.Provider, spec.BaseURL)
	if err != nil {
		return b
	}
	b.dest = d
	b.obs.Endpoint = originOf(d)
	// Remote is decided first: a hosted provider whose base carries a path
	// (https://host/zen/go) is "remote, not observed", not misconfigured.
	if !d.IsLocal() {
		b.obs.Hosting, b.obs.Support = HostingRemote, SupportNotObserved
		return b
	}
	b.obs.Hosting = HostingLocal
	if _, err := rootDestination(spec.Provider, spec.BaseURL); err != nil {
		return b // local with a path prefix: invalid_configuration, never contacted
	}
	switch spec.APIFormat {
	case "openai-compat":
		cl, err := newClient(d, spec.APIKey, provider.DestinationPurposeDiscovery, identifyRoutes, &b.refused)
		if err != nil {
			return b
		}
		b.identify = cl
		b.obs.Kind, b.obs.Support = KindUnidentified, SupportUnknown
	case "ollama", "":
		cl, err := newClient(d, spec.APIKey, provider.DestinationPurposeHealth, ollamaRoutes, &b.refused)
		if err != nil {
			return b
		}
		b.observe = cl
		b.obs.Kind, b.obs.Support = KindOllama, SupportSupported
	}
	return b
}

// originOf projects a destination as scheme://host[:port] only, like
// configview: a base path may carry a token and is never displayed.
func originOf(d provider.Destination) string {
	u, err := url.Parse(d.BaseURL())
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// Tick observes every backend once (subject to per-surface backoff) and
// returns a detached snapshot.
func (c *Collector) Tick(ctx context.Context) Observations {
	c.mu.Lock()
	defer c.mu.Unlock()
	wall, mono := c.now()
	if c.ticked {
		elapsed := mono - c.lastMono
		skew := wall.Sub(c.lastWall) - elapsed
		diverged := skew > maxSkew || skew < -maxSkew
		late := c.opts.Interval > 0 && elapsed > 3*c.opts.Interval
		for _, b := range c.backends {
			switch {
			case diverged:
				// Monotonic time stops during sleep on macOS, so a pre-sleep
				// reading would look seconds old: drop every reading.
				b.dropSamples()
			case late:
				b.breakPeriod()
			}
		}
		c.uncertain = diverged
	}
	c.lastWall, c.lastMono, c.ticked = wall, mono, true

	tctx, cancel := context.WithTimeout(ctx, tickTimeout)
	defer cancel()
	for _, b := range c.backends {
		if b.obs.Kind == KindUnidentified {
			c.identifyBackend(ctx, tctx, b)
		}
		switch b.obs.Kind {
		case KindLlamaSwap:
			c.observeLlamaSwap(ctx, tctx, b)
		case KindOllama:
			c.observeOllama(ctx, tctx, b)
		}
	}
	return c.snapshot()
}

func (c *Collector) now() (time.Time, time.Duration) {
	return c.opts.Clock.Wall(), c.opts.Clock.Mono()
}

// fetch performs one surface request when its backoff allows, decodes it and
// records the outcome. It reports whether a fresh sample was applied.
func (c *Collector) fetch(callerCtx, ctx context.Context, b *backendState, cl *client, surface, path string, limit int64, apply func([]byte) error) bool {
	s := b.surface(surface)
	if _, mono := c.now(); mono < s.next {
		return false
	}
	// A request the tick deadline or the caller stopped before it started
	// says nothing about the backend: nothing is sent, and reachability and
	// backoff stay untouched. A skipped residency read is still a missing
	// sample, so it opens a gap like a failed one (spec §4.5).
	if ctx.Err() != nil {
		if surface == "running" || surface == "ps" {
			b.breakPeriod()
		}
		return false
	}
	body, err := cl.get(ctx, surface, path, limit)
	if err == nil {
		err = apply(body)
	}
	c.record(callerCtx, b, surface, s, err)
	return err == nil
}

func (c *Collector) record(callerCtx context.Context, b *backendState, surface string, s *surfaceState, err error) {
	wall, mono := c.now()
	if err == nil {
		s.failures, s.next, s.lastSuccess, s.lastErr = 0, 0, wall, ""
		b.obs.Reachable, b.obs.ReachCode, b.obs.ReachRetry = &Sample[bool]{Value: true, At: wall, Mono: mono}, "", 0
		return
	}
	code, ok := CodeOf(err)
	if !ok || callerCtx.Err() != nil {
		return // caller cancellation: unclassified, no backoff
	}
	s.failures++
	s.next = mono + backoff(s.failures)
	s.lastErr = code
	switch code {
	case CodeUnreachable, CodeTimeout:
		b.obs.Reachable, b.obs.ReachCode, b.obs.ReachRetry = &Sample[bool]{Value: false, At: wall, Mono: mono}, code, s.next
		b.breakPeriod()
		if b.obs.Kind == KindLlamaSwap {
			b.obs.Kind, b.obs.Support = KindUnidentified, SupportUnknown
		}
	case CodeRefused, CodeDenied:
		// Decided inside this process; says nothing about the backend.
	default:
		b.obs.Reachable, b.obs.ReachCode, b.obs.ReachRetry = &Sample[bool]{Value: true, At: wall, Mono: mono}, "", 0
	}
	if surface == "running" || surface == "ps" {
		b.breakPeriod()
	}
}

func backoff(failures int) time.Duration {
	d := backoffBase << (failures - 1)
	if d <= 0 || d > backoffMax {
		return backoffMax
	}
	return d
}

func (c *Collector) identifyBackend(callerCtx, ctx context.Context, b *backendState) {
	var version string
	var isLS bool
	if !c.fetch(callerCtx, ctx, b, b.identify, "version", "/api/version", versionLimit, func(body []byte) error {
		version, isLS = decodeVersion(body)
		return nil
	}) && b.surface("version").lastErr != CodeHTTPStatus {
		// No answer yet: retried under backoff. A 401 or 403 is coded
		// unauthorized, not http_status, so it lands here too: v235 serves
		// /api/version behind its API-key check (server.go:208,260;
		// auth.go:34), so a wrong api_key is not another runtime.
		return
	}
	// Every outcome sets Version: a backend that read v235 before an outage
	// must not keep that version once it answers as something else.
	switch {
	case !isLS:
		// Includes a non-2xx answer (llama-server direct, vLLM: 404): a
		// runtime that is not llama-swap. Stop asking.
		b.dropSamples()
		b.obs.Kind, b.obs.Support, b.obs.Version = KindNone, SupportUnrecognized, ""
	case version != "v235":
		b.dropSamples()
		b.obs.Kind, b.obs.Support, b.obs.Version = KindNone, SupportUnsupported, bound(version)
	default:
		if b.observe == nil {
			cl, err := newClient(b.dest, b.spec.APIKey, provider.DestinationPurposeHealth, llamaSwapRoutes, &b.refused)
			if err != nil {
				b.obs.Kind, b.obs.Support = KindNone, SupportInvalidConfig
				return
			}
			b.observe = cl
		}
		b.obs.Kind, b.obs.Support, b.obs.Version = KindLlamaSwap, SupportSupported, version
	}
}

func bound(s string) string {
	if len(s) > maxVersionLen {
		return s[:maxVersionLen]
	}
	return s
}

func (c *Collector) observeLlamaSwap(callerCtx, ctx context.Context, b *backendState) {
	c.fetch(callerCtx, ctx, b, b.observe, "running", "/running", runningLimit, func(body []byte) error {
		models, err := decodeRunning(body)
		if err != nil {
			return err
		}
		wall, mono := c.now()
		b.obs.Running = &Sample[[]RunningModel]{Value: models, At: wall, Mono: mono}
		cur := make(map[string]string, len(models))
		for _, m := range models {
			cur[m.Model] = m.State
		}
		b.applyResidency(cur, wall)
		return nil
	})
	if b.obs.Kind != KindLlamaSwap {
		return
	}
	c.fetch(callerCtx, ctx, b, b.observe, "metrics", "/api/metrics", metricsLimit, func(body []byte) error {
		rows, err := decodeMetrics(body)
		if err != nil {
			return err
		}
		wall, mono := c.now()
		b.obs.Rows = &Sample[[]ActivityRow]{Value: rows, At: wall, Mono: mono}
		return nil
	})
	if b.obs.Kind != KindLlamaSwap {
		return
	}
	if !c.fetch(callerCtx, ctx, b, b.observe, "models", "/v1/models", modelsLimit, func(body []byte) error {
		listed, err := decodeModels(body)
		if err != nil {
			return err
		}
		wall, mono := c.now()
		b.obs.Listed = &Sample[[]ListedModel]{Value: listed, At: wall, Mono: mono}
		return nil
	}) {
		b.obs.Listed = nil // peer confirmation fails closed without a current listing
	}
}

func (c *Collector) observeOllama(callerCtx, ctx context.Context, b *backendState) {
	c.fetch(callerCtx, ctx, b, b.observe, "ps", "/api/ps", psLimit, func(body []byte) error {
		models, err := decodePS(body)
		if err != nil {
			return err
		}
		wall, mono := c.now()
		b.obs.PS = &Sample[[]PSModel]{Value: models, At: wall, Mono: mono}
		cur := make(map[string]string, len(models))
		for _, m := range models {
			cur[NormalizeOllama(m.Name)] = "ready"
		}
		b.applyResidency(cur, wall)
		return nil
	})
}

func (b *backendState) surface(name string) *surfaceState {
	s, ok := b.surfaces[name]
	if !ok {
		s = &surfaceState{}
		b.surfaces[name] = s
		b.order = append(b.order, name)
	}
	return s
}

// liveState reduces a raw state to the residency memory's alphabet.
func liveState(cur map[string]string, name string) string {
	switch st := cur[name]; st {
	case "starting", "ready", "stopping":
		return st
	}
	return "absent"
}

// applyResidency updates load memory from one trusted residency sample. New
// names are admitted in sorted order, so the cap keeps the same models on
// every run. The first sample of a period is a baseline and counts nothing;
// afterwards one load episode is a move from absent or stopping into
// starting or ready.
func (b *backendState) applyResidency(cur map[string]string, at time.Time) {
	untracked := 0
	for _, name := range slices.Sorted(maps.Keys(cur)) {
		if _, ok := b.mem[name]; ok {
			continue
		}
		if len(b.mem) >= maxTracked || len(name) > maxNameLen {
			untracked++
			continue
		}
		b.mem[name] = &ModelMemory{Model: name, Since: at}
	}
	// The same untracked names arrive every tick, so they are not summed.
	b.obs.ModelsOverflow = max(b.obs.ModelsOverflow, untracked)
	if b.prev == nil {
		b.prev = make(map[string]string, len(b.mem))
		b.obs.Periods++
		b.obs.Since = at
		for name, m := range b.mem {
			st := liveState(cur, name)
			b.prev[name] = st
			if st == "ready" {
				m.FirstObserved = at
			}
		}
		return
	}
	for name, m := range b.mem {
		was, ok := b.prev[name]
		if !ok {
			was = "absent"
		}
		now := liveState(cur, name)
		if was == now {
			continue
		}
		b.prev[name] = now
		if (was == "absent" || was == "stopping") && (now == "starting" || now == "ready") {
			m.Loads++
		}
		switch now {
		case "ready":
			if m.FirstObserved.IsZero() {
				m.FirstObserved = at
			}
		case "absent", "stopping":
			m.FirstObserved = time.Time{}
		}
		to := ResidencyOf(now)
		if now == "absent" && b.obs.Kind == KindOllama {
			to = "unknown" // absence from /api/ps is not proof of unload (spec §4.4)
		}
		m.Transitions = append(m.Transitions, Transition{To: to, At: at})
		if len(m.Transitions) > maxTransitions {
			m.Transitions = slices.Clone(m.Transitions[len(m.Transitions)-maxTransitions:])
		}
	}
}

// breakPeriod ends the current observation period: no transition is inferred
// across the gap and the next trusted sample is a new baseline.
func (b *backendState) breakPeriod() {
	if b.prev == nil {
		return
	}
	b.prev = nil
	b.obs.Gaps++
	for _, m := range b.mem {
		m.Gaps++
		m.FirstObserved = time.Time{}
	}
}

// dropSamples discards every reading: after a clock jump (their ages are
// unknowable) or when the backend is re-identified as something else (they
// describe a different process).
func (b *backendState) dropSamples() {
	b.breakPeriod()
	b.obs.Reachable, b.obs.ReachCode, b.obs.ReachRetry = nil, "", 0
	b.obs.Running, b.obs.Rows, b.obs.Listed, b.obs.PS = nil, nil, nil, nil
}

func (c *Collector) snapshot() Observations {
	wall, mono := c.now()
	out := Observations{Interval: c.opts.Interval, TimingUncertain: c.uncertain, CollectedAt: wall, CollectedMono: mono}
	for _, b := range c.backends {
		o := b.obs
		o.Refused = b.refused.Load()
		o.Surfaces = make([]Surface, 0, len(b.order))
		for _, name := range b.order {
			s := b.surfaces[name]
			o.Surfaces = append(o.Surfaces, Surface{Name: name, LastSuccess: s.lastSuccess, LastError: s.lastErr, NextAttempt: s.next})
		}
		o.Models = make([]ModelMemory, 0, len(b.mem))
		for _, name := range slices.Sorted(maps.Keys(b.mem)) {
			o.Models = append(o.Models, *b.mem[name])
		}
		out.Backends = append(out.Backends, o)
	}
	return out.clone()
}

// clone deep-copies every slice and sample so callers can never alias
// collector state.
func (o Observations) clone() Observations {
	out := o
	out.Backends = make([]BackendObservation, len(o.Backends))
	for i, b := range o.Backends {
		nb := b
		nb.Reachable = cloneSample(b.Reachable, func(v bool) bool { return v })
		nb.Running = cloneSample(b.Running, func(v []RunningModel) []RunningModel { return slices.Clone(v) })
		nb.Rows = cloneSample(b.Rows, func(v []ActivityRow) []ActivityRow { return slices.Clone(v) })
		nb.Listed = cloneSample(b.Listed, func(v []ListedModel) []ListedModel { return slices.Clone(v) })
		nb.PS = cloneSample(b.PS, func(v []PSModel) []PSModel { return slices.Clone(v) })
		nb.Surfaces = slices.Clone(b.Surfaces)
		nb.Models = make([]ModelMemory, len(b.Models))
		for j, m := range b.Models {
			m.Transitions = slices.Clone(m.Transitions)
			nb.Models[j] = m
		}
		out.Backends[i] = nb
	}
	return out
}

func cloneSample[T any](s *Sample[T], copyValue func(T) T) *Sample[T] {
	if s == nil {
		return nil
	}
	return &Sample[T]{Value: copyValue(s.Value), At: s.At, Mono: s.Mono}
}
