package main

import (
	"fmt"
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
