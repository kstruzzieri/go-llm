package tools

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateSandboxEnv pins the pure #722 checks: the closed Env and TempEnv
// tables and the spelling of declared roots. A refusal never echoes the value
// it refused.
func TestValidateSandboxEnv(t *testing.T) {
	valid := func() SandboxConfig {
		return SandboxConfig{
			Runtime:       SandboxRuntimeSeatbelt,
			ReadOnlyRoots: []string{"/opt/x"},
			Env: []SandboxEnv{
				{Name: "GOMODCACHE", Value: "/opt/x/mod"},
				{Name: "GOPROXY", Value: "off"},
				{Name: "GOTOOLCHAIN", Value: "local"},
				{Name: "GOFLAGS", Value: "-buildvcs=false"},
			},
			TempEnv: []string{"GOCACHE"},
		}
	}
	if err := validateToolchainFields(valid()); err != nil {
		t.Fatalf("valid control refused: %v", err)
	}
	equal := valid()
	equal.Env[0].Value = "/opt/x"
	devDir := valid()
	devDir.DeveloperDirectory = "/Library/Developer/CommandLineTools"
	devDir.Env[0].Value = "/Library/Developer/CommandLineTools/mod"
	for name, cfg := range map[string]SandboxConfig{"GOMODCACHE equal to a root": equal, "GOMODCACHE in the developer directory": devDir} {
		if err := validateToolchainFields(cfg); err != nil {
			t.Fatalf("%s refused: %v", name, err)
		}
	}

	tests := []struct {
		name    string
		edit    func(*SandboxConfig)
		reason  string
		refused []string // must not appear in the error
	}{
		{"unknown name", func(c *SandboxConfig) { c.Env[1] = SandboxEnv{Name: "LD_PRELOAD", Value: "/tmp/evil.so"} },
			"outside the allowed table", []string{"/tmp/evil.so", "LD_PRELOAD"}},
		{"GOPROXY value", func(c *SandboxConfig) { c.Env[1].Value = "https://proxy.golang.org" },
			`GOPROXY accepts only "off"`, []string{"https://proxy.golang.org"}},
		{"GOFLAGS value", func(c *SandboxConfig) { c.Env[3].Value = "-toolexec=/bin/x" },
			`GOFLAGS accepts only "-buildvcs=false"`, []string{"-toolexec=/bin/x"}},
		{"GOMODCACHE outside every root", func(c *SandboxConfig) { c.Env[0].Value = "/opt/xy/mod" },
			"inside a declared root", []string{"/opt/xy/mod"}},
		{"GOMODCACHE not clean", func(c *SandboxConfig) { c.Env[0].Value = "/opt/x/../y" },
			"inside a declared root", []string{"/opt/x/../y"}},
		{"GOMODCACHE control character", func(c *SandboxConfig) { c.Env[0].Value = "/opt/x/m\nod" },
			"inside a declared root", []string{"m\nod"}},
		{"duplicate name", func(c *SandboxConfig) { c.Env = append(c.Env, SandboxEnv{Name: "GOPROXY", Value: "off"}) },
			"Env[4] repeats", nil},
		{"unknown temp name", func(c *SandboxConfig) { c.TempEnv = []string{"XDG_CACHE_HOME"} },
			"TempEnv[0] names a variable outside the allowed table", []string{"XDG_CACHE_HOME"}},
		{"duplicate temp name", func(c *SandboxConfig) { c.TempEnv = []string{"GOCACHE", "GOCACHE"} },
			"TempEnv[1] repeats", nil},
		{"root newline", func(c *SandboxConfig) { c.ReadOnlyRoots = []string{"/opt/x", "/opt/n\nl"} },
			"ReadOnlyRoots[1]", []string{"n\nl"}},
		{"root DEL", func(c *SandboxConfig) { c.ReadOnlyRoots = []string{"/opt/d\x7fel"} },
			"ReadOnlyRoots[0]", []string{"d\x7fel"}},
		{"root invalid UTF-8", func(c *SandboxConfig) { c.ReadOnlyRoots = []string{"/opt/\xffbad"} },
			"ReadOnlyRoots[0]", []string{"\xffbad"}},
		{"root relative", func(c *SandboxConfig) { c.ReadOnlyRoots = []string{"opt/rel"} },
			"ReadOnlyRoots[0]", []string{"opt/rel"}},
		{"root not clean", func(c *SandboxConfig) { c.ReadOnlyRoots = []string{"/opt/x/"} },
			"ReadOnlyRoots[0]", []string{"/opt/x/"}},
		{"root empty", func(c *SandboxConfig) { c.ReadOnlyRoots = []string{""} },
			"ReadOnlyRoots[0]", nil},
		{"developer directory relative", func(c *SandboxConfig) { c.DeveloperDirectory = "Developer/rel" },
			"DeveloperDirectory", []string{"Developer/rel"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.edit(&cfg)
			err := validateToolchainFields(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("validateToolchainFields() = %v, want reason %q", err, tt.reason)
			}
			for _, refused := range tt.refused {
				if strings.Contains(err.Error(), refused) {
					t.Fatalf("error echoes the refused %q: %v", refused, err)
				}
			}
			// The same refusal reaches every public path through normalization.
			if _, err := normalizeSandboxConfig(cfg); err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("normalizeSandboxConfig() = %v, want reason %q", err, tt.reason)
			}
		})
	}

	// Host accepts none of the new fields, each on its own and with valid
	// values, so the host check is the only reason. ValidateSandbox refuses
	// before any probe and wraps ErrSandboxPolicy.
	hostRows := map[string]SandboxConfig{
		"ReadOnlyRoots":      {ReadOnlyRoots: []string{"/opt/x"}},
		"DeveloperDirectory": {DeveloperDirectory: "/Library/Developer/CommandLineTools"},
		"Env":                {Env: []SandboxEnv{{Name: "GOPROXY", Value: "off"}}},
		"TempEnv":            {Runtime: SandboxRuntimeHost, TempEnv: []string{"GOCACHE"}},
	}
	for name, cfg := range hostRows {
		t.Run("host "+name, func(t *testing.T) {
			err := ValidateSandbox(cfg, t.TempDir())
			if !errors.Is(err, ErrSandboxPolicy) || !strings.Contains(err.Error(), "host runtime accepts no constraints") {
				t.Fatalf("ValidateSandbox(host + %s) = %v, want host refusal wrapping ErrSandboxPolicy", name, err)
			}
		})
	}
}
