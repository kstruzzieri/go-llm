package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// enabledScratchConfig returns a fully explicit enabled config so tests can
// flip exactly one field at a time.
func enabledScratchConfig() ScratchConfig {
	return ScratchConfig{
		Enabled:               true,
		MaxConcurrentSessions: 2,
		MaxWorkspaceFiles:     100_000,
		MaxWorkspaceBytes:     1 << 30,
		MaxChangedFiles:       64,
		MaxFileBytes:          256 << 10,
		MaxTotalBytes:         4 << 20,
		SnapshotTimeout:       30 * time.Second,
		CaptureTimeout:        30 * time.Second,
	}
}

func TestScratchConfigNormalizeDefaults(t *testing.T) {
	got, err := normalizeScratchConfig(ScratchConfig{Enabled: true})
	if err != nil {
		t.Fatalf("normalize enabled zero config: %v", err)
	}
	want := enabledScratchConfig()
	if got != want {
		t.Fatalf("defaults not applied:\n got %+v\nwant %+v", got, want)
	}
}

func TestScratchConfigNormalizeExplicitValuesKept(t *testing.T) {
	in := ScratchConfig{
		Enabled:               true,
		MaxConcurrentSessions: 1,
		MaxWorkspaceFiles:     10,
		MaxWorkspaceBytes:     1 << 20,
		MaxChangedFiles:       3,
		MaxFileBytes:          1 << 10,
		MaxTotalBytes:         2 << 10,
		SnapshotTimeout:       time.Second,
		CaptureTimeout:        2 * time.Second,
	}
	got, err := normalizeScratchConfig(in)
	if err != nil {
		t.Fatalf("normalize explicit config: %v", err)
	}
	if got != in {
		t.Fatalf("explicit values rewritten:\n got %+v\nwant %+v", got, in)
	}
}

func TestScratchConfigDisabledZeroValue(t *testing.T) {
	got, err := normalizeScratchConfig(ScratchConfig{})
	if err != nil {
		t.Fatalf("normalize zero config: %v", err)
	}
	if got != (ScratchConfig{}) {
		t.Fatalf("disabled config must normalize to the zero value, got %+v", got)
	}
}

func TestScratchConfigNormalizeRejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*ScratchConfig)
	}{
		{"disabled with sessions", func(c *ScratchConfig) { c.Enabled = false; c.MaxConcurrentSessions = 2 }},
		{"disabled with files", func(c *ScratchConfig) { c.Enabled = false; c.MaxWorkspaceFiles = 1 }},
		{"disabled with workspace bytes", func(c *ScratchConfig) { c.Enabled = false; c.MaxWorkspaceBytes = 1 }},
		{"disabled with changes", func(c *ScratchConfig) { c.Enabled = false; c.MaxChangedFiles = 1 }},
		{"disabled with file bytes", func(c *ScratchConfig) { c.Enabled = false; c.MaxFileBytes = 1 }},
		{"disabled with total bytes", func(c *ScratchConfig) { c.Enabled = false; c.MaxTotalBytes = 1 }},
		{"disabled with snapshot timeout", func(c *ScratchConfig) { c.Enabled = false; c.SnapshotTimeout = time.Second }},
		{"disabled with capture timeout", func(c *ScratchConfig) { c.Enabled = false; c.CaptureTimeout = time.Second }},
		{"negative sessions", func(c *ScratchConfig) { c.MaxConcurrentSessions = -1 }},
		{"negative files", func(c *ScratchConfig) { c.MaxWorkspaceFiles = -1 }},
		{"negative workspace bytes", func(c *ScratchConfig) { c.MaxWorkspaceBytes = -1 }},
		{"negative changes", func(c *ScratchConfig) { c.MaxChangedFiles = -1 }},
		{"negative file bytes", func(c *ScratchConfig) { c.MaxFileBytes = -1 }},
		{"negative total bytes", func(c *ScratchConfig) { c.MaxTotalBytes = -1 }},
		{"negative snapshot timeout", func(c *ScratchConfig) { c.SnapshotTimeout = -time.Second }},
		{"negative capture timeout", func(c *ScratchConfig) { c.CaptureTimeout = -time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := enabledScratchConfig()
			tc.mut(&cfg)
			if _, err := normalizeScratchConfig(cfg); err == nil {
				t.Fatalf("normalize accepted invalid config %+v", cfg)
			}
		})
	}
}

func TestScratchRuntimeRejectsWorkspaceAtTempBase(t *testing.T) {
	base, err := CanonicalWorkspaceRoot(scratchTempBase())
	if err != nil {
		t.Skipf("scratch temp base unavailable: %v", err)
	}
	if _, err := newScratchRuntime(base, ScratchConfig{Enabled: true}, nil, SandboxRuntimeHost); err == nil {
		t.Fatalf("scratch runtime accepted workspace equal to temp base %q", base)
	}
}

func TestScratchEffectGraceIsFiveSeconds(t *testing.T) {
	if scratchEffectGrace != 5*time.Second {
		t.Fatalf("scratchEffectGrace = %v, approved value is 5s", scratchEffectGrace)
	}
}

func TestScratchOuterTimeoutSum(t *testing.T) {
	cfg := enabledScratchConfig()
	got, err := scratchOuterTimeout(cfg, 90*time.Second)
	if err != nil {
		t.Fatalf("outer timeout: %v", err)
	}
	want := 30*time.Second + 90*time.Second + 2*30*time.Second + 5*time.Second
	if got != want {
		t.Fatalf("outer timeout = %v, want %v", got, want)
	}
}

func TestScratchOuterTimeoutOverflow(t *testing.T) {
	cfg := enabledScratchConfig()
	cfg.SnapshotTimeout = time.Duration(1<<62) + 1<<61
	cfg.CaptureTimeout = time.Duration(1<<62) + 1<<61
	if _, err := scratchOuterTimeout(cfg, 1<<62); err == nil {
		t.Fatal("duration addition overflow not detected")
	}
	if _, err := scratchOuterTimeout(enabledScratchConfig(), -time.Second); err == nil {
		t.Fatal("negative command timeout accepted")
	}
}

func TestScratchApprovalDisabledIsZeroIdentity(t *testing.T) {
	a, err := approvalForScratch(ScratchConfig{}, false, SandboxRuntimeHost)
	if err != nil {
		t.Fatalf("approval for disabled config: %v", err)
	}
	if a != (scratchApproval{}) {
		t.Fatalf("disabled scratch must have zero approval identity, got %+v", a)
	}
}

func TestScratchApprovalComponentShape(t *testing.T) {
	a, err := approvalForScratch(ScratchConfig{Enabled: true}, false, SandboxRuntimeHost)
	if err != nil {
		t.Fatalf("approval: %v", err)
	}
	if !strings.HasPrefix(a.keyComponent, "scr:") || !strings.HasSuffix(a.keyComponent, ":") {
		t.Fatalf("key component shape: %q", a.keyComponent)
	}
	digest := strings.TrimSuffix(strings.TrimPrefix(a.keyComponent, "scr:"), ":")
	if len(digest) != 64 {
		t.Fatalf("component digest must be full sha256 hex, got %d chars: %q", len(digest), digest)
	}
	if a.preview == "" {
		t.Fatal("enabled scratch must render a preview line")
	}
}

func TestScratchApprovalEveryFieldRekeys(t *testing.T) {
	base, err := approvalForScratch(enabledScratchConfig(), false, SandboxRuntimeHost)
	if err != nil {
		t.Fatalf("base approval: %v", err)
	}
	muts := []struct {
		name string
		mut  func(*ScratchConfig)
	}{
		{"sessions", func(c *ScratchConfig) { c.MaxConcurrentSessions = 3 }},
		{"workspace files", func(c *ScratchConfig) { c.MaxWorkspaceFiles = 50_000 }},
		{"workspace bytes", func(c *ScratchConfig) { c.MaxWorkspaceBytes = 2 << 30 }},
		{"changed files", func(c *ScratchConfig) { c.MaxChangedFiles = 32 }},
		{"file bytes", func(c *ScratchConfig) { c.MaxFileBytes = 128 << 10 }},
		{"total bytes", func(c *ScratchConfig) { c.MaxTotalBytes = 2 << 20 }},
		{"snapshot timeout", func(c *ScratchConfig) { c.SnapshotTimeout = 10 * time.Second }},
		{"capture timeout", func(c *ScratchConfig) { c.CaptureTimeout = 10 * time.Second }},
	}
	for _, tc := range muts {
		t.Run(tc.name, func(t *testing.T) {
			cfg := enabledScratchConfig()
			tc.mut(&cfg)
			got, err := approvalForScratch(cfg, false, SandboxRuntimeHost)
			if err != nil {
				t.Fatalf("approval: %v", err)
			}
			if got.keyComponent == base.keyComponent {
				t.Fatalf("%s change did not re-key", tc.name)
			}
		})
	}
}

func TestScratchApprovalPromotionAvailabilityRekeys(t *testing.T) {
	off, err := approvalForScratch(enabledScratchConfig(), false, SandboxRuntimeHost)
	if err != nil {
		t.Fatalf("approval: %v", err)
	}
	on, err := approvalForScratch(enabledScratchConfig(), true, SandboxRuntimeHost)
	if err != nil {
		t.Fatalf("approval: %v", err)
	}
	if off.keyComponent == on.keyComponent {
		t.Fatal("promotion availability must re-key")
	}
	if off.preview == on.preview {
		t.Fatal("promotion availability must be visible in the preview")
	}
}

func TestScratchTempBaseRekeys(t *testing.T) {
	cfg := enabledScratchConfig()
	if scratchFingerprint(cfg, false, "/safe/temp") == scratchFingerprint(cfg, false, "/shared/temp") {
		t.Fatal("temp-base policy change did not re-key")
	}
}

func TestScratchApprovalPreviewContent(t *testing.T) {
	const hostLead = "disposable cwd copy; host process remains unrestricted"
	rows := []struct {
		name    string
		runtime SandboxRuntime
		lead    string // literal lead of the scratch line for this runtime
		forbid  string // wording this runtime must never claim
	}{
		{"host", SandboxRuntimeHost, hostLead, ""},
		{"unset runtime is host", "", hostLead, ""},
		{"seatbelt", SandboxRuntimeSeatbelt, "disposable cwd copy under the seatbelt sandbox", "host process remains unrestricted"},
		{"bwrap", SandboxRuntimeBwrap, "disposable cwd copy under the bwrap sandbox", "host process remains unrestricted"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			a, err := approvalForScratch(enabledScratchConfig(), true, row.runtime)
			if err != nil {
				t.Fatalf("approval: %v", err)
			}
			for _, want := range []string{
				row.lead + " git-metadata=omitted",
				"sessions<=2",
				"setup<=30s",
				"capture<=30s",
				"cleanup<=30s",
				"grace=5s",
				"promote=available",
			} {
				if !strings.Contains(a.preview, want) {
					t.Fatalf("preview missing %q:\n%s", want, a.preview)
				}
			}
			if row.forbid != "" && strings.Contains(a.preview, row.forbid) {
				t.Fatalf("%s preview claims %q:\n%s", row.name, row.forbid, a.preview)
			}
			if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
				if !strings.Contains(a.preview, "temp=private") {
					t.Fatalf("preview missing fixed private temp policy:\n%s", a.preview)
				}
				// The ephemeral root is created long after Plan and never appears.
				for _, forbid := range []string{"/tmp", "/private", "go-llm-scratch"} {
					if strings.Contains(a.preview, forbid) {
						t.Fatalf("preview leaks ephemeral path material %q:\n%s", forbid, a.preview)
					}
				}
			} else if !strings.Contains(a.preview, fmt.Sprintf("temp=%q", a.tempBase)) {
				t.Fatalf("preview omits environment-selected temp policy %q:\n%s", a.tempBase, a.preview)
			}
		})
	}
}

// scratchPreview plans a trivial argv through the named tool of set and
// returns the rendered approval preview.
func scratchPreview(t *testing.T, set []agent.Tool, name string) string {
	t.Helper()
	planner, ok := toolNamed(t, set, name).(agent.PlanningTool)
	if !ok {
		t.Fatalf("%s is not a planning tool", name)
	}
	plan, err := planner.Plan(context.Background(), json.RawMessage(`{"argv":["go","version"]}`))
	if err != nil {
		t.Fatalf("plan %s: %v", name, err)
	}
	return plan.Preview
}

// TestScratchPreviewNativeForegroundAndBackground drives the real RunCommand
// and StartCommand previews over a seatbelt-bound normalized stub manager: the
// scratch line must name the native runtime next to the sandbox line and must
// never claim the host process is unrestricted.
func TestScratchPreviewNativeForegroundAndBackground(t *testing.T) {
	set := execSet(t, canonicalTempRoot(t), stubManager(t, bindingSeatbelt), true)
	rows := []struct {
		tool   string
		sbLine string // label padding differs between the two preview renderers
		scLine string
	}{
		{"run_command", `  sandbox: runtime="seatbelt" `, "  scratch: disposable cwd copy under the seatbelt sandbox git-metadata=omitted "},
		{"start_command", `  sandbox:  runtime="seatbelt" `, "  scratch:  disposable cwd copy under the seatbelt sandbox git-metadata=omitted "},
	}
	for _, row := range rows {
		t.Run(row.tool, func(t *testing.T) {
			preview := scratchPreview(t, set, row.tool)
			for _, want := range []string{row.sbLine, row.scLine} {
				if !strings.Contains(preview, want) {
					t.Fatalf("%s preview missing %q:\n%s", row.tool, want, preview)
				}
			}
			if strings.Contains(preview, "host process remains unrestricted") {
				t.Fatalf("%s native preview claims the host is unrestricted:\n%s", row.tool, preview)
			}
		})
	}
}

// TestScratchPreviewHostKeepsUnrestrictedWording pins the host side of the
// same boundary on every host construction path: no sandbox line, and the
// scratch line keeps saying the host process is unrestricted.
func TestScratchPreviewHostKeepsUnrestrictedWording(t *testing.T) {
	const lead = "disposable cwd copy; host process remains unrestricted git-metadata=omitted "
	root := canonicalTempRoot(t)
	hostSet, err := NewExecToolsWithOptions(root, ExecToolsOptions{Scratch: ScratchConfig{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	sandboxedHost, err := NewSandboxedExecToolsWithOptions(root, SandboxConfig{}, ExecToolsOptions{Scratch: ScratchConfig{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	hostManager := execSet(t, root, stubManager(t, SandboxConfig{}), true)
	cases := []struct {
		name string
		set  []agent.Tool
		tool string
		want string // label padding differs between the two preview renderers
	}{
		{"host factory run_command", hostSet, "run_command", "  scratch: " + lead},
		{"sandboxed factory with host config run_command", sandboxedHost, "run_command", "  scratch: " + lead},
		{"host manager run_command", hostManager, "run_command", "  scratch: " + lead},
		{"host manager start_command", hostManager, "start_command", "  scratch:  " + lead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			preview := scratchPreview(t, tc.set, tc.tool)
			if !strings.Contains(preview, tc.want) {
				t.Fatalf("host preview missing %q:\n%s", tc.want, preview)
			}
			if strings.Contains(preview, "sandbox:") {
				t.Fatalf("host preview carries a sandbox line:\n%s", preview)
			}
		})
	}
}

// TestScratchApprovalRuntimeKeepsKeyComponent pins that the runtime wording is
// presentation only: the scratch key component (the sandbox component already
// carries the runtime identity) is identical for every runtime.
func TestScratchApprovalRuntimeKeepsKeyComponent(t *testing.T) {
	host, err := approvalForScratch(enabledScratchConfig(), true, SandboxRuntimeHost)
	if err != nil {
		t.Fatal(err)
	}
	for _, rt := range []SandboxRuntime{SandboxRuntimeSeatbelt, SandboxRuntimeBwrap} {
		got, err := approvalForScratch(enabledScratchConfig(), true, rt)
		if err != nil {
			t.Fatal(err)
		}
		if got.keyComponent != host.keyComponent {
			t.Fatalf("%s re-keyed scratch: %q vs host %q", rt, got.keyComponent, host.keyComponent)
		}
	}
}

func TestScratchApprovalRejectsInvalidConfig(t *testing.T) {
	cfg := enabledScratchConfig()
	cfg.MaxChangedFiles = -1
	if _, err := approvalForScratch(cfg, false, SandboxRuntimeHost); err == nil {
		t.Fatal("approvalForScratch must reject invalid config")
	}
}
