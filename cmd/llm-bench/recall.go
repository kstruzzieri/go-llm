package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// RecallEval marks a #864 Stage 0 trace: a recalled Golem agent-memory record
// whose files=differ label may change model behavior. It is metadata for the
// scorer and report; the frozen turns alone go to the candidate.
type RecallEval struct {
	FamilyID  string      `json:"family_id"`
	Template  string      `json:"template"`
	TaskStyle string      `json:"task_style"`
	Split     string      `json:"split"` // pilot | confirmatory
	Trace     string      `json:"trace"` // A | B
	Arm       string      `json:"arm"`   // 0 | 1 | 2 | P
	Cases     []string    `json:"cases"`
	Repeat    bool        `json:"repeat,omitempty"`
	Value     RecallValue `json:"value"`
	NamedFile string      `json:"named_file"`
	TaskPaths []string    `json:"task_paths,omitempty"`
}

// RecallValue is the recalled fact a case asks the model to rely on. Changed is
// S1's current value; S2's current value is Lesson.
type RecallValue struct {
	Type         string   `json:"type"` // number | identifier
	Identifier   string   `json:"identifier"`
	Lesson       string   `json:"lesson"`
	Changed      string   `json:"changed"`
	Alternatives []string `json:"alternatives,omitempty"`
}

// anyRecallTrace reports whether any trace carries a recall block.
func anyRecallTrace(traces []Trace) bool {
	for _, t := range traces {
		if t.Recall != nil {
			return true
		}
	}
	return false
}

// validateRecallTrace checks the recall block's closed vocabularies and the
// Trace/Arm/Cases combinations the spec allows, then the frozen turn shape.
func validateRecallTrace(t Trace) error {
	r := t.Recall
	if t.Injection != nil || t.AssemblyEval != nil || t.Benign != nil {
		return errors.New("recall trace carries injection, assembly_eval or benign")
	}
	if r.Split != "pilot" && r.Split != "confirmatory" {
		return fmt.Errorf("recall: split %q invalid (want pilot or confirmatory)", r.Split)
	}
	if r.Value.Type != "number" && r.Value.Type != "identifier" {
		return fmt.Errorf("recall: value.type %q invalid (want number or identifier)", r.Value.Type)
	}
	if strings.TrimSpace(r.Value.Lesson) == "" || strings.TrimSpace(r.Value.Identifier) == "" {
		return errors.New("recall: value.lesson and value.identifier must be non-empty")
	}
	if r.Value.Changed == r.Value.Lesson {
		return errors.New("recall: value.changed must differ from value.lesson")
	}
	switch r.Trace {
	case "A":
		if !slices.Contains([]string{"0", "1", "2"}, r.Arm) {
			return fmt.Errorf("recall: arm %q invalid on trace A (want 0, 1 or 2)", r.Arm)
		}
		okCases := slices.Equal(r.Cases, []string{"S1", "S2"}) || (slices.Equal(r.Cases, []string{"S7"}) && r.Arm == "2")
		if !okCases {
			return fmt.Errorf("recall: cases %v invalid on trace A arm %s", r.Cases, r.Arm)
		}
	case "B":
		s1, s2 := slices.Equal(r.Cases, []string{"S1"}), slices.Equal(r.Cases, []string{"S2"})
		if !s1 && !s2 {
			return fmt.Errorf("recall: cases %v invalid on trace B (want [S1] or [S2])", r.Cases)
		}
		if r.Arm != "1" && r.Arm != "2" && (!s2 || r.Arm != "P") {
			return fmt.Errorf("recall: arm %q invalid on trace B cases %v", r.Arm, r.Cases)
		}
	default:
		return fmt.Errorf("recall: trace %q invalid (want A or B)", r.Trace)
	}
	if r.Repeat && (r.Trace != "B" || r.Arm != "1" || !slices.Equal(r.Cases, []string{"S2"})) {
		return errors.New("recall: repeat is only allowed on trace B, arm 1, cases [S2]")
	}
	return validateToolEndingTurns(t.Turns)
}
