package golem

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncatePreviewBoundsUTF8WithMarker(t *testing.T) {
	input := strings.Repeat("a", 8*1024-1) + "界tail"
	got := truncatePreview(input)

	if len(got) > 8*1024 {
		t.Fatalf("preview bytes = %d, want at most 8192", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("preview is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Fatalf("preview suffix = %q, want explicit truncation marker", got[len(got)-min(len(got), 32):])
	}
	if got == input {
		t.Fatal("oversized preview was not truncated")
	}
	if want := strings.Repeat("a", 8*1024); truncatePreview(want) != want {
		t.Fatal("preview at the limit was changed")
	}
}

func TestTruncateErrorMessageBoundsUTF8WithMarker(t *testing.T) {
	input := strings.Repeat("界", 8*1024) + "tail"
	got := truncateErrorMessage(input)

	if len(got) > 8*1024 {
		t.Fatalf("error message bytes = %d, want at most 8192", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("error message is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Fatalf("error message lacks truncation marker: %q", got[len(got)-min(len(got), 32):])
	}
}

func TestSplitDeltaAccountsForEscapedJSONAndPreservesUTF8(t *testing.T) {
	text := strings.Repeat("\"😀\\\n", 24*1024)
	fits := func(candidate string) (bool, error) {
		event, err := deltaTestEvent(candidate)
		return len(event) <= 128*1024, err
	}

	chunks, err := splitDelta(text, fits)
	if err != nil {
		t.Fatalf("splitDelta: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want JSON escaping to require more than one", len(chunks))
	}
	if got := strings.Join(chunks, ""); got != text {
		t.Fatalf("joined chunks differ: got %d bytes, want %d", len(got), len(text))
	}
	for i, chunk := range chunks {
		if chunk == "" {
			t.Fatalf("chunk %d is empty", i)
		}
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is not valid UTF-8", i)
		}
		event, err := deltaTestEvent(chunk)
		if err != nil {
			t.Fatalf("marshal chunk %d: %v", i, err)
		}
		if len(event) > 128*1024 {
			t.Fatalf("chunk %d event bytes = %d, want at most 131072", i, len(event))
		}
	}
}

func TestSplitDeltaRejectsRuneThatCannotFit(t *testing.T) {
	_, err := splitDelta("界", func(string) (bool, error) { return false, nil })
	if !errors.Is(err, errEventTooLarge) {
		t.Fatalf("splitDelta error = %v, want errEventTooLarge", err)
	}
}

func TestValidateEventSizeRejectsOversizedEnvelope(t *testing.T) {
	payload, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: strings.Repeat("\\", 128*1024)})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	event := Event{
		Protocol: ProtocolVersion,
		RunID:    "run-1",
		Seq:      1,
		Type:     "message.delta",
		Payload:  payload,
	}

	if err := validateEventSize(event); !errors.Is(err, errEventTooLarge) {
		t.Fatalf("validateEventSize error = %v, want errEventTooLarge", err)
	}
}

func deltaTestEvent(text string) ([]byte, error) {
	payload, err := json.Marshal(struct {
		MessageID string `json:"messageId"`
		Text      string `json:"text"`
	}{
		MessageID: strings.Repeat("id", 512),
		Text:      text,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(Event{
		Protocol: ProtocolVersion,
		ThreadID: strings.Repeat("thread", 128),
		RunID:    strings.Repeat("run", 128),
		Seq:      999999,
		Type:     "message.delta",
		Payload:  payload,
	})
}
