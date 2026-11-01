package agent

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/internal/promptfence"
)

// mintFenceKeys gives every role "tool" message at index from or later that
// has no key yet a fresh key of its own, and returns len(st.Messages), the
// index the next call starts from.
//
// Run calls it at the top of each step, before assembly. That is the first
// moment a tool message's bytes are final: verifyBatch appends to the anchor
// after recordResult has appended it, so minting at append would key bytes
// that are still changing. Each later render frames the message under the
// same key, so the request bytes up to and including it stay identical
// across renders.
func mintFenceKeys(st *State, from int) int {
	for i := from; i < len(st.Messages); i++ {
		if m := &st.Messages[i]; m.Role == "tool" && m.fence == (promptfence.Fence{}) {
			m.fence = promptfence.New()
		}
	}
	return len(st.Messages)
}

// advisoryOwner is the owner index refreshFenceKeys gives the advisory key;
// a tool key's owner is its message index.
const advisoryOwner = -1

// refreshFenceKeys rotates every live key whose value appears in the text
// appended since the last call, then keys the new tool messages
// (mintFenceKeys). It returns the index the next call starts from, the number
// and owners rotated, and whether the scan work limit was reached. Run calls
// it at the top of each step, before assembly, so no render ever frames a
// message under a key that the text the model reads can quote.
//
// The live keys are every tool message's key and the advisory key, which is
// rotated in place; a nil or zero advisory is not live. Each message at
// index from or later contributes every string replayed to the provider or
// rendered into the prompt (replayedTexts). A key that occurs in any of them
// as keyLetters reads it (any letter case, split by any non-key characters)
// is replaced by a fresh promptfence.New key, never a retired one. Every key
// a text exposes rotates, and each rotation counts once. Rotation changes
// the owner's frame on every later render, so the prompt cache misses from
// that message on. Ordinary text within the work limit rarely matches a
// random key, so it rotates nothing.
//
// Trust boundary: the scan reads Run state. A custom Compactor is trusted
// library code: Run-state text it copies has been scanned here, but text it
// synthesizes during assembly is not. Golem uses RecencyCompactor, and mixed
// assembly forbids custom compactors.
//
// Each decoded layer is checked against the live keys. The work budget bounds
// the total bytes checked; reaching it resets every live key once.
func refreshFenceKeys(st *State, advisory *promptfence.Fence, from int) (next int, rotated int, limited bool, rotatedOwners map[int]bool) {
	scan := newFenceScan(st, advisory)
	for i := from; i < len(st.Messages); i++ {
		for _, text := range replayedTexts(st.Messages[i]) {
			if !scan.inspect(text) {
				rotated = scan.rotateAll(st, nil, advisory, nil)
				return mintFenceKeys(st, from), rotated, true, scan.rotatedOwners()
			}
		}
	}
	rotated = scan.rotate(st, nil, advisory)
	return mintFenceKeys(st, from), rotated, false, scan.rotatedOwners()
}

// refreshMaterializedFenceKeys checks the joined content that mixed assembly
// selected for each multi-group tool result. The individual alternatives were
// checked by refreshFenceKeys, but a key can straddle the newline anchorJoined
// inserts between two selected groups. The assembled copy and canonical State
// must both receive each new key before buildChatRequest frames the result.
func refreshMaterializedFenceKeys(st *State, assembled *State, advisory *promptfence.Fence, skipBefore int, alreadyRotated map[int]bool) (rotated int, limited bool) {
	structured := map[string]bool{}
	for i, m := range st.Messages {
		if i < skipBefore {
			continue // replacement keys were minted after this text was final
		}
		if m.Context != nil && len(m.Context.Groups) > 1 && m.fence != (promptfence.Fence{}) {
			structured[m.fence.ID()] = true
		}
	}
	if len(structured) == 0 {
		return 0, false
	}
	var indexes []int
	for i, m := range assembled.Messages {
		if structured[m.fence.ID()] {
			indexes = append(indexes, i)
		}
	}
	scan := newFenceScan(st, advisory)
	for _, i := range indexes {
		if !scan.inspect(assembled.Messages[i].Content) {
			// The preassembly scan has already replaced these owners' keys
			// since their source text became final. Reset the other live keys
			// without minting a second key for the same owner in one render.
			return scan.rotateAll(st, assembled, advisory, alreadyRotated), true
		}
	}
	return scan.rotate(st, assembled, advisory), false
}

func liveFenceKeys(st *State, advisory *promptfence.Fence) map[string]int {
	live := map[string]int{}
	for i, m := range st.Messages {
		if m.fence != (promptfence.Fence{}) {
			live[m.fence.ID()] = i
		}
	}
	if advisory != nil && *advisory != (promptfence.Fence{}) {
		live[advisory.ID()] = advisoryOwner
	}
	return live
}

// The limit bounds nested escapes such as repeated \u005c, where each decode
// removes only five bytes and rescanning the entire text would be quadratic.
// If the limit is reached, Run resets all live keys once after the text is
// final, then skips that older text on later mixed assemblies.
const maxFenceScanBytes = 8 << 20

type fenceScan struct {
	live      map[string]int
	exposed   map[string]int
	remaining int
	keyLength int
}

func newFenceScan(st *State, advisory *promptfence.Fence) fenceScan {
	live := liveFenceKeys(st, advisory)
	var keyLength int
	for id := range live {
		keyLength = len(id) // every promptfence.New ID has the same width
		break
	}
	return fenceScan{live: live, exposed: make(map[string]int), remaining: maxFenceScanBytes, keyLength: keyLength}
}

// inspect checks every decoded escape layer. Fullwidth ASCII forms map to
// their ordinary characters before key projection; other non-key characters
// remain separators. Matches are collected before rotation; exhausting the
// byte budget resets all keys.
func (s *fenceScan) inspect(text string) bool {
	if s.keyLength == 0 {
		return true
	}
	for {
		if len(text) > s.remaining {
			return false
		}
		s.remaining -= len(text)
		s.scanLayer(text)
		decoded := decodeCompatibleEscapes(text)
		if decoded == text {
			return true
		}
		text = decoded
	}
}

func (s *fenceScan) scanLayer(text string) {
	letters := keyLetters(text)
	for i := 0; i+s.keyLength <= len(letters); i++ {
		id := letters[i : i+s.keyLength]
		if owner, ok := s.live[id]; ok {
			s.exposed[id] = owner
		}
	}
}

func (s *fenceScan) rotate(st, assembled *State, advisory *promptfence.Fence) (rotated int) {
	for id, owner := range s.exposed {
		rotateFenceKey(st, assembled, advisory, s.live, id, owner)
		rotated++
	}
	return rotated
}

func (s *fenceScan) rotateAll(st, assembled *State, advisory *promptfence.Fence, alreadyRotated map[int]bool) int {
	// Snapshot before rotate mutates live; a map range could otherwise visit
	// newly inserted keys and rotate the same owner more than once.
	for id, owner := range s.live {
		if !alreadyRotated[owner] {
			s.exposed[id] = owner
		} else {
			delete(s.exposed, id)
		}
	}
	return s.rotate(st, assembled, advisory)
}

func (s *fenceScan) rotatedOwners() map[int]bool {
	owners := make(map[int]bool, len(s.exposed))
	for _, owner := range s.exposed {
		owners[owner] = true
	}
	return owners
}

func rotateFenceKey(st, assembled *State, advisory *promptfence.Fence, live map[string]int, id string, owner int) {
	fresh := promptfence.New() // never a retired key
	if owner == advisoryOwner {
		*advisory = fresh
	} else {
		st.Messages[owner].fence = fresh
		if assembled != nil {
			for i := range assembled.Messages {
				if assembled.Messages[i].fence.ID() == id {
					assembled.Messages[i].fence = fresh
				}
			}
		}
	}
	delete(live, id)
	live[fresh.ID()] = owner
}

// keyLetters is what the key search reads of text: fullwidth ASCII forms
// mapped to ordinary ASCII, each rune case-folded (lower, then upper, so the
// Kelvin sign folds to K as k does), and projected onto the key alphabet
// [A-Z2-7], every other rune dropped. A key split by
// invisible format characters (U+200B, U+2060, U+FEFF, U+00AD), spaces,
// hyphens, line breaks or combining marks still matches, as a reader still
// reads it. Keys are 12 random characters of that alphabet, so the
// projection loses no match; a false match needs a chance run of a live
// key's 12 characters, about N x 2^-60 per live key over N projected
// characters. Other compatibility forms and cross-script confusables such as
// Cyrillic look-alikes remain out of scope.
func keyLetters(text string) string {
	return strings.Map(func(r rune) rune {
		if c := compatibleASCII(r); c != 0 {
			r = rune(c)
		}
		r = unicode.ToUpper(unicode.ToLower(r))
		if 'A' <= r && r <= 'Z' || '2' <= r && r <= '7' {
			return r
		}
		return -1
	}, text)
}

// decodeCompatibleEscapes decodes a \uXXXX escape behind one or more
// backslashes. Fullwidth ASCII spellings of the escape syntax are accepted;
// unrelated Unicode runes remain unchanged.
func decodeCompatibleEscapes(text string) string {
	var out strings.Builder
	changed := false
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if compatibleASCII(r) == '\\' {
			j := i
			for j < len(text) {
				slash, n := utf8.DecodeRuneInString(text[j:])
				if compatibleASCII(slash) != '\\' {
					break
				}
				j += n
			}
			if decoded, end, ok := compatibleEscapeAt(text, j); ok {
				if !changed {
					out.Grow(len(text))
					out.WriteString(text[:i])
					changed = true
				}
				out.WriteRune(decoded)
				i = end
				continue
			}
			if changed {
				out.WriteString(text[i:j])
			}
			i = j
			continue
		}
		if changed {
			out.WriteString(text[i : i+size])
		}
		i += size
	}
	if !changed {
		return text
	}
	return out.String()
}

func compatibleEscapeAt(text string, start int) (rune, int, bool) {
	if start >= len(text) {
		return 0, 0, false
	}
	r, size := utf8.DecodeRuneInString(text[start:])
	if compatibleASCII(r) != 'u' {
		return 0, 0, false
	}
	pos, value := start+size, 0
	for range 4 {
		if pos >= len(text) {
			return 0, 0, false
		}
		r, size = utf8.DecodeRuneInString(text[pos:])
		nibble := hexNibble(compatibleASCII(r))
		if nibble < 0 {
			return 0, 0, false
		}
		value = value<<4 | nibble
		pos += size
	}
	return rune(value), pos, true
}

func compatibleASCII(r rune) byte {
	if r < utf8.RuneSelf {
		return byte(r)
	}
	if 0xFF01 <= r && r <= 0xFF5E {
		return byte(r - 0xFEE0)
	}
	return 0
}

func hexNibble(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	default:
		return -1
	}
}

// replayedTexts returns every string of m that is replayed to the provider
// or rendered into the prompt: Content, ToolName and ToolCallID; each tool
// call's ID, Type, Function.Name and Function.Arguments; and, when m carries
// a ContextSet (mixed assembly renders its alternatives instead of Content),
// each group's subject domain and ID and each alternative's Content. An
// AlternativeDesc holds no string. fenceScan inspects every decoded escape
// layer within its work budget. Over-scanning costs a rotation; reaching the
// limit resets every live key before the next request.
func replayedTexts(m Message) []string {
	texts := []string{m.Content, m.ToolName, m.ToolCallID}
	for _, c := range m.ToolCalls {
		texts = append(texts, c.ID, c.Type, c.Function.Name, string(c.Function.Arguments))
	}
	if m.Context != nil {
		for _, g := range m.Context.Groups {
			texts = append(texts, g.Desc.Subject.Domain, g.Desc.Subject.ID)
			for _, a := range g.Alternatives {
				texts = append(texts, a.Content)
			}
		}
	}
	return texts
}
