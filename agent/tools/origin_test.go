package tools

import (
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// Every built-in declares its provenance (#436 spec D4). A new tool that ships
// without a declaration is inspected as unknown, which detectors may block;
// add it here when you add the tool.
func TestBuiltInToolsDeclareOrigin(t *testing.T) {
	cases := []struct {
		name string
		tool agent.Tool
		want agent.Origin
	}{
		{"read_file", &ReadFile{}, agent.OriginWorkspace},
		{"search", &Search{}, agent.OriginWorkspace},
		{"glob", &Glob{}, agent.OriginWorkspace},
		{"list", &List{}, agent.OriginWorkspace},
		{"edit_file", &EditFile{}, agent.OriginWorkspace},
		{"write_file", &WriteFile{}, agent.OriginWorkspace},
		{"run_command", &RunCommand{}, agent.OriginWorkspace},
		{"start_command", &StartCommand{}, agent.OriginWorkspace},
		{"command_status", &CommandStatus{}, agent.OriginWorkspace},
		{"command_tail", &CommandTail{}, agent.OriginWorkspace},
		{"stop_command", &StopCommand{}, agent.OriginWorkspace},
		{"scratch_changes", ScratchChanges{}, agent.OriginWorkspace},
		{"promote_artifact", &PromoteArtifact{}, agent.OriginWorkspace},
		{"retrieve", Retrieve{}, agent.OriginWorkspace},
		{"memory_search", MemorySearch{}, agent.OriginWorkspace},
		{"agent_memory_search", AgentMemorySearch{}, agent.OriginWorkspace},
		{"agent_memory_create", AgentMemoryCreate{}, agent.OriginWorkspace},
		{"agent_memory_promote", AgentMemoryPromote{}, agent.OriginWorkspace},
		{"delegate_code", &DelegateCode{}, agent.OriginModel},
		{"dispatch", &Dispatch{}, agent.OriginModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ot, ok := tc.tool.(agent.OriginTool)
			if !ok {
				t.Fatalf("%s does not implement agent.OriginTool", tc.name)
			}
			if got := ot.Origin(); got != tc.want {
				t.Fatalf("Origin() = %s, want %s", got, tc.want)
			}
		})
	}
}
