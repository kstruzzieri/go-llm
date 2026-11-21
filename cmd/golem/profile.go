package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
)

// trustedOptIn returns the first legacy explicit tool opt-in that selects the
// Trusted profile when -profile is omitted (spec section 4), or "" when none is
// set. An explicit -sandbox counts whatever runtime it names, host included
// (spec section 4). -agent-memory counts only with a session, since without
// one it is dropped with a warning (agentMemoryRequest). resolveProfile calls it after
// normalization, so a flag one-shot mode dropped selects nothing; validateFlags
// calls it before, with -no-session cleared, so an explicit -profile inspect
// still conflicts with every one of them.
func trustedOptIn(f flags) string {
	switch {
	case f.allowWrite:
		return "-allow-write"
	case f.allowExec:
		return "-allow-exec"
	case f.scratch:
		return "-scratch"
	case len(f.allowTools) > 0:
		return "-allow-tool"
	case f.ragDB != "":
		return "-rag-db"
	case f.agentMemory && !f.noSession:
		return "-agent-memory"
	case f.delegate:
		return "-delegate"
	case f.dispatch:
		return "-dispatch"
	case len(f.mcpStdio) > 0:
		return "-mcp-stdio"
	case len(f.mcpHTTP) > 0:
		return "-mcp-http"
	case f.progressive:
		return "-progressive"
	case f.grounding:
		return "-grounding"
	case f.sandboxSet:
		return "-sandbox"
	}
	return ""
}

// afModes is the AgentFlow mode list the inapplicable-flag errors name.
const afModes = "AgentFlow modes (-plan, -goal, -agentflow-status, -agentflow-resume)"

// validateSandboxFlags rejects -sandbox/-sandbox-network values and
// combinations (spec section 4) before normalization or any resource opens.
// It never probes a backend: availability is checked in run, on the
// canonical root.
func validateSandboxFlags(f flags) error {
	if f.sandboxSet {
		switch f.sandbox {
		case "host", "seatbelt", "bwrap":
		default:
			// Value-free on purpose, like validateProfile.
			return errors.New("golem: -sandbox must be host, seatbelt, or bwrap")
		}
	}
	if f.sandboxNetworkSet && f.sandboxNetwork != "denied" && f.sandboxNetwork != "allowed" {
		return errors.New("golem: -sandbox-network must be denied or allowed")
	}
	if agentflowMode(f) {
		if f.sandboxSet {
			return errors.New("golem: -sandbox does not apply to " + afModes)
		}
		if f.sandboxNetworkSet {
			return errors.New("golem: -sandbox-network does not apply to " + afModes)
		}
	}
	policy := resolveSandbox(f)
	if f.sandboxNetworkSet && !nativeSandbox(policy) {
		return errors.New("golem: -sandbox-network requires -sandbox seatbelt or -sandbox bwrap")
	}
	for _, opt := range []struct {
		set  bool
		flag string
	}{
		{len(f.sandboxReadRoots) > 0, "-sandbox-read-root"},
		{len(f.sandboxEnv) > 0, "-sandbox-env"},
		{len(f.sandboxTempEnv) > 0, "-sandbox-temp-env"},
		{f.sandboxDeveloperDir, "-sandbox-developer-dir"},
	} {
		if opt.set && policy.Runtime != agenttools.SandboxRuntimeSeatbelt {
			return errors.New("golem: " + opt.flag + " requires -sandbox seatbelt")
		}
	}
	// An empty value is usually an unset variable or a failed $(go env ...);
	// filepath.Abs would turn an empty root into the working directory.
	for _, opt := range []struct {
		vals []string
		flag string
	}{
		{f.sandboxReadRoots, "-sandbox-read-root"},
		{f.sandboxEnv, "-sandbox-env"},
		{f.sandboxTempEnv, "-sandbox-temp-env"},
	} {
		if slices.Contains(opt.vals, "") {
			return errors.New("golem: " + opt.flag + " must not be empty")
		}
	}
	for _, kv := range f.sandboxEnv {
		if !strings.Contains(kv, "=") {
			// Value-free: the library's table refuses the rest at startup.
			return errors.New("golem: -sandbox-env must be NAME=VALUE")
		}
	}
	if !nativeSandbox(policy) {
		return nil
	}
	// One-shot normalization drops sessionless -agent-memory, so only the
	// surviving request is refused.
	survives, _ := applyOneShotMode(f)
	for _, req := range []struct {
		set  bool
		flag string
	}{
		{f.ragDB != "", "-rag-db"},
		{survives.agentMemory && !survives.noSession, "-agent-memory"},
		{f.delegate, "-delegate"},
		{f.dispatch, "-dispatch"},
		{len(f.mcpStdio) > 0, "-mcp-stdio"},
		{len(f.mcpHTTP) > 0, "-mcp-http"},
		{f.progressive, "-progressive"},
		{f.grounding, "-grounding"},
	} {
		if req.set {
			return errors.New("golem: -sandbox " + f.sandbox + " does not admit " + req.flag)
		}
	}
	return nil
}

// resolveSandbox maps the validated flags to the session's one sandbox
// policy. "" (the zero flags value) and "host" are the host zero value;
// network is denied unless -sandbox-network allowed. Any other runtime name
// is passed through, so an unvalidated value fails closed in
// agenttools.ValidateSandbox rather than becoming host. The declared
// toolchain's roots stay as typed and its developer directory unset: this
// runs twice (validateSandboxFlags and run), so it touches no host state, and
// run resolves both once with declareToolchain.
func resolveSandbox(f flags) agenttools.SandboxConfig {
	if f.sandbox == "" || f.sandbox == string(agenttools.SandboxRuntimeHost) {
		return agenttools.SandboxConfig{}
	}
	policy := agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntime(f.sandbox), AllowNetwork: f.sandboxNetwork == "allowed",
		ReadOnlyRoots: slices.Clone(f.sandboxReadRoots), TempEnv: slices.Clone(f.sandboxTempEnv)}
	for _, kv := range f.sandboxEnv {
		name, value, _ := strings.Cut(kv, "=")
		policy.Env = append(policy.Env, agenttools.SandboxEnv{Name: name, Value: value})
	}
	return policy
}

// declareToolchain completes policy's declared toolchain (#722) for run:
// each -sandbox-read-root in its canonical spelling, the one the backend,
// approval key and permissions notice carry; a GOMODCACHE typed inside a root
// respelled under that root's canonical spelling; and with
// -sandbox-developer-dir the active developer directory, asked for once per
// run. It returns new slices and leaves policy's untouched; a nil slice stays
// nil, so a policy declaring no toolchain is unchanged.
func declareToolchain(ctx context.Context, policy agenttools.SandboxConfig, f flags) (agenttools.SandboxConfig, error) {
	typed := make([]string, len(policy.ReadOnlyRoots))
	roots := slices.Clone(policy.ReadOnlyRoots)
	for i, root := range policy.ReadOnlyRoots {
		abs, err := filepath.Abs(root)
		if err == nil {
			roots[i], err = agenttools.CanonicalToolchainRoot(abs)
		}
		if err != nil {
			return policy, fmt.Errorf("golem: -sandbox-read-root: %w", err)
		}
		typed[i] = abs
	}
	env := slices.Clone(policy.Env)
	for i, e := range env {
		if e.Name == "GOMODCACHE" {
			env[i].Value = rebaseUnderRoot(e.Value, typed, roots)
		}
	}
	policy.ReadOnlyRoots, policy.Env = roots, env
	if f.sandboxDeveloperDir {
		dir, err := activeDeveloperDirectory(ctx)
		if err != nil {
			return policy, fmt.Errorf("golem: -sandbox-developer-dir: %w", err)
		}
		policy.DeveloperDirectory = dir
	}
	return policy, nil
}

// rebaseUnderRoot respells a clean absolute path that lies lexically in a
// typed root (the longest, when several hold it) under that root's canonical
// spelling. It resolves nothing, so the path need not exist and stays inside
// the canonical root; any other value is returned unchanged, for the library
// to refuse without echoing it.
func rebaseUnderRoot(path string, typed, canon []string) string {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return path
	}
	best, out := -1, path
	for i, root := range typed {
		rel, err := filepath.Rel(root, path)
		if err == nil && (rel == "." || filepath.IsLocal(rel)) && len(root) > best {
			best, out = len(root), filepath.Join(canon[i], rel)
		}
	}
	return out
}

// validateSandbox is agenttools.ValidateSandbox, replaceable only by this
// package's tests so run's startup probes are observable without a backend
// (mirrors golem's own seam).
var validateSandbox = agenttools.ValidateSandbox

// activeDeveloperDirectory is agenttools.ActiveDeveloperDirectory, replaceable
// only by this package's tests, which count its calls.
var activeDeveloperDirectory = agenttools.ActiveDeveloperDirectory

// nativeSandbox reports whether policy selects a runtime other than host.
func nativeSandbox(policy agenttools.SandboxConfig) bool {
	return policy.Runtime != "" && policy.Runtime != agenttools.SandboxRuntimeHost
}

// agentflowMode reports the AgentFlow invocations. They keep their existing
// tool policy, so they take no -profile and resolve Trusted silently.
func agentflowMode(f flags) bool {
	return f.planPath != "" || f.goalSet || f.agentflowStatus || f.agentflowResume
}

// validateProfile rejects -profile values and combinations before any
// normalization or resource construction.
func validateProfile(f flags) error {
	if !f.profileSet {
		return nil
	}
	p, err := golemruntime.ParseProfile(f.profile)
	if err != nil {
		// Value-free on purpose, like parseQuietly: argv may hold a secret.
		return errors.New("golem: -profile must be inspect, preview, or trusted")
	}
	if agentflowMode(f) {
		return errors.New("golem: -profile does not apply to AgentFlow modes (-plan, -goal, -agentflow-status, -agentflow-resume)")
	}
	switch p {
	case golemruntime.ProfilePreview:
		return errors.New("golem: Preview unavailable: staged-workspace runtime requires #388/#389")
	case golemruntime.ProfileInspect:
		// -no-session only stops -agent-memory selecting a profile; against an
		// explicit -profile inspect it is still a conflict (spec section 4).
		f.noSession = false
		if opt := trustedOptIn(f); opt != "" {
			return errors.New("golem: -profile inspect conflicts with " + opt)
		}
	}
	return nil
}

// resolveProfile picks the session profile after normalization. An explicit
// -profile wins (validateProfile already rejected bad values and conflicts);
// AgentFlow modes are Trusted; otherwise a surviving legacy opt-in selects
// Trusted and names itself in the returned notice, and nothing selects Inspect.
// The notice is "" except for that legacy selection.
func resolveProfile(f flags) (golemruntime.Profile, string) {
	if f.profileSet {
		p, _ := golemruntime.ParseProfile(f.profile)
		return p, ""
	}
	if agentflowMode(f) {
		return golemruntime.ProfileTrusted, ""
	}
	if opt := trustedOptIn(f); opt != "" {
		return golemruntime.ProfileTrusted, "profile: trusted (selected by " + opt + ")"
	}
	return golemruntime.ProfileInspect, ""
}

// applyProfileGates records the capability decision the profile and sandbox
// policy imply: Inspect and a native session both suppress automatic
// retrieval and the model memory tools (spec section 4). Startup gates read
// f.suppressAutoTools, never a rewritten -no-rag, so host-side features keyed
// on the user's own flags (feedback) are unchanged.
func applyProfileGates(f flags, p golemruntime.Profile, policy agenttools.SandboxConfig) flags {
	f.suppressAutoTools = p == golemruntime.ProfileInspect || nativeSandbox(policy)
	return f
}

// renderPermissions is the one-line notice for a runtime's effective
// permissions (spec section 7): profile, mounted tools, command runtime (with
// its warning, or a native backend's network and limits), scratch and
// verification state, and, when set, strict-approval=on with what exposes a
// conversation (#584). Inspect names how to get the old defaults back.
func renderPermissions(ep golemruntime.EffectivePermissions) string {
	names := make([]string, len(ep.Tools))
	for i, tool := range ep.Tools {
		names[i] = tool.Name
	}
	line := "permissions: profile=" + ep.Profile.String() + " tools=" + strings.Join(names, ",") + " commands=" + ep.Command.Runtime
	if ep.Command.Warning != "" {
		line += " (" + ep.Command.Warning + ")"
	}
	// Keyed on the runtime, not on Limits: a native projection that ever
	// came without limits must still show its network policy.
	if rt := agenttools.SandboxRuntime(ep.Command.Runtime); rt == agenttools.SandboxRuntimeSeatbelt || rt == agenttools.SandboxRuntimeBwrap {
		line += " network=" + ep.Command.Network + " limits=" + strings.Join(ep.Command.Limits, "; ")
		// The declared toolchain (#722), quoted like the approval preview and
		// shown only when set, so earlier lines are unchanged.
		if len(ep.Command.ReadOnlyRoots) > 0 {
			line += " read-only-roots=" + previewValues(ep.Command.ReadOnlyRoots)
		}
		if ep.Command.DeveloperDirectory != "" {
			line += " developer-dir=" + previewText(ep.Command.DeveloperDirectory)
		}
		if len(ep.Command.Env) > 0 {
			line += " env=" + previewValues(ep.Command.Env)
		}
	}
	scratch := "off"
	if ep.Scratch {
		scratch = "on"
	}
	line += " scratch=" + scratch + " verification=" + ep.Verification.State
	if ep.StrictApproval {
		line += " strict-approval=on (exposed by MCP results, accepted /consult advice or unrecorded history)"
	}
	if ep.Profile == golemruntime.ProfileInspect {
		line += "; restart with -profile trusted to enable retrieval, memory tools, writes or commands"
	}
	return line
}
