package agenttrace

import "github.com/kstruzzieri/go-llm/agent"

// BuildTrace assembles a content-full trace from request metadata and the final
// (or partial) agent.Result. status/partial are computed by the caller because
// agent.StopReason defaults to Completed and is meaningless on an error return.
// RunID, StartedAt, and EndedAt are stamped by the caller after BuildTrace.
func BuildTrace(meta TraceMeta, res agent.Result, status string, partial bool, runErr error) TraceRecord {
	errStr := ""
	if runErr != nil {
		errStr = runErr.Error()
	}
	// TraceMeta and TraceRequest are identical in shape (TraceRequest only adds
	// JSON tags), so a direct conversion keeps the two in lockstep — a field
	// drift becomes a compile error here rather than a silent omission.
	return TraceRecord{
		SchemaVersion: SchemaVersion,
		Status:        status,
		Partial:       partial,
		Request:       TraceRequest(meta),
		Result:        res,
		Error:         errStr,
	}
}
