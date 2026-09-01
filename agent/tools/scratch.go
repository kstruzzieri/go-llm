package tools

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
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
			" git-metadata=omitted files<=%d tree-bytes<=%d changes<=%d change-bytes<=%d/%d sessions<=%d"+
			" setup<=%s capture<=%s grace=%s temp=private %s",
		cfg.MaxWorkspaceFiles, cfg.MaxWorkspaceBytes, cfg.MaxChangedFiles,
		cfg.MaxFileBytes, cfg.MaxTotalBytes, cfg.MaxConcurrentSessions,
		cfg.SnapshotTimeout, cfg.CaptureTimeout, scratchEffectGrace, promote,
	)
}

// scratchTempBase returns the platform scratch parent. Like the Seatbelt
// temp base (D5 there), it is policy, never inherited from the environment:
// inherited temp locations are command input.
func scratchTempBase() string {
	switch runtime.GOOS {
	case "darwin":
		return "/private/tmp"
	case "linux":
		return "/tmp"
	default:
		return os.TempDir()
	}
}

// scratchStatus is the query-tool view of one scratch id.
type scratchStatus int

const (
	scratchStatusUnknown scratchStatus = iota
	scratchStatusPending
	scratchStatusCaptured
)

// scratchStoreRetainedCap bounds completed outcomes (FIFO, mirroring
// backgroundRetainedCap). Pending sessions are never evicted.
const scratchStoreRetainedCap = 8

// scratchStore is the session-scoped registry of scratch outcomes. IDs are
// 128-bit random capability handles; completed outcomes are bounded FIFO;
// promotion claims/consumption are tracked per (id, path) so one artifact
// can never be promoted twice or concurrently.
type scratchStore struct {
	mu        sync.Mutex
	random    io.Reader
	pending   map[string]struct{}
	completed map[string]scratchOutcome
	order     []string
	claimed   map[string]map[string]bool
	consumed  map[string]map[string]bool
}

func newScratchStore(random io.Reader) *scratchStore {
	return &scratchStore{
		random:    random,
		pending:   make(map[string]struct{}),
		completed: make(map[string]scratchOutcome),
		claimed:   make(map[string]map[string]bool),
		consumed:  make(map[string]map[string]bool),
	}
}

// newID draws a fresh 128-bit capability id, regenerating on collision.
func (s *scratchStore) newID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		var buf [16]byte
		if _, err := io.ReadFull(s.random, buf[:]); err != nil {
			return "", fmt.Errorf("tools: scratch id entropy: %w", err)
		}
		id := "scr-" + hex.EncodeToString(buf[:])
		if _, dup := s.pending[id]; dup {
			continue
		}
		if _, dup := s.completed[id]; dup {
			continue
		}
		return id, nil
	}
}

func (s *scratchStore) beginPending(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[id] = struct{}{}
}

// completePending publishes one outcome, evicting the oldest completed
// entries beyond the retained cap (pending sessions are never evicted).
func (s *scratchStore) completePending(id string, out scratchOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[id]; !ok {
		// A concurrent discard already deleted this session; a late finish
		// must not resurrect the outcome as an unreachable zombie.
		return
	}
	delete(s.pending, id)
	out.id = id
	s.completed[id] = out
	s.order = append(s.order, id)
	for len(s.order) > scratchStoreRetainedCap {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.completed, oldest)
		delete(s.claimed, oldest)
		delete(s.consumed, oldest)
	}
}

func (s *scratchStore) dropPending(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
}

// delete removes every trace of one id (pending, outcome, claims).
func (s *scratchStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
	if _, ok := s.completed[id]; ok {
		delete(s.completed, id)
		for i, o := range s.order {
			if o == id {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	delete(s.claimed, id)
	delete(s.consumed, id)
}

// get returns a deep copy of one outcome and its status; callers can never
// mutate stored state through the result.
func (s *scratchStore) get(id string) (scratchOutcome, scratchStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[id]; ok {
		return scratchOutcome{id: id}, scratchStatusPending
	}
	out, ok := s.completed[id]
	if !ok {
		return scratchOutcome{}, scratchStatusUnknown
	}
	cp := out
	cp.changes = make([]scratchChange, len(out.changes))
	copy(cp.changes, out.changes)
	for i := range cp.changes {
		if cp.changes[i].data != nil {
			cp.changes[i].data = append([]byte(nil), cp.changes[i].data...)
		}
	}
	return cp, scratchStatusCaptured
}

// claim atomically reserves one (id, path) for promotion. A consumed path
// can never be claimed again; a released claim can.
func (s *scratchStore) claim(id, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, ok := s.completed[id]
	if !ok {
		return fmt.Errorf("tools: unknown scratch id %q", id)
	}
	found := false
	for _, c := range out.changes {
		if c.path == path {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("tools: scratch %s has no change for %q", id, path)
	}
	if s.consumed[id][path] {
		return fmt.Errorf("tools: scratch %s path %q already promoted or indeterminate", id, path)
	}
	if s.claimed[id][path] {
		return fmt.Errorf("tools: scratch %s path %q promotion already in progress", id, path)
	}
	if s.claimed[id] == nil {
		s.claimed[id] = make(map[string]bool)
	}
	s.claimed[id][path] = true
	return nil
}

func (s *scratchStore) release(id, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claimed[id], path)
}

// consume permanently retires one (id, path): promoted, or indeterminate
// after a post-write failure — either way never retryable automatically.
func (s *scratchStore) consume(id, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claimed[id], path)
	if _, ok := s.completed[id]; !ok {
		// Evicted mid-promotion: nothing references this id any more, so
		// recreating permanent consumed state would leak forever.
		return
	}
	if s.consumed[id] == nil {
		s.consumed[id] = make(map[string]bool)
	}
	s.consumed[id][path] = true
}

// scratchRuntime is the one shared engine behind every scratch tool a
// factory returns: normalized policy, approval identity, canonical-root
// binding, bounded admission, entropy, temp placement, the outcome store,
// and the optional promotion journal. Frozen at construction.
type scratchRuntime struct {
	cfg      ScratchConfig
	approval scratchApproval
	journal  PreparingJournal
	root     string // canonical workspace root every session must match
	tempBase string
	store    *scratchStore
	slots    chan struct{}
	clone    func(*os.File, string) error
}

// newScratchRuntime builds the shared scratch engine for one factory.
func newScratchRuntime(root string, cfg ScratchConfig, journal PreparingJournal) (*scratchRuntime, error) {
	normalized, err := normalizeScratchConfig(cfg)
	if err != nil {
		return nil, err
	}
	if !normalized.Enabled {
		return nil, fmt.Errorf("tools: scratch runtime requires an enabled config")
	}
	if journal != nil && !scratchPromotionSupported {
		return nil, fmt.Errorf("tools: scratch promotion is unsupported on this platform; construct without a promotion journal (S5)")
	}
	approval, err := approvalForScratch(normalized, journal != nil)
	if err != nil {
		return nil, err
	}
	canonical, err := CanonicalWorkspaceRoot(root)
	if err != nil {
		return nil, fmt.Errorf("tools: scratch canonical root: %w", err)
	}
	return &scratchRuntime{
		cfg:      normalized,
		approval: approval,
		journal:  journal,
		root:     canonical,
		tempBase: scratchTempBase(),
		store:    newScratchStore(cryptorand.Reader),
		slots:    make(chan struct{}, normalized.MaxConcurrentSessions),
		clone:    cloneFile,
	}, nil
}

// ScratchChanges is the read-only, approval-free query tool for scratch
// outcomes: status, bounded per-change metadata, and reasons — never
// artifact bytes or previews (those appear only inside a promotion prompt).
type ScratchChanges struct {
	rt *scratchRuntime
}

type scratchChangesArgs struct {
	ID string `json:"id"`
}

// Spec implements agent.Tool.
func (ScratchChanges) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "scratch_changes",
		Description: "Report what an isolated scratch command changed: per-file kind, size, hash, and whether promote_artifact can apply it. Metadata only; content appears only in a promotion approval prompt.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "id":{"type":"string","description":"scratch id from a run_command/start_command result"}
  },
  "required":["id"]
}`),
	}
}

// Effect implements agent.Tool. The output cap covers the tool's real
// bounded worst case so dispatch never clips a change report: 64 changes x
// (a PATH_MAX path %q-expanded up to 4x (~16 KiB) + a sanitized reason that
// can embed another escaped path + fixed fields) is under 3 MiB; 4 MiB
// leaves margin. Typical reports are a few hundred bytes.
func (ScratchChanges) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever, OutputCap: 4 << 20}
}

// Invoke implements agent.Tool.
func (t ScratchChanges) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args scratchChangesArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolResult{IsError: true, Content: "invalid arguments: " + err.Error()}, nil
	}
	if strings.TrimSpace(args.ID) == "" {
		return agent.ToolResult{IsError: true, Content: "id is required"}, nil
	}
	out, status := t.rt.store.get(args.ID)
	switch status {
	case scratchStatusUnknown:
		return agent.ToolResult{IsError: true, Content: fmt.Sprintf("unknown scratch id %q (evicted or never issued)", args.ID)}, nil
	case scratchStatusPending:
		return agent.ToolResult{Content: fmt.Sprintf("scratch %s: pending (command still running or capture not finished)", args.ID)}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "scratch %s: %d change(s)", args.ID, len(out.changes))
	if out.truncated {
		b.WriteString(" [truncated: nothing promotable]")
	}
	if out.captureErr != "" {
		fmt.Fprintf(&b, " capture-error: %s", out.captureErr)
	}
	if out.cleanupErr != "" {
		fmt.Fprintf(&b, " cleanup-error: %s", out.cleanupErr)
	}
	for _, c := range out.changes {
		fmt.Fprintf(&b, "\n%s %q size=%d", c.kind, c.path, c.size)
		if c.hash != "" {
			fmt.Fprintf(&b, " hash=%s", c.hash[:min(len(c.hash), fingerprintLen)])
		}
		if c.promotable {
			b.WriteString(" promotable")
		} else if c.reason != "" {
			fmt.Fprintf(&b, " (%s)", c.reason)
		}
	}
	return agent.ToolResult{Content: b.String()}, nil
}

// sanitizeScratchText makes command-influenced diagnostic text (reasons,
// capture/cleanup errors — which can embed hostile filenames) control-safe
// before it is stored, so no render path can leak ANSI sequences or forged
// lines to the model or the terminal. Presentation only, never parsed.
func sanitizeScratchText(s string) string {
	q := strconv.QuoteToGraphic(s)
	return q[1 : len(q)-1]
}
