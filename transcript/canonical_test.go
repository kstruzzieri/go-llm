package transcript

import (
	"encoding/json"
	"testing"

	"github.com/kstruzzieri/go-llm/conversation"
)

func TestCanonicalToolCallsJSON_NormalizesAbsence(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"empty", ``},
		{"null", `null`},
		{"empty array", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalToolCallsJSON(json.RawMessage(tc.raw)); got != "" {
				t.Errorf("canonicalToolCallsJSON(%q) = %q, want \"\"", tc.raw, got)
			}
		})
	}
}

func TestCanonicalToolCallsJSON_StableKeyOrder(t *testing.T) {
	a := json.RawMessage(`[{"id":"c1","type":"function","function":{"name":"x","index":0,"arguments":{"b":2,"a":1}}}]`)
	b := json.RawMessage(`[{"type":"function","id":"c1","function":{"arguments":{"a":1,"b":2},"index":0,"name":"x"}}]`)
	if canonicalToolCallsJSON(a) != canonicalToolCallsJSON(b) {
		t.Errorf("key-order/whitespace differences produced different canonical forms:\n a=%s\n b=%s",
			canonicalToolCallsJSON(a), canonicalToolCallsJSON(b))
	}
}

func TestCanonicalMessage_TreatsAbsenceUniformly(t *testing.T) {
	withNull := conversation.Message{Role: "assistant", Content: "hi", ToolCalls: json.RawMessage(`null`)}
	without := conversation.Message{Role: "assistant", Content: "hi"}
	if canonicalMessage(withNull) != canonicalMessage(without) {
		t.Error("null tool_calls should canonicalize identically to absent tool_calls")
	}
}

func TestSHA256Hex_Deterministic(t *testing.T) {
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" // SHA-256("abc"), NIST vector
	got := sha256Hex("abc")
	if got != want {
		t.Errorf("sha256Hex(%q) = %q, want %q", "abc", got, want)
	}
	if len(got) != 64 {
		t.Errorf("sha256Hex length = %d, want 64", len(got))
	}
}
