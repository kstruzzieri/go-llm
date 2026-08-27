package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

func writeGolemJSON(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, verifyConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadVerifyConfigAbsentIsSilent(t *testing.T) {
	spec, err := loadVerifyConfig(t.TempDir())
	if err != nil {
		t.Fatalf("a missing %s must not be an error: %v", verifyConfigName, err)
	}
	if spec != nil {
		t.Fatalf("expected no verifier, got %+v", spec)
	}
}

func TestLoadVerifyConfigWithoutVerifyKeyIsSilent(t *testing.T) {
	root := t.TempDir()
	writeGolemJSON(t, root, `{}`)
	spec, err := loadVerifyConfig(root)
	if err != nil {
		t.Fatalf("loadVerifyConfig: %v", err)
	}
	if spec != nil {
		t.Fatalf("expected no verifier, got %+v", spec)
	}
}

func TestLoadVerifyConfigAccepted(t *testing.T) {
	root := t.TempDir()
	writeGolemJSON(t, root, `{"verify":{"argv":["go","build","./..."],"dir":"sub","timeout_seconds":90}}`)
	spec, err := loadVerifyConfig(root)
	if err != nil {
		t.Fatalf("loadVerifyConfig: %v", err)
	}
	if got := strings.Join(spec.Argv, " "); got != "go build ./..." {
		t.Fatalf("argv = %q", got)
	}
	if spec.Dir != "sub" {
		t.Fatalf("dir = %q", spec.Dir)
	}
	if spec.Timeout() != 90*1e9 {
		t.Fatalf("timeout = %s, want 90s", spec.Timeout())
	}
}

func TestLoadVerifyConfigDefaultTimeout(t *testing.T) {
	root := t.TempDir()
	writeGolemJSON(t, root, `{"verify":{"argv":["go","build"]}}`)
	spec, err := loadVerifyConfig(root)
	if err != nil {
		t.Fatalf("loadVerifyConfig: %v", err)
	}
	if spec.Timeout() != verifyDefaultTimeout {
		t.Fatalf("timeout = %s, want the %s default", spec.Timeout(), verifyDefaultTimeout)
	}
}

func TestLoadVerifyConfigRefusals(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"unknown top-level key", `{"verify":{"argv":["go"]},"extra":1}`, "extra"},
		{"unknown verify key", `{"verify":{"argv":["go"],"env":{"A":"b"}}}`, "env"},
		{"shell string instead of argv", `{"verify":{"command":"go build ./..."}}`, "command"},
		{"trailing content", `{"verify":{"argv":["go"]}} trailing`, "trailing"},
		{"not an object", `[1,2,3]`, "must contain a single JSON object"},
		{"empty argv", `{"verify":{"argv":[]}}`, "argv"},
		{"blank argv0", `{"verify":{"argv":["   "]}}`, "argv[0]"},
		{"NUL in argv", "{\"verify\":{\"argv\":[\"go\",\"a\\u0000b\"]}}", "NUL"},
		{"absolute dir", `{"verify":{"argv":["go"],"dir":"/etc"}}`, "relative"},
		{"escaping dir", `{"verify":{"argv":["go"],"dir":"../outside"}}`, "workspace"},
		{"zero timeout", `{"verify":{"argv":["go"],"timeout_seconds":0}}`, "timeout_seconds"},
		{"negative timeout", `{"verify":{"argv":["go"],"timeout_seconds":-1}}`, "timeout_seconds"},
		{"timeout above ceiling", `{"verify":{"argv":["go"],"timeout_seconds":601}}`, "timeout_seconds"},
		{"malformed json", `{"verify":`, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeGolemJSON(t, root, tc.body)
			spec, err := loadVerifyConfig(root)
			if err == nil {
				t.Fatalf("expected a refusal, got spec=%+v", spec)
			}
			if spec != nil {
				t.Fatalf("a refusal must yield no verifier, got %+v", spec)
			}
			if !strings.Contains(err.Error(), verifyConfigName) {
				t.Fatalf("error must name the file: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q must mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadVerifyConfigRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix symlink semantics")
	}
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"verify":{"argv":["go"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, verifyConfigName)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadVerifyConfig(root); err == nil ||
		!strings.Contains(err.Error(), "regular file") {
		t.Fatalf("a symlinked %s must be refused, got %v", verifyConfigName, err)
	}
}

func TestLoadVerifyConfigRefusesDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, verifyConfigName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := loadVerifyConfig(root); err == nil ||
		!strings.Contains(err.Error(), "regular file") {
		t.Fatalf("a directory named %s must be refused, got %v", verifyConfigName, err)
	}
}

func TestLoadVerifyConfigRefusesOversizedFile(t *testing.T) {
	root := t.TempDir()
	writeGolemJSON(t, root, `{"verify":{"argv":["go","`+strings.Repeat("x", verifyConfigMaxBytes)+`"]}}`)
	if _, err := loadVerifyConfig(root); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Fatalf("an oversized %s must be refused, got %v", verifyConfigName, err)
	}
}

// TestLoadVerifyConfigDoesNotSearchAncestors pins that a verify command can
// only be declared by the workspace actually being worked in: an outer
// repository must not silently arm a command for a subdirectory session.
func TestLoadVerifyConfigDoesNotSearchAncestors(t *testing.T) {
	parent := t.TempDir()
	writeGolemJSON(t, parent, `{"verify":{"argv":["go","build"]}}`)
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	spec, err := loadVerifyConfig(child)
	if err != nil {
		t.Fatalf("loadVerifyConfig: %v", err)
	}
	if spec != nil {
		t.Fatalf("an ancestor's %s must be ignored, got %+v", verifyConfigName, spec)
	}
}

func TestBuildVerifierFromWorkspace(t *testing.T) {
	root := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeGolemJSON(t, root, `{"verify":{"argv":["`+self+`","__golem_exec_helper__","echo","ok"]}}`)

	v, warn := buildVerifier(root)
	if warn != "" {
		t.Fatalf("unexpected warning: %s", warn)
	}
	if v == nil {
		t.Fatal("a valid config must arm a verifier")
	}
}

func TestBuildVerifierAbsentConfigArmsNothingQuietly(t *testing.T) {
	v, warn := buildVerifier(t.TempDir())
	if v != nil {
		t.Fatalf("expected no verifier, got %+v", v)
	}
	if warn != "" {
		t.Fatalf("an absent config must be silent, got %q", warn)
	}
}

func TestBuildVerifierInvalidConfigWarnsAndDisables(t *testing.T) {
	cases := map[string]string{
		"malformed":          `{"verify":`,
		"unknown field":      `{"verify":{"argv":["go"],"shell":true}}`,
		"missing executable": `{"verify":{"argv":["definitely-not-on-path-347"]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeGolemJSON(t, root, body)
			v, warn := buildVerifier(root)
			if v != nil {
				t.Fatalf("an invalid config must arm nothing, got %+v", v)
			}
			if warn == "" {
				t.Fatal("an invalid config must warn")
			}
			if !strings.Contains(warn, "verification disabled") {
				t.Fatalf("warning must say verification is off: %q", warn)
			}
		})
	}
}

// TestVerificationArmedOnlyInInteractiveWriteMode pins that every mode which
// must not verify is already excluded by the single -allow-write condition,
// so no separate gate can drift away from the flag rules.
func TestVerificationArmedOnlyInInteractiveWriteMode(t *testing.T) {
	t.Run("one-shot clears allow-write before anything is loaded", func(t *testing.T) {
		got, warns := applyOneShotMode(flags{promptSet: true, prompt: "hi", allowWrite: true})
		if got.allowWrite {
			t.Fatal("one-shot must clear allowWrite")
		}
		if len(warns) == 0 {
			t.Fatal("one-shot must explain that write mode was dropped")
		}
	})
	rejected := []struct {
		name string
		f    flags
	}{
		{"task mode", flags{planPath: "p.json", allowWrite: true, approveEdits: true, approveGates: true}},
		{"planning mode", flags{goalSet: true, goal: "g", allowWrite: true}},
		{"agentflow status", flags{agentflowStatus: true, allowWrite: true, planWorkers: 1}},
		{"agentflow resume", flags{agentflowResume: true, allowWrite: true, planWorkers: 1}},
	}
	for _, tc := range rejected {
		t.Run(tc.name+" rejects allow-write", func(t *testing.T) {
			if err := validateFlags(tc.f); err == nil {
				t.Fatal("expected -allow-write to be rejected for this mode")
			}
		})
	}
}

// TestVerifierIsFrozenAtStartup pins that the command is resolved once. A
// workspace that rewrites .golem.json mid-session must not be able to change
// what an already-approved verifier executes.
func TestVerifierIsFrozenAtStartup(t *testing.T) {
	root := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeGolemJSON(t, root, `{"verify":{"argv":["`+self+`","__golem_exec_helper__","echo","first"]}}`)
	v, warn := buildVerifier(root)
	if warn != "" || v == nil {
		t.Fatalf("buildVerifier: v=%v warn=%q", v, warn)
	}
	before := v.cmd.Command()

	writeGolemJSON(t, root, `{"verify":{"argv":["`+self+`","__golem_exec_helper__","echo","second"]}}`)
	if after := v.cmd.Command(); after != before {
		t.Fatalf("a mid-session config rewrite changed the verifier: %q -> %q", before, after)
	}
	if !strings.Contains(before, "first") {
		t.Fatalf("fixture invalid: %q", before)
	}
}

// TestOrchestratorFactoryWithoutVerifierDoesNotArmATypedNil guards the classic
// Go trap: a (*verifyRunner)(nil) stored in the agent.Verifier interface is
// non-nil to the runtime and would panic on the first mutating batch.
func TestOrchestratorFactoryWithoutVerifierDoesNotArmATypedNil(t *testing.T) {
	caller := &scriptCaller{responses: []agent.ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "w1", Type: "function",
			Function: provider.ToolCallFunction{
				Name: "write_file", Arguments: json.RawMessage(`{"path":"a.txt","content":"x"}`),
			},
		}}}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	root := t.TempDir()
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	orch := newOrchestratorFactory(caller, flags{}, nil)()
	res, err := orch.Run(context.Background(), agent.Request{
		Goal:     "write",
		Tools:    agenttools.NewMutatingTools(ws, nil),
		Approver: allowAllApprover{},
	}, nil)
	if err != nil {
		t.Fatalf("Run with no verifier: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].IsError {
		t.Fatalf("fixture must perform a successful write: %+v", res.ToolCalls)
	}
}

type allowAllApprover struct{}

func (allowAllApprover) Approve(context.Context, provider.ToolCall, string) (bool, error) {
	return true, nil
}
