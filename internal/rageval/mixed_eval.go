package rageval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/kstruzzieri/go-llm/agent"
)

// MixedSchemaVersion identifies the mixed-assembly experiment JSON schema.
const MixedSchemaVersion = "mixed-assembly-eval/v1"

// mixedSweepFractions is the registered budget-fraction sweep: the 0.6 set
// around llm-bench's registered mixedBudgetFraction (0.6), one rung below and
// one above. Pinned by literal in TestMixedEvalSchemaAndShape.
var mixedSweepFractions = []float64{0.4, 0.6, 0.8}

// MixedOptions is the (currently knob-free) options struct for the mixed
// experiment: the fixture, sweep and budget formula are all registered
// constants, so there is deliberately nothing to configure. It exists so the
// runner call site has the same shape as the other experiments.
type MixedOptions struct{}

// MixedReport is the deterministic mixed-assembly sweep report: five
// hand-built states x three budget fractions, both assembly arms each.
//
// It records each arm's SHAPE independently and carries NO field that diffs
// decisions or causes across arms: agent.Pressure documents Cause as not
// comparable between the legacy and mixed arms, and
// TestMixedEvalNoCrossArmDiffKeys pins the key set.
type MixedReport struct {
	SchemaVersion string            `json:"schema_version"`
	Cases         []MixedCaseReport `json:"cases"`
	Summary       MixedSummary      `json:"summary"`
}

// MixedCaseReport is one fixture case's full fraction sweep.
type MixedCaseReport struct {
	Name      string             `json:"name"`
	Stratum   string             `json:"stratum"`
	RawTokens int                `json:"raw_tokens"`
	Fractions []MixedFractionRow `json:"fractions"`
}

// MixedFractionRow is one (case, fraction) cell: the resolved budget and both
// arms' assembly shapes at that budget.
type MixedFractionRow struct {
	Fraction float64            `json:"fraction"`
	Budget   int                `json:"budget"`
	Legacy   MixedArmStats      `json:"legacy"`
	Mixed    MixedArmTraceStats `json:"mixed"`
}

// MixedArmStats is one arm's assembled shape. ContextTokens is the registered
// estimator est=(len+3)/4 over the assembled System plus every message
// Content — the same arm-independent basis as RawTokens, NOT agent's internal
// messageCost — so raw and assembled sizes compare on one scale.
type MixedArmStats struct {
	ContextTokens int    `json:"context_tokens"`
	ContextBytes  int    `json:"context_bytes"`
	Messages      int    `json:"messages"`
	ShedMessages  int    `json:"shed_messages"`
	ShedBytes     int    `json:"shed_bytes"`
	PressureLevel string `json:"pressure_level"`
}

// MixedArmTraceStats is the mixed arm's shape plus its trace-only fields. For
// a no-anchor case the mixed arm takes the legacy path and returns the zero
// trace, so OmittedSubjects/AnchorOmissions are 0 and the histogram is empty.
type MixedArmTraceStats struct {
	MixedArmStats
	OmittedSubjects int `json:"omitted_subjects"`
	AnchorOmissions int `json:"anchor_omissions"`
	// DecisionHistogram counts trace subject rows by Decision, INCLUDING the
	// "omitted" bucket (agent.DecisionOmitted). Always non-nil so it marshals
	// {} rather than null; map keys marshal sorted, keeping the report
	// byte-deterministic.
	DecisionHistogram map[string]int `json:"decision_histogram"`
}

// MixedSummary aggregates the sweep.
type MixedSummary struct {
	Cases     int `json:"cases"`
	Fractions int `json:"fractions"`
	// AllDeterministic records that the whole sweep ran twice inside
	// RunMixedExperiment and the two marshaled reports were byte-identical.
	// It is always true on a report that was returned at all: a mismatch is a
	// hard error, not a flag.
	AllDeterministic bool `json:"all_deterministic"`
}

// RunMixedExperiment builds the five fixture states and sweeps both assembly
// arms over the registered budget fractions. The whole sweep runs TWICE and
// the marshaled reports are byte-compared; any difference fails closed with
// an error rather than shipping a report whose determinism claim is a lie.
func RunMixedExperiment(ctx context.Context, _ MixedOptions) (*MixedReport, error) {
	first, err := runMixedSweep(ctx)
	if err != nil {
		return nil, err
	}
	second, err := runMixedSweep(ctx)
	if err != nil {
		return nil, err
	}
	if err := mixedReportsIdentical(first, second); err != nil {
		return nil, err
	}
	first.Summary.AllDeterministic = true
	return first, nil
}

// mixedReportsIdentical byte-compares two reports on their canonical JSON
// encoding — the exact representation the committed baseline stores.
func mixedReportsIdentical(a, b *MixedReport) error {
	rawA, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("rag eval: marshal mixed report: %w", err)
	}
	rawB, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("rag eval: marshal mixed report: %w", err)
	}
	if !bytes.Equal(rawA, rawB) {
		return errors.New("rag eval: mixed experiment produced differing reports across identical runs")
	}
	return nil
}

// runMixedSweep is one full pass: fresh fixture states, every fraction, both
// arms.
func runMixedSweep(ctx context.Context) (*MixedReport, error) {
	cases, err := buildMixedEvalStates(ctx)
	if err != nil {
		return nil, err
	}
	report := &MixedReport{SchemaVersion: MixedSchemaVersion}
	for _, c := range cases {
		cr := MixedCaseReport{Name: c.Name, Stratum: c.Stratum, RawTokens: c.RawTokens}
		for _, fraction := range mixedSweepFractions {
			row, err := runMixedFraction(ctx, c, fraction)
			if err != nil {
				return nil, fmt.Errorf("rag eval: mixed case %s f=%v: %w", c.Name, fraction, err)
			}
			cr.Fractions = append(cr.Fractions, row)
		}
		report.Cases = append(report.Cases, cr)
	}
	report.Summary = MixedSummary{Cases: len(report.Cases), Fractions: len(mixedSweepFractions)}
	return report, nil
}

// mixedEvalBudget resolves one cell's input budget:
// max(minViable, round(fraction*RawTokens)) with
// minViable = est(System) + est(final pinned goal) + slack, restating the
// registered formula in cmd/llm-bench/assembly_mixed_fixture.go. The floor
// guarantees the pinned segment always fits, so assembly never exhausts.
func mixedEvalBudget(c mixedEvalCase, fraction float64) int {
	goal := c.State.Messages[len(c.State.Messages)-1] // builder-validated pinned user goal
	minViable := mixedEvalEst(c.State.System) + mixedEvalEst(goal.Content) + mixedEvalMinViableSlack
	budget := int(math.Round(fraction * float64(c.RawTokens)))
	if budget < minViable {
		budget = minViable
	}
	return budget
}

// runMixedFraction assembles one case under one budget with BOTH arms.
// toolSchemaTokens is 0: the experiment measures state assembly, and a
// nonzero schema cost would shift both arms identically.
func runMixedFraction(ctx context.Context, c mixedEvalCase, fraction float64) (MixedFractionRow, error) {
	budget := mixedEvalBudget(c, fraction)
	tb := agent.TokenBudget{Input: budget}

	legacyOut, legacyPressure, err := agent.ContextManager{}.Assemble(ctx, c.State, 0, tb)
	if err != nil {
		return MixedFractionRow{}, fmt.Errorf("legacy arm: %w", err)
	}
	mixedOut, mixedPressure, trace, err := agent.ContextManager{Mixed: true}.AssembleWithTrace(ctx, c.State, 0, tb)
	if err != nil {
		return MixedFractionRow{}, fmt.Errorf("mixed arm: %w", err)
	}

	hist := make(map[string]int, 4)
	for _, row := range trace.Subjects {
		hist[row.Decision]++
	}
	return MixedFractionRow{
		Fraction: fraction,
		Budget:   budget,
		Legacy:   mixedArmStats(c.State, legacyOut, legacyPressure),
		Mixed: MixedArmTraceStats{
			MixedArmStats:     mixedArmStats(c.State, mixedOut, mixedPressure),
			OmittedSubjects:   trace.OmittedSubjects,
			AnchorOmissions:   mixedPressure.AnchorOmissions,
			DecisionHistogram: hist,
		},
	}, nil
}

// mixedArmStats measures one assembled arm against its input state.
func mixedArmStats(in, out agent.State, p agent.Pressure) MixedArmStats {
	return MixedArmStats{
		ContextTokens: mixedEvalStateTokens(out),
		ContextBytes:  mixedEvalStateBytes(out),
		Messages:      len(out.Messages),
		ShedMessages:  len(in.Messages) - len(out.Messages),
		ShedBytes:     mixedEvalStateBytes(in) - mixedEvalStateBytes(out),
		PressureLevel: p.Level.String(),
	}
}

// mixedEvalStateTokens is est over the assembled System + message Contents —
// no envelopes, deliberately: assembled size is a content measure, while
// RawTokens' envelope charge prices the registered budget formula.
func mixedEvalStateTokens(st agent.State) int {
	n := mixedEvalEst(st.System)
	for _, m := range st.Messages {
		n += mixedEvalEst(m.Content)
	}
	return n
}

func mixedEvalStateBytes(st agent.State) int {
	n := len(st.System)
	for _, m := range st.Messages {
		n += len(m.Content)
	}
	return n
}

// WriteMixedReport writes a stable, pretty JSON mixed experiment report.
func WriteMixedReport(path string, report *MixedReport) error {
	return writeJSONReport(path, report)
}
