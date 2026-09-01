package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestParseOutputFormat(t *testing.T) {
	cases := []struct {
		in      string
		want    outputFormat
		wantErr bool
	}{
		{"", outputText, false},
		{"text", outputText, false},
		{"json", outputJSON, false},
		{"stream-json", outputStreamJSON, false},
		{"TEXT", 0, true}, // exact match only
		{"jsonl", 0, true},
		{"stream_json", 0, true}, // underscore is not the spelling
		{" json", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseOutputFormat(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseOutputFormat(%q) = %v, want error", tc.in, got)
				}
				for _, want := range []string{"text", "json", "stream-json"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q must list the accepted value %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOutputFormat(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseOutputFormat(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestOutputFormatMachineReportsOnlyJSONModes(t *testing.T) {
	if outputText.machine() {
		t.Error("text is not a machine format")
	}
	if !outputJSON.machine() || !outputStreamJSON.machine() {
		t.Error("json and stream-json are machine formats")
	}
}

func TestValidateFlagsOutputFormatRequiresOneShot(t *testing.T) {
	err := validateFlags(flags{outputFormat: "json"})
	if err == nil {
		t.Fatal("-output-format without -p must be rejected")
	}
	if exitCodeFor(err) != 1 {
		t.Errorf("the requires-p rejection is a mode error, not a headless usage error: exit %d, want 1", exitCodeFor(err))
	}
	if err := validateFlags(flags{outputFormat: "json", promptSet: true, prompt: "hi"}); err != nil {
		t.Fatalf("-output-format with -p must be accepted: %v", err)
	}
	if err := validateFlags(flags{outputFormat: "bogus", promptSet: true, prompt: "hi"}); err == nil {
		t.Fatal("unknown -output-format must be rejected")
	}
	// The default (unset) must stay valid in every mode, including the REPL.
	if err := validateFlags(flags{}); err != nil {
		t.Fatalf("default flags must stay valid: %v", err)
	}
	if err := validateFlags(flags{outputFormat: "text"}); err != nil {
		t.Fatalf("an explicit text format is valid without -p (it is the default behavior): %v", err)
	}
}

func TestResolveStdinPromptReadsToEOF(t *testing.T) {
	got, err := resolveStdinPrompt(strings.NewReader("summarize this diff\n"), false)
	if err != nil {
		t.Fatalf("resolveStdinPrompt: %v", err)
	}
	if got != "summarize this diff\n" {
		t.Errorf("prompt = %q, want the stdin bytes verbatim", got)
	}
}

func TestResolveStdinPromptRejectsTTY(t *testing.T) {
	_, err := resolveStdinPrompt(strings.NewReader("ignored"), true)
	if err == nil {
		t.Fatal("a TTY stdin must fail fast")
	}
	if exitCodeFor(err) != 2 {
		t.Errorf("TTY refusal must be a usage error (exit 2), got %d", exitCodeFor(err))
	}
	if !strings.Contains(err.Error(), "pipe") {
		t.Errorf("error %q must give usage guidance mentioning a pipe", err)
	}
}

func TestResolveStdinPromptRejectsEmptyAndBlank(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n\t\n"} {
		_, err := resolveStdinPrompt(strings.NewReader(in), false)
		if err == nil {
			t.Fatalf("stdin %q must be rejected as an empty prompt", in)
		}
		if exitCodeFor(err) != 2 {
			t.Errorf("empty stdin must be a usage error (exit 2), got %d", exitCodeFor(err))
		}
	}
}

func TestResolveStdinPromptEnforcesByteCap(t *testing.T) {
	// Exactly at the cap is admitted; one byte over is refused. The cap equals
	// maxGoalBytes, which is what main.go already passes as the runtime's
	// MaxMessageBytes, so the CLI never admits input the runtime would reject.
	atCap := strings.Repeat("a", maxGoalBytes)
	got, err := resolveStdinPrompt(strings.NewReader(atCap), false)
	if err != nil {
		t.Fatalf("a prompt exactly at the cap must be admitted: %v", err)
	}
	if len(got) != maxGoalBytes {
		t.Errorf("len(prompt) = %d, want %d", len(got), maxGoalBytes)
	}
	_, err = resolveStdinPrompt(strings.NewReader(atCap+"a"), false)
	if err == nil {
		t.Fatal("a prompt one byte over the cap must be refused")
	}
	if exitCodeFor(err) != 2 {
		t.Errorf("over-cap stdin must be a usage error (exit 2), got %d", exitCodeFor(err))
	}
}

// stdinFileWith returns run()-compatible stdio files with the given bytes
// preloaded on stdin, positioned at the start.
func stdinFileWith(t *testing.T, content string) (stdin, stdout, stderr *os.File) {
	t.Helper()
	stdin, stdout, stderr = runTestFiles(t)
	if _, err := stdin.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return stdin, stdout, stderr
}

func TestRunStdinPromptOverCapExitsTwo(t *testing.T) {
	// Drives the real run() wiring, not just the helper: a giant piped prompt
	// must be refused as a usage error before any provider work happens.
	stdin, stdout, stderr := stdinFileWith(t, strings.Repeat("a", maxGoalBytes+1))
	err := run([]string{"-p", "-"}, stdin, stdout, stderr)
	if exitCodeFor(err) != 2 {
		t.Fatalf("exitCodeFor(%v) = %d, want 2", err, exitCodeFor(err))
	}
}

func TestRunStdinPromptEmptyExitsTwo(t *testing.T) {
	stdin, stdout, stderr := stdinFileWith(t, "   \n")
	err := run([]string{"-p", "-"}, stdin, stdout, stderr)
	if exitCodeFor(err) != 2 {
		t.Fatalf("exitCodeFor(%v) = %d, want 2", err, exitCodeFor(err))
	}
}

func TestStdinPromptRequestedOnlyForTheSentinel(t *testing.T) {
	cases := []struct {
		f    flags
		want bool
	}{
		{flags{promptSet: true, prompt: "-"}, true},
		{flags{promptSet: true, prompt: "hello"}, false},
		{flags{promptSet: true, prompt: "--"}, false},
		{flags{promptSet: false, prompt: "-"}, false},
	}
	for _, tc := range cases {
		if got := stdinPromptRequested(tc.f); got != tc.want {
			t.Errorf("stdinPromptRequested(%+v) = %v, want %v", tc.f, got, tc.want)
		}
	}
}

func TestAllowToolSetAcceptsExactlyTheSupportedGatedTools(t *testing.T) {
	// The frozen list (#341/#346). Anything else is a hard error before the run.
	for _, name := range []string{"write_file", "edit_file", "run_command", "start_command", "stop_command"} {
		set, err := newAllowToolSet([]string{name})
		if err != nil {
			t.Fatalf("newAllowToolSet(%q): %v", name, err)
		}
		if !set.authorized(name) {
			t.Errorf("%q must be authorized after being named", name)
		}
	}
}

func TestAllowToolSetRejectsExcludedAndUnknownNames(t *testing.T) {
	for _, name := range []string{
		"verify_command",    // synthetic, never a registered tool (#347)
		"submit_plan",       // planning mode only
		"mcp__server__tool", // MCP is excluded by contract
		"command_status",    // ungated; nothing to authorize
		"command_tail",      // ungated; nothing to authorize
		"read_file",         // ungated
		"run_commands", "Run_Command", // typo and wrong case
		"*", "", "  run_command",
	} {
		_, err := newAllowToolSet([]string{name})
		if err == nil {
			t.Fatalf("newAllowToolSet(%q) must fail", name)
		}
		if exitCodeFor(err) != 2 {
			t.Errorf("%q: unknown -allow-tool must be a usage error (exit 2), got %d", name, exitCodeFor(err))
		}
		for _, want := range allowToolNames {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: error %q must list the accepted name %q", name, err, want)
			}
		}
	}
}

func TestAllowToolSetDuplicatesAreIdempotent(t *testing.T) {
	set, err := newAllowToolSet([]string{"run_command", "run_command"})
	if err != nil {
		t.Fatalf("duplicates must be accepted: %v", err)
	}
	if !set.authorized("run_command") || set.authorized("write_file") {
		t.Error("duplicate handling must not widen or narrow the set")
	}
}

func TestAllowToolSetAuthorizesNothingWhenEmpty(t *testing.T) {
	set, err := newAllowToolSet(nil)
	if err != nil {
		t.Fatalf("an empty -allow-tool list is not an error: %v", err)
	}
	if !set.empty() {
		t.Error("an empty list must report empty()")
	}
	for _, name := range append(append([]string{}, allowToolNames...), "read_file", "mcp__x__y") {
		if set.authorized(name) {
			t.Errorf("an empty set must authorize nothing, but authorized %q", name)
		}
	}
}

func TestValidateFlagsAllowToolRequiresOneShot(t *testing.T) {
	err := validateFlags(flags{allowTools: stringSliceFlag{"run_command"}})
	if err == nil {
		t.Fatal("-allow-tool without -p must be rejected")
	}
	if exitCodeFor(err) != 1 {
		t.Errorf("the requires-p rejection is a mode error, not a headless usage error: exit %d, want 1", exitCodeFor(err))
	}
	if err := validateFlags(flags{allowTools: stringSliceFlag{"run_command"}, promptSet: true, prompt: "hi"}); err != nil {
		t.Fatalf("-allow-tool with -p must be accepted: %v", err)
	}
	if err := validateFlags(flags{allowTools: stringSliceFlag{"nope"}, promptSet: true, prompt: "hi"}); err == nil {
		t.Fatal("an unknown -allow-tool name must be rejected at validation, before the run")
	}
}
