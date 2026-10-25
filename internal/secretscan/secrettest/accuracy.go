package secrettest

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
	"github.com/kstruzzieri/go-llm/provider"
)

// The hooks of interceptor.Secrets, as Outcome.Hook and Confusion.Hook name them.
const (
	hookInput    = "input"
	hookOutput   = "output"
	hookToolCall = "tool_call"
)

// hookTarget is the target kind each hook stamps on its findings. inspect
// checks it, so a finding cannot be attributed to a hook that did not raise it.
var hookTarget = map[string]agent.TargetKind{
	hookInput:    agent.TargetMessage,
	hookOutput:   agent.TargetOutputToolCall,
	hookToolCall: agent.TargetToolCall,
}

// argsField is the JSON field that carries a tool-args value. Its name must
// not be a secret assignment key (token, secret, password, ...), or the
// interceptor's key-aware check would fire on any high-entropy value.
const argsField = "content"

// Outcome is what interceptor.Secrets reported for one case on one carrier at
// one hook. An observation case yields one Outcome (hook "input"); a tool-args
// case yields two ("output" then "tool_call"). Class, Kind and CoverageGap
// echo the case so a result needs no table lookup; Score is derived from them.
type Outcome struct {
	CaseID      string
	Class       Class
	Kind        string // the case's stratum, or empty
	Carrier     Carrier
	Hook        string // "input", "output" or "tool_call"
	CoverageGap bool
	Any         bool     // any sensitive finding fired
	Kinds       []string // kinds the findings named, in discovery order
}

// Score places the outcome in the confusion matrix (see Case.Score): "TP",
// "FN", "FP", "TN", or "" when the outcome is outside the matrix.
func (o Outcome) Score() string {
	return Case{Class: o.Class, CoverageGap: o.CoverageGap}.Score(o.Any)
}

// Evaluate renders each case and inspects it with interceptor.Secrets on every
// carrier the case declares, in the order observation then tool-args:
//   - observation: one tool message from read_file under the workspace origin,
//     through InspectInput;
//   - tool-args: the value as a JSON string field of one tool call, through
//     InspectOutput and through InspectToolCall.
//
// Outcomes follow case order, then carrier, then hook. Whether a finding fired
// is judged as "any finding"; Kinds records the kinds as observed, which can
// differ from the case's Kind where Scan merges overlapping spans by
// precedence.
func Evaluate(ctx context.Context, cases []Case) ([]Outcome, error) {
	var outs []Outcome
	for _, c := range cases {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value, err := c.Render()
		if err != nil {
			return nil, err
		}
		for _, carrier := range []Carrier{Observation, ToolArgs} {
			if !slices.Contains(c.Carriers, carrier) {
				continue
			}
			hooks := []string{hookInput}
			if carrier == ToolArgs {
				hooks = []string{hookOutput, hookToolCall}
			}
			for _, hook := range hooks {
				kinds, err := inspect(ctx, hook, value)
				if err != nil {
					return nil, fmt.Errorf("secrettest: case %q: %s: %w", c.ID, hook, err)
				}
				outs = append(outs, Outcome{
					CaseID: c.ID, Class: c.Class, Kind: c.Kind, Carrier: carrier, Hook: hook,
					CoverageGap: c.CoverageGap, Any: len(kinds) > 0, Kinds: kinds,
				})
			}
		}
	}
	return outs, nil
}

// inspect runs one hook of the Secrets interceptor over value and returns the
// finding kinds in discovery order. The interceptor reports a kind once per
// target, and each hook here has one text, so no kind repeats.
func inspect(ctx context.Context, hook, value string) ([]string, error) {
	var (
		s        interceptor.Secrets
		findings []agent.Finding
		err      error
	)
	if hook == hookInput {
		findings, err = s.InspectInput(ctx, agent.InputInspection{Messages: []agent.InspectedMessage{{
			Role: "tool", Origin: agent.OriginWorkspace, ToolName: "read_file", ToolCallID: "c1", Content: value,
		}}})
	} else {
		var args json.RawMessage
		if args, err = toolArgs(value); err != nil {
			return nil, err
		}
		call := provider.ToolCall{ID: "c1", Type: "function", Function: provider.ToolCallFunction{Name: "write_file", Arguments: args}}
		if hook == hookOutput {
			findings, err = s.InspectOutput(ctx, agent.OutputInspection{ToolCalls: []provider.ToolCall{call}})
		} else {
			findings, err = s.InspectToolCall(ctx, agent.ToolCallInspection{Call: call})
		}
	}
	if err != nil {
		return nil, err
	}
	var kinds []string
	for _, f := range findings {
		kind, ok := strings.CutPrefix(f.Rule, "sensitive_")
		if !ok {
			return nil, fmt.Errorf("unexpected finding rule %q", f.Rule)
		}
		if f.Target != hookTarget[hook] {
			return nil, fmt.Errorf("finding %q has target %v, not the hook's %v", f.Rule, f.Target, hookTarget[hook])
		}
		kinds = append(kinds, kind)
	}
	return kinds, nil
}

// toolArgs encodes value as {"content": value}. HTML escaping is off, so the
// only rewriting is what JSON requires; escaped forms are table cases.
func toolArgs(value string) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]string{argsField: value}); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Confusion counts the scored outcomes of one kind on one carrier at one hook.
// Kind is the case stratum; empty holds the cases that probe no one kind.
type Confusion struct {
	Kind           string
	Carrier        Carrier
	Hook           string
	TP, FN, FP, TN int
}

// Confusions tallies the outcomes Outcome.Score places in the matrix:
// expected-sensitive and benign cases, minus coverage gaps. Lookalikes,
// fixtures (see Tables) and unclassified cases are excluded. Rows are sorted by
// kind, carrier, then hook, and a row exists only if it counts an outcome.
func Confusions(outs []Outcome) []Confusion {
	var rows []Confusion
	for _, o := range outs {
		cell := o.Score()
		if cell == "" {
			continue
		}
		i := slices.IndexFunc(rows, func(r Confusion) bool {
			return r.Kind == o.Kind && r.Carrier == o.Carrier && r.Hook == o.Hook
		})
		if i < 0 {
			rows = append(rows, Confusion{Kind: o.Kind, Carrier: o.Carrier, Hook: o.Hook})
			i = len(rows) - 1
		}
		switch cell {
		case "TP":
			rows[i].TP++
		case "FN":
			rows[i].FN++
		case "FP":
			rows[i].FP++
		case "TN":
			rows[i].TN++
		}
	}
	slices.SortFunc(rows, func(a, b Confusion) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(string(a.Carrier), string(b.Carrier)), strings.Compare(a.Hook, b.Hook))
	})
	return rows
}

// Tables splits out the outcomes tabulated apart from the confusion matrix:
// benign lookalikes and synthetic fixtures, each in input order and including
// coverage-gap rows.
func Tables(outs []Outcome) (lookalike, fixture []Outcome) {
	for _, o := range outs {
		switch o.Class {
		case BenignLookalike:
			lookalike = append(lookalike, o)
		case SyntheticFixture:
			fixture = append(fixture, o)
		}
	}
	return lookalike, fixture
}
