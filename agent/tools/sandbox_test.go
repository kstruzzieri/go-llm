package tools

import (
	"math"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeSandboxConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     SandboxConfig
		wantErr string
	}{
		{"zero is host", SandboxConfig{}, ""},
		{"explicit host", SandboxConfig{Runtime: SandboxRuntimeHost}, ""},
		{"unknown runtime passes normalization", SandboxConfig{Runtime: "container"}, ""},
		{"host rejects network flag", SandboxConfig{AllowNetwork: true}, "host runtime"},
		{"host rejects memory cap", SandboxConfig{MemoryCapMB: 1}, "host runtime"},
		{"host rejects CPU limit", SandboxConfig{CPULimit: 1}, "host runtime"},
		{"host rejects dropped capabilities", SandboxConfig{DropCaps: []string{"ALL"}}, "host runtime"},
		{"negative memory", SandboxConfig{Runtime: "container", MemoryCapMB: -1}, "MemoryCapMB"},
		{"negative CPU", SandboxConfig{Runtime: "container", CPULimit: -1}, "CPULimit"},
		{"NaN CPU", SandboxConfig{Runtime: "container", CPULimit: math.NaN()}, "finite"},
		{"infinite CPU", SandboxConfig{Runtime: "container", CPULimit: math.Inf(1)}, "finite"},
		{"blank runtime", SandboxConfig{Runtime: " "}, "Runtime"},
		{"runtime control character", SandboxConfig{Runtime: "container\nforged"}, "Runtime"},
		{"blank capability", SandboxConfig{Runtime: "container", DropCaps: []string{" "}}, "DropCaps"},
		{"capability NUL", SandboxConfig{Runtime: "container", DropCaps: []string{"NET_RAW\x00SYS_ADMIN"}}, "DropCaps"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeSandboxConfig(tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("normalizeSandboxConfig() error = %v", err)
				}
				if got.Runtime == "" {
					t.Fatal("normalized runtime must not be empty")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("normalizeSandboxConfig() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeSandboxConfigOwnsCanonicalCaps(t *testing.T) {
	input := []string{"SYS_ADMIN", "NET_RAW", "NET_RAW"}
	got, err := normalizeSandboxConfig(SandboxConfig{Runtime: "container", DropCaps: input})
	if err != nil {
		t.Fatal(err)
	}
	input[0] = "CHOWN"
	if want := []string{"NET_RAW", "SYS_ADMIN"}; !slices.Equal(got.DropCaps, want) {
		t.Fatalf("DropCaps = %q, want canonical owned copy %q", got.DropCaps, want)
	}
}
