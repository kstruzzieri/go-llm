package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// trustedSelectors is spec section 4's legacy opt-in list, each with argv that
// sets only that one flag. The flag names are literals: the selection notice
// and the conflict error must name exactly these.
var trustedSelectors = []struct {
	flag string
	args []string
}{
	{"-allow-write", []string{"-allow-write"}},
	{"-allow-exec", []string{"-allow-exec"}},
	{"-scratch", []string{"-scratch"}},
	{"-allow-tool", []string{"-allow-tool", "run_command"}},
	{"-rag-db", []string{"-rag-db", "x.db"}},
	{"-agent-memory", []string{"-agent-memory"}},
	{"-delegate", []string{"-delegate"}},
	{"-dispatch", []string{"-dispatch"}},
	{"-mcp-stdio", []string{"-mcp-stdio", "srv=cmd"}},
	{"-mcp-http", []string{"-mcp-http", "srv=https://mcp.invalid"}},
	{"-progressive", []string{"-progressive"}},
	{"-grounding", []string{"-grounding"}},
	// An explicit -sandbox selects Trusted even when it names host (spec
	// section 4: "a sandbox flag also selects Trusted").
	{"-sandbox", []string{"-sandbox", "host"}},
}

// agentflowInvocations are the four AgentFlow modes, which keep their own tool
// policy and take no profile.
var agentflowInvocations = []struct {
	name string
	args []string
}{
	{"plan", []string{"-plan", "p.json", "-approve-plan-edits", "-approve-plan-gates"}},
	{"goal", []string{"-goal", "g"}},
	{"status", []string{"-agentflow-status"}},
	{"resume", []string{"-plan", "p.json", "-agentflow-resume", "-approve-plan-edits", "-approve-plan-gates"}},
}

func validateArgs(t *testing.T, args ...string) error {
	t.Helper()
	f, err := parseFlags(args)
	if err != nil {
		t.Fatalf("parseFlags(%q) = %v", args, err)
	}
	return validateFlags(f)
}

func TestValidateFlagsProfile(t *testing.T) {
	type row struct {
		name string
		args []string
		want string // "" means valid
	}
	rows := []row{
		{"inspect alone", []string{"-profile", "inspect"}, ""},
		{"trusted alone", []string{"-profile", "trusted"}, ""},
		{"trusted with writes", []string{"-profile", "trusted", "-allow-write"}, ""},
		{"inspect with feedback", []string{"-profile", "inspect", "-feedback"}, ""},
		// -no-session stops -agent-memory promoting a profile, not conflicting
		// with an explicit one (spec section 4: conflicts are errors).
		{"inspect with sessionless agent memory", []string{"-profile", "inspect", "-agent-memory", "-no-session"}, "golem: -profile inspect conflicts with -agent-memory"},
		{"preview", []string{"-profile", "preview"}, "golem: Preview unavailable: staged-workspace runtime requires #388/#389"},
		{"preview with tools", []string{"-profile", "preview", "-allow-write"}, "golem: Preview unavailable: staged-workspace runtime requires #388/#389"},
		// The conflict check runs before one-shot normalization drops -allow-write.
		{"inspect one-shot write", []string{"-profile", "inspect", "-p", "hi", "-allow-write"}, "golem: -profile inspect conflicts with -allow-write"},
	}
	for _, sel := range trustedSelectors {
		rows = append(rows, row{"inspect conflicts with " + sel.flag,
			append([]string{"-profile", "inspect"}, sel.args...),
			"golem: -profile inspect conflicts with " + sel.flag})
	}
	for _, af := range agentflowInvocations {
		for _, p := range []string{"trusted", "inspect"} {
			rows = append(rows, row{p + " with " + af.name,
				append([]string{"-profile", p}, af.args...),
				"golem: -profile does not apply to AgentFlow modes (-plan, -goal, -agentflow-status, -agentflow-resume)"})
		}
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArgs(t, tc.args...)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateFlags(%q) = %v, want nil", tc.args, err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("validateFlags(%q) = %v, want %q", tc.args, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"bogus", "Trusted", ""} {
		t.Run("bad value "+fmt.Sprintf("%q", bad), func(t *testing.T) {
			err := validateArgs(t, "-profile", bad)
			if err == nil || err.Error() != "golem: -profile must be inspect, preview, or trusted" {
				t.Fatalf("validateFlags(-profile %q) = %v", bad, err)
			}
		})
	}
}

// resolveArgs mirrors run: parse, then task/goal/one-shot normalization, then
// resolution. It skips validateFlags on purpose so a selector can be set alone
// (-scratch without -allow-exec), which isolates each trustedOptIn row.
func resolveArgs(t *testing.T, args ...string) (golemruntime.Profile, string) {
	t.Helper()
	f, err := parseFlags(args)
	if err != nil {
		t.Fatalf("parseFlags(%q) = %v", args, err)
	}
	f, _ = applyTaskMode(f)
	f, _ = applyGoalMode(f)
	f, _ = applyOneShotMode(f)
	return resolveProfile(f)
}

func TestResolveProfile(t *testing.T) {
	type row struct {
		name   string
		args   []string
		want   golemruntime.Profile
		notice string
	}
	rows := []row{
		{"no flags", nil, golemruntime.ProfileInspect, ""},
		{"one-shot", []string{"-p", "hi"}, golemruntime.ProfileInspect, ""},
		{"feedback", []string{"-feedback"}, golemruntime.ProfileInspect, ""},
		{"explicit trusted", []string{"-profile", "trusted"}, golemruntime.ProfileTrusted, ""},
		{"explicit trusted with a selector", []string{"-profile", "trusted", "-allow-write"}, golemruntime.ProfileTrusted, ""},
		{"explicit inspect", []string{"-profile", "inspect"}, golemruntime.ProfileInspect, ""},
	}
	for _, sel := range trustedSelectors {
		rows = append(rows, row{sel.flag, sel.args, golemruntime.ProfileTrusted, "profile: trusted (selected by " + sel.flag + ")"})
	}
	for _, af := range agentflowInvocations {
		rows = append(rows, row{"agentflow " + af.name, af.args, golemruntime.ProfileTrusted, ""})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			got, notice := resolveArgs(t, tc.args...)
			if got != tc.want || notice != tc.notice {
				t.Fatalf("resolveProfile(%q) = (%s, %q), want (%s, %q)", tc.args, got, notice, tc.want, tc.notice)
			}
		})
	}
}

// A flag that normalization drops must not promote the profile (Review Focus 2).
func TestResolveProfileIgnoresNormalizedFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-p", "hi", "-allow-write"},
		{"-p", "hi", "-allow-exec", "-scratch"},
		{"-agent-memory", "-no-session"},
		{"-p", "hi", "-agent-memory"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got, notice := resolveArgs(t, args...)
			if got != golemruntime.ProfileInspect || notice != "" {
				t.Fatalf("resolveProfile(%q) = (%s, %q), want (inspect, \"\")", args, got, notice)
			}
		})
	}
}

// The production gate itself, no hook override: Inspect never starts the
// auto-index job, Trusted keeps today's default.
func TestShouldStartAutoIndexSuppressed(t *testing.T) {
	var f flags
	if !shouldStartAutoIndex(applyProfileGates(f, golemruntime.ProfileTrusted, agenttools.SandboxConfig{})) {
		t.Fatal("Trusted with default flags must keep the auto-index default")
	}
	if shouldStartAutoIndex(applyProfileGates(f, golemruntime.ProfileInspect, agenttools.SandboxConfig{})) {
		t.Fatal("Inspect must not start the auto-index job")
	}
}

// nativeUnadmitted are spec section 3.2's extension requests a native
// session rejects at validation (#576): MCP, retrieval, model memory,
// delegate and dispatch. The args reuse trustedSelectors' argv.
var nativeUnadmitted = []string{"-mcp-stdio", "-mcp-http", "-rag-db", "-agent-memory", "-delegate", "-dispatch", "-progressive", "-grounding"}

func TestValidateFlagsSandbox(t *testing.T) {
	type row struct {
		name string
		args []string
		want string // "" means valid
	}
	const (
		badRuntime     = "golem: -sandbox must be host, seatbelt, or bwrap"
		badNetwork     = "golem: -sandbox-network must be denied or allowed"
		networkNative  = "golem: -sandbox-network requires -sandbox seatbelt or -sandbox bwrap"
		sandboxAF      = "golem: -sandbox does not apply to AgentFlow modes (-plan, -goal, -agentflow-status, -agentflow-resume)"
		sandboxNetAF   = "golem: -sandbox-network does not apply to AgentFlow modes (-plan, -goal, -agentflow-status, -agentflow-resume)"
		inspectSandbox = "golem: -profile inspect conflicts with -sandbox"
	)
	rows := []row{
		// Positive controls: validation never probes a backend, so every
		// runtime name is valid on every OS.
		{"host", []string{"-sandbox", "host"}, ""},
		{"seatbelt", []string{"-sandbox", "seatbelt"}, ""},
		{"bwrap", []string{"-sandbox", "bwrap"}, ""},
		{"seatbelt network denied", []string{"-sandbox", "seatbelt", "-sandbox-network", "denied"}, ""},
		{"bwrap network allowed", []string{"-sandbox", "bwrap", "-sandbox-network", "allowed"}, ""},
		{"trusted seatbelt", []string{"-profile", "trusted", "-sandbox", "seatbelt"}, ""},
		{"native writes and exec", []string{"-sandbox", "bwrap", "-allow-write", "-allow-exec", "-scratch"}, ""},
		{"host keeps extensions", []string{"-sandbox", "host", "-delegate", "-rag-db", "x.db"}, ""},
		// Sessionless and one-shot -agent-memory is dropped by normalization,
		// so it requests nothing a native session must refuse.
		{"native sessionless agent memory", []string{"-sandbox", "seatbelt", "-agent-memory", "-no-session"}, ""},
		{"native one-shot agent memory", []string{"-sandbox", "seatbelt", "-p", "hi", "-agent-memory"}, ""},
		{"native one-shot allowed tool", []string{"-sandbox", "bwrap", "-p", "hi", "-allow-tool", "run_command"}, ""},

		{"bad runtime", []string{"-sandbox", "bogus"}, badRuntime},
		{"runtime is case-sensitive", []string{"-sandbox", "Seatbelt"}, badRuntime},
		{"empty runtime", []string{"-sandbox", ""}, badRuntime},
		{"bad network", []string{"-sandbox", "seatbelt", "-sandbox-network", "open"}, badNetwork},
		{"empty network", []string{"-sandbox", "bwrap", "-sandbox-network", ""}, badNetwork},
		{"network without sandbox", []string{"-sandbox-network", "denied"}, networkNative},
		{"network with host", []string{"-sandbox", "host", "-sandbox-network", "allowed"}, networkNative},
		{"inspect seatbelt", []string{"-profile", "inspect", "-sandbox", "seatbelt"}, inspectSandbox},

		// #722: the declared toolchain flags are Seatbelt-only; validation
		// parses -sandbox-env's shape and leaves its table to the library.
		{"seatbelt toolchain", []string{"-sandbox", "seatbelt", "-sandbox-read-root", "/a", "-sandbox-read-root", "/b",
			"-sandbox-env", "GOPROXY=off", "-sandbox-env", "GOFLAGS=", "-sandbox-temp-env", "GOCACHE", "-sandbox-developer-dir"}, ""},
		{"env without equals", []string{"-sandbox", "seatbelt", "-sandbox-env", "GOPROXY"}, "golem: -sandbox-env must be NAME=VALUE"},
	}
	for _, tool := range [][]string{
		{"-sandbox-read-root", "/a"}, {"-sandbox-env", "GOPROXY=off"}, {"-sandbox-temp-env", "GOCACHE"}, {"-sandbox-developer-dir"},
	} {
		want := "golem: " + tool[0] + " requires -sandbox seatbelt"
		for _, sel := range [][]string{nil, {"-sandbox", "host"}, {"-sandbox", "bwrap"}} {
			rows = append(rows, row{tool[0] + " with " + fmt.Sprint(sel), append(slices.Clone(sel), tool...), want})
		}
	}
	for _, rt := range []string{"seatbelt", "bwrap"} {
		for _, sel := range trustedSelectors {
			if !slices.Contains(nativeUnadmitted, sel.flag) {
				continue
			}
			rows = append(rows, row{rt + " with " + sel.flag,
				append([]string{"-sandbox", rt}, sel.args...),
				"golem: -sandbox " + rt + " does not admit " + sel.flag})
		}
	}
	for _, af := range agentflowInvocations {
		rows = append(rows,
			row{"sandbox with " + af.name, append([]string{"-sandbox", "seatbelt"}, af.args...), sandboxAF},
			row{"host sandbox with " + af.name, append([]string{"-sandbox", "host"}, af.args...), sandboxAF},
			row{"sandbox network with " + af.name, append([]string{"-sandbox-network", "allowed"}, af.args...), sandboxNetAF})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArgs(t, tc.args...)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateFlags(%q) = %v, want nil", tc.args, err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("validateFlags(%q) = %v, want %q", tc.args, err, tc.want)
			}
		})
	}
	// Every native-unadmitted flag has a row: a selector rename cannot
	// silently drop one from the table above.
	for _, flag := range nativeUnadmitted {
		if !slices.ContainsFunc(trustedSelectors, func(sel struct {
			flag string
			args []string
		}) bool {
			return sel.flag == flag
		}) {
			t.Fatalf("no trustedSelectors argv for %s", flag)
		}
	}
}

func TestResolveProfileSandboxSelectsTrusted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		notice string
	}{
		{"seatbelt", []string{"-sandbox", "seatbelt"}, "profile: trusted (selected by -sandbox)"},
		{"bwrap with network", []string{"-sandbox", "bwrap", "-sandbox-network", "allowed"}, "profile: trusted (selected by -sandbox)"},
		{"one-shot seatbelt", []string{"-p", "hi", "-sandbox", "seatbelt"}, "profile: trusted (selected by -sandbox)"},
		{"explicit trusted", []string{"-profile", "trusted", "-sandbox", "seatbelt"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, notice := resolveArgs(t, tc.args...)
			if got != golemruntime.ProfileTrusted || notice != tc.notice {
				t.Fatalf("resolveProfile(%q) = (%s, %q), want (trusted, %q)", tc.args, got, notice, tc.notice)
			}
		})
	}
}

// resolveSandbox maps the flags to the one policy every construction path
// receives. "" (the zero flags value tests use) and "host" are the host zero
// value; network is denied unless explicitly allowed.
func TestResolveSandbox(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    flags
		want agenttools.SandboxConfig
	}{
		{"zero flags", flags{}, agenttools.SandboxConfig{}},
		{"host", flags{sandbox: "host", sandboxSet: true}, agenttools.SandboxConfig{}},
		{"seatbelt", flags{sandbox: "seatbelt", sandboxSet: true}, agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeSeatbelt}},
		{"seatbelt denied", flags{sandbox: "seatbelt", sandboxSet: true, sandboxNetwork: "denied", sandboxNetworkSet: true},
			agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeSeatbelt}},
		{"bwrap allowed", flags{sandbox: "bwrap", sandboxSet: true, sandboxNetwork: "allowed", sandboxNetworkSet: true},
			agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeBwrap, AllowNetwork: true}},
		// #722: roots pass through as typed and the developer directory stays
		// unset: run resolves both once, since this function runs twice.
		{"seatbelt toolchain", flags{sandbox: "seatbelt", sandboxSet: true, sandboxReadRoots: stringSliceFlag{"/b", "/a"},
			sandboxEnv: stringSliceFlag{"GOPROXY=off", "GOMODCACHE=/a/m=x"}, sandboxTempEnv: stringSliceFlag{"GOCACHE"}, sandboxDeveloperDir: true},
			agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeSeatbelt, ReadOnlyRoots: []string{"/b", "/a"},
				Env: []agenttools.SandboxEnv{{Name: "GOPROXY", Value: "off"}, {Name: "GOMODCACHE", Value: "/a/m=x"}}, TempEnv: []string{"GOCACHE"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveSandbox(tc.f); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("resolveSandbox = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A native session suppresses automatic retrieval and the model memory tools
// like Inspect (spec section 4); host Trusted keeps today's defaults.
func TestApplyProfileGatesNative(t *testing.T) {
	trusted := golemruntime.ProfileTrusted
	for _, tc := range []struct {
		name     string
		profile  golemruntime.Profile
		policy   agenttools.SandboxConfig
		suppress bool
	}{
		{"host zero value", trusted, agenttools.SandboxConfig{}, false},
		{"explicit host", trusted, agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeHost}, false},
		{"seatbelt", trusted, agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeSeatbelt}, true},
		{"bwrap allowed", trusted, agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeBwrap, AllowNetwork: true}, true},
		{"inspect", golemruntime.ProfileInspect, agenttools.SandboxConfig{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := applyProfileGates(flags{}, tc.profile, tc.policy)
			if got.suppressAutoTools != tc.suppress {
				t.Fatalf("suppressAutoTools = %t, want %t", got.suppressAutoTools, tc.suppress)
			}
			if shouldStartAutoIndex(got) == tc.suppress {
				t.Fatalf("shouldStartAutoIndex = %t with suppressAutoTools %t", !tc.suppress, tc.suppress)
			}
		})
	}
}

// The native fields render between the command runtime and scratch; Inspect's
// restart guidance stays last. Inputs are literal projections, so the format
// is pinned on every OS.
func TestRenderPermissionsFormat(t *testing.T) {
	read := []golemruntime.ToolPermission{{Name: "read_file"}, {Name: "list"}}
	for _, tc := range []struct {
		name string
		ep   golemruntime.EffectivePermissions
		want string
	}{
		{"inspect", golemruntime.EffectivePermissions{Profile: golemruntime.ProfileInspect, Tools: read,
			Command: golemruntime.CommandPermissions{Runtime: "n/a", Network: "n/a"}, Verification: golemruntime.Verification{State: "none"}},
			"permissions: profile=inspect tools=read_file,list commands=n/a scratch=off verification=none; restart with -profile trusted to enable retrieval, memory tools, writes or commands"},
		{"host", golemruntime.EffectivePermissions{Profile: golemruntime.ProfileTrusted, Tools: read,
			Command: golemruntime.CommandPermissions{Runtime: "host", Network: "unrestricted", Warning: "W"}, Verification: golemruntime.Verification{State: "armed", Target: "/r"}},
			"permissions: profile=trusted tools=read_file,list commands=host (W) scratch=off verification=armed"},
		{"native", golemruntime.EffectivePermissions{Profile: golemruntime.ProfileTrusted, Tools: read,
			Command: golemruntime.CommandPermissions{Runtime: "bwrap", Network: "ip-denied", Limits: []string{"L1", "L2"}},
			Scratch: true, Verification: golemruntime.Verification{State: "disabled", Target: "/r"}},
			"permissions: profile=trusted tools=read_file,list commands=bwrap network=ip-denied limits=L1; L2 scratch=on verification=disabled"},
		{"native without limits", golemruntime.EffectivePermissions{Profile: golemruntime.ProfileTrusted, Tools: read,
			Command: golemruntime.CommandPermissions{Runtime: "seatbelt", Network: "allowed"}, Verification: golemruntime.Verification{State: "none"}},
			"permissions: profile=trusted tools=read_file,list commands=seatbelt network=allowed limits= scratch=off verification=none"},
		{"native toolchain", golemruntime.EffectivePermissions{Profile: golemruntime.ProfileTrusted, Tools: read,
			Command: golemruntime.CommandPermissions{Runtime: "seatbelt", Network: "denied", Limits: []string{"L1"},
				ReadOnlyRoots: []string{"/opt/go", "/opt/mod cache"}, DeveloperDirectory: "/Library/Developer/CommandLineTools",
				Env: []string{"GOCACHE=<private temp>/gocache", "GOPROXY=off"}},
			Verification: golemruntime.Verification{State: "none"}},
			`permissions: profile=trusted tools=read_file,list commands=seatbelt network=denied limits=L1 read-only-roots=["/opt/go", "/opt/mod cache"] developer-dir="/Library/Developer/CommandLineTools" env=["GOCACHE=<private temp>/gocache", "GOPROXY=off"] scratch=off verification=none`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderPermissions(tc.ep); got != tc.want {
				t.Fatalf("renderPermissions =\n %q\nwant\n %q", got, tc.want)
			}
		})
	}
}

// -help states each flag's values and that -sandbox confines only the model's
// commands, not host services (spec section 7).
func TestParseFlags_SandboxHelp(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	t.Cleanup(func() { os.Stderr = original })
	os.Stderr = w
	_, parseErr := parseFlags([]string{"-help"})
	os.Stderr = original
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	help, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(parseErr, flag.ErrHelp) {
		t.Fatalf("parse error = %v", parseErr)
	}
	// The flag package wraps nothing: each usage string is one line after
	// its "  -name string" header.
	for _, want := range []string{
		"  -sandbox string\n    \tcommand sandbox for Trusted sessions: host (default), seatbelt (macOS) or bwrap (Linux); fails closed when unavailable; does not confine /consult, git context, MCP servers or the /edit editor\n",
		"  -sandbox-network string\n    \tnetwork policy for -sandbox seatbelt|bwrap: denied (default) or allowed\n",
		"  -sandbox-read-root value\n    \tread-only toolchain directory for -sandbox seatbelt commands (repeatable): readable and runnable, never writable; resolved to its canonical spelling at startup; refused if it overlaps the workspace or a credential or data store\n",
		"  -sandbox-env value\n    \tset NAME=VALUE for -sandbox seatbelt commands (repeatable) from a closed table: GOMODCACHE=<path inside a -sandbox-read-root>, GOPROXY=off, GOTOOLCHAIN=local, GOFLAGS=-buildvcs=false\n",
		"  -sandbox-temp-env value\n    \tpoint NAME into each -sandbox seatbelt command's private temp (repeatable) from a closed table: GOCACHE (<private temp>/gocache)\n",
		"  -sandbox-developer-dir\n    \tfor -sandbox seatbelt: declare the active developer directory (xcode-select -p) read-only, so git, python3 and Go's VCS stamping run, and set GIT_CONFIG_NOSYSTEM=1, GIT_CONFIG_GLOBAL=/dev/null and PYTHONDONTWRITEBYTECODE=1\n",
	} {
		if !strings.Contains(string(help), want) {
			t.Fatalf("help lacks %q:\n%s", want, help)
		}
	}
}

// A backend this OS cannot provide fails run before any resource opens: no
// checkpoint store (the startup -allow-write path would open one first), no
// session, fingerprint or other data under XDG_DATA_HOME, and no host
// fallback. Native rejects MCP at validation, so no MCP flag is needed to
// prove no connection was made.
func TestNativeProbeBeforeResources(t *testing.T) {
	isolateRunEnv(t)
	configPath, root := writeRunLifecycleConfig(t)
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	backend, want := "bwrap", `tools: sandbox runtime "bwrap" requires Linux`
	if runtime.GOOS == "linux" {
		backend, want = "seatbelt", `tools: sandbox runtime "seatbelt" requires macOS`
	}
	stdin, stdout, stderr := runTestFiles(t)
	opens, ready := 0, false
	err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
		"-no-project-context", "-allow-write", "-sandbox", backend}, stdin, stdout, stderr, runHooks{
		afterCheckpointOpen: func(*checkpointStore) error { opens++; return nil },
		afterSessionReady:   func(*replSession) error { ready = true; return nil },
	})
	if !errors.Is(err, agenttools.ErrSandboxPolicy) || !strings.Contains(err.Error(), want) {
		t.Fatalf("run = %v, want ErrSandboxPolicy naming %q\nstderr:\n%s", err, want, readRunTestFile(t, stderr))
	}
	if opens != 0 || ready {
		t.Fatalf("checkpoint store opens = %d, session ready = %t; want 0, false", opens, ready)
	}
	entries, rerr := os.ReadDir(data)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("XDG_DATA_HOME has %d entries (first %q), want none", len(entries), entries[0].Name())
	}
}

// TestRunDeclaresToolchainOnce: run resolves each -sandbox-read-root to its
// canonical spelling (a symlink to the real directory), respells a GOMODCACHE
// typed through a root the same way, and asks for the active developer
// directory exactly once, although resolveSandbox runs twice; the startup
// probe receives that one policy. A GOMODCACHE outside every typed root
// passes through unchanged, and the library then refuses it. A root or
// developer directory that cannot be resolved stops run before the probe.
// Portable: both backend seams are stubbed, and the missing config stops a
// good run.
func TestRunDeclaresToolchainOnce(t *testing.T) {
	const devDir = "/Library/Developer/CommandLineTools"
	devFailed := errors.New("no developer directory")
	real := canonicalDir(t, t.TempDir())
	link := filepath.Join(t.TempDir(), "toolchain")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// A sibling sharing the link's name as a prefix: lexically outside it.
	outside := link + "x/mod"
	for _, tc := range []struct {
		name        string
		root, gomod string
		devErr      error
		wantGomod   string
		wantErr     string // "" means the policy reaches the probe
	}{
		{"resolved", link, filepath.Join(link, "mod"), nil, filepath.Join(real, "mod"), ""},
		{"module cache is the root", link, link, nil, real, ""},
		{"module cache outside every root", link, outside, nil, outside, ""},
		{"missing root", filepath.Join(real, "missing"), filepath.Join(real, "missing"), nil, "", "golem: -sandbox-read-root: "},
		{"developer directory fails", link, link, devFailed, "", "golem: -sandbox-developer-dir: no developer directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateRunEnv(t)
			var probes []agenttools.SandboxConfig
			savedProbe, savedDev := validateSandbox, activeDeveloperDirectory
			t.Cleanup(func() { validateSandbox, activeDeveloperDirectory = savedProbe, savedDev })
			validateSandbox = func(cfg agenttools.SandboxConfig, _ string) error {
				probes = append(probes, cfg)
				return nil
			}
			devCalls := 0
			activeDeveloperDirectory = func(context.Context) (string, error) {
				devCalls++
				return devDir, tc.devErr
			}
			stdin, stdout, stderr := runTestFiles(t)
			err := run([]string{"-config", filepath.Join(t.TempDir(), "missing-models.json"), "-root", t.TempDir(),
				"-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-project-context",
				"-sandbox", "seatbelt", "-sandbox-read-root", tc.root, "-sandbox-env", "GOPROXY=off",
				"-sandbox-env", "GOMODCACHE=" + tc.gomod, "-sandbox-temp-env", "GOCACHE", "-sandbox-developer-dir"}, stdin, stdout, stderr)
			if tc.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) || len(probes) != 0 {
					t.Fatalf("run = %v with %d probes, want %q before any probe", err, len(probes), tc.wantErr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "golem: load config") {
				t.Fatalf("run = %v, want the missing config's error after the sandbox checks", err)
			}
			if devCalls != 1 {
				t.Fatalf("ActiveDeveloperDirectory calls = %d, want 1", devCalls)
			}
			want := []agenttools.SandboxConfig{{Runtime: agenttools.SandboxRuntimeSeatbelt, ReadOnlyRoots: []string{real},
				DeveloperDirectory: devDir, Env: []agenttools.SandboxEnv{{Name: "GOPROXY", Value: "off"}, {Name: "GOMODCACHE", Value: tc.wantGomod}},
				TempEnv: []string{"GOCACHE"}}}
			if !reflect.DeepEqual(probes, want) {
				t.Fatalf("probes = %+v, want %+v", probes, want)
			}
			// The real check refuses a module cache outside every root by its
			// pure table check, before any backend: on every OS, value-free.
			verr := agenttools.ValidateSandbox(probes[0], t.TempDir())
			if refused := verr != nil && strings.Contains(verr.Error(), "GOMODCACHE must be a clean absolute path inside a declared root"); refused != (tc.wantGomod == outside) {
				t.Fatalf("ValidateSandbox = %v, want a GOMODCACHE refusal only for the module cache outside every root", verr)
			}
			if verr != nil && strings.Contains(verr.Error(), outside) {
				t.Fatalf("ValidateSandbox echoed the refused value: %v", verr)
			}
		})
	}
}

// rebaseUnderRoot respells only clean absolute paths lexically in a typed
// root, under the longest such root: an inner root may be a link of its own,
// and the longest match is where the path resolves.
func TestRebaseUnderRoot(t *testing.T) {
	typed := []string{"/t", "/t/inner", "/u"}
	canon := []string{"/c", "/elsewhere", "/u"}
	for _, tc := range []struct{ in, want string }{
		{"/t", "/c"},
		{"/t/mod", "/c/mod"},
		{"/t/inner/mod", "/elsewhere/mod"},
		{"/t/innerx/mod", "/c/innerx/mod"},
		{"/tx/mod", "/tx/mod"},
		{"/u/mod", "/u/mod"},
		{"/other/mod", "/other/mod"},
		{"t/mod", "t/mod"},
		{"/t/../etc", "/t/../etc"},
		{"/t/mod/", "/t/mod/"},
	} {
		if got := rebaseUnderRoot(tc.in, typed, canon); got != tc.want {
			t.Errorf("rebaseUnderRoot(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// isolateRunEnv keeps a run off the developer's real data and config dirs.
func isolateRunEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

// writeRemoteEmbeddingConfig serves a local agent model and configures the
// embedding default on a remote destination that nothing admits: a
// noninteractive run that plans the embedding route fails closed.
func writeRemoteEmbeddingConfig(t *testing.T) (configPath, root string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveCompat(w, r, "agent-model", nil)
	}))
	t.Cleanup(server.Close)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
  "providers": {
    "local":  {"base_url": %q, "api_format": "openai-compat", "timeout": "2s"},
    "remote": {"base_url": "https://embeddings.invalid", "api_format": "openai-compat", "timeout": "2s"}
  },
  "models": {
    "agent": {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "stream", "tool_call"]},
    "embedding": {"name": "qwen3-embedding:8b", "provider": "remote", "type": "embedding", "dimensions": 768,
      "capabilities": ["embed"]}
  },
  "defaults": {"agent": "agent", "embedding": "embedding"}
}`, server.URL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, root
}

func stderrLines(t *testing.T, file *os.File) []string {
	t.Helper()
	return strings.Split(strings.TrimSpace(readRunTestFile(t, file)), "\n")
}

func TestInspectStartupCapabilities(t *testing.T) {
	isolateRunEnv(t)
	configPath, root := writeRemoteEmbeddingConfig(t)
	// The workspace already has an auto index from an earlier Trusted run;
	// Inspect must not infer Trusted from it.
	dbPath, workspaceID, err := indexDBPathForWorkspace(os.Getenv, root)
	if err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dbPath, workspaceID, "remote/qwen3-embedding:8b")
	args := []string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
		"-no-session", "-no-compress", "-no-project-context", "-no-git-context"}

	// Positive control: the same fixture under Trusted plans the remote
	// embedding route and is denied, so the Inspect run below proves the
	// route was never planned rather than that nothing was remote.
	t.Run("trusted control is denied", func(t *testing.T) {
		stdin, stdout, stderr := runTestFiles(t)
		err := run(append(append([]string{}, args...), "-profile", "trusted"), stdin, stdout, stderr)
		if !errors.Is(err, provider.ErrDestinationDenied) {
			t.Fatalf("trusted run = %v, want ErrDestinationDenied\nstderr:\n%s", err, readRunTestFile(t, stderr))
		}
	})

	stdin, stdout, stderr := stdinFileWith(t, "/remember x\n/tools\n")
	var sess *replSession
	var perms golemruntime.EffectivePermissions
	err = run(args, stdin, stdout, stderr, runHooks{
		afterSessionReady: func(s *replSession) error {
			sess = s
			perms = s.runtime.EffectivePermissions()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("inspect run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
	}
	if sess == nil {
		t.Fatal("session was not captured")
	}
	errOut := readRunTestFile(t, stderr)
	if strings.Contains(errOut, "Allow this session") || strings.Contains(errOut, "embeddings.invalid") {
		t.Fatalf("inspect startup asked about the embedding destination:\n%s", errOut)
	}
	if perms.Profile != golemruntime.ProfileInspect || sess.profile != golemruntime.ProfileInspect {
		t.Fatalf("profile = %s (session %s), want inspect", perms.Profile, sess.profile)
	}
	var mounted []string
	for _, tool := range perms.Tools {
		mounted = append(mounted, tool.Name)
	}
	if want := []string{"read_file", "search", "glob", "list"}; !slices.Equal(mounted, want) {
		t.Fatalf("mounted tools = %q, want %q", mounted, want)
	}
	fragment := memorySystemFragment(true)
	if !strings.Contains(fragment, "memory_search") {
		t.Fatalf("fixture: memory fragment %q does not name memory_search", fragment)
	}
	if strings.Contains(sess.baseSystem, fragment) || sess.sysInputs.memory {
		t.Fatalf("inspect system prompt advertises memory_search:\n%s", sess.baseSystem)
	}
	for _, unwanted := range []string{"retrieve unavailable", "retrieve:", "retrieve auto-index", "retrieve disabled"} {
		if strings.Contains(errOut, unwanted) {
			t.Errorf("stderr carries %q under inspect:\n%s", unwanted, errOut)
		}
	}
	// Human memory stays a host operation (spec section 4, REV 3).
	if sess.memory == nil || !strings.Contains(errOut, "memory: enabled") {
		t.Fatalf("human memory store not opened under inspect:\n%s", errOut)
	}
	out := readRunTestFile(t, stdout)
	if strings.Contains(out, "memory disabled") || !strings.Contains(out, "remembered ") {
		t.Fatalf("/remember under inspect:\n%s", out)
	}
	if strings.Contains(out, "retrieve") || strings.Contains(out, "memory_search") {
		t.Fatalf("/tools under inspect names retrieval or memory tools:\n%s", out)
	}
}

// An unusable auto-index path is a retrieval warning; Inspect plans no
// retrieval, so it stays silent there while Trusted still reports it.
func TestInspectSilencesAutoIndexPathWarning(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
		want    bool
	}{
		{"inspect", "inspect", false},
		{"trusted control", "trusted", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateRunEnv(t)
			configPath, root := writeRunLifecycleConfig(t)
			// A data dir inside the workspace makes the auto-index path invalid.
			t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
			stdin, stdout, stderr := runTestFiles(t)
			errStop := errors.New("stop at session ready")
			err := run([]string{"-config", configPath, "-root", root, "-profile", tc.profile, "-no-probe", "-no-cap-probe",
				"-no-session", "-no-memory", "-no-project-context"}, stdin, stdout, stderr,
				runHooks{afterSessionReady: func(*replSession) error { return errStop }})
			if !errors.Is(err, errStop) {
				t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
			}
			if got := strings.Contains(readRunTestFile(t, stderr), "retrieve auto-index disabled"); got != tc.want {
				t.Fatalf("auto-index path warning = %t, want %t\nstderr:\n%s", got, tc.want, readRunTestFile(t, stderr))
			}
		})
	}
}

func TestStartupNoticeInspectNamesTrustedRestore(t *testing.T) {
	isolateRunEnv(t)
	configPath, root := writeRunLifecycleConfig(t)
	stdin, stdout, stderr := runTestFiles(t)
	errStop := errors.New("stop at session ready")
	err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
		"-no-session", "-no-memory", "-no-project-context"}, stdin, stdout, stderr,
		runHooks{afterSessionReady: func(*replSession) error { return errStop }})
	if !errors.Is(err, errStop) {
		t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
	}
	lines := stderrLines(t, stderr)
	want := "permissions: profile=inspect tools=read_file,search,glob,list commands=n/a scratch=off verification=none; restart with -profile trusted to enable retrieval, memory tools, writes or commands"
	if !slices.Contains(lines, want) {
		t.Fatalf("stderr lines = %q, want %q", lines, want)
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "profile:") {
			t.Fatalf("default inspect printed a selection notice %q", line)
		}
	}
	if strings.Contains(readRunTestFile(t, stdout), "permissions") {
		t.Fatalf("stdout carries the notice: %q", readRunTestFile(t, stdout))
	}
}

func TestStartupNoticeTrustedHost(t *testing.T) {
	const host = "commands=host (unsandboxed; runs with the host user's filesystem and network authority)"
	t.Run("selected by -allow-exec", func(t *testing.T) {
		isolateRunEnv(t)
		configPath, root := writeRunLifecycleConfig(t)
		stdin, stdout, stderr := runTestFiles(t)
		errStop := errors.New("stop at session ready")
		var got golemruntime.Profile
		err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
			"-no-session", "-no-memory", "-no-rag", "-no-project-context", "-allow-exec"}, stdin, stdout, stderr,
			runHooks{afterSessionReady: func(s *replSession) error { got = s.profile; return errStop }})
		if !errors.Is(err, errStop) {
			t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
		}
		if got != golemruntime.ProfileTrusted {
			t.Fatalf("session profile = %s, want trusted", got)
		}
		lines := stderrLines(t, stderr)
		for _, want := range []string{
			"profile: trusted (selected by -allow-exec)",
			"permissions: profile=trusted tools=read_file,search,glob,list,run_command,start_command,command_status,command_tail,stop_command " + host + " scratch=off verification=absent",
		} {
			if !slices.Contains(lines, want) {
				t.Errorf("stderr lines = %q, want %q", lines, want)
			}
		}
	})
	t.Run("one-shot json keeps the record clean", func(t *testing.T) {
		isolateRunEnv(t)
		configPath, root := writeRunLifecycleConfig(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serveCompat(w, r, "agent-model", nil)
		}))
		t.Cleanup(server.Close)
		stdin, stdout, stderr := runTestFiles(t)
		err := run([]string{"-config", configPath, "-root", root, "-base-url", server.URL, "-no-probe", "-no-cap-probe",
			"-no-rag", "-no-project-context", "-profile", "trusted", "-p", "hi", "-output-format", "json"},
			stdin, stdout, stderr)
		if err != nil {
			t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
		}
		out := readRunTestFile(t, stdout)
		recLines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(recLines) != 1 {
			t.Fatalf("stdout = %d lines, want one result record:\n%s", len(recLines), out)
		}
		if m := decodeResult(t, recLines[0]); string(m["status"]) != `"completed"` {
			t.Fatalf("status = %s, want completed: %s", m["status"], recLines[0])
		}
		if strings.Contains(out, "permissions") {
			t.Fatalf("result record carries the notice: %s", out)
		}
		lines := stderrLines(t, stderr)
		// One-shot carries no verifier slot, so verification is "none".
		if want := "permissions: profile=trusted tools=read_file,search,glob,list " + host + " scratch=off verification=none"; !slices.Contains(lines, want) {
			t.Fatalf("stderr lines = %q, want %q", lines, want)
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "profile:") {
				t.Fatalf("explicit -profile printed a selection notice %q", line)
			}
		}
	})
}

// AgentFlow modes keep their own tool policy: Trusted, silently.
func TestAgentflowModesPrintNoProfileNotice(t *testing.T) {
	plan := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(plan, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"plan", []string{"-plan", plan, "-approve-plan-edits", "-approve-plan-gates"}},
		{"goal", []string{"-goal", "plan it"}},
		{"resume", []string{"-agentflow-resume", "-plan", plan, "-approve-plan-edits", "-approve-plan-gates"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateRunEnv(t)
			configPath, root := writeRunLifecycleConfig(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serveCompat(w, r, "agent-model", nil)
			}))
			t.Cleanup(server.Close)
			stdin, stdout, stderr := runTestFiles(t)
			errStop := errors.New("stop at session ready")
			var got golemruntime.Profile
			args := append([]string{"-config", configPath, "-root", root, "-base-url", server.URL,
				"-no-probe", "-no-cap-probe", "-no-project-context"}, tc.args...)
			err := run(args, stdin, stdout, stderr, runHooks{afterSessionReady: func(s *replSession) error {
				got = s.runtime.EffectivePermissions().Profile
				return errStop
			}})
			if !errors.Is(err, errStop) {
				t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
			}
			if got != golemruntime.ProfileTrusted {
				t.Fatalf("agentflow runtime profile = %s, want trusted", got)
			}
			for _, line := range stderrLines(t, stderr) {
				if strings.HasPrefix(line, "permissions:") || strings.HasPrefix(line, "profile:") {
					t.Fatalf("agentflow mode printed %q", line)
				}
			}
		})
	}
}
