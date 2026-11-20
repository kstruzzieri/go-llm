package fingerprint

import "strings"

// TemplateUsesSuffix reports whether an Ollama prompt template references
// .Suffix, which is how a model supports fill-in-the-middle insertion.
func TemplateUsesSuffix(template string) bool {
	return strings.Contains(template, ".Suffix")
}

// OllamaCapabilityFacts maps what Ollama's /api/show reported to capability
// answers. It is shared by the native prober and the provider adapter so the two
// cannot drift.
//
// Tokens are matched case-insensitively. completion gives chat, generate and
// stream; tools or tool_call gives tool_call; embedding or embed gives embed;
// thinking gives thinking; insert gives generate, stream and insert; the other
// canonical names give themselves. Unknown tokens are ignored. A non-nil array
// without tools or tool_call answers tool_call no, as the native prober does; no
// other negative is inferred from a missing token, and a nil array supplies no
// array-derived facts. A nonempty template independently answers insert yes or
// no by its use of .Suffix, overriding an insert token; an empty template makes
// no claim.
func OllamaCapabilityFacts(capabilities []string, template string) map[string]CapProbeState {
	facts := map[string]CapProbeState{}
	if capabilities != nil {
		tools := false
		for _, token := range capabilities {
			var names []string
			switch strings.ToLower(token) {
			case "completion":
				names = []string{"chat", "generate", "stream"}
			case "insert":
				names = []string{"generate", "stream", "insert"}
			case "tools", "tool_call":
				names, tools = []string{"tool_call"}, true
			case "embedding", "embed":
				names = []string{"embed"}
			case "chat", "generate", "stream", "thinking":
				names = []string{strings.ToLower(token)}
			}
			for _, name := range names {
				facts[name] = CapProbeYes
			}
		}
		if !tools {
			facts["tool_call"] = CapProbeNo
		}
	}
	if template != "" {
		if TemplateUsesSuffix(template) {
			facts["insert"] = CapProbeYes
		} else {
			facts["insert"] = CapProbeNo
		}
	}
	return facts
}
