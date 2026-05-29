// mcp/tools_chat_transcript_test.go
package mcp

import (
	"encoding/json"
	"testing"
)

func TestChatArgs_ParsesConversationID(t *testing.T) {
	var args chatArgs
	raw := `{"messages":[{"role":"user","content":"hi"}],"conversation_id":"conv-1"}`
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if args.ConversationID != "conv-1" {
		t.Errorf("ConversationID = %q, want conv-1", args.ConversationID)
	}
}
