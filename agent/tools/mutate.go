package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

const (
	// mutateMaxBytes is the v2 text-file ceiling. Prior content, new content, and
	// final content must each stay at or below this for write_file / edit_file.
	mutateMaxBytes = 256 * 1024
	// absentHash is the sentinel returned for a file that does not exist, distinct
	// from ContentHash of any real (including empty) content.
	absentHash = "absent"
	// diffContext is the number of unchanged lines shown around a change.
	diffContext = 3
)

// WriteClassApprovalKey is the shared structural grant key (#341) for the
// write-class tools (write_file, edit_file): one session grant — the "a"
// approval answer or golem's /auto-edits on — covers the whole class.
// Exported because cmd/golem's /auto-edits toggle must reference the same
// value. MCP tools and submit_plan never emit any key and are not covered.
const WriteClassApprovalKey = "write-class:files"

// ContentHash is the SHA-256 hex of b. Stable, distinct from absentHash, and reused by cmd/golem's undo journal to detect post-write changes.
func ContentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// splitLines splits s on '\n', dropping the trailing empty element produced by a
// final newline so a normal text file's line count is intuitive.
func splitLines(s string) []string {
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// unifiedDiff renders a human-readable line diff for the approval preview ONLY.
// It is never parsed or fed back to the model. beforeExists distinguishes a
// brand-new file (all additions) from an overwrite. A non-empty before with an
// empty after renders as emptied (truncated to zero bytes).
func unifiedDiff(path string, before, after []byte, beforeExists bool) string {
	var b strings.Builder
	if !beforeExists {
		fmt.Fprintf(&b, "new file: %s\n", path)
		for _, ln := range splitLines(string(after)) {
			fmt.Fprintf(&b, "+%s\n", ln)
		}
		return b.String()
	}
	if len(after) == 0 {
		fmt.Fprintf(&b, "empty file: %s\n", path)
		for _, ln := range splitLines(string(before)) {
			fmt.Fprintf(&b, "-%s\n", ln)
		}
		return b.String()
	}
	bl := splitLines(string(before))
	al := splitLines(string(after))
	p := 0
	for p < len(bl) && p < len(al) && bl[p] == al[p] {
		p++
	}
	s := 0
	for s < len(bl)-p && s < len(al)-p && bl[len(bl)-1-s] == al[len(al)-1-s] {
		s++
	}
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", path, path)
	if p > 0 {
		start := p - diffContext
		if start < 0 {
			start = 0
		}
		for i := start; i < p; i++ {
			fmt.Fprintf(&b, " %s\n", bl[i])
		}
	}
	for i := p; i < len(bl)-s; i++ {
		fmt.Fprintf(&b, "-%s\n", bl[i])
	}
	for i := p; i < len(al)-s; i++ {
		fmt.Fprintf(&b, "+%s\n", al[i])
	}
	end := len(bl) - s + diffContext
	if end > len(bl) {
		end = len(bl)
	}
	for i := len(bl) - s; i < end; i++ {
		fmt.Fprintf(&b, " %s\n", bl[i])
	}
	return b.String()
}

// MutationRecord is what an applied write reports to a Journal so the consumer can
// undo it. PriorContent is the file's bytes before the write (nil if it did not
// exist); Existed says which. AfterHash is ContentHash of the written bytes, used
// by /undo to confirm the file is unchanged since golem wrote it.
type MutationRecord struct {
	Path         string
	PriorContent []byte
	Existed      bool
	AfterHash    string
	Summary      string
	At           time.Time
}

// Journal receives a record after each successful mutation. Defined here at the
// point of use; cmd/golem supplies the concrete in-memory implementation. A nil
// Journal is tolerated by the tools (Record is simply skipped).
type Journal interface {
	Record(MutationRecord)
}

// pendingPlan is the state a mutating tool computes in Plan and consumes in Invoke.
// agent.ToolPlan exposes only Effect+Preview, and Invoke receives only raw args,
// so the tool carries the previewed result itself, keyed by a hash of the raw args.
type pendingPlan struct {
	path         string
	priorContent []byte
	priorExists  bool
	beforeHash   string
	afterContent []byte
	afterHash    string
	summary      string
}

// mutatingBase holds the single pending plan shared by a tool's Plan/Invoke pair.
// Only the most recent successful Plan is retained; a denied call simply leaves it
// to be overwritten by the next Plan.
type mutatingBase struct {
	mu       sync.Mutex
	argsHash string
	plan     pendingPlan
	hasPlan  bool
}

func (b *mutatingBase) store(argsHash string, p pendingPlan) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.argsHash, b.plan, b.hasPlan = argsHash, p, true
}

// consume returns and clears the pending plan only if argsHash matches the stored
// one. A mismatch (or no plan) returns ok=false so Invoke fails closed.
func (b *mutatingBase) consume(argsHash string) (pendingPlan, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.hasPlan || b.argsHash != argsHash {
		return pendingPlan{}, false
	}
	p := b.plan
	b.hasPlan, b.plan, b.argsHash = false, pendingPlan{}, ""
	return p, true
}

// record forwards to the journal when one is configured.
func record(j Journal, rec MutationRecord) {
	if j != nil {
		j.Record(rec)
	}
}

// NewMutatingTools builds the workspace-mutating tool set (write_file, edit_file)
// bound to ws, reporting applied changes to journal (may be nil). The consumer
// (cmd/golem) appends these to the read-only set only when writes are enabled, and
// must supply an Approver so the runtime gates each call.
func NewMutatingTools(ws *Workspace, journal Journal) []agent.Tool {
	return []agent.Tool{
		NewWriteFile(ws, journal),
		NewEditFile(ws, journal),
	}
}
