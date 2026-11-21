package tools

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
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

func TestNewExecBackendHost(t *testing.T) {
	for _, cfg := range []SandboxConfig{{}, {Runtime: SandboxRuntimeHost}} {
		got, err := newExecBackend(cfg)
		if err != nil {
			t.Fatalf("newExecBackend(%+v): %v", cfg, err)
		}
		host, ok := got.execBackend.(hostBackend)
		if !ok {
			t.Fatalf("backend = %T, want hostBackend", got.execBackend)
		}
		if host.commandRunner == nil || host.backgroundStarter == nil {
			t.Fatal("host backend must contain both execution seams")
		}
		if got.sandbox.Runtime != SandboxRuntimeHost {
			t.Fatalf("stored runtime = %q, want host", got.sandbox.Runtime)
		}
	}
}

func TestNewExecBackendFailsClosed(t *testing.T) {
	got, err := newExecBackend(SandboxConfig{Runtime: "container"})
	if err == nil || got.execBackend != nil {
		t.Fatalf("unimplemented runtime returned backend=%T err=%v", got.execBackend, err)
	}
	if !strings.Contains(err.Error(), `"container"`) {
		t.Fatalf("error must name runtime: %v", err)
	}
}

func TestNewExecBackendRejectsInvalidConfig(t *testing.T) {
	if _, err := newExecBackend(SandboxConfig{MemoryCapMB: 1}); err == nil {
		t.Fatal("dispatch accepted invalid host constraints")
	}
}

func mustNormalizeSandbox(t *testing.T, cfg SandboxConfig) SandboxConfig {
	t.Helper()
	got, err := normalizeSandboxConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSandboxApprovalHostIsEmpty(t *testing.T) {
	got := approvalForSandbox(mustNormalizeSandbox(t, SandboxConfig{}))
	if got != (sandboxApproval{}) {
		t.Fatalf("host approval metadata = %+v, want zero value", got)
	}
}

func TestSandboxFingerprintHostVector(t *testing.T) {
	// printf 'runtime\0host\0allow_network\0false\0memory_cap_mb\0000\0cpu_limit\0000\0drop_caps\0' | shasum -a 256
	got := sandboxFingerprint(mustNormalizeSandbox(t, SandboxConfig{}))
	if want := "a396771a1742ed98ad837e2ba6ff902e5d725bf811708e3c3f9646e9fa4ed454"; got != want {
		t.Fatalf("host sandboxFingerprint = %s, want %s", got, want)
	}
}

func TestSandboxFingerprintVariesByEveryField(t *testing.T) {
	base := mustNormalizeSandbox(t, SandboxConfig{Runtime: "container"})
	baseFP := sandboxFingerprint(base)
	variants := []SandboxConfig{
		{Runtime: "bubblewrap"},
		{Runtime: "container", AllowNetwork: true},
		{Runtime: "container", MemoryCapMB: 512},
		{Runtime: "container", CPULimit: 1.5},
		{Runtime: "container", DropCaps: []string{"ALL"}},
	}
	for _, cfg := range variants {
		if got := sandboxFingerprint(mustNormalizeSandbox(t, cfg)); got == baseFP {
			t.Errorf("config %+v reused base fingerprint", cfg)
		}
	}
}

func TestSandboxFingerprintCapsUseSetSemantics(t *testing.T) {
	a := mustNormalizeSandbox(t, SandboxConfig{
		Runtime: "container", DropCaps: []string{"SYS_ADMIN", "NET_RAW", "NET_RAW"},
	})
	b := mustNormalizeSandbox(t, SandboxConfig{
		Runtime: "container", DropCaps: []string{"NET_RAW", "SYS_ADMIN"},
	})
	if sandboxFingerprint(a) != sandboxFingerprint(b) {
		t.Fatal("equivalent capability sets produced different fingerprints")
	}
}

func TestSandboxApprovalCanonicalizesSignedZeroCPU(t *testing.T) {
	positive := approvalForSandbox(mustNormalizeSandbox(t, SandboxConfig{
		Runtime: "container", CPULimit: 0,
	}))
	negative := approvalForSandbox(mustNormalizeSandbox(t, SandboxConfig{
		Runtime: "container", CPULimit: math.Copysign(0, -1),
	}))
	if negative != positive {
		t.Fatalf("approvalForSandbox(CPULimit=-0) = %+v, want %+v", negative, positive)
	}
}

func TestRenderSandboxLine(t *testing.T) {
	cfg := mustNormalizeSandbox(t, SandboxConfig{
		Runtime: "container", MemoryCapMB: 512, CPULimit: 1.5,
		DropCaps: []string{"SYS_ADMIN", "NET_RAW"},
	})
	got := renderSandboxLine(cfg)
	want := `runtime="container" network=denied memory_cap=512MiB cpu_limit=1.5 drop_caps=["NET_RAW","SYS_ADMIN"]`
	if got != want {
		t.Fatalf("renderSandboxLine() = %q, want %q", got, want)
	}
}

func TestRenderSandboxLineNoRequestedLimits(t *testing.T) {
	cfg := mustNormalizeSandbox(t, SandboxConfig{Runtime: "container", AllowNetwork: true})
	got := renderSandboxLine(cfg)
	want := `runtime="container" network=allowed memory_cap=none cpu_limit=none drop_caps=[]`
	if got != want {
		t.Fatalf("renderSandboxLine() = %q, want %q", got, want)
	}
}

func TestRenderSandboxLineQuotesDelimiters(t *testing.T) {
	cfg := mustNormalizeSandbox(t, SandboxConfig{
		Runtime: "container=custom", DropCaps: []string{"NET_RAW,ALL"},
	})
	got := renderSandboxLine(cfg)
	if !strings.Contains(got, `runtime="container=custom"`) ||
		!strings.Contains(got, `drop_caps=["NET_RAW,ALL"]`) {
		t.Fatalf("sandbox preview is structurally ambiguous: %q", got)
	}
}

type markerRunner struct{ calls int }

func (m *markerRunner) Run(context.Context, execSpec) (execResult, error) {
	m.calls++
	return execResult{ExitCode: 0, Stdout: []byte("marker")}, nil
}

func TestExecToolsWithBackgroundUseManagerBackend(t *testing.T) {
	marker := &markerRunner{}
	backend := bindExecBackend(hostBackend{
		commandRunner: marker, backgroundStarter: starterOf(),
	}, mustNormalizeSandbox(t, SandboxConfig{}))
	m := newBackgroundManagerWithBackend(backend, &countingRandom{})
	t.Cleanup(m.Shutdown)

	toolsList, err := NewExecToolsWithBackground(t.TempDir(), m)
	if err != nil {
		t.Fatal(err)
	}
	rc := toolsList[0].(*RunCommand)
	raw := json.RawMessage(`{"argv":["go","version"]}`)
	if _, err := rc.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Invoke(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if marker.calls != 1 {
		t.Fatalf("run_command bypassed manager backend; calls = %d", marker.calls)
	}
}

// TestSandboxedConstructorsFailClosed pins that every sandboxed exec
// constructor refuses an unimplemented runtime with ErrSandboxPolicy.
func TestSandboxedConstructorsFailClosed(t *testing.T) {
	cfg := SandboxConfig{Runtime: "container"}
	if _, err := NewSandboxedBackgroundManager(cfg); !errors.Is(err, ErrSandboxPolicy) {
		t.Fatalf("background manager = %v, want ErrSandboxPolicy", err)
	}
	if _, err := NewSandboxedExecTools(t.TempDir(), cfg); !errors.Is(err, ErrSandboxPolicy) {
		t.Fatalf("foreground tools = %v, want ErrSandboxPolicy", err)
	}
	scratch := ExecToolsOptions{Scratch: ScratchConfig{Enabled: true}}
	if _, err := NewSandboxedExecToolsWithOptions(t.TempDir(), cfg, scratch); !errors.Is(err, ErrSandboxPolicy) {
		t.Fatalf("scratch tools = %v, want ErrSandboxPolicy", err)
	}
}

func TestNewExecToolsWithBackgroundRejectsUnresolvedManager(t *testing.T) {
	if _, err := NewExecToolsWithBackground(t.TempDir(), &BackgroundManager{}); err == nil {
		t.Fatal("accepted manager without a resolved backend")
	}
}

func TestRunCommandSandboxApproval(t *testing.T) {
	rc, _ := planKeyRC(t)
	cfg := mustNormalizeSandbox(t, SandboxConfig{
		Runtime: "container", MemoryCapMB: 512, CPULimit: 1.5,
		DropCaps: []string{"SYS_ADMIN", "NET_RAW"},
	})
	rc.sandbox = approvalForSandbox(cfg)
	plan, err := rc.Plan(context.Background(), json.RawMessage(`{"argv":["go","version"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "exec:v3:" + rc.sandbox.keyComponent; !strings.HasPrefix(plan.ApprovalKey, want) {
		t.Fatalf("key = %q, want prefix %q", plan.ApprovalKey, want)
	}
	if !strings.Contains(plan.Preview, "sandbox: "+rc.sandbox.preview) {
		t.Fatalf("preview missing sandbox policy:\n%s", plan.Preview)
	}
}

func TestSeatbeltPlansRejectVolumeRootBeforeApproval(t *testing.T) {
	ws, err := NewWorkspace("/")
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"argv":["go","version"]}`)
	cfg := mustNormalizeSandbox(t, SandboxConfig{Runtime: SandboxRuntimeSeatbelt})
	approval := approvalForSandbox(cfg)

	t.Run("foreground", func(t *testing.T) {
		rc := NewRunCommand(ws, &markerRunner{})
		rc.sandbox = approval
		plan, err := rc.Plan(context.Background(), raw)
		if err == nil || !strings.Contains(err.Error(), "workspace root") {
			t.Fatalf("RunCommand.Plan(/) error = %v, want Seatbelt workspace-root rejection", err)
		}
		if plan.ApprovalKey != "" {
			t.Errorf("RunCommand.Plan(/) approval key = %q, want empty", plan.ApprovalKey)
		}
	})

	t.Run("direct background", func(t *testing.T) {
		backend := bindExecBackend(hostBackend{
			commandRunner: &markerRunner{}, backgroundStarter: starterOf(),
		}, cfg)
		manager := newBackgroundManagerWithBackend(backend, &countingRandom{})
		t.Cleanup(manager.Shutdown)
		plan, err := NewStartCommand(ws, manager).Plan(context.Background(), raw)
		if err == nil || !strings.Contains(err.Error(), "workspace root") {
			t.Fatalf("StartCommand.Plan(/) error = %v, want Seatbelt workspace-root rejection", err)
		}
		if plan.ApprovalKey != "" {
			t.Errorf("StartCommand.Plan(/) approval key = %q, want empty", plan.ApprovalKey)
		}
	})

	t.Run("host control", func(t *testing.T) {
		rc := NewRunCommand(ws, &markerRunner{})
		if _, err := rc.Plan(context.Background(), raw); err != nil {
			t.Fatalf("host RunCommand.Plan(/): %v", err)
		}
	})
}

func TestStartCommandDerivesSandboxApprovalFromManager(t *testing.T) {
	cfg := mustNormalizeSandbox(t, SandboxConfig{Runtime: "container", MemoryCapMB: 512})
	backend := bindExecBackend(hostBackend{
		commandRunner: &markerRunner{}, backgroundStarter: starterOf(),
	}, cfg)
	m := newBackgroundManagerWithBackend(backend, &countingRandom{})
	t.Cleanup(m.Shutdown)
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Direct construction is the regression path: no tool-set constructor may
	// be required to stamp the manager's sandbox identity.
	sc := NewStartCommand(ws, m)
	plan, err := sc.Plan(context.Background(), json.RawMessage(`{"argv":["go","version"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "exec-bg:v2:" + backend.approval.keyComponent; !strings.HasPrefix(plan.ApprovalKey, want) {
		t.Fatalf("key = %q, want prefix %q", plan.ApprovalKey, want)
	}
	if !strings.Contains(plan.Preview, "sandbox:  "+backend.approval.preview) {
		t.Fatalf("preview missing manager sandbox policy:\n%s", plan.Preview)
	}
}

func TestStartCommandPlanRejectsUnresolvedManager(t *testing.T) {
	raw := json.RawMessage(`{"argv":["go","version"]}`)
	for name, manager := range map[string]*BackgroundManager{
		"nil":  nil,
		"zero": {},
	} {
		t.Run(name, func(t *testing.T) {
			sc := NewStartCommand(nil, manager)
			if _, err := sc.Plan(context.Background(), raw); err == nil {
				t.Fatal("Plan accepted a manager without a resolved backend")
			}
		})
	}
}

// TestHostPlansCarryNoSandboxArtifacts pins the host shape ABSOLUTELY, not
// relative to another constructor: equivalence checks cannot catch a defect
// that alters both sides identically (an unconditional sandbox line renders
// on legacy and zero-config plans alike). Host previews must not contain a
// sandbox line and host keys must not contain the sandbox namespace.
func TestHostPlansCarryNoSandboxArtifacts(t *testing.T) {
	raw := json.RawMessage(`{"argv":["go","version"]}`)

	rc, _ := planKeyRC(t)
	fgPlan, err := rc.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fgPlan.Preview, "sandbox") {
		t.Fatalf("host foreground preview must carry no sandbox line:\n%s", fgPlan.Preview)
	}
	if strings.Contains(fgPlan.ApprovalKey, "sb:") {
		t.Fatalf("host foreground key must carry no sandbox namespace: %q", fgPlan.ApprovalKey)
	}

	m := NewBackgroundManager()
	t.Cleanup(m.Shutdown)
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sc := NewStartCommand(ws, m)
	bgPlan, err := sc.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bgPlan.Preview, "sandbox") {
		t.Fatalf("host background preview must carry no sandbox line:\n%s", bgPlan.Preview)
	}
	if strings.Contains(bgPlan.ApprovalKey, "sb:") {
		t.Fatalf("host background key must carry no sandbox namespace: %q", bgPlan.ApprovalKey)
	}
}

func TestZeroConfigConstructorsMatchLegacyHostPlans(t *testing.T) {
	raw := json.RawMessage(`{"argv":["go","version"]}`)
	root := t.TempDir()

	legacyFG, err := NewExecTools(root)
	if err != nil {
		t.Fatal(err)
	}
	zeroFG, err := NewSandboxedExecTools(root, SandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	legacyPlan, err := legacyFG[0].(*RunCommand).Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	zeroPlan, err := zeroFG[0].(*RunCommand).Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if legacyPlan.ApprovalKey != zeroPlan.ApprovalKey || legacyPlan.Preview != zeroPlan.Preview {
		t.Fatal("zero-config foreground plan differs from legacy host plan")
	}

	legacyManager := NewBackgroundManager()
	zeroManager, err := NewSandboxedBackgroundManager(SandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(legacyManager.Shutdown)
	t.Cleanup(zeroManager.Shutdown)
	legacyBG, err := NewExecToolsWithBackground(root, legacyManager)
	if err != nil {
		t.Fatal(err)
	}
	zeroBG, err := NewExecToolsWithBackground(root, zeroManager)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 1} {
		left, err := legacyBG[index].(agent.PlanningTool).Plan(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		right, err := zeroBG[index].(agent.PlanningTool).Plan(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		if left.ApprovalKey != right.ApprovalKey || left.Preview != right.Preview {
			t.Fatalf("zero-config tool index %d differs from legacy host plan", index)
		}
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

func TestRenderSandboxLineBwrapPrivateTempMarker(t *testing.T) {
	cfg := mustNormalizeSandbox(t, SandboxConfig{Runtime: SandboxRuntimeBwrap})
	got := renderSandboxLine(cfg)
	want := `runtime="bwrap" network=ip-denied memory_cap=none cpu_limit=none drop_caps=[] temp=private git_metadata=ro(existing)`
	if got != want {
		t.Fatalf("renderSandboxLine() = %q, want %q", got, want)
	}
}

func TestBwrapApprovalKeyDistinctFromSeatbeltAndHost(t *testing.T) {
	bwrap := approvalForSandbox(mustNormalizeSandbox(t, SandboxConfig{Runtime: SandboxRuntimeBwrap}))
	seatbelt := approvalForSandbox(mustNormalizeSandbox(t, SandboxConfig{Runtime: SandboxRuntimeSeatbelt}))
	host := approvalForSandbox(mustNormalizeSandbox(t, SandboxConfig{}))
	if bwrap.keyComponent == "" || !strings.HasPrefix(bwrap.keyComponent, "sb:") ||
		!strings.HasSuffix(bwrap.keyComponent, ":") {
		t.Fatalf("bwrap key component malformed: %q", bwrap.keyComponent)
	}
	if bwrap.keyComponent == seatbelt.keyComponent {
		t.Fatalf("bwrap and seatbelt approval namespaces collide: %q", bwrap.keyComponent)
	}
	if host.keyComponent != "" {
		t.Fatalf("host key component = %q, want empty", host.keyComponent)
	}
	if bwrap.runtime != SandboxRuntimeBwrap {
		t.Fatalf("stored runtime = %q, want bwrap", bwrap.runtime)
	}
}

func TestBwrapApprovalValidatesWorkspace(t *testing.T) {
	appr := sandboxApproval{runtime: SandboxRuntimeBwrap}
	for _, bad := range []string{"/", "/usr", "rel/path", "/home/u/../u", ""} {
		if err := appr.validateWorkspace(bad); err == nil {
			t.Errorf("workspace %q accepted", bad)
		}
	}
	if err := appr.validateWorkspace("/home/user/proj"); err != nil {
		t.Errorf("valid workspace rejected: %v", err)
	}
}

// TestRenderSandboxLineBwrapQualifiesMemoryScope pins the scope qualifier: the
// approved value is spent as three additive budgets (RLIMIT_AS plus a quota on
// each private tmpfs) and RLIMIT_AS is per process, so a bare "512MiB" would
// claim a total ceiling the backend does not enforce.
func TestRenderSandboxLineBwrapQualifiesMemoryScope(t *testing.T) {
	cfg := mustNormalizeSandbox(t, SandboxConfig{Runtime: SandboxRuntimeBwrap, MemoryCapMB: 512})
	got := renderSandboxLine(cfg)
	want := `runtime="bwrap" network=ip-denied memory_cap=512MiB/process cpu_limit=none drop_caps=[] temp=private git_metadata=ro(existing)`
	if got != want {
		t.Fatalf("renderSandboxLine() = %q, want %q", got, want)
	}
}

// TestRenderSandboxLineQualifierIsBwrapOnly keeps the qualifier from leaking
// onto runtimes whose ceiling semantics it does not describe: Seatbelt rejects
// MemoryCapMB outright, and an uncapped bwrap config has no number to qualify.
func TestRenderSandboxLineQualifierIsBwrapOnly(t *testing.T) {
	other := mustNormalizeSandbox(t, SandboxConfig{Runtime: "container", MemoryCapMB: 512})
	if got := renderSandboxLine(other); strings.Contains(got, "/process") {
		t.Fatalf("non-bwrap runtime carries the bwrap scope qualifier: %q", got)
	}
	uncapped := mustNormalizeSandbox(t, SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if got := renderSandboxLine(uncapped); !strings.Contains(got, "memory_cap=none") ||
		strings.Contains(got, "/process") {
		t.Fatalf("uncapped bwrap config must render an unqualified none: %q", got)
	}
}

func TestRenderSandboxLineGitMetadata(t *testing.T) {
	for _, tc := range []struct {
		rt   SandboxRuntime
		want string
	}{
		{SandboxRuntimeSeatbelt, " temp=private git_metadata=ro"},
		{SandboxRuntimeBwrap, " temp=private git_metadata=ro(existing)"},
	} {
		// containment: GITMETA-PREVIEW
		got := renderSandboxLine(mustNormalizeSandbox(t, SandboxConfig{Runtime: tc.rt}))
		if !strings.HasSuffix(got, tc.want) {
			t.Errorf("%s line = %q, want suffix %q", tc.rt, got, tc.want)
		}
	}
	for _, rt := range []SandboxRuntime{SandboxRuntimeHost, SandboxRuntime("other")} {
		if got := renderSandboxLine(SandboxConfig{Runtime: rt}); strings.Contains(got, "git_metadata") {
			t.Errorf("%q line = %q, must not mention git_metadata", rt, got)
		}
	}
}

// The #721 git-metadata boundary rides on the preview only; the grant key
// must not move, or every existing session grant would silently re-prompt.
func TestSandboxApprovalKeyUnmovedBy721(t *testing.T) {
	for rt, want := range map[SandboxRuntime]string{
		SandboxRuntimeSeatbelt: "sb:388c0ebf98f177d27a491abd31245b77ca188a9244797d985f89e788bf011e63:",
		SandboxRuntimeBwrap:    "sb:efa4aede38982cbe6ca95a4926f873f54881ad881c7e3d13c7e3236d1d4a46e9:",
	} {
		n := mustNormalizeSandbox(t, SandboxConfig{Runtime: rt})
		// containment: GITMETA-PREVIEW
		if got := approvalForSandbox(n).keyComponent; got != want {
			t.Errorf("%s keyComponent = %q, want %q", rt, got, want)
		}
	}
}

// toolchainConfig is a Seatbelt policy using every #722 field, roots and Env
// deliberately out of order.
func toolchainConfig() SandboxConfig {
	return SandboxConfig{
		Runtime:            SandboxRuntimeSeatbelt,
		ReadOnlyRoots:      []string{"/opt/b", "/opt/a"},
		DeveloperDirectory: "/Library/Developer/CommandLineTools",
		Env:                []SandboxEnv{{Name: "GOPROXY", Value: "off"}, {Name: "GOMODCACHE", Value: "/opt/a/mod"}},
		TempEnv:            []string{"GOCACHE"},
	}
}

func TestSandboxFingerprintNewFields(t *testing.T) {
	// Each list is framed as label, count, items and sits before the unframed
	// drop_caps tail; computed outside Go:
	// python3 -c "import hashlib; print(hashlib.sha256(b''.join(t.encode()+b'\0' for t in
	//   ['runtime','seatbelt','allow_network','false','memory_cap_mb','0','cpu_limit','0',
	//    'read_only_roots','2','/opt/a','/opt/b','developer_directory','/Library/Developer/CommandLineTools',
	//    'env','2','GOMODCACHE','/opt/a/mod','GOPROXY','off','temp_env','1','GOCACHE','drop_caps'])).hexdigest())"
	full := mustNormalizeSandbox(t, toolchainConfig())
	if got, want := sandboxFingerprint(full), "4e780b30cfa2679b0214a2550b4f058b1fbca7d3f9413fd9aa1549966416de55"; got != want {
		t.Errorf("sandboxFingerprint(all #722 fields) = %s, want %s", got, want)
	}

	// Empty but non-nil lists add nothing: existing keys do not move.
	plain := sandboxFingerprint(mustNormalizeSandbox(t, SandboxConfig{Runtime: SandboxRuntimeSeatbelt}))
	empty := sandboxFingerprint(mustNormalizeSandbox(t, SandboxConfig{
		Runtime: SandboxRuntimeSeatbelt, ReadOnlyRoots: []string{}, Env: []SandboxEnv{}, TempEnv: []string{},
	}))
	if empty != plain {
		t.Errorf("empty #722 lists moved the key: %s, want %s", empty, plain)
	}

	base := SandboxConfig{
		Runtime:       SandboxRuntimeSeatbelt,
		ReadOnlyRoots: []string{"/opt/a"},
		Env:           []SandboxEnv{{Name: "GOMODCACHE", Value: "/opt/a/mod"}},
	}
	baseFP := sandboxFingerprint(mustNormalizeSandbox(t, base))
	for name, edit := range map[string]func(*SandboxConfig){
		"one added root":      func(c *SandboxConfig) { c.ReadOnlyRoots = append(c.ReadOnlyRoots, "/opt/b") },
		"developer directory": func(c *SandboxConfig) { c.DeveloperDirectory = "/Library/Developer/CommandLineTools" },
		"GOMODCACHE":          func(c *SandboxConfig) { c.Env = []SandboxEnv{{Name: "GOMODCACHE", Value: "/opt/a/mod2"}} },
		"temp name":           func(c *SandboxConfig) { c.TempEnv = []string{"GOCACHE"} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base.Clone()
			edit(&cfg)
			if sandboxFingerprint(mustNormalizeSandbox(t, cfg)) == baseFP {
				t.Fatalf("%s did not change the key", name)
			}
		})
	}

	reordered := toolchainConfig()
	reordered.ReadOnlyRoots = []string{"/opt/a", "/opt/b", "/opt/a"}
	reordered.Env = []SandboxEnv{reordered.Env[1], reordered.Env[0]}
	if got := sandboxFingerprint(mustNormalizeSandbox(t, reordered)); got != sandboxFingerprint(full) {
		t.Fatal("root or Env order (or a repeated root) changed the key")
	}
}

func TestRenderSandboxLineNewFields(t *testing.T) {
	cfg := toolchainConfig()
	cfg.Env[1].Value = "/opt/a/sentinel-7f3a"
	got := renderSandboxLine(mustNormalizeSandbox(t, cfg))
	want := `runtime="seatbelt" network=denied memory_cap=none cpu_limit=none drop_caps=[] temp=private git_metadata=ro` +
		` read_only_roots=["/opt/a","/opt/b"] developer_directory="/Library/Developer/CommandLineTools"` +
		` env=["GIT_CONFIG_GLOBAL=/dev/null","GIT_CONFIG_NOSYSTEM=1","GOCACHE=<private temp>/gocache",` +
		`"GOMODCACHE=/opt/a/sentinel-7f3a","GOPROXY=off","PYTHONDONTWRITEBYTECODE=1"]`
	if got != want {
		t.Fatalf("renderSandboxLine() =\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(got, "/opt/a/sentinel-7f3a") {
		t.Fatalf("preview lost the GOMODCACHE sentinel: %s", got)
	}
	// A quote in a root cannot forge a field boundary.
	quoted := renderSandboxLine(SandboxConfig{Runtime: SandboxRuntimeSeatbelt, ReadOnlyRoots: []string{`/opt/a" env=["X`}})
	if !strings.Contains(quoted, `read_only_roots=["/opt/a\" env=[\"X"]`) {
		t.Fatalf("root not quoted: %s", quoted)
	}
}

func TestSandboxEffectiveEnv(t *testing.T) {
	if got := (SandboxConfig{}).EffectiveEnv("<private temp>"); len(got) != 0 {
		t.Fatalf("zero config EffectiveEnv = %q, want none", got)
	}
	cfg := toolchainConfig()
	cfg.DeveloperDirectory = ""
	got := cfg.EffectiveEnv("<private temp>")
	want := []string{"GOCACHE=<private temp>/gocache", "GOMODCACHE=/opt/a/mod", "GOPROXY=off"}
	if !slices.Equal(got, want) {
		t.Fatalf("EffectiveEnv without developer directory = %q, want %q", got, want)
	}
	if got := cfg.EffectiveEnv("/private/tmp/go-llm-seatbelt-1"); !slices.Contains(got, "GOCACHE=/private/tmp/go-llm-seatbelt-1/gocache") {
		t.Fatalf("EffectiveEnv(real temp) = %q, want GOCACHE under it", got)
	}
	cfg.DeveloperDirectory = "/Library/Developer/CommandLineTools"
	got = cfg.EffectiveEnv("<private temp>")
	for _, d4 := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PYTHONDONTWRITEBYTECODE=1"} {
		if !slices.Contains(got, d4) {
			t.Errorf("EffectiveEnv with developer directory = %q, missing %q", got, d4)
		}
	}
	if !slices.IsSorted(got) || len(got) != len(want)+3 {
		t.Fatalf("EffectiveEnv = %q, want the %d entries sorted", got, len(want)+3)
	}

	// Never normalized: an unknown temp name is skipped, a repeated one and
	// an Env entry of the same name do not add a second GOCACHE.
	raw := SandboxConfig{
		Env:     []SandboxEnv{{Name: "GOCACHE", Value: "/Users/victim/cache"}},
		TempEnv: []string{"GOCACHE", "XDG_CACHE_HOME", "GOCACHE"},
	}
	if got, want := raw.EffectiveEnv("<private temp>"), []string{"GOCACHE=<private temp>/gocache"}; !slices.Equal(got, want) {
		t.Fatalf("EffectiveEnv(un-normalized) = %q, want %q", got, want)
	}
}

func TestSandboxConfigClone(t *testing.T) {
	if got := (SandboxConfig{}).Clone(); !reflect.DeepEqual(got, SandboxConfig{}) {
		t.Fatalf("zero Clone = %+v, want zero", got)
	}
	orig := toolchainConfig()
	orig.DropCaps = []string{"ALL"}
	clone := orig.Clone()
	if !reflect.DeepEqual(clone, orig) {
		t.Fatalf("Clone = %+v, want %+v", clone, orig)
	}
	// Every slice field, including any added later, must be populated here
	// and must not share its backing array with the original.
	ov, cv := reflect.ValueOf(orig), reflect.ValueOf(clone)
	for i := range ov.NumField() {
		name := ov.Type().Field(i).Name
		if ov.Field(i).Kind() != reflect.Slice {
			continue
		}
		if ov.Field(i).Len() == 0 {
			t.Fatalf("test leaves slice field %s empty; populate it", name)
		}
		if ov.Field(i).Pointer() == cv.Field(i).Pointer() {
			t.Errorf("Clone shares %s with the original", name)
		}
	}
}
