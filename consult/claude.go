package consult

// claudeAdapter is the consultant kind selector for the Claude CLI adapter.
const claudeAdapter = "claude"

// claudeModels is the evidence-backed selector set (Stage 0 proved opus only).
var claudeModels = map[string]bool{"opus": true}

// claudeSupportedVersions is the evidence-approved version set. A new
// version needs renewed Stage 0 control evidence before it is added.
var claudeSupportedVersions = map[string]bool{"2.1.240": true}

// claudeSystemPrompt is the fixed system prompt forced on every consult
// invocation: advisory text only, no tool or filesystem/network access.
const claudeSystemPrompt = "Give advisory text only. Do not use tools or access files or networks."

// claudeArgs is the adapter-owned argv: the E2 fixed launch contract plus
// --setting-sources= (E2.1) and stream-json + verbose (E2.2). No template,
// no shell, no caller-supplied arguments.
func claudeArgs(model string) []string {
	return []string{
		"-p", "--safe-mode", "--tools", "", "--allowedTools", "",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--no-session-persistence", "--disable-slash-commands", "--no-chrome",
		"--model", model,
		"--system-prompt", claudeSystemPrompt,
		"--setting-sources=",
		"--output-format", "stream-json", "--verbose",
	}
}
