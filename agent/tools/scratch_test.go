package tools

import (
	"strings"
	"testing"
	"time"
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
	want := 30*time.Second + 90*time.Second + 30*time.Second + 5*time.Second
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
	a, err := approvalForScratch(ScratchConfig{}, false)
	if err != nil {
		t.Fatalf("approval for disabled config: %v", err)
	}
	if a != (scratchApproval{}) {
		t.Fatalf("disabled scratch must have zero approval identity, got %+v", a)
	}
}

func TestScratchApprovalComponentShape(t *testing.T) {
	a, err := approvalForScratch(ScratchConfig{Enabled: true}, false)
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
	base, err := approvalForScratch(enabledScratchConfig(), false)
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
			got, err := approvalForScratch(cfg, false)
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
	off, err := approvalForScratch(enabledScratchConfig(), false)
	if err != nil {
		t.Fatalf("approval: %v", err)
	}
	on, err := approvalForScratch(enabledScratchConfig(), true)
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

func TestScratchApprovalPreviewContent(t *testing.T) {
	a, err := approvalForScratch(enabledScratchConfig(), true)
	if err != nil {
		t.Fatalf("approval: %v", err)
	}
	for _, want := range []string{
		"disposable cwd copy",
		"host process remains unrestricted",
		"sessions<=2",
		"setup<=30s",
		"capture<=30s",
		"grace=5s",
		"temp=private",
		"promote=available",
	} {
		if !strings.Contains(a.preview, want) {
			t.Fatalf("preview missing %q:\n%s", want, a.preview)
		}
	}
	// The preview is policy, never path material: nothing resembling a temp
	// path may appear (the ephemeral root is created long after Plan).
	for _, forbid := range []string{"/tmp", "/private", "go-llm-scratch"} {
		if strings.Contains(a.preview, forbid) {
			t.Fatalf("preview leaks path material %q:\n%s", forbid, a.preview)
		}
	}
}

func TestScratchApprovalRejectsInvalidConfig(t *testing.T) {
	cfg := enabledScratchConfig()
	cfg.MaxChangedFiles = -1
	if _, err := approvalForScratch(cfg, false); err == nil {
		t.Fatal("approvalForScratch must reject invalid config")
	}
}
