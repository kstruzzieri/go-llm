package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// Scratch execution (#443, ZT-204): approved commands run against a
// disposable copy-on-write snapshot of the workspace, so cwd-relative
// mistakes cannot alter the canonical tree. Composed with a sandbox runtime
// (#441/#442) the rewritten workspace root becomes an enforced write
// boundary; on the host runtime it is accident isolation only, and the
// approval preview says so.

const (
	// scratchRecipeVersion names the scratch policy recipe inside the scr:
	// digest. A semantic policy change bumps this literal; the outer key
	// prefixes (exec:v3:, exec-bg:v2:) stay untouched because the command
	// fingerprint recipe itself is unchanged.
	scratchRecipeVersion = "scratch-recipe-v1"

	// scratchEffectGrace is the fixed orchestration allowance added on top of
	// the setup, command, and capture budgets when sizing the outer effect
	// timeout. It covers recheck, reaping, and rendering; no phase child
	// context may spend it.
	scratchEffectGrace = 5 * time.Second

	scratchDefaultMaxConcurrentSessions = 2
	scratchDefaultMaxWorkspaceFiles     = 100_000
	scratchDefaultMaxWorkspaceBytes     = 1 << 30 // 1 GiB logical bytes per tree
	scratchDefaultMaxChangedFiles       = 64
	scratchDefaultMaxFileBytes          = 256 << 10 // matches mutateMaxBytes
	scratchDefaultMaxTotalBytes         = 4 << 20
	scratchDefaultSnapshotTimeout       = 30 * time.Second
	scratchDefaultCaptureTimeout        = 30 * time.Second
)

// ScratchConfig configures ephemeral scratchpad execution (#443). The zero
// value disables scratch and preserves existing behavior byte-for-byte.
// Bounds are policy, frozen at tool construction, and participate in the
// approval-key namespace; they can never be chosen by model arguments.
type ScratchConfig struct {
	Enabled bool

	// MaxConcurrentSessions bounds admitted (pending or running) scratch
	// sessions per tool factory. Default 2.
	MaxConcurrentSessions int
	// MaxWorkspaceFiles bounds the entries one snapshot may traverse.
	// Default 100_000.
	MaxWorkspaceFiles int
	// MaxWorkspaceBytes bounds the logical regular-file bytes one snapshot
	// tree may contain. Default 1 GiB.
	MaxWorkspaceBytes int64
	// MaxChangedFiles bounds the captured changeset. Default 64.
	MaxChangedFiles int
	// MaxFileBytes bounds each retained create's content. Default 256 KiB.
	MaxFileBytes int64
	// MaxTotalBytes bounds aggregate retained create bytes. Default 4 MiB.
	MaxTotalBytes int64
	// SnapshotTimeout bounds session setup (both clones plus manifest
	// verification). Default 30s.
	SnapshotTimeout time.Duration
	// CaptureTimeout bounds post-command diff and cleanup. Default 30s.
	CaptureTimeout time.Duration
}

// ExecToolsOptions carries the frozen per-factory execution policy for the
// options constructors (#443). The zero value reproduces the legacy
// constructors exactly.
type ExecToolsOptions struct {
	// Scratch enables and bounds scratchpad execution.
	Scratch ScratchConfig
	// PromotionJournal is the write-ahead journal promotion must use. Nil
	// means capture/query only: promote_artifact is not registered.
	PromotionJournal PreparingJournal
}

// normalizeScratchConfig validates and defaults a ScratchConfig. Disabled
// accepts only the zero value (the sandbox permission-ceiling precedent);
// enabled rejects negative bounds and fills zero fields with the approved
// defaults.
func normalizeScratchConfig(cfg ScratchConfig) (ScratchConfig, error) {
	if !cfg.Enabled {
		if cfg != (ScratchConfig{}) {
			return ScratchConfig{}, fmt.Errorf("tools: scratch disabled but config fields are set; enable scratch or use the zero value")
		}
		return ScratchConfig{}, nil
	}
	if cfg.MaxConcurrentSessions < 0 || cfg.MaxWorkspaceFiles < 0 || cfg.MaxWorkspaceBytes < 0 ||
		cfg.MaxChangedFiles < 0 || cfg.MaxFileBytes < 0 || cfg.MaxTotalBytes < 0 ||
		cfg.SnapshotTimeout < 0 || cfg.CaptureTimeout < 0 {
		return ScratchConfig{}, fmt.Errorf("tools: scratch config has a negative bound: %+v", cfg)
	}
	if cfg.MaxConcurrentSessions == 0 {
		cfg.MaxConcurrentSessions = scratchDefaultMaxConcurrentSessions
	}
	if cfg.MaxWorkspaceFiles == 0 {
		cfg.MaxWorkspaceFiles = scratchDefaultMaxWorkspaceFiles
	}
	if cfg.MaxWorkspaceBytes == 0 {
		cfg.MaxWorkspaceBytes = scratchDefaultMaxWorkspaceBytes
	}
	if cfg.MaxChangedFiles == 0 {
		cfg.MaxChangedFiles = scratchDefaultMaxChangedFiles
	}
	if cfg.MaxFileBytes == 0 {
		cfg.MaxFileBytes = scratchDefaultMaxFileBytes
	}
	if cfg.MaxTotalBytes == 0 {
		cfg.MaxTotalBytes = scratchDefaultMaxTotalBytes
	}
	if cfg.SnapshotTimeout == 0 {
		cfg.SnapshotTimeout = scratchDefaultSnapshotTimeout
	}
	if cfg.CaptureTimeout == 0 {
		cfg.CaptureTimeout = scratchDefaultCaptureTimeout
	}
	return cfg, nil
}

// scratchOuterTimeout is the overflow-checked outer effect budget for one
// scratched invocation: setup + command (or admission) + capture + the fixed
// grace. Each component must be non-negative and the sum must not wrap.
func scratchOuterTimeout(cfg ScratchConfig, command time.Duration) (time.Duration, error) {
	sum := time.Duration(0)
	for _, d := range []time.Duration{cfg.SnapshotTimeout, command, cfg.CaptureTimeout, scratchEffectGrace} {
		if d < 0 {
			return 0, fmt.Errorf("tools: scratch effect timeout component is negative: %v", d)
		}
		next := sum + d
		if next < sum {
			return 0, fmt.Errorf("tools: scratch effect timeout overflows")
		}
		sum = next
	}
	return sum, nil
}

// scratchApproval is the frozen approval metadata for one scratch policy:
// the key-namespace component and the rendered preview line. The zero value
// is the disabled identity (no component, no line — byte-identical
// approvals), mirroring sandboxApproval.
type scratchApproval struct {
	keyComponent string
	preview      string
}

// scratchFingerprint hashes the approval-relevant scratch policy using the
// same labeled, NUL-separated style as sandboxFingerprint. It accepts only a
// normalized enabled config. The ephemeral root path is deliberately absent:
// it is created after approval, like the Seatbelt private temp dir, and is
// policy output, not identity.
func scratchFingerprint(cfg ScratchConfig, promotable bool) string {
	h := sha256.New()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	write("recipe")
	write(scratchRecipeVersion)
	write("max_concurrent_sessions")
	write(strconv.Itoa(cfg.MaxConcurrentSessions))
	write("max_workspace_files")
	write(strconv.Itoa(cfg.MaxWorkspaceFiles))
	write("max_workspace_bytes")
	write(strconv.FormatInt(cfg.MaxWorkspaceBytes, 10))
	write("max_changed_files")
	write(strconv.Itoa(cfg.MaxChangedFiles))
	write("max_file_bytes")
	write(strconv.FormatInt(cfg.MaxFileBytes, 10))
	write("max_total_bytes")
	write(strconv.FormatInt(cfg.MaxTotalBytes, 10))
	write("snapshot_timeout")
	write(cfg.SnapshotTimeout.String())
	write("capture_timeout")
	write(cfg.CaptureTimeout.String())
	write("effect_grace")
	write(scratchEffectGrace.String())
	write("temp")
	write("private-session")
	write("promotion_available")
	write(strconv.FormatBool(promotable))
	return hex.EncodeToString(h.Sum(nil))
}

// approvalForScratch derives the approval metadata for a scratch policy.
// Disabled returns the zero value so existing keys and previews stay
// byte-identical; enabled gets its own key namespace ("scr:<digest>:") and
// preview line, so a grant never crosses the scratch boundary in either
// direction. promotable records whether a promotion journal is wired: making
// promotion available is a policy change and must re-key.
func approvalForScratch(cfg ScratchConfig, promotable bool) (scratchApproval, error) {
	normalized, err := normalizeScratchConfig(cfg)
	if err != nil {
		return scratchApproval{}, err
	}
	if !normalized.Enabled {
		return scratchApproval{}, nil
	}
	return scratchApproval{
		keyComponent: "scr:" + scratchFingerprint(normalized, promotable) + ":",
		preview:      renderScratchLine(normalized, promotable),
	}, nil
}

// renderScratchLine renders the scratch policy the user is approving.
// Presentation only, never parsed. The wording states the host-runtime truth
// (#443 threat model): the copy protects against cwd-relative accidents, and
// only a composed sandbox line upgrades that to an enforced boundary. No
// ephemeral path material may ever appear here.
func renderScratchLine(cfg ScratchConfig, promotable bool) string {
	promote := "promote=unavailable"
	if promotable {
		promote = "promote=available"
	}
	return fmt.Sprintf(
		"disposable cwd copy; host process remains unrestricted"+
			" files<=%d tree-bytes<=%d changes<=%d change-bytes<=%d/%d sessions<=%d"+
			" setup<=%s capture<=%s grace=%s temp=private %s",
		cfg.MaxWorkspaceFiles, cfg.MaxWorkspaceBytes, cfg.MaxChangedFiles,
		cfg.MaxFileBytes, cfg.MaxTotalBytes, cfg.MaxConcurrentSessions,
		cfg.SnapshotTimeout, cfg.CaptureTimeout, scratchEffectGrace, promote,
	)
}
