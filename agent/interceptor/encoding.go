package interceptor

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
)

const (
	// minBase64Run is the shortest run decoded: raw base64 of the 11-byte
	// shortest default phrase ("you are now") is 15 characters. Overrides
	// shorter than 11 bytes are not searched inside encodings.
	minBase64Run = 15
	// minHexRun is hex of the same 11 bytes.
	minHexRun = 22
)

// Encoding flags base64 or hex runs that decode to text carrying an
// instruction phrase or a zero-width character: smuggling a plain scan
// cannot see. Line folds are removed first so folded runs rejoin; every run
// is scanned and the strongest finding wins (strong phrase, then decoded
// zero-width, then weak phrase). One layer is decoded.
type Encoding struct {
	Phrases Phrases
}

var _ agent.Interceptor = Encoding{}

// Name returns "encoding".
func (Encoding) Name() string { return "encoding" }

// unfold removes newlines (CRLF or LF) together with any spaces or tabs that
// follow them, and nothing else: MIME folds and base64(1)'s 76-column wraps
// rejoin, while words separated by ordinary spaces stay apart.
func unfold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			i++
			c = '\n'
		}
		if c != '\n' {
			b.WriteByte(c)
			continue
		}
		for i+1 < len(s) && (s[i+1] == ' ' || s[i+1] == '\t') {
			i++
		}
	}
	return b.String()
}

func isBase64Char(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
		r == '+' || r == '/' || r == '-' || r == '_' || r == '='
}

func isHexChar(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// runsOf returns maximal runs of characters satisfying pred, split after any
// base64 padding ("=" followed by a non-padding character ends a run, since
// padding can only close a payload), keeping pieces of at least minLen bytes.
// Without the split, one alphabet byte appended after "==" makes the whole
// run undecodable and the payload evades detection.
func runsOf(s string, pred func(rune) bool, minLen int) []string {
	var runs []string
	emit := func(run string) {
		for len(run) > 0 {
			cut := len(run)
			for i := 0; i+1 < len(run); i++ {
				if run[i] == '=' && run[i+1] != '=' {
					cut = i + 1
					break
				}
			}
			if cut >= minLen {
				runs = append(runs, run[:cut])
			}
			run = run[cut:]
		}
	}
	start := -1
	for i, r := range s {
		if pred(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			emit(s[start:i])
		}
		start = -1
	}
	if start >= 0 {
		emit(s[start:])
	}
	return runs
}

var base64Forms = []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding}

func decodeBase64(run string) (string, bool) {
	for _, enc := range base64Forms {
		if b, err := enc.DecodeString(run); err == nil && utf8.Valid(b) {
			return string(b), true
		}
	}
	return "", false
}

func decodeHex(run string) (string, bool) {
	if len(run)%2 != 0 {
		return "", false
	}
	b, err := hex.DecodeString(run)
	if err != nil || !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}

// rank orders candidate findings: strong phrase > zero-width > weak phrase.
const (
	rankNone = iota
	rankWeak
	rankZeroWidth
	rankStrong
)

// scanLayer inspects one decoded layer and returns its best finding and rank.
func (e Encoding) scanLayer(t target, prefix, decoded string) (agent.Finding, int) {
	if p, exact, isStrong, ok := matchPhrases(decoded, e.Phrases.strong(), e.Phrases.weak()); ok {
		f := phraseFinding(t, p, exact, isStrong)
		f.Detail = "decodes to text containing " + strconv.Quote(p)
		if isStrong {
			f.Rule, f.Risk = prefix+"_instruction", EncodingRisk
			return f, rankStrong
		}
		f.Rule = prefix + "_weak_phrase"
		if first, n, escaped := scanZeroWidth(decoded); n > 0 {
			return zeroWidthFinding(t, prefix+"_zero_width", first, n, escaped), rankZeroWidth
		}
		return f, rankWeak
	}
	if first, n, escaped := scanZeroWidth(decoded); n > 0 {
		return zeroWidthFinding(t, prefix+"_zero_width", first, n, escaped), rankZeroWidth
	}
	return agent.Finding{}, rankNone
}

// scan decodes every run of text and returns the highest-ranked finding; the
// earliest run wins within a rank.
func (e Encoding) scan(t target, text string) (agent.Finding, bool) {
	flat := unfold(text)
	var best agent.Finding
	bestRank := rankNone
	consider := func(runs []string, decode func(string) (string, bool), prefix string) {
		for _, run := range runs {
			decoded, ok := decode(run)
			if !ok {
				continue
			}
			f, rank := e.scanLayer(t, prefix, decoded)
			if rank > bestRank {
				best, bestRank = f, rank
			}
		}
	}
	consider(runsOf(flat, isBase64Char, minBase64Run), decodeBase64, "base64")
	consider(runsOf(flat, isHexChar, minHexRun), decodeHex, "hex")
	return best, bestRank > rankNone
}

// InspectInput checks the system prompt, summary, every message and every
// alternative.
func (e Encoding) InspectInput(_ context.Context, in agent.InputInspection) ([]agent.Finding, error) {
	var out []agent.Finding
	eachText(in, func(t target, text string) {
		if f, ok := e.scan(t, text); ok {
			out = append(out, f)
		}
	})
	return out, nil
}

// InspectOutput returns nothing: model output policy belongs to #438/#437.
func (Encoding) InspectOutput(context.Context, agent.OutputInspection) ([]agent.Finding, error) {
	return nil, nil
}

// InspectToolCall checks the call's raw JSON arguments.
func (e Encoding) InspectToolCall(_ context.Context, call agent.ToolCallInspection) ([]agent.Finding, error) {
	if f, ok := e.scan(toolCallTarget(call.Call.ID), string(call.Call.Function.Arguments)); ok {
		return []agent.Finding{f}, nil
	}
	return nil, nil
}
