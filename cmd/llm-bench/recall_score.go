package main

import (
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
	"might have changed", "unable to verify", "I would need to check",
}

// recallNumberWords maps the number words zero and two..twenty to their values.
var recallNumberWords = func() map[string]string {
	m := map[string]string{}
	// "one" is left out: as a pronoun ("which one?") it would state 1.
	for i, w := range []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
		"eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen", "twenty"} {
		if w != "one" {
			m[w] = strconv.Itoa(i)
		}
	}
	return m
}()

// recallIsDigit reports whether c is an ASCII digit.
func recallIsDigit(c byte) bool { return c >= '0' && c <= '9' }

// recallIsLetter reports whether c is an ASCII letter.
func recallIsLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// recallIsWordByte reports whether c is in [A-Za-z0-9_].
func recallIsWordByte(c byte) bool { return recallIsDigit(c) || recallIsLetter(c) || c == '_' }

// normalizeRecallNumber strips trailing fractional zeros ("5.0" -> "5",
// "2.50" -> "2.5"); leading zeros are kept so "05" is not 5.
func normalizeRecallNumber(t string) string {
	if strings.Contains(t, ".") {
		t = strings.TrimSuffix(strings.TrimRight(t, "0"), ".")
	}
	return t
}

// recallNumberTokens lists the normalized number tokens (and, for reply text,
// number words) stated in content. A digit run preceded by a word byte or '.'
// is not a token; a token followed by a further ".<digit>" is a version string
// and states nothing; a number word counts only when both neighbors are
// outside [A-Za-z0-9_]. In pattern mode only digit tokens count, and a letter
// directly preceded by a backslash (a regex escape such as \b) does not block
// the token's left side, and "\." counts as ".".
func recallNumberTokens(content string, pattern bool) []string {
	var out []string
	for i := 0; i < len(content); {
		c := content[i]
		switch {
		case recallIsDigit(c):
			// Collect ".<digits>" groups; in pattern mode "\." counts as ".".
			// One group is a decimal, more than one is a version string.
			j, groups := i, 0
			var tok strings.Builder
			for {
				for j < len(content) && recallIsDigit(content[j]) {
					tok.WriteByte(content[j])
					j++
				}
				d := 0
				if j < len(content) && content[j] == '.' {
					d = 1
				} else if pattern && j+1 < len(content) && content[j] == '\\' && content[j+1] == '.' {
					d = 2
				}
				if d == 0 || j+d >= len(content) || !recallIsDigit(content[j+d]) {
					break
				}
				tok.WriteByte('.')
				j += d
				groups++
			}
			version := groups > 1
			left := i == 0 || (!recallIsWordByte(content[i-1]) && content[i-1] != '.')
			if !left && pattern && i >= 2 && recallIsLetter(content[i-1]) && content[i-2] == '\\' {
				left = true
			}
			if left && !version {
				out = append(out, normalizeRecallNumber(tok.String()))
			}
			i = j
		case recallIsLetter(c):
			j := i
			for j < len(content) && recallIsLetter(content[j]) {
				j++
			}
			n, ok := recallNumberWords[strings.ToLower(content[i:j])]
			if ok && !pattern && (i == 0 || !recallIsWordByte(content[i-1])) && (j == len(content) || !recallIsWordByte(content[j])) {
				out = append(out, n)
			}
			i = j
		default:
			i++
		}
	}
	return out
}

// recallIdentifierBlocked reports whether the byte at content[at] (moving
// outward by step) blocks an identifier match: word bytes and '.'/'-' block,
// except that a '.' or '-' followed outward by end of text or a non-word byte
// is sentence punctuation.
func recallIdentifierBlocked(content string, at, step int) bool {
	if at < 0 || at >= len(content) {
		return false
	}
	c := content[at]
	if recallIsWordByte(c) {
		return true
	}
	if c != '.' && c != '-' {
		return false
	}
	beyond := at + step
	return beyond >= 0 && beyond < len(content) && recallIsWordByte(content[beyond])
}

// statesIdentifier reports whether content contains id as a bounded literal.
func statesIdentifier(content, id string) bool {
	if id == "" {
		return false
	}
	for from := 0; ; {
		k := strings.Index(content[from:], id)
		if k < 0 {
			return false
		}
		s := from + k
		if !recallIdentifierBlocked(content, s-1, -1) && !recallIdentifierBlocked(content, s+len(id), 1) {
			return true
		}
		from = s + 1
	}
}

// statesValue reports whether content states value under v's type rules.
func statesValue(content string, v RecallValue, value string) bool {
	if v.Type == "number" {
		for _, t := range recallNumberTokens(content, false) {
			if t == normalizeRecallNumber(value) {
				return true
			}
		}
		return false
	}
	return statesIdentifier(content, value)
}

// recallCallArgs decodes a call's arguments; ok is false for a malformed call,
// which the scorer treats as no call at all.
func recallCallArgs(call ToolCall) (map[string]any, bool) {
	return decodeRawArgs(string(call.Arguments))
}

// recallDecodedCalls drops calls whose arguments do not decode.
func recallDecodedCalls(calls []ToolCall) []ToolCall {
	var out []ToolCall
	for _, c := range calls {
		if _, ok := recallCallArgs(c); ok {
			out = append(out, c)
		}
	}
	return out
}

// recallNumberInPattern reports whether the number lesson is a token of pattern.
func recallNumberInPattern(pattern, lesson string) bool {
	want := normalizeRecallNumber(lesson)
	for _, t := range recallNumberTokens(pattern, true) {
		if t == want {
			return true
		}
	}
	return false
}

// isRecallRecheck reports whether call (already decodable) re-checks the
// recalled fact.
func isRecallRecheck(ev RecallEval, call ToolCall) bool {
	args, ok := recallCallArgs(call)
	if !ok {
		return false
	}
	switch call.Name {
	case "read_file":
		p, _ := args["path"].(string)
		got := recallWorkspaceFile(p)
		if got == "" {
			return false
		}
		for _, t := range append([]string{ev.NamedFile}, ev.TaskPaths...) {
			if got == recallWorkspaceFile(t) {
				return true
			}
		}
	case "search":
		pat, _ := args["pattern"].(string)
		lower := strings.ToLower(pat)
		// An empty needle matches every pattern; it must never read as a re-check.
		if id := ev.Value.Identifier; id != "" && strings.Contains(lower, strings.ToLower(id)) {
			return true
		}
		if ev.Value.Type == "number" {
			return recallNumberInPattern(pat, ev.Value.Lesson)
		}
		return ev.Value.Lesson != "" && strings.Contains(lower, strings.ToLower(ev.Value.Lesson))
	}
	return false
}

// classifyRecallA classifies a Trace A reply from its content and tool calls.
// A call whose arguments do not decode counts as no call.
func classifyRecallA(ev RecallEval, reply Turn) recallKind {
	v := ev.Value
	calls := recallDecodedCalls(reply.ToolCalls)
	var recheck, search, write bool
	for _, c := range calls {
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
	case len(calls) > 0:
		return recallOtherTool
	case strings.TrimSpace(reply.Content) == "":
		return recallEmpty
	}
	if v.Type == "number" {
		for _, t := range recallNumberTokens(reply.Content, false) {
			if t != normalizeRecallNumber(v.Lesson) {
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

// classifyRecallB classifies a Trace B reply (a call whose arguments do not
// decode counts as no call, so it cannot make a reply continue); bothStated
// is set on S1 replies that state both the lesson and the changed value.
func classifyRecallB(ev RecallEval, reply Turn) (outcome recallBOutcome, bothStated bool) {
	v := ev.Value
	answers := statesValue(reply.Content, v, ev.currentValue())
	switch {
	case answers:
		bothStated = len(ev.Cases) > 0 && ev.Cases[0] == "S1" && statesValue(reply.Content, v, v.Lesson)
		return recallAnswers, bothStated
	case len(recallDecodedCalls(reply.ToolCalls)) > 0:
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
