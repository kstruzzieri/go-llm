package main

import (
	"context"
	"encoding/json"
)

// toolSchemaSource is the capture-time hook for fetching the tool
// contract that a trace was captured against. A future implementation
// can be backed by the conversation store once tool schemas are
// recorded there; today the only impl is MCP-SDK over stdio/HTTP.
//
// Snapshot is called exactly once per capture run, before any
// conversation is converted to a trace. Returning a nil/empty slice
// is honored — capture writes traces with an empty trace.Tools and the
// replay-time validator records ToolArgsValid as not-computed.
type toolSchemaSource interface {
	Snapshot(ctx context.Context) ([]json.RawMessage, error)
}

// staticToolSchemaSource is a test helper that returns a fixed slice
// of pre-marshaled tool entries. It exists so unit tests do not need a
// live MCP transport.
type staticToolSchemaSource struct {
	tools []json.RawMessage
}

func (s staticToolSchemaSource) Snapshot(context.Context) ([]json.RawMessage, error) {
	if s.tools == nil {
		return []json.RawMessage{}, nil
	}
	out := make([]json.RawMessage, len(s.tools))
	copy(out, s.tools)
	return out, nil
}
