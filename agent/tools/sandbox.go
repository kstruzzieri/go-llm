package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// SandboxRuntime names one execution-isolation runtime (#440).
type SandboxRuntime string

// SandboxRuntimeHost selects direct host execution. The empty runtime is an
// alias retained so SandboxConfig{} preserves pre-#440 behavior.
const SandboxRuntimeHost SandboxRuntime = "host"

// SandboxRuntimeSeatbelt selects the macOS Seatbelt (sandbox-exec) backend
// (#442): per-invocation deny-default profiles scoping reads/writes to the
// workspace plus a private temp directory. Selecting it off-darwin, or on a
// host whose Seatbelt capability probe fails, fails closed at construction.
const SandboxRuntimeSeatbelt SandboxRuntime = "seatbelt"

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

// resolvedExecBackend binds an implementation to the exact normalized
// policy used to construct it and to render approvals.
type resolvedExecBackend struct {
	execBackend
	sandbox  SandboxConfig
	approval sandboxApproval
}

type hostBackend struct {
	commandRunner
	backgroundStarter
}

// bindExecBackend freezes one backend with its normalized policy and the
// approval metadata derived from it. Future runtime switch cases must use
// this helper and extend the key/preview with an immutable digest of any
// execution-affecting policy not present in SandboxConfig before the case
// is enabled.
func bindExecBackend(backend execBackend, cfg SandboxConfig) resolvedExecBackend {
	return resolvedExecBackend{
		execBackend: backend,
		sandbox:     cfg,
		approval:    approvalForSandbox(cfg),
	}
}

func newHostExecBackend(starter backgroundStarter) resolvedExecBackend {
	return bindExecBackend(hostBackend{
		commandRunner:     newPlatformRunner(),
		backgroundStarter: starter,
	}, SandboxConfig{Runtime: SandboxRuntimeHost})
}

// sandboxApproval is the frozen approval metadata for one resolved backend:
// the key-namespace component and the rendered preview line. The zero value
// is the host identity (no component, no line — byte-identical approvals).
type sandboxApproval struct {
	keyComponent string
	preview      string
}

// sandboxFingerprint hashes the approval-relevant sandbox identity using
// the same labeled, NUL-separated style as commandFingerprint. It accepts
// only a normalized config (caps sorted and de-duplicated, runtime and
// capability names control-character-free).
func sandboxFingerprint(cfg SandboxConfig) string {
	h := sha256.New()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	write("runtime")
	write(string(cfg.Runtime))
	write("allow_network")
	write(strconv.FormatBool(cfg.AllowNetwork))
	write("memory_cap_mb")
	write(strconv.Itoa(cfg.MemoryCapMB))
	write("cpu_limit")
	write(strconv.FormatFloat(cfg.CPULimit, 'g', -1, 64))
	write("drop_caps")
	for _, name := range cfg.DropCaps {
		write(name)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// approvalForSandbox derives the approval metadata for a normalized config.
// Host returns the zero value so host keys and previews stay byte-identical
// to the pre-#440 recipe; every non-host config gets its own key namespace
// ("sb:<digest>:") and preview line, so a grant never crosses a
// runtime/policy boundary.
func approvalForSandbox(cfg SandboxConfig) sandboxApproval {
	if cfg.isHost() {
		return sandboxApproval{}
	}
	return sandboxApproval{
		keyComponent: "sb:" + sandboxFingerprint(cfg) + ":",
		preview:      renderSandboxLine(cfg),
	}
}

// renderSandboxLine renders the permission ceiling the user is approving.
// Zero resource fields render as "none" (no requested limit, not a backend
// default); runtime and capability strings are quoted/escaped so delimiters
// cannot forge ambiguous fields. Presentation only, never parsed.
func renderSandboxLine(cfg SandboxConfig) string {
	network := "denied"
	if cfg.AllowNetwork {
		network = "allowed"
	}
	memory, cpu, caps := "none", "none", "[]"
	if cfg.MemoryCapMB > 0 {
		memory = fmt.Sprintf("%dMiB", cfg.MemoryCapMB)
	}
	if cfg.CPULimit > 0 {
		cpu = strconv.FormatFloat(cfg.CPULimit, 'g', -1, 64)
	}
	if len(cfg.DropCaps) > 0 {
		quoted := make([]string, len(cfg.DropCaps))
		for i, name := range cfg.DropCaps {
			quoted[i] = strconv.QuoteToGraphic(name)
		}
		caps = "[" + strings.Join(quoted, ",") + "]"
	}
	line := fmt.Sprintf(
		"runtime=%s network=%s memory_cap=%s cpu_limit=%s drop_caps=%s",
		strconv.QuoteToGraphic(string(cfg.Runtime)), network, memory, cpu, caps,
	)
	// Seatbelt-only marker (#442): the user approves that the command's temp
	// directory is a fresh private one, not the ambient TMPDIR. Other runtimes
	// must not claim a behavior they do not implement.
	if cfg.Runtime == SandboxRuntimeSeatbelt {
		line += " temp=private"
	}
	return line
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
		return bindExecBackend(hostBackend{
			commandRunner:     newPlatformRunner(),
			backgroundStarter: newPlatformStarter(),
		}, normalized), nil
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
	if c.CPULimit == 0 {
		c.CPULimit = 0
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
