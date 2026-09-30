package consult

import (
	"reflect"
	"testing"
)

func TestCodexArgs(t *testing.T) {
	expected := []string{"exec", "--strict-config", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--color", "never", "--json", "--sandbox", "read-only", "--model", "gpt-6-astra", "-c", `approval_policy="never"`, "-c", "project_doc_max_bytes=0", "-c", `web_search="disabled"`, "-c", "features.hooks=false", "-c", "features.apps=false", "-c", "features.plugins=false", "-c", "features.external_agent_memory_import=false", "-c", "features.memories=false", "-c", "features.goals=false", "-c", "features.image_generation=false", "-c", "features.multi_agent_v2=false", "-c", "agents.enabled=false", "-c", "orchestrator.skills.enabled=false", "-c", "skills.bundled.enabled=false", "-c", "orchestrator.mcp.enabled=false", "-c", "tools.update_plan.enabled=false", "-c", "tools.experimental_request_user_input.enabled=false", "-"}
	got := codexArgs("gpt-6-astra")
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("codexArgs = %q, want %q", got, expected)
	}
	got[0] = "changed"
	if !reflect.DeepEqual(codexArgs("gpt-6-astra"), expected) {
		t.Fatal("codexArgs shared mutable backing array")
	}
}
