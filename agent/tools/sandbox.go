package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/kstruzzieri/go-llm/agent"
)

// SandboxRuntime names one execution-isolation runtime (#440).
type SandboxRuntime string

// SandboxRuntimeHost selects direct host execution. The empty runtime is an
// alias retained so SandboxConfig{} preserves pre-#440 behavior.
const SandboxRuntimeHost SandboxRuntime = "host"

// SandboxRuntimeSeatbelt selects the macOS Seatbelt (sandbox-exec) backend
// (#442): per-invocation deny-default profiles scoping writes to the
// workspace plus a private temp directory and reads to reviewed system roots,
// the workspace and that temp. Selecting it off-darwin, or on a
// host whose Seatbelt capability probe fails, fails closed at construction.
//
// Commands start with no launchd connection (#712), which has process-wide
// side effects on the host. Before each spawn the backend empties the host
// process's registered Mach port table: nothing in go-llm reads it, but a
// host that registers ports with mach_ports_register loses them. It holds
// syscall.ForkLock across each spawn, so Go forks wait for it. Forks from C
// code that do not take syscall.ForkLock are not serialized: such a fork
// running concurrently with a Seatbelt spawn can hand that command the host's
// launchd connection.
const SandboxRuntimeSeatbelt SandboxRuntime = "seatbelt"

// SandboxRuntimeBwrap selects the Linux Bubblewrap backend (#441): a fresh
// user/mount/pid namespace set per invocation, deny-default by construction,
// with network unshared unless AllowNetwork is true. Selecting it off Linux,
// or on a host whose namespace-capability probe fails, fails closed at
// construction.
const SandboxRuntimeBwrap SandboxRuntime = "bwrap"

// ErrSandboxPolicy reports that a selected sandbox policy cannot be honored
// here: its backend could not be constructed (an invalid config, a runtime
// unsupported on this OS, a failed capability probe) or the workspace root
// cannot safely bound it, as when a declared root (#722) overlaps it.
// ValidateSandbox, CheckSandboxBindings, NewVerifierSlot, the verification
// constructors and the sandboxed exec tool constructors (NewSandboxedExecTools,
// NewSandboxedExecToolsWithOptions, NewSandboxedBackgroundManager and the
// NewExecToolsWithBackground factories) wrap it with the cause; a
// constructor's own workspace or scratch-option error does not. Test with
// errors.Is. Command errors (a missing executable, an invalid cwd) never wrap
// it.
var ErrSandboxPolicy = errors.New("tools: sandbox policy unavailable")

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

	// ReadOnlyRoots are host-declared toolchain directories (#722) that
	// commands may read and execute but never write. Each is a clean
	// absolute path in its canonical spelling. The host runtime refuses it.
	ReadOnlyRoots []string
	// DeveloperDirectory opts in to the macOS developer tools (#722): "" is
	// off; otherwise a canonical directory declared like ReadOnlyRoots, and
	// commands also get the fixed settings EffectiveEnv lists. The host
	// runtime refuses it.
	DeveloperDirectory string
	// Env sets host-chosen variables from a closed table (#722): GOMODCACHE
	// (a clean absolute path in a declared root), GOPROXY=off,
	// GOTOOLCHAIN=local and GOFLAGS=-buildvcs=false. Any other name or value,
	// or a repeated name, is refused. The host runtime refuses it.
	Env []SandboxEnv
	// TempEnv names variables pointed into each command's private temp
	// (#722), from a closed table: GOCACHE is <private temp>/gocache. The
	// host runtime refuses it.
	TempEnv []string
}

// SandboxEnv is one SandboxConfig.Env variable.
type SandboxEnv struct{ Name, Value string }

// Clone returns a deep copy of c: no slice of the result shares its backing
// array with c, so a caller's later mutation never reaches the copy.
func (c SandboxConfig) Clone() SandboxConfig {
	c.DropCaps = slices.Clone(c.DropCaps)
	c.ReadOnlyRoots = slices.Clone(c.ReadOnlyRoots)
	c.Env = slices.Clone(c.Env)
	c.TempEnv = slices.Clone(c.TempEnv)
	return c
}

// EffectiveEnv returns, sorted as NAME=value, every variable the backend
// adds: the fixed git and python settings when DeveloperDirectory is set,
// each TempEnv name under privateTemp, and Env. Display passes
// "<private temp>"; a backend passes the command's real directory. Each name
// appears once. On a config that was never normalized, TempEnv names outside
// the table are skipped and, for a repeated name, the first source in that
// order wins, so a fixed setting is never overridden.
func (c SandboxConfig) EffectiveEnv(privateTemp string) []string {
	var env []string
	seen := make(map[string]bool)
	add := func(name, value string) {
		if !seen[name] {
			seen[name] = true
			env = append(env, name+"="+value)
		}
	}
	if c.DeveloperDirectory != "" {
		for _, kv := range developerDirectoryEnv {
			name, value, _ := strings.Cut(kv, "=")
			add(name, value)
		}
	}
	for _, name := range c.TempEnv {
		if dir, ok := sandboxTempEnv[name]; ok {
			add(name, privateTemp+"/"+dir)
		}
	}
	for _, e := range c.Env {
		add(e.Name, e.Value)
	}
	slices.Sort(env)
	return env
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
// policy used to construct it and to render approvals. variant is its
// network-denied twin (#584), nil unless the policy is non-host and allows
// network; every copy of the struct shares it.
type resolvedExecBackend struct {
	execBackend
	sandbox  SandboxConfig
	approval sandboxApproval
	variant  *networkVariant
}

type hostBackend struct {
	commandRunner
	backgroundStarter
}

// bindExecBackend freezes one backend with its normalized policy and the
// approval metadata derived from it. Future runtime switch cases must use
// this helper and extend the key/preview with an immutable digest of any
// execution-affecting policy not present in SandboxConfig before the case
// is enabled. It also attaches the policy's network-denied variant, so no
// constructor stamps one and none can forget to.
func bindExecBackend(backend execBackend, cfg SandboxConfig) resolvedExecBackend {
	return resolvedExecBackend{
		execBackend: backend,
		sandbox:     cfg,
		approval:    approvalForSandbox(cfg),
		variant:     newNetworkVariant(cfg),
	}
}

// networkVariant is a native backend's network-denied twin (#584): while
// approvals are suspended (agent.ApprovalsSuspended), a command approved for
// a native (non-host) backend whose policy allows network runs on this copy
// instead. It is built on first use, because construction probes the runtime
// and a session that never suspends must not be refused for a variant it does
// not use. The variant is shared by every tool built over one bound backend.
type networkVariant struct {
	cfg       SandboxConfig                                    // the original policy with AllowNetwork=false
	build     func(SandboxConfig) (resolvedExecBackend, error) // newExecBackend; tests substitute it
	suspended func(context.Context) bool                       // agent.ApprovalsSuspended; tests substitute it

	mu     sync.Mutex
	denied *resolvedExecBackend // the first successful build
}

// approvalStateChanged refuses a command approved in the other suspension
// state than the one it would now run in (#584): run_command and
// start_command return it, the verifier reports it as its reason.
const approvalStateChanged = "approval state changed; retry"

// newNetworkVariant returns the variant for a backend bound to the
// normalized cfg: nil unless cfg is a non-host runtime that allows network.
// The denied copy owns its slices.
func newNetworkVariant(cfg SandboxConfig) *networkVariant {
	if cfg.isHost() || !cfg.AllowNetwork {
		return nil
	}
	cfg = cfg.Clone()
	cfg.AllowNetwork = false
	return &networkVariant{cfg: cfg, build: newExecBackend, suspended: agent.ApprovalsSuspended}
}

// active reports whether a command in ctx's run must use the variant: v is
// non-nil and the run's approvals are suspended.
func (v *networkVariant) active(ctx context.Context) bool {
	return v != nil && v.suspended(ctx)
}

// forContext returns the network-denied backend when v is active for ctx,
// building it on first use, and (nil, nil) otherwise. A build failure is
// returned to this caller, which must fail the call rather than fall back to
// the original backend; it is not cached, so the next call tries again.
func (v *networkVariant) forContext(ctx context.Context) (*resolvedExecBackend, error) {
	if !v.active(ctx) {
		return nil, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.denied == nil {
		b, err := v.build(v.cfg)
		if err != nil {
			return nil, fmt.Errorf("tools: network-denied sandbox unavailable; command not run: %w", err)
		}
		v.denied = &b
	}
	return v.denied, nil
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
	runtime      SandboxRuntime
	roots        string // declared roots (#722), NUL-joined; "" for none
}

// validateWorkspace rejects roots that cannot safely bound the selected
// sandbox before Plan returns an approval prompt. Host execution deliberately
// keeps accepting volume roots for backward compatibility.
func (s sandboxApproval) validateWorkspace(root string) error {
	switch s.runtime {
	case SandboxRuntimeSeatbelt:
		if _, err := seatbeltAllowancePath("workspace root", root); err != nil {
			return err
		}
	case SandboxRuntimeBwrap:
		if !posixCleanAbs(root) || bwrapBroadRoot(root) {
			return fmt.Errorf("tools: bwrap workspace root %q must be a canonical non-root path outside top-level system directories", root)
		}
	}
	return s.checkRootOverlap(root)
}

// checkRootOverlap refuses a declared root that is, covers or lies inside the
// workspace root, compared by file identity in both directions. It is the
// check every exec factory makes where its backend meets its workspace.
func (s sandboxApproval) checkRootOverlap(workspace string) error {
	if s.roots == "" {
		return nil
	}
	for _, root := range strings.Split(s.roots, "\x00") {
		if overlaps(root, workspace) {
			return fmt.Errorf("tools: declared root %q overlaps the workspace root %q", root, workspace)
		}
	}
	return nil
}

// checkFactoryRoots is checkRootOverlap at an exec factory, wrapping
// ErrSandboxPolicy.
func checkFactoryRoots(approval sandboxApproval, ws *Workspace) error {
	if err := approval.checkRootOverlap(ws.root); err != nil {
		return fmt.Errorf("tools: build exec tools: %w: %w", ErrSandboxPolicy, err)
	}
	return nil
}

// sandboxFingerprint hashes the approval-relevant sandbox identity using
// the same labeled, NUL-separated style as commandFingerprint. It accepts
// only a normalized config (caps sorted and de-duplicated, runtime and
// capability names control-character-free).
//
// The #722 fields are written only when set, so every earlier key is
// unchanged. Each is a labeled section (a list as label, count, items) placed
// before drop_caps: the caps are the unframed tail, and anything after them
// could be forged by capability names.
func sandboxFingerprint(cfg SandboxConfig) string {
	h := sha256.New()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	list := func(label string, n int) {
		if n > 0 {
			write(label)
			write(strconv.Itoa(n))
		}
	}
	write("runtime")
	write(string(cfg.Runtime))
	write("allow_network")
	write(strconv.FormatBool(cfg.AllowNetwork))
	write("memory_cap_mb")
	write(strconv.Itoa(cfg.MemoryCapMB))
	write("cpu_limit")
	write(strconv.FormatFloat(cfg.CPULimit, 'g', -1, 64))
	list("read_only_roots", len(cfg.ReadOnlyRoots))
	for _, root := range cfg.ReadOnlyRoots {
		write(root)
	}
	if cfg.DeveloperDirectory != "" {
		write("developer_directory")
		write(cfg.DeveloperDirectory)
	}
	list("env", len(cfg.Env))
	for _, e := range cfg.Env {
		write(e.Name)
		write(e.Value)
	}
	list("temp_env", len(cfg.TempEnv))
	for _, name := range cfg.TempEnv {
		write(name)
	}
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
		runtime:      cfg.Runtime,
		roots:        strings.Join(declaredRoots(cfg), "\x00"),
	}
}

// renderSandboxLine renders the permission ceiling the user is approving.
// Zero resource fields render as "none" (no requested limit, not a backend
// default); runtime and capability strings are quoted/escaped so delimiters
// cannot forge ambiguous fields. Presentation only, never parsed.
func renderSandboxLine(cfg SandboxConfig) string {
	network := "denied"
	// bwrap unshares the IP network namespace, but pathname Unix sockets in
	// the shared workspace remain reachable.
	if cfg.Runtime == SandboxRuntimeBwrap {
		network = "ip-denied"
	}
	if cfg.AllowNetwork {
		network = "allowed"
	}
	memory, cpu, caps := "none", "none", "[]"
	if cfg.MemoryCapMB > 0 {
		memory = fmt.Sprintf("%dMiB", cfg.MemoryCapMB)
		// Scope qualifier (#441): bwrap spends the approved value as three
		// independent, additive budgets -- RLIMIT_AS plus a quota on each
		// private tmpfs -- and RLIMIT_AS is per process, so a forking payload
		// is not bounded by it at all. A bare "512MiB" reads as a total
		// ceiling the backend does not enforce, and an approval prompt must
		// not claim more than it delivers. Aggregate enforcement needs
		// delegated cgroup v2 and is deliberately out of scope; until then
		// the number is qualified rather than inflated.
		if cfg.Runtime == SandboxRuntimeBwrap {
			memory += "/process"
		}
	}
	if cfg.CPULimit > 0 {
		cpu = strconv.FormatFloat(cfg.CPULimit, 'g', -1, 64)
	}
	if len(cfg.DropCaps) > 0 {
		caps = quoteList(cfg.DropCaps)
	}
	line := fmt.Sprintf(
		"runtime=%s network=%s memory_cap=%s cpu_limit=%s drop_caps=%s",
		strconv.QuoteToGraphic(string(cfg.Runtime)), network, memory, cpu, caps,
	)
	// Private-temp marker (#442, extended by #441): the user approves that the
	// command's temp directory is a fresh private one, not the ambient TMPDIR.
	// Seatbelt provides a private host directory; bwrap provides a
	// namespace-local tmpfs. Other runtimes must not claim a behavior they do
	// not implement.
	if cfg.Runtime == SandboxRuntimeSeatbelt || cfg.Runtime == SandboxRuntimeBwrap {
		line += " temp=private"
	}
	// Git-metadata marker (#721): sandboxed commands cannot write git
	// metadata. Seatbelt matches path patterns per write, so it also covers
	// metadata created later; bwrap fixes its mounts at launch, so it
	// protects only what exists when the command starts. The preview must not
	// claim more than each backend delivers.
	switch cfg.Runtime {
	case SandboxRuntimeSeatbelt:
		line += " git_metadata=ro"
	case SandboxRuntimeBwrap:
		line += " git_metadata=ro(existing)"
	}
	// Declared toolchain (#722): each field renders only when set, so earlier
	// previews are unchanged. Env shows every variable the command gets, with
	// its value.
	if len(cfg.ReadOnlyRoots) > 0 {
		line += " read_only_roots=" + quoteList(cfg.ReadOnlyRoots)
	}
	if cfg.DeveloperDirectory != "" {
		line += " developer_directory=" + strconv.QuoteToGraphic(cfg.DeveloperDirectory)
	}
	if env := cfg.EffectiveEnv("<private temp>"); len(env) > 0 {
		line += " env=" + quoteList(env)
	}
	return line
}

// quoteList renders items as a bracketed, comma-separated list of quoted
// strings, so no item can forge a delimiter.
func quoteList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = strconv.QuoteToGraphic(s)
	}
	return "[" + strings.Join(quoted, ",") + "]"
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
	case SandboxRuntimeSeatbelt:
		backend, err := newSeatbeltExecBackend(normalized)
		if err != nil {
			return resolvedExecBackend{}, err
		}
		return bindExecBackend(backend, normalized), nil
	case SandboxRuntimeBwrap:
		backend, err := newBwrapExecBackend(normalized)
		if err != nil {
			return resolvedExecBackend{}, err
		}
		return bindExecBackend(backend, normalized), nil
	default:
		return resolvedExecBackend{}, fmt.Errorf("tools: sandbox runtime %q is not implemented", normalized.Runtime)
	}
}

// normalizeSandboxConfig validates and canonicalizes a caller-supplied
// config: it bounds-checks memory and CPU, rejects unsafe runtime and
// capability names, checks the #722 fields (validateToolchainFields),
// canonicalizes the empty runtime to host, deep-copies every slice, sorts
// DropCaps and ReadOnlyRoots and removes their repeats, sorts Env by name and
// TempEnv, and rejects host plus any requested constraint. The returned config
// owns its slices; the same frozen value backs backend construction, approval
// identity, and previews.
func normalizeSandboxConfig(c SandboxConfig) (SandboxConfig, error) {
	if c.Runtime == "" {
		c.Runtime = SandboxRuntimeHost
	}
	c = c.Clone() // every check below reads the owned copy
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
	for _, name := range c.DropCaps {
		if strings.TrimSpace(name) == "" || containsControl(name) {
			return SandboxConfig{}, fmt.Errorf("tools: sandbox DropCaps entries must be non-blank and contain no control characters")
		}
	}
	if err := validateToolchainFields(c); err != nil {
		return SandboxConfig{}, err
	}
	slices.Sort(c.DropCaps)
	c.DropCaps = slices.Compact(c.DropCaps)
	slices.Sort(c.ReadOnlyRoots)
	c.ReadOnlyRoots = slices.Compact(c.ReadOnlyRoots)
	slices.SortFunc(c.Env, func(a, b SandboxEnv) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(c.TempEnv)
	if c.Runtime == SandboxRuntimeHost &&
		(c.AllowNetwork || c.MemoryCapMB != 0 || c.CPULimit != 0 || len(c.DropCaps) != 0 ||
			len(c.ReadOnlyRoots) != 0 || c.DeveloperDirectory != "" || len(c.Env) != 0 || len(c.TempEnv) != 0) {
		return SandboxConfig{}, fmt.Errorf("tools: sandbox host runtime accepts no constraints")
	}
	return c, nil
}

// ValidateSandbox reports whether cfg can bound a session over root on this
// host, before any tool is built (#576): it normalizes cfg as every exec
// constructor does, resolves its backend (the availability probe), and checks
// that root, canonicalized as a Workspace does, can bound that backend. Every
// failure wraps ErrSandboxPolicy, and there is no host fallback. It holds no
// resources: a resolved backend owns nothing to close. Validating a Seatbelt
// config runs the probe spawn, so it empties the host's registered Mach port
// table too (see SandboxRuntimeSeatbelt).
func ValidateSandbox(cfg SandboxConfig, root string) error {
	backend, err := newExecBackend(cfg)
	if err != nil {
		return fmt.Errorf("tools: validate sandbox: %w: %w", ErrSandboxPolicy, err)
	}
	canon, err := CanonicalWorkspaceRoot(root)
	if err != nil {
		return fmt.Errorf("tools: validate sandbox: %w: %w", ErrSandboxPolicy, err)
	}
	if err := backend.approval.validateWorkspace(canon); err != nil {
		return fmt.Errorf("tools: validate sandbox: %w: %w", ErrSandboxPolicy, err)
	}
	return nil
}

// CheckSandboxBindings admits tools into a native sandbox session (#576, spec
// 3.3) only when each is a library-built workspace, command or scratch tool
// bound to root (canonical, as CanonicalWorkspaceRoot returns it) and to cfg.
// It decides by concrete type and private state, never by name, Effect or a
// caller-implementable interface, so a custom runner, an opaque wrapper or a
// tool built for another root or policy cannot pass:
//
//   - read_file, search, glob, list, write_file, edit_file: their workspace is
//     root;
//   - run_command: its workspace is root, and both its runner (which must be a
//     resolved backend, so NewRunCommand with a custom runner fails) and its
//     approval identity carry cfg's;
//   - start_command: its workspace is root and its manager's backend carries
//     cfg's identity;
//   - command_status, command_tail, stop_command: their manager is one an
//     admitted start_command uses;
//   - scratch_changes (value or pointer) and promote_artifact: their scratch
//     runtime is one an admitted run_command or start_command carries, and
//     promote_artifact's workspace is root.
//
// Any other tool, a nil one, a typed-nil pointer and a zero value are rejected
// with an error naming the tool. A command tool's scratch runtime must also be
// bound to root. scratchKey is the session's frozen scratch identity: when it
// is non-empty every command tool's scratch identity ("" without scratch) must
// equal it; when it is empty the command tools must agree with each other.
// The result is the scratch identity in use, "" for none, for the caller to
// freeze. A cfg that does not normalize wraps ErrSandboxPolicy. It performs no
// I/O and calls no tool method except Spec, to name a rejected tool.
func CheckSandboxBindings(tools []agent.Tool, cfg SandboxConfig, root, scratchKey string) (string, error) {
	normalized, err := normalizeSandboxConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("tools: check sandbox bindings: %w: %w", ErrSandboxPolicy, err)
	}
	key := approvalForSandbox(normalized).keyComponent
	inRoot := func(ws *Workspace) bool { return ws != nil && ws.root == root }
	runnerBound := func(runner commandRunner) bool {
		b, ok := runner.(resolvedExecBackend)
		return ok && b.execBackend != nil && b.approval.keyComponent == key
	}
	managers := make(map[*BackgroundManager]bool)
	scratch := make(map[*scratchRuntime]bool)
	frozen, decided := scratchKey, scratchKey != ""
	scratchBound := func(rt *scratchRuntime) bool {
		got := ""
		if rt != nil {
			if rt.root != root {
				return false
			}
			got = rt.approval.keyComponent
			scratch[rt] = true
		}
		if !decided {
			frozen, decided = got, true
		}
		return got == frozen
	}
	// Pass 1 admits every tool whose binding is its own state and records the
	// managers and scratch runtimes the admitted command tools carry; it defers
	// the tools whose provenance is one of those, and pass 2 admits a deferred
	// tool only through its own case. A deferred type without one is rejected.
	deferred := make([]bool, len(tools))
	for i, tool := range tools {
		if rv := reflect.ValueOf(tool); rv.Kind() == reflect.Pointer && rv.IsNil() {
			return "", errNotAdmitted(i, tool)
		}
		var ok bool
		switch t := tool.(type) {
		case *ReadFile:
			ok = inRoot(t.ws)
		case *Search:
			ok = inRoot(t.ws)
		case *Glob:
			ok = inRoot(t.ws)
		case *List:
			ok = inRoot(t.ws)
		case *WriteFile:
			ok = inRoot(t.ws)
		case *EditFile:
			ok = inRoot(t.ws)
		case *RunCommand:
			ok = inRoot(t.ws) && runnerBound(t.runner) && t.sandbox.keyComponent == key && scratchBound(t.scratchRT)
		case *StartCommand:
			ok = inRoot(t.ws) && t.manager != nil && t.manager.backend.execBackend != nil &&
				t.manager.backend.approval.keyComponent == key && scratchBound(t.scratchRT)
			if ok {
				managers[t.manager] = true
			}
		case *CommandStatus, *CommandTail, *StopCommand, ScratchChanges, *ScratchChanges, *PromoteArtifact:
			deferred[i] = true
			continue
		}
		if !ok {
			return "", errNotAdmitted(i, tool)
		}
	}
	for i, tool := range tools {
		if !deferred[i] {
			continue
		}
		var ok bool
		switch t := tool.(type) {
		case *CommandStatus:
			ok = managers[t.manager]
		case *CommandTail:
			ok = managers[t.manager]
		case *StopCommand:
			ok = managers[t.manager]
		case ScratchChanges:
			ok = scratch[t.rt]
		case *ScratchChanges:
			ok = scratch[t.rt]
		case *PromoteArtifact:
			ok = inRoot(t.ws) && scratch[t.rt]
		}
		if !ok {
			return "", errNotAdmitted(i, tool)
		}
	}
	return frozen, nil
}

// errNotAdmitted names tool by its Spec, or by index and type when it is nil
// or a typed nil whose Spec may not be safe to call.
func errNotAdmitted(i int, tool agent.Tool) error {
	name := fmt.Sprintf("tool at index %d", i)
	if rv := reflect.ValueOf(tool); rv.Kind() == reflect.Pointer && rv.IsNil() {
		name = fmt.Sprintf("%T at index %d", tool, i)
	} else if tool != nil {
		name = strconv.Quote(tool.Spec().Name)
	}
	return fmt.Errorf("tools: %s is not admitted in a native sandbox session", name)
}
