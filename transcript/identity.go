package transcript

import (
	"encoding/json"
	"strings"

	"github.com/kstruzzieri/go-llm/conversation"
)

// identity_source values recorded for a conversation. "forked" is assigned at
// projection time (see stitch.go), never at key derivation.
const (
	identityExplicit = "explicit"
	identityDerived  = "derived"
	identityForked   = "forked"
)

// RecordInput is one chat call to persist. Request is the original user-facing
// history (any RAG-injected system message MUST be excluded by the caller);
// Response is the assistant turn that answered it.
//
// Request alone drives identity (conversationKey) and stitching (decideStitch),
// so it must stay stable across the turns of one session — an ephemeral
// RAG-injected system message that changes per turn would fork the session.
// RenderedRequest optionally carries the exact model-visible request (including
// that RAG context) for replay fidelity; it never participates in identity or
// stitching and is persisted separately for capture.
type RecordInput struct {
	ConversationID  string                 // explicit id from the chat tool arg; "" if absent
	Request         []conversation.Message // original user-facing history; RAG excluded
	RenderedRequest []conversation.Message // optional effective model-visible request (RAG included); empty => same as Request
	Response        conversation.Message   // assistant turn: content + tool_calls when present
	Model           string                 // model that served the call; "" if unknown
	Provider        string                 // provider instance that served the call; "" if unknown
	RouteOutcome    json.RawMessage        // optional serialized RouteOutcome; nil ok
	SessionHint     string                 // stable MCP session/client id; "" when unavailable
}

// conversationKey derives the base conversation key and its identity source
// (§5). An explicit id always wins; otherwise the key is derived from the
// stable parts of the request so the growing history of one session resolves
// to the same key on every turn.
func conversationKey(in RecordInput) (key, source string) {
	if in.ConversationID != "" {
		return in.ConversationID, identityExplicit
	}
	if first, ok := firstUserContent(in.Request); ok {
		seed := systemContent(in.Request) + "\x00" + first + "\x00" + in.SessionHint
		return sha256Hex(seed), identityDerived
	}
	// No user turn: hash the full canonical request so distinct user-less calls
	// (system-only, tool-only) each become their own conversation.
	return sha256Hex(canonicalMessagesJSON(in.Request)), identityDerived
}

// firstUserContent returns the content of the first user-role message and
// whether one exists.
func firstUserContent(msgs []conversation.Message) (string, bool) {
	for _, m := range msgs {
		if m.Role == "user" {
			return m.Content, true
		}
	}
	return "", false
}

// systemContent concatenates the content of every system-role message in the
// request, preserving order. Messages are NUL-separated (matching the seed's
// field separators) so a multi-message block can never collide with a single
// message that happens to contain the separator. Empty when none were sent.
func systemContent(msgs []conversation.Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == "system" {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\x00")
}
