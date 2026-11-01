package agentflow

import "encoding/json"

// RecordedBlocker is one current blocked or failed attempt close from
// next-action's recorded_blockers (AgentFlow 1.2, agentflow#72). A nil pointer
// is JSON null or absent.
type RecordedBlocker struct {
	StepID, AttemptID, Event         string
	Reason, RecordedAt, AttemptOwner *string
}

// RecordedBlockers is the display view of recorded_blockers.
type RecordedBlockers struct {
	Current    []RecordedBlocker // superseded_by null, in ledger order
	Malformed  int               // elements that break the shape rules
	NotAnArray bool              // present, not null, and not an array
}

// ParseRecordedBlockers reads next-action's recorded_blockers for display
// (#731). It never fails: the field is display-only, so a bad shape is counted
// rather than allowed to change what Golem decides.
func ParseRecordedBlockers(raw json.RawMessage) RecordedBlockers {
	var view RecordedBlockers
	if len(raw) == 0 || string(raw) == "null" {
		return view
	}
	var elements []json.RawMessage
	if json.Unmarshal(raw, &elements) != nil {
		view.NotAnArray = true
		return view
	}
	for _, element := range elements {
		blocker, current, ok := parseRecordedBlocker(element)
		switch {
		case !ok:
			view.Malformed++
		case current:
			view.Current = append(view.Current, blocker)
		}
	}
	return view
}

// parseRecordedBlocker applies the shape rules: an object whose step_id,
// attempt_id and event are non-empty strings, whose superseded_by is present
// and null or a string, and whose reason, recorded_at and attempt_owner are a
// string, null or absent. Unknown keys are ignored. current reports
// superseded_by null.
func parseRecordedBlocker(element json.RawMessage) (blocker RecordedBlocker, current, ok bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(element, &fields) != nil || fields == nil {
		return RecordedBlocker{}, false, false
	}
	for key, dst := range map[string]*string{"step_id": &blocker.StepID, "attempt_id": &blocker.AttemptID, "event": &blocker.Event} {
		if json.Unmarshal(fields[key], dst) != nil || *dst == "" {
			return RecordedBlocker{}, false, false
		}
	}
	superseded, present := fields["superseded_by"]
	var supersededBy *string
	if !present || json.Unmarshal(superseded, &supersededBy) != nil {
		return RecordedBlocker{}, false, false
	}
	for key, dst := range map[string]**string{"reason": &blocker.Reason, "recorded_at": &blocker.RecordedAt, "attempt_owner": &blocker.AttemptOwner} {
		if value, present := fields[key]; present && json.Unmarshal(value, dst) != nil {
			return RecordedBlocker{}, false, false
		}
	}
	return blocker, supersededBy == nil, true
}
