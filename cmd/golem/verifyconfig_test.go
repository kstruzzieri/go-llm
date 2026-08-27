package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
