package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

const (
	unknownBucket  = "unknown"           // a step or side task with no model
	childrenBucket = "dispatch children" // Result.DescendantUsage
)

// modelUsage is the clamped token usage of one bucket. CachedReportedRequests
// counts requests whose usage reported a cached count, keeping "reported on
// some requests" apart from "reported on all". UnreliableRequests counts
// requests whose RAW usage had no prompt or needed a clamp (ruling P4-j, see
// add): billing-weighted, missing or inconsistent counts that pricing cannot
// trust.
// ChildCacheReported records a cached report from the dispatch-children
// aggregate, which has no requests to count.
type modelUsage struct {
	Prompt, Cached, CacheWrite, Completion                       int
	Requests, CachedReportedRequests, CacheWriteReportedRequests int
	UnreliableRequests                                           int
	ChildCacheReported                                           bool
}

// clampTokens is the one clamp display and pricing share: a negative counter
// counts as 0, cached is at most prompt, and cache-write at most the prompt
// left after cached. Providers pass oversized counters through raw.
func clampTokens(prompt, cached, write int) (int, int, int) {
	prompt = max(prompt, 0)
	cached = min(max(cached, 0), prompt)
	write = min(max(write, 0), prompt-cached)
	return prompt, cached, write
}

// add folds one usage report in, clamped. A report with no prompt, or one
// that any clamp changes (a negative counter, cached above prompt,
// cache-write above the prompt left after cached), is unreliable. request is
// false for the dispatch-children aggregate, which adds tokens but no
// requests.
func (m *modelUsage) add(u provider.Usage, request bool) {
	p, c, w := clampTokens(u.PromptTokens, u.CachedPromptTokens, u.CacheWriteTokens)
	if u.PromptTokens <= 0 || c != u.CachedPromptTokens || w != u.CacheWriteTokens || u.CompletionTokens < 0 {
		m.UnreliableRequests++
	}
	m.Prompt += p
	m.Cached += c
	m.CacheWrite += w
	m.Completion += max(u.CompletionTokens, 0)
	if !request {
		m.ChildCacheReported = m.ChildCacheReported || u.CachedPromptTokensReported
		return
	}
	m.Requests++
	if u.CachedPromptTokensReported {
		m.CachedReportedRequests++
	}
	if u.CacheWriteTokensReported {
		m.CacheWriteReportedRequests++
	}
}

func (m *modelUsage) merge(o modelUsage) {
	m.Prompt += o.Prompt
	m.Cached += o.Cached
	m.CacheWrite += o.CacheWrite
	m.Completion += o.Completion
	m.Requests += o.Requests
	m.CachedReportedRequests += o.CachedReportedRequests
	m.CacheWriteReportedRequests += o.CacheWriteReportedRequests
	m.UnreliableRequests += o.UnreliableRequests
	m.ChildCacheReported = m.ChildCacheReported || o.ChildCacheReported
}

// costUSD prices u at p (USD per 1,000,000 tokens). A nil cached or
// cache-write rate bills those tokens at Input.
func costUSD(u modelUsage, p *config.ModelPrice) float64 {
	if p == nil {
		return 0
	}
	prompt, cached, write := clampTokens(u.Prompt, u.Cached, u.CacheWrite)
	cachedRate, writeRate := p.Input, p.Input
	if p.CachedInput != nil {
		cachedRate = *p.CachedInput
	}
	if p.CacheWrite != nil {
		writeRate = *p.CacheWrite
	}
	return (float64(prompt-cached-write)*p.Input + float64(cached)*cachedRate +
		float64(write)*writeRate + float64(max(u.Completion, 0))*p.Output) / 1e6
}

type sideKey struct{ task, model string }

// usageLedger is the process-local usage record /context prints. byModel and
// lastRun hold agent-run usage keyed by "provider/model", "unknown" or
// "dispatch children"; side holds side-task usage per task and model. Every
// method takes mu (background goroutines record into it) and is nil-safe.
type usageLedger struct {
	mu      sync.Mutex
	byModel map[string]*modelUsage
	side    map[sideKey]*modelUsage
	lastRun map[string]*modelUsage
}

func bucket[K comparable](m map[K]*modelUsage, k K) *modelUsage {
	u := m[k]
	if u == nil {
		u = &modelUsage{}
		m[k] = u
	}
	return u
}

// routedModel is the "provider/model" that served a call, or "" when the
// call reported no route.
func routedModel(oc *provider.RouteOutcome) string {
	if oc == nil || oc.ActualModel == (provider.ModelKey{}) {
		return ""
	}
	return oc.ActualModel.String()
}

// addRun records one finished agent run on any exit path.
func (l *usageLedger) addRun(res agent.Result) {
	if l == nil {
		return
	}
	run := make(map[string]*modelUsage)
	for _, st := range res.Steps {
		model := routedModel(st.RouteOutcome)
		if model == "" {
			model = unknownBucket
		}
		bucket(run, model).add(st.Response.Usage, true)
	}
	if d := res.DescendantUsage; d != nil {
		bucket(run, childrenBucket).add(*d, false)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byModel == nil {
		l.byModel = make(map[string]*modelUsage)
	}
	for model, u := range run {
		bucket(l.byModel, model).merge(*u)
	}
	l.lastRun = run
}

// addSide records one successful side-task model call (summarizer, probes,
// embeddings, grounding, source summaries). An empty model prices as
// "unknown".
func (l *usageLedger) addSide(task, model string, u provider.Usage) {
	if l == nil {
		return
	}
	if model == "" {
		model = unknownBucket
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.side == nil {
		l.side = make(map[sideKey]*modelUsage)
	}
	bucket(l.side, sideKey{task, model}).add(u, true)
}

func sumUsage(m map[string]*modelUsage) modelUsage {
	var total modelUsage
	for _, u := range m {
		total.merge(*u)
	}
	return total
}

// scopeUsage totals one scope of agent-run buckets (the process or the last
// run) for lines 1 and 2. reported is false when no request, and not the
// children aggregate, reported a cached count (the line then reads
// "cached n/a"). suffix holds the qualifiers both lines share, in this fixed
// order:
//  1. "; cache reported on X of Y requests": a cached figure is shown but not
//     every request reported one;
//  2. "; dispatch children: N input tok, cache coverage unknown": children
//     contributed input without per-request detail;
//  3. "; unreliable usage on N requests": raw counts had no prompt or needed
//     a clamp (ruling P4-j); the children aggregate counts as one.
func scopeUsage(scope map[string]*modelUsage) (total modelUsage, reported bool, suffix string) {
	total = sumUsage(scope)
	reported = total.CachedReportedRequests > 0 || total.ChildCacheReported
	if reported && total.CachedReportedRequests < total.Requests {
		suffix += fmt.Sprintf("; cache reported on %d of %d requests", total.CachedReportedRequests, total.Requests)
	}
	unreliable := total.UnreliableRequests
	if c := scope[childrenBucket]; c != nil {
		if c.Prompt > 0 {
			suffix += fmt.Sprintf("; dispatch children: %d input tok, cache coverage unknown", c.Prompt)
		}
		unreliable += min(c.UnreliableRequests, 1) - c.UnreliableRequests
	}
	if unreliable > 0 {
		suffix += fmt.Sprintf("; unreliable usage on %d requests", unreliable)
	}
	return total, reported, suffix
}

// render returns the /context usage lines. Lines 1-2 cover agent runs only;
// line 3 prices agent runs and side tasks, each by its own model, and names
// each excluded bucket: a run bucket by its model, a side-task bucket as
// "model (task)" ("unknown" stays bare); one line per side task follows,
// sorted by name, naming its unreliable calls (ruling P4-l). prices is keyed
// "provider/model".
func (l *usageLedger) render(prices map[string]*config.ModelPrice) []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	total, reported, suffix := scopeUsage(l.byModel)
	cached := "cached n/a"
	if reported {
		pct := 0
		if total.Prompt > 0 {
			pct = total.Cached * 100 / total.Prompt
		}
		cached = fmt.Sprintf("%d cached, %d%%", total.Cached, pct)
	}
	lines := []string{fmt.Sprintf("usage (this process): input %d tok (%s), output %d tok, %d requests", total.Prompt, cached, total.Completion, total.Requests) + suffix}

	if l.lastRun == nil {
		lines = append(lines, "last run: none")
	} else {
		last, reported, suffix := scopeUsage(l.lastRun)
		cached := "cached n/a"
		if reported {
			cached = fmt.Sprintf("%d cached", last.Cached)
		}
		lines = append(lines, fmt.Sprintf("last run: input %d tok (%s), output %d tok", last.Prompt, cached, last.Completion)+suffix)
	}

	if len(prices) == 0 {
		lines = append(lines, "est. cost: unavailable (no prices set)")
	} else {
		var cost float64
		var unpriced, unreliable []string
		// Every bucket has input tokens or an unreliable request (a request
		// without input is unreliable), so none is skipped as empty: a
		// zero-usage request is named, never priced as free.
		price := func(model, label string, u *modelUsage) {
			switch p := prices[model]; {
			case p == nil:
				unpriced = append(unpriced, label)
			case u.UnreliableRequests > 0:
				unreliable = append(unreliable, label)
			default:
				cost += costUSD(*u, p)
			}
		}
		for model, u := range l.byModel {
			price(model, model, u)
		}
		for k, u := range l.side {
			label := k.model
			if label != unknownBucket {
				label += " (" + k.task + ")"
			}
			price(k.model, label, u)
		}
		var clauses []string
		for _, c := range []struct {
			label string
			names []string
		}{{"excludes unpriced: ", unpriced}, {"excludes unreliable usage: ", unreliable}} {
			if len(c.names) > 0 {
				slices.Sort(c.names)
				clauses = append(clauses, c.label+strings.Join(slices.Compact(c.names), ", "))
			}
		}
		line := fmt.Sprintf("est. cost: $%.4f", cost)
		if len(clauses) > 0 {
			line += " (" + strings.Join(clauses, "; ") + ")"
		}
		lines = append(lines, line)
	}

	tasks := make(map[string]*modelUsage)
	for k, u := range l.side {
		bucket(tasks, k.task).merge(*u)
	}
	for _, name := range slices.Sorted(maps.Keys(tasks)) {
		u := tasks[name]
		line := fmt.Sprintf("side task %s: input %d tok, output %d tok, %d calls", name, u.Prompt, u.Completion, u.Requests)
		if u.UnreliableRequests > 0 {
			line += fmt.Sprintf("; unreliable usage on %d calls", u.UnreliableRequests)
		}
		lines = append(lines, line)
	}
	return lines
}

// priceLookup keys a bundle's prices by ModelKey.String() ("provider/model"),
// the form RouteOutcome.ActualModel renders to. The values alias the
// bundle's read-only rate pointers and are never mutated.
func priceLookup(in map[provider.ModelKey]config.ModelPrice) map[string]*config.ModelPrice {
	out := make(map[string]*config.ModelPrice, len(in))
	for k, p := range in {
		out[k.String()] = &p
	}
	return out
}
