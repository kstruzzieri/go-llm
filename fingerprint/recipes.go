package fingerprint

import (
	"slices"
	"time"
)

// Probe method names, as stored in capability_evidence.method.
const (
	MethodOllamaShow      = "ollama-show"
	MethodChatStream      = "chat-stream"
	MethodChatStreamTools = "chat-stream-tools"
)

// maxProbeResponseBytes bounds a probe's decoded response body, framing included.
const maxProbeResponseBytes = 1 << 20

// ProbeRecipe is the fixed contract of one probe method: what it can answer,
// what it sends, and what it may spend. Request builders, planning metadata and
// identity encoding read it instead of copying its numbers. A change to
// elicitation, reply acceptance, adapter parsing requirements or limits needs a
// Version bump, which retires every answer the old contract produced.
type ProbeRecipe struct {
	Method  string
	Adapter string // the API the method speaks; rows through any other are not understood
	Version int

	// Capabilities lists what the method can answer. Rows claiming any other
	// capability are not understood.
	Capabilities []string

	UsualRequests int
	MaxRequests   int

	// MaxOutputTokensPerRequest is a limit requested of the server, not one
	// enforced locally or a billing ceiling.
	MaxOutputTokensPerRequest int

	RequestTimeout   time.Duration
	MaxResponseBytes int64

	Message                 string
	ToolName                string
	ToolDescription         string
	ToolParameters          string // JSON Schema
	RequiredToolChoiceFirst bool   // tool_choice "required" on the first request only
}

// probeRecipes is the fixed table. chat-tools, the non-streamed request shape,
// is deliberately absent: it never answers for a streamed route.
var probeRecipes = map[string]ProbeRecipe{
	MethodOllamaShow: {
		Method: MethodOllamaShow, Adapter: AdapterOllama, Version: 1,
		Capabilities:  []string{"chat", "generate", "insert", "embed", "stream", "tool_call", "thinking"},
		UsualRequests: 1, MaxRequests: 1, MaxOutputTokensPerRequest: 0,
		RequestTimeout: 30 * time.Second, MaxResponseBytes: maxProbeResponseBytes,
	},
	MethodChatStream: {
		Method: MethodChatStream, Adapter: AdapterOpenAICompat, Version: 1,
		Capabilities:  []string{"chat", "stream"},
		UsualRequests: 1, MaxRequests: 1, MaxOutputTokensPerRequest: 1,
		RequestTimeout: 3 * time.Minute, MaxResponseBytes: maxProbeResponseBytes,
		Message: "Call get_time.",
	},
	MethodChatStreamTools: {
		Method: MethodChatStreamTools, Adapter: AdapterOpenAICompat, Version: 1,
		Capabilities:  []string{"chat", "stream", "tool_call"},
		UsualRequests: 1, MaxRequests: 2, MaxOutputTokensPerRequest: 2048,
		RequestTimeout: 3 * time.Minute, MaxResponseBytes: maxProbeResponseBytes,
		Message:                 "Call get_time.",
		ToolName:                "get_time",
		ToolDescription:         "Get the current time.",
		ToolParameters:          `{"type":"object","properties":{}}`,
		RequiredToolChoiceFirst: true,
	},
}

// RecipeFor returns a copy of the method's recipe, and false when the method is
// unknown. The table has no registration or mutation API.
func RecipeFor(method string) (ProbeRecipe, bool) {
	recipe, ok := probeRecipes[method]
	recipe.Capabilities = slices.Clone(recipe.Capabilities)
	return recipe, ok
}

// MethodVersion returns the version this binary reads for method, or 0 when the
// method is unknown. A row counts only when its method_version equals this.
func MethodVersion(method string) int {
	return probeRecipes[method].Version
}
