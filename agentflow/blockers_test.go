package agentflow

import (
	"encoding/json"
	"reflect"
	"testing"
)

func blockerText(s string) *string { return &s }

// #731 B2: recorded_blockers is display-only, so the parser never fails: it
// keeps current, well-formed entries in ledger order and counts the rest.
func TestParseRecordedBlockers(t *testing.T) {
	mixed := `[
		{"step_id":"P1","attempt_id":"A1","event":"blocked","reason":"golem: gate execution failed","recorded_at":"2026-10-31T15:30:06+00:00","attempt_owner":"golem","superseded_by":null},
		{"step_id":"P1","attempt_id":"A2","event":"failed","reason":"r","recorded_at":null,"attempt_owner":null,"superseded_by":"A3"},
		{"step_id":"P2","attempt_id":"A4","event":"failed","reason":null,"recorded_at":null,"attempt_owner":null,"superseded_by":null},
		{"step_id":"P3","attempt_id":"A5","event":"abandoned_future","superseded_by":null,"future":true},
		{"step_id":"P4","attempt_id":"A6","event":"blocked","reason":"x"},
		{"step_id":"","attempt_id":"A7","event":"blocked","superseded_by":null},
		{"step_id":null,"attempt_id":"A8","event":"blocked","superseded_by":null},
		{"attempt_id":"A9","event":"blocked","superseded_by":null},
		{"step_id":"P5","attempt_id":7,"event":"blocked","superseded_by":null},
		{"step_id":"P5","attempt_id":"A10","event":"","superseded_by":null},
		{"step_id":"P5","attempt_id":"A11","event":"blocked","reason":7,"superseded_by":null},
		{"step_id":"P5","attempt_id":"A12","event":"blocked","superseded_by":7},
		"x"
	]`
	for _, tt := range []struct {
		name string
		raw  json.RawMessage
		want RecordedBlockers
	}{
		{"absent", nil, RecordedBlockers{}},
		{"null", json.RawMessage(`null`), RecordedBlockers{}},
		{"number", json.RawMessage(`5`), RecordedBlockers{NotAnArray: true}},
		{"string", json.RawMessage(`"x"`), RecordedBlockers{NotAnArray: true}},
		{"object", json.RawMessage(`{}`), RecordedBlockers{NotAnArray: true}},
		{"empty", json.RawMessage(`[]`), RecordedBlockers{}},
		{"mixed", json.RawMessage(mixed), RecordedBlockers{
			Current: []RecordedBlocker{
				{StepID: "P1", AttemptID: "A1", Event: "blocked", Reason: blockerText("golem: gate execution failed"),
					RecordedAt: blockerText("2026-10-31T15:30:06+00:00"), AttemptOwner: blockerText("golem")},
				{StepID: "P2", AttemptID: "A4", Event: "failed"},
				{StepID: "P3", AttemptID: "A5", Event: "abandoned_future"},
			},
			Malformed: 9,
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseRecordedBlockers(tt.raw); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}
