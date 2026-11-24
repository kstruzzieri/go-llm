package main

import (
	"errors"
	"fmt"
	"path"
	"regexp"
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

// currentValue is the value the scripted file read shows in Trace B.
func (r RecallEval) currentValue() string {
	if len(r.Cases) > 0 && r.Cases[0] == "S1" {
		return r.Value.Changed
	}
	return r.Value.Lesson
}

// allRecallTraces reports whether every trace carries a recall block.
func allRecallTraces(traces []Trace) bool {
	for _, t := range traces {
		if t.Recall == nil {
			return false
		}
	}
	return true
}

// frozenWireTrace reports whether t replays a frozen decision-point wire
// (injection or recall): registered decoding, raw tool-call channel, one turn.
func frozenWireTrace(t Trace) bool { return t.Injection != nil || t.Recall != nil }

// anyFrozenWireTrace reports whether any trace is a frozen-wire trace.
func anyFrozenWireTrace(traces []Trace) bool {
	for _, t := range traces {
		if frozenWireTrace(t) {
			return true
		}
	}
	return false
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

// recallNumberPattern is the canonical number token (no leading zeros, no
// trailing fractional zeros), so string equality equals numeric equality.
var recallNumberPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]*[1-9])?$`)

// recallWorkspaceFile returns the path relative to the fixture's /workspace
// display root. Empty, escaping and unrelated absolute paths identify no file.
func recallWorkspaceFile(p string) string {
	p = path.Clean(p)
	if path.IsAbs(p) {
		var ok bool
		p, ok = strings.CutPrefix(p, "/workspace/")
		if !ok {
			return ""
		}
	}
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return ""
	}
	return p
}

// validateRecallTrace checks the recall block's closed vocabularies and the
// Trace/Arm/Cases/Split combinations the spec allows, then the frozen turn
// shape. Every error starts with "recall: ".
func validateRecallTrace(t Trace) error {
	r := t.Recall
	if t.Injection != nil || t.AssemblyEval != nil || t.Benign != nil {
		return errors.New("recall: trace carries injection, assembly_eval or benign beside recall")
	}
	if r.Split != "pilot" && r.Split != "confirmatory" {
		return fmt.Errorf("recall: split %q invalid (want pilot or confirmatory)", r.Split)
	}
	if r.TaskStyle != "direct" && r.TaskStyle != "task" {
		return fmt.Errorf("recall: task_style %q invalid (want direct or task)", r.TaskStyle)
	}
	for _, f := range []struct{ name, v string }{{"family_id", r.FamilyID}, {"template", r.Template}, {"named_file", r.NamedFile}} {
		if strings.TrimSpace(f.v) == "" || f.v != strings.TrimSpace(f.v) {
			return fmt.Errorf("recall: %s must be non-empty without surrounding whitespace", f.name)
		}
	}
	if r.Value.Type != "number" && r.Value.Type != "identifier" {
		return fmt.Errorf("recall: value.type %q invalid (want number or identifier)", r.Value.Type)
	}
	if recallWorkspaceFile(r.NamedFile) == "" {
		return fmt.Errorf("recall: named_file %q must name a file in the fixture workspace", r.NamedFile)
	}
	for _, p := range r.TaskPaths {
		if p != strings.TrimSpace(p) || recallWorkspaceFile(p) == "" {
			return fmt.Errorf("recall: task_paths entry %q must name a file in the fixture workspace", p)
		}
	}
	for _, a := range r.Value.Alternatives {
		if strings.TrimSpace(a) == "" || a != strings.TrimSpace(a) {
			return fmt.Errorf("recall: value.alternatives entry %q must be non-empty without surrounding whitespace", a)
		}
	}
	vals := []struct{ name, v string }{{"identifier", r.Value.Identifier}, {"lesson", r.Value.Lesson}, {"changed", r.Value.Changed}}
	for _, f := range vals {
		if strings.TrimSpace(f.v) == "" {
			return fmt.Errorf("recall: value.%s must be non-empty", f.name)
		}
		if f.v != strings.TrimSpace(f.v) {
			return fmt.Errorf("recall: value.%s %q has leading or trailing whitespace", f.name, f.v)
		}
	}
	if r.Value.Type == "number" {
		for _, f := range vals[1:] {
			if !recallNumberPattern.MatchString(f.v) {
				return fmt.Errorf("recall: value.%s %q is not a canonical number token", f.name, f.v)
			}
		}
	}
	if r.Value.Type == "number" {
		for _, f := range vals[1:] {
			// Values 0 and 1 are rejected (and no other range is enforced): the
			// scorer drops the pronoun "one" from its number words, so it cannot
			// recognize 1, and 0 is excluded with it.
			if f.v == "0" || f.v == "1" {
				return fmt.Errorf("recall: value.%s %q must not be 0 or 1", f.name, f.v)
			}
		}
	}
	if r.Value.Changed == r.Value.Lesson {
		return errors.New("recall: value.changed must differ from value.lesson")
	}
	s1, s2 := slices.Equal(r.Cases, []string{"S1"}), slices.Equal(r.Cases, []string{"S2"})
	switch r.Trace {
	case "A":
		if !slices.Contains([]string{"0", "1", "2"}, r.Arm) {
			return fmt.Errorf("recall: arm %q invalid on trace A (want 0, 1 or 2)", r.Arm)
		}
		if !slices.Equal(r.Cases, []string{"S1", "S2"}) && (!slices.Equal(r.Cases, []string{"S7"}) || r.Arm != "2") {
			return fmt.Errorf("recall: cases %v invalid on trace A arm %s", r.Cases, r.Arm)
		}
	case "B":
		if !s1 && !s2 {
			return fmt.Errorf("recall: cases %v invalid on trace B (want [S1] or [S2])", r.Cases)
		}
		if r.Arm != "1" && r.Arm != "2" && (!s2 || r.Arm != "P") {
			return fmt.Errorf("recall: arm %q invalid on trace B cases %v", r.Arm, r.Cases)
		}
	default:
		return fmt.Errorf("recall: trace %q invalid (want A or B)", r.Trace)
	}
	if r.Repeat && (r.Trace != "B" || r.Arm != "1" || !s2) {
		return errors.New("recall: repeat is only allowed on trace B, arm 1, cases [S2]")
	}
	if r.Split == "pilot" {
		// Pilot cells: Trace A arms 0/1 (S1,S2), Trace B S1 arm 1, Trace B S2 arms 1/P; never Repeat.
		ok := (r.Trace == "A" && (r.Arm == "0" || r.Arm == "1") && len(r.Cases) == 2) ||
			(r.Trace == "B" && s1 && r.Arm == "1") ||
			(r.Trace == "B" && s2 && (r.Arm == "1" || r.Arm == "P"))
		if !ok || r.Repeat {
			return fmt.Errorf("recall: pilot split does not include trace %s arm %s cases %v repeat=%t", r.Trace, r.Arm, r.Cases, r.Repeat)
		}
	}
	return validateRecallTurns(t.Turns, *r)
}

func validateRecallTurns(turns []Turn, r RecallEval) error {
	if err := validateToolEndingTurns(turns); err != nil {
		return fmt.Errorf("recall: %w", err)
	}
	// A stops after recall. B additionally shows the named file's contents.
	tools := []string{"agent_memory_search"}
	if r.Trace == "B" {
		tools = append(tools, "read_file")
	}
	if len(turns) != 1+2*len(tools) {
		return fmt.Errorf("recall: trace %s requires the %s call and result sequence", r.Trace, strings.Join(tools, ", "))
	}
	for i, tool := range tools {
		request, result := turns[2*i+1], turns[2*i+2]
		if request.Role != "assistant" || len(request.ToolCalls) != 1 || request.ToolCalls[0].Name != tool {
			return fmt.Errorf("recall: turn %d must call %s exactly once", 2*i+1, tool)
		}
		call := request.ToolCalls[0]
		if result.Role != "tool" || result.ToolCallID != call.ID || (result.Name != "" && result.Name != tool) || strings.TrimSpace(result.Content) == "" {
			return fmt.Errorf("recall: turn %d must hold the matching non-empty %s result", 2*i+2, tool)
		}
		args, ok := recallCallArgs(call)
		if !ok {
			return fmt.Errorf("recall: %s arguments must decode to an object", tool)
		}
		if tool == "read_file" {
			p, _ := args["path"].(string)
			if recallWorkspaceFile(p) != recallWorkspaceFile(r.NamedFile) {
				return fmt.Errorf("recall: trace B must read named_file %q", r.NamedFile)
			}
			if !statesValue(result.Content, r.Value, r.currentValue()) {
				return fmt.Errorf("recall: read_file result for %q must contain the current value", r.NamedFile)
			}
		}
	}
	return nil
}
