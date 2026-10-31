package agent

import (
	"regexp"
	"strconv"
	"strings"

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
// (mintFenceKeys). It returns the index the next call starts from and the
// number of owners rotated. Run calls it at the top of each step, before
// assembly, so no render ever frames a message under a key that the text
// the model reads can quote.
//
// The live keys are every tool message's key and the advisory key, which is
// rotated in place; a nil or zero advisory is not live. Each message at
// index from or later contributes every string replayed to the provider or
// rendered into the prompt (replayedTexts). A key that occurs in any of them,
// in any letter case, is replaced by a fresh promptfence.New key, never a
// retired one. Rotation changes the owner's frame on every later render, so
// the prompt cache misses from that message on; ordinary text never matches
// a random key, so it rotates nothing.
//
// Trust boundary: the scan reads Run state. A custom Compactor is trusted
// library code: Run-state text it copies has been scanned here, but text it
// synthesizes during assembly is not. Golem uses RecencyCompactor, and mixed
// assembly forbids custom compactors.
//
// ponytail: each text is searched once per live key, O(text x keys); a single
// pass over fixed-width windows looked up in the live map is the upgrade if
// a Run ever holds enough keys for that to show.
func refreshFenceKeys(st *State, advisory *promptfence.Fence, from int) (next int, rotated int) {
	live := map[string]int{}
	for i, m := range st.Messages {
		if m.fence != (promptfence.Fence{}) {
			live[m.fence.ID()] = i
		}
	}
	if advisory != nil && *advisory != (promptfence.Fence{}) {
		live[advisory.ID()] = advisoryOwner
	}
	for i := from; i < len(st.Messages); i++ {
		for _, text := range replayedTexts(st.Messages[i]) {
			// ToLower first so the Kelvin sign folds to K like k does.
			up := strings.ToUpper(strings.ToLower(text))
			for id, owner := range live {
				if !strings.Contains(up, id) {
					continue
				}
				fresh := promptfence.New() // never a retired key
				if owner == advisoryOwner {
					*advisory = fresh
				} else {
					st.Messages[owner].fence = fresh
				}
				delete(live, id)
				live[fresh.ID()] = owner
				rotated++
			}
		}
	}
	return mintFenceKeys(st, from), rotated
}

// escapedRune is a \uXXXX escape behind one or more backslashes: a JSON
// escape, or an escape of one inside a JSON-encoded string.
var escapedRune = regexp.MustCompile(`\\+u[0-9A-Fa-f]{4}`)

// replayedTexts returns every string of m that is replayed to the provider
// or rendered into the prompt: Content, ToolName and ToolCallID; each tool
// call's ID, Type, Function.Name and Function.Arguments; and, when m carries
// a ContextSet (mixed assembly renders its alternatives instead of Content),
// each group's subject domain and ID and each alternative's Content. An
// AlternativeDesc holds no string. Each text that holds a \uXXXX escape is
// also returned with every escape decoded, so a key spelled in escapes is
// found in arguments decoded at any JSON depth, double-encoded or malformed.
// Over-scanning costs a rotation; under-scanning lets a quoted key live.
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
	for _, t := range texts {
		if d := escapedRune.ReplaceAllStringFunc(t, decodeEscape); d != t {
			texts = append(texts, d)
		}
	}
	return texts
}

// decodeEscape decodes the \uXXXX that ends esc; surrogates become U+FFFD.
func decodeEscape(esc string) string {
	n, err := strconv.ParseUint(esc[len(esc)-4:], 16, 32)
	if err != nil {
		return esc // unreachable: escapedRune admits only four hex digits
	}
	return string(rune(n))
}
