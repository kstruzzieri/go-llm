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
	return TraceRecord{
		SchemaVersion: SchemaVersion,
		Status:        status,
		Partial:       partial,
		Request: TraceRequest{
			Goal:           meta.Goal,
			System:         meta.System,
			HistorySummary: meta.HistorySummary,
			History:        meta.History,
			ToolSchemaHash: meta.ToolSchemaHash,
			ModelHint:      meta.ModelHint,
			MaxSteps:       meta.MaxSteps,
			Budget:         meta.Budget,
		},
		Result: res,
		Error:  errStr,
	}
}
