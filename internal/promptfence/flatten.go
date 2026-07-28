package promptfence

import "strings"

var lineReplacer = strings.NewReplacer(
	"\r", " ", "\n", " ", "\v", " ", "\f", " ",
	"\u0085", " ", "\u2028", " ", "\u2029", " ",
)

// FlattenLine forces an untrusted value that occupies exactly one line of the
// prompt onto one line. Typical values: Chunk.Source, because newlines are
// legal in POSIX filenames, nothing in the rag write path rejects control
// characters on chunks.source, and the managed-document path takes source
// straight from the caller; and any model-authored text a renderer places in a
// labeled field.
//
// This is not what stops forgery -- the unguessable fence is, and it covers
// labels and content alike. What flattening still guarantees is that a labeled
// line stays one line, so a newline in a value cannot strand the rest of that
// line or forge an additional labeled line with fabricated attribution.
//
// Content must never pass through it: its newlines carry meaning, and the fence
// already protects content without touching its bytes.
//
// It replaces rather than trims, so the caller's own alignment is preserved and
// the result is never shorter in rune count than the input.
func FlattenLine(s string) string {
	return lineReplacer.Replace(s)
}
