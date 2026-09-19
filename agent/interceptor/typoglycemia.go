package interceptor

import (
	"context"

	"github.com/kstruzzieri/go-llm/agent"
)

// Typoglycemia matches instruction phrases whose words keep their first and
// last letters but scramble the interior ("ingore pervious isntructions"), a
// classic filter evasion that models still read. Exact strong matches report
// "instruction_phrase"; weak indicators report "weak_phrase" and only tag; a
// strong phrase anywhere dominates a weak one anywhere.
type Typoglycemia struct {
	Phrases Phrases
}

var _ agent.Interceptor = Typoglycemia{}

// Name returns "typoglycemia".
func (Typoglycemia) Name() string { return "typoglycemia" }

func (t Typoglycemia) scan(tg target, text string) (agent.Finding, bool) {
	p, exact, isStrong, ok := matchPhrases(text, t.Phrases.strong(), t.Phrases.weak())
	if !ok {
		return agent.Finding{}, false
	}
	return phraseFinding(tg, p, exact, isStrong), true
}

// InspectInput checks the system prompt, summary, every message and every
// alternative.
func (t Typoglycemia) InspectInput(_ context.Context, in agent.InputInspection) ([]agent.Finding, error) {
	var out []agent.Finding
	eachText(in, func(tg target, text string) {
		if f, ok := t.scan(tg, text); ok {
			out = append(out, f)
		}
	})
	return out, nil
}

// InspectOutput returns nothing: model output policy belongs to #438/#437.
func (Typoglycemia) InspectOutput(context.Context, agent.OutputInspection) ([]agent.Finding, error) {
	return nil, nil
}

// InspectToolCall checks both the raw JSON arguments and their decoded object
// keys and string values.
func (t Typoglycemia) InspectToolCall(_ context.Context, call agent.ToolCallInspection) ([]agent.Finding, error) {
	tg := toolCallTarget(call.Call.ID)
	var best agent.Finding
	found, bestStrong := false, false
	for _, text := range toolCallTexts(call.Call.Function.Arguments) {
		f, ok := t.scan(tg, text)
		if !ok {
			continue
		}
		strong := f.Rule != "weak_phrase"
		if !found || strong && !bestStrong {
			best, found, bestStrong = f, true, strong
		}
	}
	if found {
		return []agent.Finding{best}, nil
	}
	return nil, nil
}
