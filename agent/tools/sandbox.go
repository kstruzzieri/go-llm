package tools

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"
)

// SandboxRuntime names one execution-isolation runtime (#440).
type SandboxRuntime string

// SandboxRuntimeHost selects direct host execution. The empty runtime is an
// alias retained so SandboxConfig{} preserves pre-#440 behavior.
const SandboxRuntimeHost SandboxRuntime = "host"

// SandboxConfig is a permission ceiling for a non-host execution runtime: a
// backend may enforce stricter isolation but must never silently provide
// less. The zero value is the compatibility exception and means direct host
// execution with no isolation.
type SandboxConfig struct {
	Runtime      SandboxRuntime
	AllowNetwork bool
	MemoryCapMB  int
	CPULimit     float64
	DropCaps     []string
}

func (c SandboxConfig) isHost() bool {
	return c.Runtime == "" || c.Runtime == SandboxRuntimeHost
}

func containsControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// normalizeSandboxConfig validates and canonicalizes a caller-supplied
// config: it bounds-checks memory and CPU, rejects unsafe runtime and
// capability names, canonicalizes the empty runtime to host, deep-copies,
// sorts, and de-duplicates DropCaps, and rejects host plus any requested
// constraint. The returned config owns its DropCaps backing array; the same
// frozen value backs backend construction, approval identity, and previews.
func normalizeSandboxConfig(c SandboxConfig) (SandboxConfig, error) {
	if c.Runtime == "" {
		c.Runtime = SandboxRuntimeHost
	}
	if strings.TrimSpace(string(c.Runtime)) == "" || containsControl(string(c.Runtime)) {
		return SandboxConfig{}, fmt.Errorf("tools: sandbox Runtime must be non-blank and contain no control characters")
	}
	if c.MemoryCapMB < 0 {
		return SandboxConfig{}, fmt.Errorf("tools: sandbox MemoryCapMB must not be negative")
	}
	if c.CPULimit < 0 || math.IsNaN(c.CPULimit) || math.IsInf(c.CPULimit, 0) {
		return SandboxConfig{}, fmt.Errorf("tools: sandbox CPULimit must be finite and not negative")
	}
	caps := append([]string(nil), c.DropCaps...)
	for _, name := range caps {
		if strings.TrimSpace(name) == "" || containsControl(name) {
			return SandboxConfig{}, fmt.Errorf("tools: sandbox DropCaps entries must be non-blank and contain no control characters")
		}
	}
	slices.Sort(caps)
	c.DropCaps = slices.Compact(caps)
	if c.Runtime == SandboxRuntimeHost &&
		(c.AllowNetwork || c.MemoryCapMB != 0 || c.CPULimit != 0 || len(c.DropCaps) != 0) {
		return SandboxConfig{}, fmt.Errorf("tools: sandbox host runtime accepts no constraints")
	}
	return c, nil
}
