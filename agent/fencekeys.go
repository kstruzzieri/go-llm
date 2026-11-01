package agent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/kstruzzieri/go-llm/internal/promptfence"
	"github.com/kstruzzieri/go-llm/provider"
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

// advisoryOwner and systemOwner are the owner indexes refreshFenceKeys gives
// the advisory key and every system key; a tool key's owner is its message
// index.
const (
	advisoryOwner = -1
	systemOwner   = -2
)

// refreshFenceKeys rotates every live key whose value appears in the text
// appended since the last call, then keys the new tool messages
// (mintFenceKeys). It returns the index the next call starts from and the
// number of owners rotated. Run calls it at the top of each step, before
// assembly, so no render ever frames a message under a key that the text
// the model reads can quote.
//
// The live keys are every tool message's key, the advisory key, which is
// rotated in place (a nil or zero advisory is not live), and every system key
// (st.systemKeys), rotated by replacing each occurrence of it in st.System
// and in st.systemKeys, so System changes only where it is keyed. Each
// message at index from or later contributes every string replayed to the
// provider or rendered into the prompt (replayedTexts), and so does each
// pinned text: text rendered into every request from outside st.Messages,
// which Run passes at step 0 (the durable summary and the staged advisory).
// It was fixed before the Run, so it can quote a system key but no key the
// Run minted. A key that occurs in any of them
// as keyLetters reads it (any letter case, split by any non-key characters)
// is replaced by a fresh promptfence.New key, never a retired one. Every key
// a text exposes rotates, and each rotation counts once. Rotation changes
// the owner's frame on every later render, so the prompt cache misses from
// that message on; ordinary text never matches
// a random key, so it rotates nothing.
//
// Trust boundary: the scan reads Run state. A custom Compactor is trusted
// library code: Run-state text it copies has been scanned here, but text it
// synthesizes during assembly is not. Golem uses RecencyCompactor, and mixed
// assembly forbids custom compactors.
//
// ponytail: each decoded view is projected once, then searched per live key;
// a single pass over fixed-width windows of the projection looked up in the
// live map is the upgrade if a Run ever holds enough keys for that to show.
func refreshFenceKeys(st *State, advisory *promptfence.Fence, from int, pinned ...string) (next int, rotated int) {
	live := map[string]int{}
	for i, m := range st.Messages {
		if m.fence != (promptfence.Fence{}) {
			live[m.fence.ID()] = i
		}
	}
	if advisory != nil && *advisory != (promptfence.Fence{}) {
		live[advisory.ID()] = advisoryOwner
	}
	for _, id := range st.systemKeys {
		live[id] = systemOwner
	}
	var texts []string
	var saturated bool
	for _, p := range pinned {
		var views []string
		views, saturated = replayedTexts(Message{ChatMessage: provider.ChatMessage{Content: p}})
		if saturated {
			break
		}
		texts = append(texts, views...)
	}
	if !saturated {
		for i := from; i < len(st.Messages); i++ {
			var views []string
			views, saturated = replayedTexts(st.Messages[i])
			if saturated {
				break
			}
			texts = append(texts, views...)
		}
	}
	rotate := func(id string, owner int) {
		fresh := promptfence.New() // never a retired key
		switch owner {
		case advisoryOwner:
			*advisory = fresh
		case systemOwner:
			st.System = strings.ReplaceAll(st.System, id, fresh.ID())
			for j, k := range st.systemKeys {
				if k == id {
					st.systemKeys[j] = fresh.ID()
				}
			}
		default:
			st.Messages[owner].fence = fresh
		}
		delete(live, id)
		live[fresh.ID()] = owner
		rotated++
	}
	if saturated {
		// Decoding stopped at the work limit while another escape remained.
		// Any live key might be hidden beyond it, so rotate each original
		// owner once rather than letting an encoded marker keep its key.
		original := make(map[string]int, len(live))
		for id, owner := range live {
			original[id] = owner
		}
		for id, owner := range original {
			rotate(id, owner)
		}
		return mintFenceKeys(st, from), rotated
	}
	for _, text := range texts {
		letters := keyLetters(text)
		for id, owner := range live {
			if !strings.Contains(letters, id) {
				continue
			}
			rotate(id, owner)
		}
	}
	return mintFenceKeys(st, from), rotated
}

// validateSystemKeys refuses a system key the scan would match everywhere
// (the empty key), by chance (a short one), or never (one outside the
// alphabet keyLetters reads), or that is a proper substring of another key.
// Rotating one overlapping key would also rewrite the other's System frame
// without updating its entry. Twelve is promptfence's id length.
func validateSystemKeys(keys []string) error {
	for i, k := range keys {
		if len(k) < 12 || keyLetters(k) != k {
			return fmt.Errorf("agent: SystemKeys entry %d is not a fence key: want 12 or more characters of A-Z and 2-7", i)
		}
		for j, prior := range keys[:i] {
			if k != prior && (strings.Contains(k, prior) || strings.Contains(prior, k)) {
				return fmt.Errorf("agent: SystemKeys entry %d overlaps entry %d", i, j)
			}
		}
	}
	return nil
}

// keyLetters is what the key search reads of text: each rune case-folded
// (lower, then upper, so the Kelvin sign folds to K as k does) and projected
// onto the key alphabet [A-Z2-7], every other rune dropped. A key split by
// invisible format characters (U+200B, U+2060, U+FEFF, U+00AD), spaces,
// hyphens, line breaks or combining marks still matches, as a reader still
// reads it. Keys are 12 random characters of that alphabet, so the
// projection loses no match; a false match needs a chance run of a live
// key's 12 characters, about N x 2^-60 per live key over N projected
// characters. Confusables (Cyrillic or fullwidth look-alikes, precomposed
// accented letters) are dropped, not mapped: mapping them needs the UTS #39
// confusables table, which is out of scope.
func keyLetters(text string) string {
	return strings.Map(func(r rune) rune {
		r = unicode.ToUpper(unicode.ToLower(r))
		if 'A' <= r && r <= 'Z' || '2' <= r && r <= '7' {
			return r
		}
		return -1
	}, text)
}

// escapedRune is a \uXXXX escape behind one or more backslashes: a JSON
// escape, or an escape of one inside a JSON-encoded string.
var escapedRune = regexp.MustCompile(`\\+u[0-9A-Fa-f]{4}`)

// A capped amount of simultaneous decoding keeps adversarial nesting from
// making each request scan the same text an unbounded number of times.
const maxEscapeDecodeWork = 2 << 20

// replayedTexts returns every string of m that is replayed to the provider
// or rendered into the prompt: Content, ToolName and ToolCallID; each tool
// call's ID, Type, Function.Name and Function.Arguments; and, when m carries
// a ContextSet (mixed assembly renders its alternatives instead of Content),
// each group's subject domain and ID and each alternative's Content. An
// AlternativeDesc holds no string. Each text that holds a \uXXXX escape is
// also returned after each simultaneous decode pass. If more decoding would
// exceed the bounded work budget, the caller rotates all live keys. This
// catches keys at any nesting depth without unbounded repeated scans.
// Over-scanning costs a rotation; under-scanning lets a quoted key live.
func replayedTexts(m Message) ([]string, bool) {
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
	initial := len(texts)
	for i := 0; i < initial; i++ {
		work := 0
		for t := texts[i]; strings.Contains(t, `\u`); {
			if len(t) > maxEscapeDecodeWork-work {
				if escapedRune.MatchString(t) {
					return texts, true
				}
				break
			}
			work += len(t)
			d := escapedRune.ReplaceAllStringFunc(t, decodeEscape)
			if d == t {
				break
			}
			texts = append(texts, d)
			t = d
		}
	}
	return texts, false
}

// decodeEscape decodes the \uXXXX that ends esc; surrogates become U+FFFD.
func decodeEscape(esc string) string {
	n, err := strconv.ParseUint(esc[len(esc)-4:], 16, 32)
	if err != nil {
		return esc // unreachable: escapedRune admits only four hex digits
	}
	return string(rune(n))
}
