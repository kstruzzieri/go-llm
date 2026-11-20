package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatResponse_FinishReasonJSON(t *testing.T) {
	b, err := json.Marshal(ChatResponse{FinishReason: "length"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"finish_reason":"length"`) {
		t.Errorf("set: %s lacks finish_reason", b)
	}
	b, err = json.Marshal(ChatResponse{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "finish_reason") {
		t.Errorf("empty: %s carries finish_reason", b)
	}
}
