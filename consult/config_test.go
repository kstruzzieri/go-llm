package consult

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realTempDir is t.TempDir resolved through symlinks: on macOS temp paths live
// under /var, itself a symlink to /private/var, so an unresolved temp path
// would trip the command symlink-traversal check on its own.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeConfig writes body as a consultants file, substituting @CMD@ with a
// real executable it creates, and returns the config and command paths.
func writeConfig(t *testing.T, body string) (configPath, cmdPath string) {
	t.Helper()
	dir := realTempDir(t)
	cmdPath = filepath.Join(dir, "claude-2.1.240")
	if err := os.WriteFile(cmdPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(dir, "consultants.json")
	encoded, err := json.Marshal(cmdPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(strings.ReplaceAll(body, `"@CMD@"`, string(encoded))), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, cmdPath
}

// symlinkedParentCmd returns a path to a real executable reached through a
// symlinked parent directory; the leaf itself is not a symlink.
func symlinkedParentCmd(t *testing.T) string {
	t.Helper()
	root := realTempDir(t)
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := filepath.Join(realDir, "claude-2.1.240")
	if err := os.WriteFile(cmd, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "link", "claude-2.1.240")
}

const goodConfig = `{"version":1,"consultants":[{"name":"claude","adapter":"claude","command":"@CMD@","sha256":"","model":"opus","timeout_seconds":120,"max_output_bytes":1048576,"trusted_process_egress":true}]}`

func TestLoadAcceptsMinimalValidConfig(t *testing.T) {
	p, _ := writeConfig(t, goodConfig)
	cs, err := Load(p)
	if err != nil || len(cs) != 1 || cs["claude"].Adapter != "claude" || cs["claude"].TimeoutSeconds != 120 {
		t.Fatalf("load: %v %+v", err, cs)
	}
}

func TestLoadRejectsInvalidConfigs(t *testing.T) {
	sub := func(old, new string) string { return strings.Replace(goodConfig, old, new, 1) }
	for name, tc := range map[string]struct{ body, want string }{
		"unknown_field":    {sub(`"model":"opus"`, `"model":"opus","args":["--evil"]`), `unknown field "args"`},
		"relative_command": {sub(`"command":"@CMD@"`, `"command":"claude"`), "command must be an absolute path"},
		"dir_command":      {strings.Replace(goodConfig, "@CMD@", realTempDir(t), 1), "command must be a regular file"},
		"symlink_parent":   {strings.Replace(goodConfig, "@CMD@", symlinkedParentCmd(t), 1), "command path must not traverse symlinks"},
		"egress_false":     {sub(`"trusted_process_egress":true`, `"trusted_process_egress":false`), "trusted_process_egress must be true"},
		"egress_missing":   {sub(`,"trusted_process_egress":true`, ``), "trusted_process_egress must be true"},
		"bad_name":         {sub(`"name":"claude"`, `"name":"Claude Max"`), "name must match"},
		"bad_adapter":      {sub(`"adapter":"claude"`, `"adapter":"codex"`), `unsupported adapter "codex"`},
		"bad_model":        {sub(`"model":"opus"`, `"model":"sonnet"`), `unsupported model "sonnet"`},
		"timeout_too_long": {sub(`"timeout_seconds":120`, `"timeout_seconds":301`), "timeout_seconds must be 0..300"},
		"timeout_negative": {sub(`"timeout_seconds":120`, `"timeout_seconds":-1`), "timeout_seconds must be 0..300"},
		"output_too_large": {sub(`"max_output_bytes":1048576`, `"max_output_bytes":1048577`), "max_output_bytes must be 0..1048576"},
		"output_negative":  {sub(`"max_output_bytes":1048576`, `"max_output_bytes":-1`), "max_output_bytes must be 0..1048576"},
		"bad_sha":          {sub(`"sha256":""`, `"sha256":"ABC"`), "sha256 must be 64 lowercase hex characters"},
		"version_2":        {sub(`"version":1`, `"version":2`), "unsupported config version 2"},
		"trailing_content": {goodConfig + ` {"version":1,"consultants":[]}`, "trailing content after JSON object"},
		"duplicate_name":   {sub(`]}`, `,{"name":"claude","adapter":"claude","command":"@CMD@","model":"opus","trusted_process_egress":true}]}`), `duplicate consultant name "claude"`},
	} {
		p, _ := writeConfig(t, tc.body)
		_, err := Load(p)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: rejected for the wrong reason:\n got  %v\n want substring %q", name, err, tc.want)
		}
	}
}

func TestLoadDefaultsAndDisabled(t *testing.T) {
	p, _ := writeConfig(t, strings.Replace(goodConfig, `"timeout_seconds":120,"max_output_bytes":1048576,`, ``, 1))
	cs, err := Load(p)
	if err != nil || cs["claude"].TimeoutSeconds != 120 || cs["claude"].MaxOutputBytes != 1<<20 {
		t.Fatalf("defaults: %v %+v", err, cs)
	}
	// os.UserConfigDir reads XDG_CONFIG_HOME on unix and HOME on darwin; set
	// both so the default path is absent whichever one this platform uses.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if _, err := Load(""); !errors.Is(err, ErrDisabled) {
		t.Fatalf("missing default must disable, got %v", err)
	}
	// The guard must be what rejects a relative path, not a failing open: put
	// a real, loadable config in the working directory so the only reason to
	// fail is the IsAbs check.
	rel, _ := writeConfig(t, goodConfig)
	t.Chdir(filepath.Dir(rel))
	if _, err := Load("consultants.json"); err == nil || !strings.Contains(err.Error(), "config path must be absolute") {
		t.Fatalf("relative explicit path must be rejected as non-absolute, got %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil || errors.Is(err, ErrDisabled) {
		t.Fatalf("missing explicit path must fail, not disable: %v", err)
	}
}

// TestLoadDisablesWhenConfigDirIsUnresolvable covers a host with no usable
// config directory at all: a caller that starts with the default path must be
// told /consult is unavailable, not that it is misconfigured, or the whole
// program refuses to start in a stripped environment.
func TestLoadDisablesWhenConfigDirIsUnresolvable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if _, err := DefaultPath(); err == nil {
		t.Skip("this platform resolves a config dir without HOME or XDG_CONFIG_HOME")
	}
	if _, err := Load(""); !errors.Is(err, ErrDisabled) {
		t.Fatalf("unresolvable config dir must disable, got %v", err)
	}
	// An explicit path never consults the environment, so it still fails loud.
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil || errors.Is(err, ErrDisabled) {
		t.Fatalf("explicit path must fail, not disable: %v", err)
	}
}

// TestLoadRejectsAConfigSwappedDuringOpen closes the Lstat-to-open window: a
// file renamed over the path between the two is a different inode, and reading
// it would mean validating one file and obeying another. The swap is a
// write-sibling-then-rename, never a remove-and-recreate: inode reuse would let
// a broken identity check pass on APFS.
func TestLoadRejectsAConfigSwappedDuringOpen(t *testing.T) {
	path, _ := writeConfig(t, goodConfig)
	other, _ := writeConfig(t, goodConfig)
	restore := afterConfigLstat
	t.Cleanup(func() { afterConfigLstat = restore })
	afterConfigLstat = func(string) {
		afterConfigLstat = nil // swap once, so the retry inside Load cannot loop
		if err := os.Rename(other, path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("a config swapped during open must be rejected, got %v", err)
	}
}

// TestLoadHardensTheConfigRead covers the file that names the executable: it
// gets the same regular-file, identity and size discipline as the command it
// declares, so a symlinked, swapped or unbounded config cannot be read.
func TestLoadHardensTheConfigRead(t *testing.T) {
	real, _ := writeConfig(t, goodConfig)
	link := filepath.Join(realTempDir(t), "linked.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked config must be rejected, got %v", err)
	}

	big := filepath.Join(realTempDir(t), "big.json")
	if err := os.WriteFile(big, append([]byte(goodConfig), make([]byte, maxConfigBytes)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(big); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized config must be rejected, got %v", err)
	}
}

func TestDefaultPathIsTheDocumentedLocation(t *testing.T) {
	path, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("go-llm", "consultants.json"); !strings.HasSuffix(path, want) {
		t.Fatalf("DefaultPath %q does not end in %q", path, want)
	}
}

func TestLoadRejectsSymlinkCommand(t *testing.T) {
	p, cmd := writeConfig(t, goodConfig)
	link := filepath.Join(filepath.Dir(p), "claude")
	if err := os.Symlink(cmd, link); err != nil {
		t.Fatal(err)
	}
	// Pin the message: Lstat also reports a symlink as non-regular, so only the
	// wording distinguishes the symlink arm from the regular-file arm.
	lp, _ := writeConfig(t, strings.Replace(goodConfig, "@CMD@", link, 1))
	_, err := Load(lp)
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("symlink command must be rejected as a symlink, got %v", err)
	}
}
