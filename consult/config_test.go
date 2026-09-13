package consult

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	cmd := filepath.Join(dir, "claude-2.1.240")
	if err := os.WriteFile(cmd, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "consultants.json")
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(body, "@CMD@", cmd)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodConfig = `{"version":1,"consultants":[{"name":"claude","adapter":"claude","command":"@CMD@","sha256":"","model":"opus","timeout_seconds":120,"max_output_bytes":1048576,"trusted_process_egress":true}]}`

func TestLoadAcceptsMinimalValidConfig(t *testing.T) {
	cs, err := Load(writeConfig(t, goodConfig))
	if err != nil || len(cs) != 1 || cs["claude"].Adapter != "claude" || cs["claude"].TimeoutSeconds != 120 {
		t.Fatalf("load: %v %+v", err, cs)
	}
}

func TestLoadRejectsInvalidConfigs(t *testing.T) {
	for name, body := range map[string]string{
		"unknown_field":    strings.Replace(goodConfig, `"model":"opus"`, `"model":"opus","args":["--evil"]`, 1),
		"relative_command": strings.Replace(goodConfig, `"command":"@CMD@"`, `"command":"claude"`, 1),
		"egress_false":     strings.Replace(goodConfig, `"trusted_process_egress":true`, `"trusted_process_egress":false`, 1),
		"egress_missing":   strings.Replace(goodConfig, `,"trusted_process_egress":true`, ``, 1),
		"bad_name":         strings.Replace(goodConfig, `"name":"claude"`, `"name":"Claude Max"`, 1),
		"bad_adapter":      strings.Replace(goodConfig, `"adapter":"claude"`, `"adapter":"codex"`, 1),
		"bad_model":        strings.Replace(goodConfig, `"model":"opus"`, `"model":"sonnet"`, 1),
		"timeout_too_long": strings.Replace(goodConfig, `"timeout_seconds":120`, `"timeout_seconds":301`, 1),
		"output_too_large": strings.Replace(goodConfig, `"max_output_bytes":1048576`, `"max_output_bytes":1048577`, 1),
		"bad_sha":          strings.Replace(goodConfig, `"sha256":""`, `"sha256":"ABC"`, 1),
		"version_2":        strings.Replace(goodConfig, `"version":1`, `"version":2`, 1),
		"trailing_content": goodConfig + ` {"version":1,"consultants":[]}`,
		"duplicate_name":   strings.Replace(goodConfig, `]}`, `,{"name":"claude","adapter":"claude","command":"@CMD@","model":"opus","trusted_process_egress":true}]}`, 1),
	} {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadDefaultsAndDisabled(t *testing.T) {
	cs, err := Load(writeConfig(t, strings.Replace(goodConfig, `"timeout_seconds":120,"max_output_bytes":1048576,`, ``, 1)))
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
	if _, err := Load("relative/consultants.json"); err == nil {
		t.Fatal("relative explicit path accepted")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil || errors.Is(err, ErrDisabled) {
		t.Fatalf("missing explicit path must fail, not disable: %v", err)
	}
}

func TestLoadRejectsSymlinkCommand(t *testing.T) {
	p := writeConfig(t, goodConfig)
	cs, _ := Load(p)
	link := filepath.Join(filepath.Dir(p), "claude")
	if err := os.Symlink(cs["claude"].Command, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(writeConfig(t, strings.Replace(goodConfig, "@CMD@", link, 1))); err == nil {
		t.Fatal("symlink command accepted")
	}
}
