package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// maxHistoryEntries bounds what is kept in memory and loaded at startup.
	// The file itself is append-only: compaction would need cross-process
	// locking and risks losing another instance's records, for no acceptance
	// criterion.
	maxHistoryEntries = 500

	historyDirMode  os.FileMode = 0o700
	historyFileMode os.FileMode = 0o600
)

// goalHistory is the per-workspace store behind arrow-key recall.
//
// Storage is full fidelity; recall is a filtered projection. x/term recalls
// through setLine with no bound and no printability filter (terminal.go:679-690
// writes runes verbatim), while its insertion guard tests
// len(line) == maxLineLength exactly (terminal.go:653). So an entry carrying a
// control rune would emit raw terminal control codes during recall, and one
// longer than the limit would slip past it and keep growing. Both are stored
// and neither is offered to the arrow keys.
//
// Not safe for concurrent use; the editor's sole reader owns it.
type goalHistory struct {
	file  *os.File
	write func([]byte) (int, error) // file.Write in production; counted in tests

	entries    []string // oldest first, bounded to maxHistoryEntries
	recallable []string // newest first, filtered for terminal safety

	last     string
	haveLast bool

	// needsSeparator marks a file whose final record was torn by a crash. The
	// separating newline rides in the next record's single write rather than
	// becoming a write of its own, so nothing this package does can interleave
	// between them.
	//
	// It is decided once, from the same size snapshot the load scanned, and is
	// best-effort across processes. Two instances that both observe a torn tail
	// each prepend a separator, leaving a blank line that the next load skips
	// as undecodable -- harmless. The case this cannot cover is an instance
	// crashing mid-write after another has already decided the file was
	// intact: the next record concatenates onto the fragment and that one line
	// fails to decode. History is best-effort by design and never blocks input,
	// so losing a record to a concurrent crash is accepted rather than paid for
	// with cross-process locking.
	needsSeparator bool

	degraded  bool
	warn      func(string)
	warnOnce  sync.Once
	closeOnce sync.Once
	closeErr  error
}

// newGoalHistory opens the history for root's workspace. Every failure degrades
// to memory-only with one warning rather than blocking input: a session must
// never fail to accept a goal because history could not be written.
func newGoalHistory(getenv func(string) string, root string, warn func(string)) *goalHistory {
	h := &goalHistory{warn: warn}
	h.write = func(b []byte) (int, error) {
		if h.file == nil {
			return 0, os.ErrClosed
		}
		return h.file.Write(b)
	}

	path, err := historyPath(getenv, root)
	if err != nil {
		h.degrade(err)
		return h
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, historyDirMode); err != nil {
		h.degrade(err)
		return h
	}
	// MkdirAll leaves an existing directory's mode alone, so a history
	// directory restored from a backup or created by an older build can sit at
	// 0777 and expose every goal. memory.PrepareDBFile deliberately does not do
	// this, but its reason -- DB paths may live under shared, user-configured
	// directories -- does not apply here: golem/history is ours alone, while
	// $XDG_DATA_HOME and its golem parent are left untouched.
	if err := os.Chmod(dir, historyDirMode); err != nil {
		h.degrade(err)
		return h
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, historyFileMode)
	if err != nil {
		h.degrade(err)
		return h
	}
	// Re-chmod through the descriptor, not the path, so an existing file with
	// a permissive mode is tightened without a second name resolution to race.
	// Mirrors memory/open.go.
	if err := f.Chmod(historyFileMode); err != nil {
		_ = f.Close()
		h.degrade(err)
		return h
	}
	h.file = f
	h.load()
	return h
}

// historyPath resolves $XDG_DATA_HOME/golem/history/<workspace>, with ':'
// replaced so the name is a legal Windows path component, and refuses any
// location inside the workspace itself.
func historyPath(getenv func(string) string, root string) (string, error) {
	base, err := dataDirBase(getenv)
	if err != nil {
		return "", err
	}
	name := strings.ReplaceAll(workspaceID(root), ":", "-")
	p := filepath.Join(base, "golem", "history", name)
	if err := validatePathOutsideWorkspace(p, root); err != nil {
		return "", err
	}
	return p, nil
}

// load reads the last maxHistoryEntries decodable records from the already-open
// descriptor. Reading through h.file rather than reopening by name avoids a
// second name resolution to race and keeps one handle for the file's lifetime.
//
// A record that fails to decode is skipped rather than fatal: a torn or
// hand-edited file must not cost the user the rest of their history.
func (h *goalHistory) load() {
	size, err := h.fileSize()
	if err != nil {
		h.degrade(err)
		return
	}

	// O_APPEND affects writes only, so a SectionReader over the same
	// descriptor reads the file without disturbing the write offset.
	sc := bufio.NewScanner(io.NewSectionReader(h.file, 0, size))
	// A control byte quotes to four characters, so a maxGoalBytes goal of NULs
	// needs four times its size plus the quotes and newline. A flat 4 MiB
	// limit would reject exactly the largest goal the editor accepts.
	sc.Buffer(make([]byte, 0, 64*1024), 4*maxGoalBytes+16)

	for sc.Scan() {
		entry, err := strconv.Unquote(sc.Text())
		if err != nil {
			continue
		}
		h.appendEntry(entry)
	}
	scanErr := sc.Err()

	// Ordering matters: the projection is built from whatever decoded, before
	// any scan failure is reported. Returning early here used to leave recall
	// empty while entries held data, so one over-long record silently disabled
	// arrow recall for the whole session.
	h.needsSeparator = h.tornTail(size)
	h.rebuildRecallable()

	if scanErr != nil {
		h.degrade(scanErr)
	}
}

func (h *goalHistory) fileSize() (int64, error) {
	fi, err := h.file.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// tornTail reports whether the file ends without a newline, meaning a previous
// write was cut short and the next record must be preceded by a separator.
func (h *goalHistory) tornTail(size int64) bool {
	if size == 0 {
		return false
	}
	buf := make([]byte, 1)
	if _, err := h.file.ReadAt(buf, size-1); err != nil {
		return false
	}
	return buf[0] != '\n'
}

// Add satisfies term.History and is deliberately a no-op. x/term calls it for
// every accepted line (terminal.go:859), which is both the wrong granularity --
// a multi-segment paste is one goal -- and too early, since runREPL has not yet
// decided whether the line is empty or a slash command. runREPL calls Record
// instead, after it classifies.
func (h *goalHistory) Add(string) {}

// Len reports the number of recallable entries.
func (h *goalHistory) Len() int { return len(h.recallable) }

// At returns the recallable entry i positions back, newest first. It panics
// out of range, as term.History requires.
func (h *goalHistory) At(i int) string {
	if i < 0 || i >= len(h.recallable) {
		panic(fmt.Sprintf("golem: history index %d out of range [0,%d)", i, len(h.recallable)))
	}
	return h.recallable[i]
}

// Record appends an accepted goal: in memory always, and on disk unless the
// store has degraded.
func (h *goalHistory) Record(goal string) {
	if goal == "" {
		return
	}
	if h.haveLast && goal == h.last {
		return
	}
	h.last, h.haveLast = goal, true
	h.appendEntry(goal)
	h.rebuildRecallable()
	h.persist(goal)
}

// persist hands each record to the writer in exactly one call, so nothing this
// package does splits a record. That is as far as the guarantee goes:
// os.File.Write loops internally on a partial write, and POSIX does not
// promise atomicity for a large write(2) even under O_APPEND, so a
// multi-megabyte record could still be split by the kernel. Records are goals,
// so in practice they are far below any such threshold.
//
// A short write degrades rather than retrying the missing suffix: a retry
// could land after a concurrently appending instance's record and interleave.
func (h *goalHistory) persist(goal string) {
	if h.degraded || h.file == nil {
		return
	}
	record := strconv.Quote(goal) + "\n"
	if h.needsSeparator {
		record = "\n" + record
	}
	n, err := h.write([]byte(record))
	if err != nil {
		h.degrade(err)
		return
	}
	if n != len(record) {
		h.degrade(io.ErrShortWrite)
		return
	}
	h.needsSeparator = false
}

func (h *goalHistory) appendEntry(entry string) {
	h.entries = append(h.entries, entry)
	if len(h.entries) <= maxHistoryEntries {
		return
	}
	// Compact in place rather than reslicing forward. Reslicing would leave
	// the evicted string headers in the dead slots ahead of the new start,
	// still reachable from the allocation, so their bodies stay alive until
	// append happens to reallocate. With entries up to maxGoalBytes each that
	// silently doubles the retained ceiling during a long load. Clearing the
	// tail drops those references immediately.
	n := copy(h.entries, h.entries[len(h.entries)-maxHistoryEntries:])
	clear(h.entries[n:])
	h.entries = h.entries[:n]
}

// rebuildRecallable recomputes the projection newest-first. The list is bounded
// at maxHistoryEntries, so recomputing beats maintaining a parallel structure
// that could drift from entries.
func (h *goalHistory) rebuildRecallable() {
	h.recallable = h.recallable[:0]
	for i := len(h.entries) - 1; i >= 0; i-- {
		if recallableEntry(h.entries[i]) {
			h.recallable = append(h.recallable, h.entries[i])
		}
	}
}

// recallableEntry reports whether an entry is safe to hand to x/term's recall.
func recallableEntry(s string) bool {
	if utf8.RuneCountInString(s) > maxEditorRunes {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// degrade switches to memory-only operation and warns exactly once. Input is
// never blocked by a history failure.
func (h *goalHistory) degrade(err error) {
	h.degraded = true
	if h.file != nil {
		// Fold a close failure into the same warning instead of dropping it.
		// Close() afterwards reports nil, since closeOnce sees the file already
		// gone, so this is the only place the user can learn about it.
		if cerr := h.file.Close(); cerr != nil {
			err = fmt.Errorf("%w (and closing the file failed: %v)", err, cerr)
		}
		h.file = nil
	}
	h.warnOnce.Do(func() {
		if h.warn != nil {
			h.warn(fmt.Sprintf("warning: goal history disabled for this session: %v", err))
		}
	})
}

// Close closes the file once and remembers the result, so a second call from a
// different teardown path reports the same outcome rather than a spurious
// os.ErrClosed.
func (h *goalHistory) Close() error {
	h.closeOnce.Do(func() {
		if h.file != nil {
			h.closeErr = h.file.Close()
			h.file = nil
		}
	})
	return h.closeErr
}

// stored exposes the full-fidelity entries for tests; recall must go through
// Len and At.
func (h *goalHistory) stored() []string { return h.entries }
