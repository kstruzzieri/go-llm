package transcript

import (
	"testing"

	"github.com/kstruzzieri/go-llm/conversation"
)

func mustKey(t *testing.T, in RecordInput) string {
	t.Helper()
	k, _ := conversationKey(in)
	return k
}

func TestConversationKey_ExplicitWins(t *testing.T) {
	in := RecordInput{
		ConversationID: "conv-123",
		Request:        []conversation.Message{{Role: "user", Content: "hi"}},
	}
	key, src := conversationKey(in)
	if key != "conv-123" || src != identityExplicit {
		t.Errorf("got (%q,%q), want (conv-123, explicit)", key, src)
	}
}

func TestConversationKey_DerivedStableAcrossTurns(t *testing.T) {
	turn1 := RecordInput{Request: []conversation.Message{
		{Role: "system", Content: "be helpful"},
		{Role: "user", Content: "hi"},
	}}
	turn2 := RecordInput{Request: []conversation.Message{
		{Role: "system", Content: "be helpful"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "more"},
	}}
	k1, s1 := conversationKey(turn1)
	k2, s2 := conversationKey(turn2)
	if k1 != k2 {
		t.Errorf("derived key not stable across turns: %q vs %q", k1, k2)
	}
	if s1 != identityDerived || s2 != identityDerived {
		t.Errorf("identity source = (%q,%q), want derived", s1, s2)
	}
}

func TestConversationKey_SessionHintChangesKey(t *testing.T) {
	base := RecordInput{Request: []conversation.Message{{Role: "user", Content: "hi"}}}
	withHint := base
	withHint.SessionHint = "sess-A"
	if mustKey(t, base) == mustKey(t, withHint) {
		t.Error("session hint should change the derived key")
	}
}

func TestConversationKey_NoUserMessageHashesFullRequest(t *testing.T) {
	a := RecordInput{Request: []conversation.Message{{Role: "system", Content: "A"}}}
	b := RecordInput{Request: []conversation.Message{{Role: "system", Content: "B"}}}
	ka, sa := conversationKey(a)
	kb, _ := conversationKey(b)
	if ka == kb {
		t.Error("distinct user-less requests must not collapse to one key")
	}
	if sa != identityDerived {
		t.Errorf("identity source = %q, want derived", sa)
	}
}

func TestConversationKey_NilRequestProducesStableKey(t *testing.T) {
	in := RecordInput{} // nil Request, no ConversationID
	k1, s1 := conversationKey(in)
	k2, _ := conversationKey(in)
	if k1 != k2 {
		t.Errorf("nil-request key not stable: %q vs %q", k1, k2)
	}
	if s1 != identityDerived {
		t.Errorf("identity source = %q, want derived", s1)
	}
}
