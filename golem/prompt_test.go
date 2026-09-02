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

func TestSystemPromptHeadlessNamesOnlyMountedTools(t *testing.T) {
	gated := []string{"write_file", "edit_file", "run_command", "start_command", "stop_command"}
	cases := []struct {
		name     string
		caps     golem.HeadlessToolCaps
		mentions []string // gated names that MUST appear
	}{
		{"exec only", golem.HeadlessToolCaps{RunCommand: true}, []string{"run_command"}},
		{"write pair", golem.HeadlessToolCaps{WriteFile: true, EditFile: true}, []string{"write_file", "edit_file"}},
		{"write only", golem.HeadlessToolCaps{WriteFile: true}, []string{"write_file"}},
		{"start without stop", golem.HeadlessToolCaps{StartCommand: true}, []string{"start_command", "command_status", "command_tail"}},
		{"start with stop", golem.HeadlessToolCaps{StartCommand: true, StopCommand: true}, []string{"start_command", "command_status", "command_tail", "stop_command"}},
		{"nothing", golem.HeadlessToolCaps{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := golem.SystemPromptHeadless(tc.caps)
			for _, want := range tc.mentions {
				if !strings.Contains(got, want) {
					t.Errorf("prompt must name mounted tool %q:\n%s", want, got)
				}
			}
			// The honesty half: no unmounted GATED tool may be named. The
			// ungated readers are companions of start_command only.
			mentioned := map[string]bool{}
			for _, m := range tc.mentions {
				mentioned[m] = true
			}
			for _, g := range gated {
				if !mentioned[g] && strings.Contains(got, g) {
					t.Errorf("prompt names unmounted gated tool %q:\n%s", g, got)
				}
			}
			if !tc.caps.StartCommand {
				for _, reader := range []string{"command_status", "command_tail"} {
					if strings.Contains(got, reader) {
						t.Errorf("prompt names %q without start_command:\n%s", reader, got)
					}
				}
			}
			// Headless runs have no approver: interactive-approval prose is a lie.
			if strings.Contains(got, "after they approve") || strings.Contains(got, "shown to the user") {
				t.Errorf("headless prompt must not describe interactive approval:\n%s", got)
			}
		})
	}
}
