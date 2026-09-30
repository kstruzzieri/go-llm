package consult

const codexAdapter = "codex"
const codexVersion = "0.153.4"

// codexModels is the evidence-backed selector set for both Codex transports.
var codexModels = map[string]bool{"gpt-6-astra": true}

// codexArgs is the Stage 0 fixed native-runtime profile. These controls reduce
// capabilities; they do not establish independent host-resource isolation.
func codexArgs(model string) []string {
	return []string{
		"exec", "--strict-config", "--skip-git-repo-check", "--ephemeral",
		"--ignore-user-config", "--ignore-rules", "--color", "never", "--json",
		"--sandbox", "read-only", "--model", model,
		"-c", `approval_policy="never"`, "-c", "project_doc_max_bytes=0", "-c", `web_search="disabled"`,
		"-c", "features.hooks=false", "-c", "features.apps=false", "-c", "features.plugins=false",
		"-c", "features.external_agent_memory_import=false", "-c", "features.memories=false",
		"-c", "features.goals=false", "-c", "features.image_generation=false",
		"-c", "features.multi_agent_v2=false", "-c", "agents.enabled=false",
		"-c", "orchestrator.skills.enabled=false", "-c", "skills.bundled.enabled=false",
		"-c", "orchestrator.mcp.enabled=false", "-c", "tools.update_plan.enabled=false",
		"-c", "tools.experimental_request_user_input.enabled=false", "-",
	}
}
