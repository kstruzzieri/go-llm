package mcpclient

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/kstruzzieri/go-llm/signing"
)

const emptyObjectSchema = `{"type":"object"}`

// normalizeSchema converts the SDK's Tool.InputSchema (typed `any`, usually a
// map[string]any from the client) into a JSON Schema object suitable for
// agent.ToolSpec.Parameters. A nil/null schema becomes {"type":"object"}; a
// schema whose top level is not a JSON object, or that fails to marshal, is an
// error so the caller can skip the tool and warn.
func normalizeSchema(in any) (json.RawMessage, error) {
	if in == nil {
		return json.RawMessage(emptyObjectSchema), nil
	}
	raw, err := signing.MarshalCanonical(in)
	if err != nil {
		return nil, fmt.Errorf("marshal input schema: %w", err)
	}
	if bytes.Equal(raw, []byte("null")) {
		return json.RawMessage(emptyObjectSchema), nil
	}
	if len(raw) == 0 || raw[0] != '{' {
		return nil, fmt.Errorf("input schema is not a JSON object")
	}
	return json.RawMessage(raw), nil
}
