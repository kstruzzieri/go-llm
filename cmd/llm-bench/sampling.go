package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
)

// captureSampleSpec is the parsed -capture-sample flag. Reserved
// dimensions (token-length, turn-count, has-tool-calls,
// has-final-answer-criteria, source, recency) match spec §4.4.
type captureSampleSpec struct {
	N                      int
	Stratify               []string
	Recency                string
	HasToolCalls           *bool
	HasFinalAnswerCriteria *bool
	Source                 string
}

var allowedStratifyDimensions = map[string]struct{}{
	"token-length":              {},
	"turn-count":                {},
	"has-tool-calls":            {},
	"has-final-answer-criteria": {},
	"source":                    {},
	"recency":                   {},
}

func parseCaptureSample(spec string) (captureSampleSpec, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return captureSampleSpec{}, fmt.Errorf("empty -capture-sample spec")
	}
	var out captureSampleSpec
	for _, part := range strings.Split(spec, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return captureSampleSpec{}, fmt.Errorf("malformed token %q (expected key=value)", part)
		}
		key, val := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		switch key {
		case "n":
			n, err := strconv.Atoi(val)
			if err != nil || n <= 0 {
				return captureSampleSpec{}, fmt.Errorf("n=%q (want positive integer)", val)
			}
			out.N = n
		case "stratify":
			dims := strings.Split(val, ":")
			for _, d := range dims {
				d = strings.TrimSpace(d)
				if _, ok := allowedStratifyDimensions[d]; !ok {
					return captureSampleSpec{}, fmt.Errorf("unknown stratify dimension %s", d)
				}
				out.Stratify = append(out.Stratify, d)
			}
		case "recency":
			switch val {
			case "last-7d", "last-30d", "older":
				out.Recency = val
			default:
				return captureSampleSpec{}, fmt.Errorf("recency=%q (want last-7d|last-30d|older)", val)
			}
		case "has-tool-calls":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return captureSampleSpec{}, fmt.Errorf("has-tool-calls=%q (want true|false)", val)
			}
			out.HasToolCalls = &b
		case "has-final-answer-criteria":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return captureSampleSpec{}, fmt.Errorf("has-final-answer-criteria=%q (want true|false)", val)
			}
			out.HasFinalAnswerCriteria = &b
		case "source":
			out.Source = val
		default:
			return captureSampleSpec{}, fmt.Errorf("unknown key %s", key)
		}
	}
	if out.N == 0 {
		return captureSampleSpec{}, fmt.Errorf("missing required key n=")
	}
	return out, nil
}

// tokenLengthBucket maps total content rune-count to size buckets.
// The conversion factor (~4 runes/token) mirrors the rough estimate
// used elsewhere in the harness; it overestimates for code-heavy
// content, which is fine for stratification.
// Boundaries: small ≤ 4 000 runes (~1 k tokens), medium ≤ 32 000 (~8 k tokens).
func tokenLengthBucket(runes int) string {
	switch {
	case runes <= 4000:
		return "small"
	case runes <= 32000:
		return "medium"
	default:
		return "large"
	}
}

func turnCountBucket(n int) string {
	switch {
	case n <= 2:
		return "short"
	case n <= 10:
		return "medium"
	default:
		return "long"
	}
}

func recencyBucket(capturedAt, now time.Time) string {
	delta := now.Sub(capturedAt)
	switch {
	case delta <= 7*24*time.Hour:
		return "last-7d"
	case delta <= 30*24*time.Hour:
		return "last-30d"
	default:
		return "older"
	}
}

func stratumKey(t Trace, dims []string, now time.Time) string {
	if len(dims) == 0 {
		return ""
	}
	parts := make([]string, 0, len(dims))
	for _, dim := range dims {
		parts = append(parts, dim+"="+stratumValue(t, dim, now))
	}
	return strings.Join(parts, "|")
}

func stratumValue(t Trace, dim string, now time.Time) string {
	switch dim {
	case "token-length":
		var n int
		for _, turn := range t.Turns {
			n += len([]rune(turn.Content))
		}
		return tokenLengthBucket(n)
	case "turn-count":
		return turnCountBucket(len(t.Turns))
	case "has-tool-calls":
		return strconv.FormatBool(traceHasToolCalls(t))
	case "has-final-answer-criteria":
		return strconv.FormatBool(strings.TrimSpace(t.Golden.FinalAnswerCriteria) != "")
	case "source":
		return t.Source
	case "recency":
		return recencyBucket(t.CapturedAt, now)
	default:
		return "unknown"
	}
}

func traceHasToolCalls(t Trace) bool {
	for _, turn := range t.Turns {
		if turn.Role == "assistant" && len(turn.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// sampleManifest records what stratifiedSample selected and why.
// It is emitted as _sample-manifest.json alongside the sampled trace set.
type sampleManifest struct {
	Seed       int64          `json:"seed"`
	Spec       string         `json:"spec"`
	CellCounts map[string]int `json:"cell_counts"`
	SampledIDs []string       `json:"sampled_ids"`
}

// stratifiedSample applies hard filters, groups into cells by the spec's
// stratify dimensions, allocates slots with floor+remainder logic, then
// redistributes any slack from short cells to non-empty cells.
func stratifiedSample(traces []Trace, spec captureSampleSpec, seed int64, now time.Time) ([]Trace, sampleManifest, error) {
	filtered := applyCaptureFilters(traces, spec, now)
	if len(filtered) == 0 {
		return nil, sampleManifest{Seed: seed, CellCounts: map[string]int{}, SampledIDs: []string{}}, nil
	}

	rng := rand.New(rand.NewSource(seed))
	cells := groupByStratum(filtered, spec.Stratify, now)
	keys := sortedKeys(cells)

	// First pass: balanced floor+remainder allocation capped by cell size.
	// When target < len(keys), only target cells receive one slot; the
	// first pass must never oversample beyond target.
	target := spec.N
	if target > len(filtered) {
		target = len(filtered)
	}
	if len(keys) == 0 {
		// No stratify dims: degenerate one-cell case.
		// NOTE: this branch is dead when spec.Stratify is empty because
		// groupByStratum with empty dims always produces {"": traces},
		// giving len(keys)==1. The guard is kept for safety.
		return uniformPick(filtered, target, rng, spec, seed, now)
	}

	baseAlloc := target / len(keys)
	remainder := target % len(keys)

	picked := make([]Trace, 0, target)
	remaining := make(map[string][]Trace, len(cells))
	cellCounts := make(map[string]int, len(cells))
	for i, k := range keys {
		c := cells[k]
		shuffle(c, rng)
		n := baseAlloc
		if i < remainder {
			n++
		}
		if n > len(c) {
			n = len(c)
		}
		picked = append(picked, c[:n]...)
		remaining[k] = c[n:]
		cellCounts[k] = n
	}

	// Slack redistribution: fill until we hit target or run out of candidates.
	for len(picked) < target {
		progressed := false
		for _, k := range keys {
			if len(picked) >= target {
				break
			}
			if len(remaining[k]) == 0 {
				continue
			}
			picked = append(picked, remaining[k][0])
			remaining[k] = remaining[k][1:]
			cellCounts[k]++
			progressed = true
		}
		if !progressed {
			break
		}
	}

	ids := make([]string, 0, len(picked))
	for _, p := range picked {
		ids = append(ids, p.ID)
	}
	return picked, sampleManifest{
		Seed:       seed,
		Spec:       captureSpecString(spec),
		CellCounts: cellCounts,
		SampledIDs: ids,
	}, nil
}

// uniformPick is the degenerate path when no stratify dimensions are set
// (or len(keys)==0 in some future caller). It shuffles and picks the
// first n traces.
func uniformPick(traces []Trace, n int, rng *rand.Rand, spec captureSampleSpec, seed int64, _ time.Time) ([]Trace, sampleManifest, error) {
	tmp := append([]Trace(nil), traces...)
	shuffle(tmp, rng)
	if n > len(tmp) {
		n = len(tmp)
	}
	out := tmp[:n]
	ids := make([]string, 0, n)
	for _, t := range out {
		ids = append(ids, t.ID)
	}
	return out, sampleManifest{
		Seed:       seed,
		Spec:       captureSpecString(spec),
		CellCounts: map[string]int{"all": n},
		SampledIDs: ids,
	}, nil
}

func shuffle(traces []Trace, rng *rand.Rand) {
	rng.Shuffle(len(traces), func(i, j int) { traces[i], traces[j] = traces[j], traces[i] })
}

func sortedKeys(m map[string][]Trace) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func groupByStratum(traces []Trace, dims []string, now time.Time) map[string][]Trace {
	cells := make(map[string][]Trace)
	for _, t := range traces {
		key := stratumKey(t, dims, now)
		cells[key] = append(cells[key], t)
	}
	return cells
}

func applyCaptureFilters(traces []Trace, spec captureSampleSpec, now time.Time) []Trace {
	var out []Trace
	for _, t := range traces {
		if spec.Recency != "" && recencyBucket(t.CapturedAt, now) != spec.Recency {
			continue
		}
		if spec.HasToolCalls != nil && traceHasToolCalls(t) != *spec.HasToolCalls {
			continue
		}
		if spec.HasFinalAnswerCriteria != nil {
			has := strings.TrimSpace(t.Golden.FinalAnswerCriteria) != ""
			if has != *spec.HasFinalAnswerCriteria {
				continue
			}
		}
		if spec.Source != "" && t.Source != spec.Source {
			continue
		}
		out = append(out, t)
	}
	return out
}

func captureSpecString(spec captureSampleSpec) string {
	parts := []string{fmt.Sprintf("n=%d", spec.N)}
	if len(spec.Stratify) > 0 {
		parts = append(parts, "stratify="+strings.Join(spec.Stratify, ":"))
	}
	if spec.Recency != "" {
		parts = append(parts, "recency="+spec.Recency)
	}
	if spec.HasToolCalls != nil {
		parts = append(parts, fmt.Sprintf("has-tool-calls=%v", *spec.HasToolCalls))
	}
	if spec.HasFinalAnswerCriteria != nil {
		parts = append(parts, fmt.Sprintf("has-final-answer-criteria=%v", *spec.HasFinalAnswerCriteria))
	}
	if spec.Source != "" {
		parts = append(parts, "source="+spec.Source)
	}
	return strings.Join(parts, ",")
}
