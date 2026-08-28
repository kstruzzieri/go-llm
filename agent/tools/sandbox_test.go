package tools

import (
	"context"
	"encoding/json"
	"math"
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

func TestSandboxedConstructorsFailClosed(t *testing.T) {
	cfg := SandboxConfig{Runtime: "container"}
	if _, err := NewSandboxedBackgroundManager(cfg); err == nil {
		t.Fatal("background manager accepted unimplemented runtime")
	}
	if _, err := NewSandboxedExecTools(t.TempDir(), cfg); err == nil {
		t.Fatal("foreground tools accepted unimplemented runtime")
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
	want := `runtime="bwrap" network=denied memory_cap=none cpu_limit=none drop_caps=[] temp=private`
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
	want := `runtime="bwrap" network=denied memory_cap=512MiB/process cpu_limit=none drop_caps=[] temp=private`
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
