package interceptor

import (
	"context"
	"fmt"
	"strconv"

	"github.com/kstruzzieri/go-llm/agent"
)

// ZeroWidth flags invisible code points that hide text from a human reader
// while a model still tokenizes it, raw or as JSON \uXXXX escape text.
type ZeroWidth struct{}

var _ agent.Interceptor = ZeroWidth{}

// Name returns "zero_width".
func (ZeroWidth) Name() string { return "zero_width" }

func isZeroWidth(r rune) bool {
	return (r >= 0x200B && r <= 0x200F) || (r >= 0x2060 && r <= 0x2064) || r == 0xFEFF || r == 0x180E
}

// scanZeroWidth counts raw zero-width runes; when there are none it counts
// literal \uXXXX escapes of them (case-insensitive hex).
func scanZeroWidth(s string) (first rune, count int, escaped bool) {
	for _, r := range s {
		if isZeroWidth(r) {
			if count == 0 {
				first = r
			}
			count++
		}
	}
	if count > 0 {
		return first, count, false
	}
	for i := 0; i+6 <= len(s); i++ {
		if s[i] != '\\' || s[i+1] != 'u' {
			continue
		}
		v, err := strconv.ParseUint(s[i+2:i+6], 16, 32)
		if err != nil || !isZeroWidth(rune(v)) {
			continue
		}
		if count == 0 {
			first = rune(v)
		}
		count++
	}
	return first, count, count > 0
}

func zeroWidthDetail(first rune, n int, escaped bool) string {
	d := fmt.Sprintf("%d zero-width code point(s), first U+%04X", n, first)
	if escaped {
		d += " (escaped)"
	}
	return d
}

func zeroWidthFinding(t target, rule string, first rune, n int, escaped bool) agent.Finding {
	return t.finding(rule, verdictFor(t.origin), ZeroWidthRisk, zeroWidthDetail(first, n, escaped))
}

// InspectInput checks the system prompt, summary, every message and every
// alternative.
func (ZeroWidth) InspectInput(_ context.Context, in agent.InputInspection) ([]agent.Finding, error) {
	var out []agent.Finding
	eachText(in, func(t target, text string) {
		if first, n, escaped := scanZeroWidth(text); n > 0 {
			out = append(out, zeroWidthFinding(t, "zero_width", first, n, escaped))
		}
	})
	return out, nil
}

// InspectOutput returns nothing: model output policy belongs to #438/#437.
func (ZeroWidth) InspectOutput(context.Context, agent.OutputInspection) ([]agent.Finding, error) {
	return nil, nil
}

// InspectToolCall checks the call's raw JSON arguments, escapes included.
func (ZeroWidth) InspectToolCall(_ context.Context, call agent.ToolCallInspection) ([]agent.Finding, error) {
	if first, n, escaped := scanZeroWidth(string(call.Call.Function.Arguments)); n > 0 {
		return []agent.Finding{zeroWidthFinding(toolCallTarget(call.Call.ID), "zero_width", first, n, escaped)}, nil
	}
	return nil, nil
}
