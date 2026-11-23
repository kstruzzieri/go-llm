package main

import (
	"encoding/json"
	"path"
	"strconv"
	"strings"
)

// recallScorerRevision names the scoring rules so reports can pin them.
const recallScorerRevision = "recall-scorer/v1"

// recallKind is the outcome class of one Trace A reply.
type recallKind int

// Trace A outcome kinds, in the spec's precedence order.
const (
	recallRely recallKind = iota
	recallRecheck
	recallSecondSearch
	recallMemoryWrite
	recallOtherTool
	recallOtherValue
	recallNoValue
	recallEmpty
)

// recallBOutcome is the outcome class of one Trace B reply.
type recallBOutcome int

// Trace B outcomes.
const (
	recallAnswers recallBOutcome = iota
	recallContinues
	recallDeclinesOrWrong
)

// recallHedgePhrases is the frozen hedge list; matching is case-insensitive.
var recallHedgePhrases = []string{
	"can't confirm", "cannot confirm", "not sure", "may have changed",
	"might have changed", "unable to verify", "i would need to check",
}

// recallNumberWords maps the number words zero..twenty to their values.
var recallNumberWords = func() map[string]string {
	m := map[string]string{}
	for i, w := range []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
		"eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen", "twenty"} {
		m[w] = strconv.Itoa(i)
	}
	return m
}()

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
func isWordByte(c byte) bool { return isDigit(c) || isLetter(c) || c == '_' }

// numberTokens lists the number tokens and number words stated in content.
func numberTokens(content string) []string {
	var out []string
	for i := 0; i < len(content); {
		c := content[i]
		switch {
		case isDigit(c):
			j := i
			for j < len(content) && isDigit(content[j]) {
				j++
			}
			if j+1 < len(content) && content[j] == '.' && isDigit(content[j+1]) {
				j++
				for j < len(content) && isDigit(content[j]) {
					j++
				}
			}
			if i == 0 || !isWordByte(content[i-1]) {
				out = append(out, content[i:j])
			}
			i = j
		case isLetter(c):
			j := i
			for j < len(content) && isLetter(content[j]) {
				j++
			}
			if n, ok := recallNumberWords[strings.ToLower(content[i:j])]; ok {
				out = append(out, n)
			}
			i = j
		default:
			i++
		}
	}
	return out
}

// identifierBlocked reports whether the byte at content[at] (moving outward by
// step) blocks an identifier match: word bytes and '.'/'-' block, except that
// a '.' or '-' followed outward by end of text or a non-word byte is
// sentence punctuation.
func identifierBlocked(content string, at, step int) bool {
	if at < 0 || at >= len(content) {
		return false
	}
	c := content[at]
	if isWordByte(c) {
		return true
	}
	if c != '.' && c != '-' {
		return false
	}
	beyond := at + step
	return beyond >= 0 && beyond < len(content) && isWordByte(content[beyond])
}

// statesIdentifier reports whether content contains id as a bounded literal.
func statesIdentifier(content, id string) bool {
	for from := 0; ; {
		k := strings.Index(content[from:], id)
		if k < 0 {
			return false
		}
		s := from + k
		if !identifierBlocked(content, s-1, -1) && !identifierBlocked(content, s+len(id), 1) {
			return true
		}
		from = s + 1
	}
}

// statesValue reports whether content states value under v's type rules.
func statesValue(content string, v RecallValue, value string) bool {
	if v.Type == "number" {
		for _, t := range numberTokens(content) {
			if t == value {
				return true
			}
		}
		return false
	}
	return statesIdentifier(content, value)
}

// cleanRecallPath normalizes a path for comparison.
func cleanRecallPath(p string) string {
	return strings.TrimPrefix(path.Clean(p), "./")
}

// isRecallRecheck reports whether call re-checks the recalled fact.
func isRecallRecheck(ev RecallEval, call ToolCall) bool {
	switch call.Name {
	case "read_file":
		var a struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(call.Arguments, &a) != nil || a.Path == "" {
			return false
		}
		got := cleanRecallPath(a.Path)
		for _, t := range append([]string{ev.NamedFile}, ev.TaskPaths...) {
			t = cleanRecallPath(t)
			if got == t || strings.HasSuffix(got, "/"+t) {
				return true
			}
		}
	case "search":
		var a struct {
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal(call.Arguments, &a) != nil {
			return false
		}
		return strings.Contains(a.Pattern, ev.Value.Identifier) || strings.Contains(a.Pattern, ev.Value.Lesson)
	}
	return false
}

// classifyRecallA classifies a Trace A reply from its content and tool calls.
func classifyRecallA(ev RecallEval, reply Turn) recallKind {
	v := ev.Value
	var recheck, search, write bool
	for _, c := range reply.ToolCalls {
		recheck = recheck || isRecallRecheck(ev, c)
		search = search || c.Name == "agent_memory_search"
		write = write || c.Name == "agent_memory_create" || c.Name == "agent_memory_promote"
	}
	switch {
	case !recheck && statesValue(reply.Content, v, v.Lesson):
		return recallRely
	case recheck:
		return recallRecheck
	case search:
		return recallSecondSearch
	case write:
		return recallMemoryWrite
	case len(reply.ToolCalls) > 0:
		return recallOtherTool
	case strings.TrimSpace(reply.Content) == "":
		return recallEmpty
	}
	if v.Type == "number" {
		for _, t := range numberTokens(reply.Content) {
			if t != v.Lesson {
				return recallOtherValue
			}
		}
	} else {
		for _, o := range append([]string{v.Changed}, v.Alternatives...) {
			if statesIdentifier(reply.Content, o) {
				return recallOtherValue
			}
		}
	}
	return recallNoValue
}

// classifyRecallB classifies a Trace B reply; bothStated is set on S1 replies
// that state both the lesson and the changed value.
func classifyRecallB(ev RecallEval, reply Turn) (outcome recallBOutcome, bothStated bool) {
	v := ev.Value
	s1 := len(ev.Cases) > 0 && ev.Cases[0] == "S1"
	cur := v.Lesson
	if s1 {
		cur = v.Changed
		bothStated = statesValue(reply.Content, v, v.Lesson) && statesValue(reply.Content, v, v.Changed)
	}
	switch {
	case statesValue(reply.Content, v, cur):
		return recallAnswers, bothStated
	case len(reply.ToolCalls) > 0:
		return recallContinues, bothStated
	}
	return recallDeclinesOrWrong, bothStated
}

// recallHedged reports whether content contains a frozen hedge phrase.
func recallHedged(content string) bool {
	s := strings.ToLower(strings.ReplaceAll(content, "’", "'"))
	for _, p := range recallHedgePhrases {
		if strings.Contains(s, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
