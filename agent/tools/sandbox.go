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

// execBackend covers both command lifetimes for one runtime.
type execBackend interface {
	commandRunner
	backgroundStarter
}

// resolvedExecBackend binds an implementation to its frozen policy.
type resolvedExecBackend struct {
	execBackend
	sandbox SandboxConfig
}

type hostBackend struct {
	commandRunner
	backgroundStarter
}

func newHostExecBackend(starter backgroundStarter) resolvedExecBackend {
	return resolvedExecBackend{
		execBackend: hostBackend{
			commandRunner:     newPlatformRunner(),
			backgroundStarter: starter,
		},
		sandbox: SandboxConfig{Runtime: SandboxRuntimeHost},
	}
}

// newExecBackend is the sole runtime-selection switch (#440). It normalizes
// first, then dispatches; every non-host runtime fails construction until
// its implementing ticket (#389, #441, #442) adds a case. There is no host
// fallback.
func newExecBackend(cfg SandboxConfig) (resolvedExecBackend, error) {
	normalized, err := normalizeSandboxConfig(cfg)
	if err != nil {
		return resolvedExecBackend{}, err
	}
	switch normalized.Runtime {
	case SandboxRuntimeHost:
		got := newHostExecBackend(newPlatformStarter())
		got.sandbox = normalized
		return got, nil
	default:
		return resolvedExecBackend{}, fmt.Errorf("tools: sandbox runtime %q is not implemented", normalized.Runtime)
	}
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
