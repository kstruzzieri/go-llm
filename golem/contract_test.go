package golem_test

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/kstruzzieri/go-llm/golem"
)

func TestV1EventFixturePinsSerializedContract(t *testing.T) {
	expected := []golem.Event{
		{Protocol: 1, ThreadID: "thread-1", RunID: "run-success", Seq: 1, Type: "run.started", Payload: json.RawMessage(`{}`)},
		{Protocol: 1, ThreadID: "thread-1", RunID: "run-success", Seq: 2, Type: "message.delta", Payload: json.RawMessage(`{"messageId":"run-success:0","text":"hello"}`)},
		{Protocol: 1, ThreadID: "thread-1", RunID: "run-success", Seq: 3, Type: "tool.started", Payload: json.RawMessage(`{"toolCallId":"call-1","name":"read_file","preview":"main.go"}`)},
		{Protocol: 1, ThreadID: "thread-1", RunID: "run-success", Seq: 4, Type: "tool.finished", Payload: json.RawMessage(`{"toolCallId":"call-1","name":"read_file","preview":"42 lines","isError":false}`)},
		{Protocol: 1, ThreadID: "thread-1", RunID: "run-success", Seq: 5, Type: "run.finished", Payload: json.RawMessage(`{"stopReason":"completed","model":"local/model"}`)},
		{Protocol: 1, ThreadID: "thread-1", RunID: "run-failure", Seq: 1, Type: "run.started", Payload: json.RawMessage(`{}`)},
		{Protocol: 1, ThreadID: "thread-1", RunID: "run-failure", Seq: 2, Type: "run.failed", Payload: json.RawMessage(`{"code":"provider_unavailable","message":"provider unavailable"}`)},
		{Protocol: 1, ThreadID: "thread-1", RunID: "run-cancel", Seq: 1, Type: "run.started", Payload: json.RawMessage(`{}`)},
		{Protocol: 1, ThreadID: "thread-1", RunID: "run-cancel", Seq: 2, Type: "run.canceled", Payload: json.RawMessage(`{}`)},
	}
	want, err := json.MarshalIndent(expected, "", "  ")
	if err != nil {
		t.Fatalf("marshal expected fixture: %v", err)
	}
	want = append(want, '\n')
	got, err := os.ReadFile("testdata/events-v1.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("events-v1.json does not match the v1 Event serialization\nwant:\n%s\ngot:\n%s", want, got)
	}
}
