package golem_test

import (
	"strings"
	"testing"

	golem "github.com/kstruzzieri/go-llm/golem"
)

// backgroundToolNames are the four background exec tools (#346) the allowExec
// prompt must describe.
var backgroundToolNames = []string{"start_command", "command_status", "command_tail", "stop_command"}

func TestSystemPromptDescribesBackgroundToolsWithExec(t *testing.T) {
	p := golem.SystemPrompt(false, true)
	for _, want := range backgroundToolNames {
		if !strings.Contains(p, want) {
			t.Errorf("exec prompt missing background tool %q:\n%s", want, p)
		}
	}
	for _, want := range []string{
		"long-running", // start_command is for long-running commands
		"next_cursor",  // cursor-based incremental tail polling
		"bounded",      // output is a bounded tail ring; read promptly
		"stop_command", // explicit stop
		"they approve", // stop prompts the user
		"manager",      // jobs are owned by the session manager
	} {
		if !strings.Contains(p, want) {
			t.Errorf("exec prompt missing background guidance %q:\n%s", want, p)
		}
	}
}

func TestSystemPromptOmitsBackgroundToolsWithoutExec(t *testing.T) {
	for _, p := range []string{golem.SystemPrompt(false, false), golem.SystemPrompt(true, false)} {
		for _, banned := range backgroundToolNames {
			if strings.Contains(p, banned) {
				t.Errorf("no-exec prompt must not mention %q:\n%s", banned, p)
			}
		}
		if strings.Contains(p, "next_cursor") {
			t.Errorf("no-exec prompt must not carry tail-cursor guidance:\n%s", p)
		}
	}
}

func TestSystemPromptClaimsNoDescendantContainment(t *testing.T) {
	// Escaped descendants (setsid, double-fork) are unsupported; the prompt
	// must not promise whole-tree containment the manager cannot deliver.
	p := golem.SystemPrompt(true, true)
	for _, over := range []string{"descendant", "process tree", "all children"} {
		if strings.Contains(p, over) {
			t.Errorf("exec prompt over-claims containment with %q:\n%s", over, p)
		}
	}
}
