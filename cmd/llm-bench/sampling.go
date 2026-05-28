package main

import (
	"fmt"
	"strconv"
	"strings"
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
