package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// historyEnv builds a getenv over a temp XDG_DATA_HOME and returns it with the
// resolved history path for a workspace root.
func historyEnv(t *testing.T, root string) (func(string) string, string) {
	t.Helper()
	xdg := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return xdg
		}
		return ""
	}
	return getenv, filepath.Join(xdg, "golem", "history", strings.ReplaceAll(workspaceID(root), ":", "-"))
}

func collectWarnings(dst *[]string) func(string) {
	return func(msg string) { *dst = append(*dst, msg) }
}

func TestGoalHistoryPath(t *testing.T) {
	root := t.TempDir()
	getenv, want := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	h.Record("hello")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("history file not at %s: %v", want, err)
	}
	if strings.Contains(filepath.Base(want), ":") {
		t.Fatalf("workspace id was not sanitized for Windows path safety: %s", want)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
}

func TestGoalHistoryHomeFallback(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()
	h.Record("hello")

	want := filepath.Join(home, ".local", "share", "golem", "history", strings.ReplaceAll(workspaceID(root), ":", "-"))
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("history file not at %s: %v", want, err)
	}
}

func TestGoalHistoryRejectsPathInsideWorkspace(t *testing.T) {
	// A data dir nested in the workspace would put goals inside the indexed,
	// edited tree. It must degrade rather than write there.
	root := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return filepath.Join(root, "data")
		}
		return ""
	}
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()
	h.Record("secret")

	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warns)
	}
	if _, err := os.Stat(filepath.Join(root, "data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("history wrote inside the workspace")
	}
	if h.Len() != 1 || h.At(0) != "secret" {
		t.Fatalf("degraded history lost in-memory recall")
	}
}

func TestGoalHistoryPermissions(t *testing.T) {
	root := t.TempDir()
	getenv, path := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()
	h.Record("hello")

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 0600", got)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("parent mode = %o, want 0700", got)
	}
}

func TestGoalHistoryPermissionsRepairedOnOpen(t *testing.T) {
	// An existing file with loose permissions must be tightened, not trusted.
	root := t.TempDir()
	getenv, path := historyEnv(t, root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strconv.Quote("old")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 0600 after re-chmod", got)
	}
}

func TestGoalHistoryOneWritePerRecord(t *testing.T) {
	// Concurrent golem instances append to the same file. More than one write
	// per record lets another process interleave inside a record.
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	real := h.write
	calls := 0
	var sizes []int
	h.write = func(b []byte) (int, error) {
		calls++
		sizes = append(sizes, len(b))
		return real(b)
	}
	h.Record("one")
	h.Record("two")
	if calls != 2 {
		t.Fatalf("write calls = %d, want 2 (one per record)", calls)
	}
	for i, n := range sizes {
		if n == 0 {
			t.Fatalf("record %d wrote an empty slice", i)
		}
	}
}

func TestGoalHistoryShortWriteDegrades(t *testing.T) {
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	h.write = func(b []byte) (int, error) { return len(b) - 1, nil }
	h.Record("one")
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warns)
	}
	if !strings.Contains(warns[0], io.ErrShortWrite.Error()) {
		t.Fatalf("warning %q does not name the short write", warns[0])
	}

	// Degraded: no further writes, but recall keeps working.
	h.write = func(b []byte) (int, error) {
		t.Fatal("wrote after degrading; a retried suffix could interleave with another process")
		return 0, nil
	}
	h.Record("two")
	if h.Len() != 2 {
		t.Fatalf("Len = %d, want 2; degraded mode must still recall", h.Len())
	}
	if len(warns) != 1 {
		t.Fatalf("warned more than once: %v", warns)
	}
}

func TestGoalHistoryRoundTrip(t *testing.T) {
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	goals := []string{
		"plain",
		"unicode é 中 𝄞",
		"embedded\nnewline",
		"carriage\rreturn",
		`quotes "and" backslash \ and \\`,
		"control\x00\x07\x1b\x7fbytes",
		"tab\tseparated",
	}
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	for _, g := range goals {
		h.Record(g)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h2 := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h2.Close() }()
	if got := h2.stored(); len(got) != len(goals) {
		t.Fatalf("stored %d entries, want %d", len(got), len(goals))
	}
	for i, want := range goals {
		if got := h2.stored()[i]; got != want {
			t.Fatalf("entry %d = %q, want %q", i, got, want)
		}
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
}

func TestGoalHistoryLoadBound(t *testing.T) {
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	for i := 0; i < maxHistoryEntries+50; i++ {
		h.Record("goal-" + strconv.Itoa(i))
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h2 := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h2.Close() }()
	if got := len(h2.stored()); got != maxHistoryEntries {
		t.Fatalf("loaded %d entries, want %d", got, maxHistoryEntries)
	}
	if got, want := h2.At(0), "goal-"+strconv.Itoa(maxHistoryEntries+49); got != want {
		t.Fatalf("At(0) = %q, want the newest %q", got, want)
	}
}

func TestGoalHistoryMalformedRecordSkipped(t *testing.T) {
	root := t.TempDir()
	getenv, path := historyEnv(t, root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("this is not a quoted string\n")
	sb.WriteString(strconv.Quote("good one") + "\n")
	sb.WriteString("\"unterminated\n")
	sb.WriteString(strconv.Quote("good two") + "\n")
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	if got, want := h.stored(), []string{"good one", "good two"}; !equalStrings(got, want) {
		t.Fatalf("stored = %q, want %q", got, want)
	}
}

func TestGoalHistoryTornTailGetsSeparator(t *testing.T) {
	root := t.TempDir()
	getenv, path := historyEnv(t, root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// A crash mid-write leaves a record with no trailing newline.
	torn := strconv.Quote("complete") + "\n" + strconv.Quote("torn")[:4]
	if err := os.WriteFile(path, []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))

	real := h.write
	var wrote [][]byte
	h.write = func(b []byte) (int, error) {
		wrote = append(wrote, append([]byte(nil), b...))
		return real(b)
	}
	h.Record("after crash")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if len(wrote) != 1 {
		t.Fatalf("wrote %d times, want 1: the separator must ride in the record's single write", len(wrote))
	}
	if wrote[0][0] != '\n' {
		t.Fatalf("first write %q does not begin with the separating newline", wrote[0])
	}

	h2 := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h2.Close() }()
	got := h2.stored()
	if len(got) == 0 || got[len(got)-1] != "after crash" {
		t.Fatalf("stored = %q; the record after a torn tail must decode", got)
	}
}

func TestGoalHistoryReaderSizeFitsQuotedMaxGoal(t *testing.T) {
	// strconv.Quote expands a control byte to four characters, so a 1 MiB goal
	// of NULs exceeds a flat 4 MiB reader.
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	big := strings.Repeat("\x00", maxGoalBytes)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	h.Record(big)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h2 := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h2.Close() }()
	got := h2.stored()
	if len(got) != 1 || got[0] != big {
		t.Fatalf("a quoted max-size goal did not round-trip (got %d entries, len %d)", len(got), len(got[0]))
	}
}

func TestGoalHistoryDegradedWhenDirUnusable(t *testing.T) {
	// XDG_DATA_HOME is a regular file, so creating its golem child fails with
	// ENOTDIR even for root.
	root := t.TempDir()
	base := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(base, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return base
		}
		return ""
	}
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	h.Record("one")
	h.Record("two")
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warns)
	}
	if h.Len() != 2 || h.At(0) != "two" {
		t.Fatalf("degraded mode lost in-memory recall: Len=%d", h.Len())
	}
}

func TestGoalHistoryPerWorkspaceIsolation(t *testing.T) {
	xdg := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return xdg
		}
		return ""
	}
	rootA, rootB := t.TempDir(), t.TempDir()
	var warns []string

	a := newGoalHistory(getenv, rootA, collectWarnings(&warns))
	a.Record("only in A")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := newGoalHistory(getenv, rootB, collectWarnings(&warns))
	defer func() { _ = b.Close() }()
	if b.Len() != 0 {
		t.Fatalf("workspace B saw %d entries from A", b.Len())
	}
}

func TestGoalHistoryRecallProjection(t *testing.T) {
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	excluded := []string{
		"multi\nline",
		"carriage\rreturn",
		"escape\x1bhere",
		"bell\x07here",
		"delete\x7fhere",
		"null\x00here",
		// Tab is 0x09. A terminal renders it as several columns while
		// visualLength counts one, so recalling it desynchronizes the cursor
		// model exactly as any other control rune would.
		"tab\there",
		strings.Repeat("x", maxEditorRunes+1),
	}
	recallable := []string{
		"plain goal",
		strings.Repeat("y", maxEditorRunes),
		"unicode é 中 𝄞",
	}
	for _, g := range append(append([]string{}, excluded...), recallable...) {
		h.Record(g)
	}

	if got := len(h.stored()); got != len(excluded)+len(recallable) {
		t.Fatalf("stored %d entries, want %d; storage stays full fidelity", got, len(excluded)+len(recallable))
	}
	if got := h.Len(); got != len(recallable) {
		t.Fatalf("Len = %d, want %d", got, len(recallable))
	}
	for i := 0; i < h.Len(); i++ {
		got := h.At(i)
		for _, bad := range excluded {
			if got == bad {
				t.Fatalf("At(%d) returned an excluded entry %q", i, got)
			}
		}
	}
	// Newest recallable first.
	if got, want := h.At(0), "unicode é 中 𝄞"; got != want {
		t.Fatalf("At(0) = %q, want %q", got, want)
	}
	if got, want := h.At(h.Len()-1), "plain goal"; got != want {
		t.Fatalf("At(last) = %q, want %q", got, want)
	}
}

func TestGoalHistoryExactly4096RunesStaysRecallable(t *testing.T) {
	// x/term rejects insertion only at exactly maxLineLength, so a recalled
	// 4097-rune entry would slip past the upstream limit and keep growing.
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	h.Record(strings.Repeat("é", maxEditorRunes)) // runes, not bytes
	if h.Len() != 1 {
		t.Fatalf("Len = %d, want 1; the cap counts runes", h.Len())
	}
	h.Record(strings.Repeat("é", maxEditorRunes+1))
	if h.Len() != 1 {
		t.Fatalf("Len = %d, want 1; a 4097-rune entry must not be recallable", h.Len())
	}
}

func TestGoalHistoryAddIsNoOp(t *testing.T) {
	// x/term calls Add per accepted line (readLine:859), which is the wrong
	// granularity and fires before runREPL classifies empty/slash input.
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	h.Record("real goal")
	beforeStored := len(h.stored())
	beforeRecall := h.Len()

	h.Add("/tools")
	h.Add("y")
	h.Add("a pasted segment")

	if got := len(h.stored()); got != beforeStored {
		t.Fatalf("stored grew from %d to %d; Add must be a no-op", beforeStored, got)
	}
	if got := h.Len(); got != beforeRecall {
		t.Fatalf("recall grew from %d to %d; Add must be a no-op", beforeRecall, got)
	}
}

func TestGoalHistoryAtBoundsPanic(t *testing.T) {
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()
	h.Record("one")

	// A raw slice index would also panic, so asserting only "it panicked"
	// cannot tell the explicit guard from an accident. Pin the message, which
	// names the index and the bound and is the reason the guard exists.
	for _, i := range []int{-1, h.Len()} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("At(%d) did not panic; term.History requires it", i)
				}
				if msg, ok := r.(string); !ok || !strings.Contains(msg, "history index") {
					t.Fatalf("At(%d) panicked with %v, want the explicit bounds message", i, r)
				}
			}()
			_ = h.At(i)
		}()
	}
}

func TestGoalHistoryDedupConsecutive(t *testing.T) {
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	h.Record("same")
	h.Record("same")
	h.Record("other")
	h.Record("same")
	if got, want := h.stored(), []string{"same", "other", "same"}; !equalStrings(got, want) {
		t.Fatalf("stored = %q, want %q; only consecutive duplicates collapse", got, want)
	}
}

func TestGoalHistoryEmptyGoalNotRecorded(t *testing.T) {
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	h.Record("")
	if len(h.stored()) != 0 {
		t.Fatalf("an empty goal was recorded")
	}
}

func TestGoalHistoryCloseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	h.Record("one")

	first := h.Close()
	if first != nil {
		t.Fatalf("first Close = %v", first)
	}
	if second := h.Close(); second != first {
		t.Fatalf("second Close = %v, want the remembered %v", second, first)
	}
}

func TestGoalHistoryScanErrorKeepsWhatDecoded(t *testing.T) {
	// A record longer than the reader is a scan error. Returning early on it
	// used to skip building the projection, so one over-long record silently
	// disabled arrow recall for the whole session even though the earlier
	// entries had decoded fine.
	root := t.TempDir()
	getenv, path := historyEnv(t, root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := strconv.Quote("recallable one") + "\n" +
		strconv.Quote("recallable two") + "\n" +
		strconv.Quote(strings.Repeat("x", 5*maxGoalBytes)) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	if got := len(h.stored()); got != 2 {
		t.Fatalf("stored = %d, want 2", got)
	}
	if got := h.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2; a scan error must not cost the decoded entries their recall", got)
	}
	if got := h.At(0); got != "recallable two" {
		t.Fatalf("At(0) = %q, want the newest decoded entry", got)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want exactly one for the scan failure", warns)
	}
}

func TestGoalHistoryDoesNotRecallMalformedEntries(t *testing.T) {
	// Entries written before the UTF-8 boundary existed are still on disk. Arrow
	// recall pushes them straight into x/term through setLine, which walks runes
	// and would submit different bytes than were stored. Storage keeps them at
	// full fidelity; recall must not offer them.
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	t.Cleanup(func() { _ = h.Close() })

	h.Record("bad\xb2entry")
	h.Record("clean entry")

	if got := h.stored(); !equalStrings(got, []string{"bad\xb2entry", "clean entry"}) {
		t.Fatalf("stored = %q, want both entries kept", got)
	}
	if h.Len() != 1 {
		t.Fatalf("Len = %d, want 1: the malformed entry must not be recallable", h.Len())
	}
	if got := h.At(0); got != "clean entry" {
		t.Fatalf("At(0) = %q, want the clean entry", got)
	}
}

func TestGoalHistoryTightensExistingLooseDir(t *testing.T) {
	// MkdirAll leaves an existing directory's mode alone, so a history
	// directory restored from a backup or left by an older build could sit at
	// 0777 and expose every goal.
	root := t.TempDir()
	getenv, path := historyEnv(t, root)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()
	h.Record("secret goal")

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("history dir mode = %o, want 0700; goals were group and world readable", got)
	}
}

func TestGoalHistoryReadsThroughOneDescriptor(t *testing.T) {
	// Loading through the already-open descriptor rather than reopening by
	// name must not disturb the append offset: the next record has to land at
	// the end, not overwrite what was loaded.
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	h.Record("first")
	h.Record("second")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h2 := newGoalHistory(getenv, root, collectWarnings(&warns))
	h2.Record("third")
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}

	h3 := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h3.Close() }()
	if got, want := h3.stored(), []string{"first", "second", "third"}; !equalStrings(got, want) {
		t.Fatalf("stored = %q, want %q", got, want)
	}
}

func TestGoalHistoryEvictionDoesNotRetainEvictedEntries(t *testing.T) {
	// Reslicing forward would leave evicted string headers in the dead slots
	// ahead of the new start, keeping their bodies alive until append happened
	// to reallocate. The observable property of compacting in place is that the
	// slice keeps starting at the allocation start, so its data pointer never
	// walks forward.
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))
	defer func() { _ = h.Close() }()

	for i := 0; i < maxHistoryEntries; i++ {
		h.Record("seed-" + strconv.Itoa(i))
	}
	start := unsafe.SliceData(h.entries)
	startCap := cap(h.entries)

	for i := 0; i < 3*maxHistoryEntries; i++ {
		h.Record("evicting-" + strconv.Itoa(i))
	}
	if got := unsafe.SliceData(h.entries); got != start {
		t.Fatalf("slice data pointer moved during eviction; evicted entries stay reachable in the slots left behind")
	}
	if got := cap(h.entries); got != startCap {
		t.Fatalf("capacity grew from %d to %d during eviction", startCap, got)
	}
	if got := len(h.entries); got != maxHistoryEntries {
		t.Fatalf("len = %d, want %d", got, maxHistoryEntries)
	}
}

func TestGoalHistoryEvictionBoundary(t *testing.T) {
	// Exactly at, one below, and one above the cap.
	for _, n := range []int{maxHistoryEntries - 1, maxHistoryEntries, maxHistoryEntries + 1} {
		root := t.TempDir()
		getenv, _ := historyEnv(t, root)
		var warns []string
		h := newGoalHistory(getenv, root, collectWarnings(&warns))

		for i := 0; i < n; i++ {
			h.Record("goal-" + strconv.Itoa(i))
		}
		wantLen := n
		if wantLen > maxHistoryEntries {
			wantLen = maxHistoryEntries
		}
		got := h.stored()
		if len(got) != wantLen {
			t.Fatalf("n=%d: stored %d, want %d", n, len(got), wantLen)
		}
		wantOldest := "goal-" + strconv.Itoa(n-wantLen)
		if got[0] != wantOldest {
			t.Fatalf("n=%d: oldest = %q, want %q", n, got[0], wantOldest)
		}
		if want := "goal-" + strconv.Itoa(n-1); got[len(got)-1] != want {
			t.Fatalf("n=%d: newest = %q, want %q", n, got[len(got)-1], want)
		}
		if h.At(0) != "goal-"+strconv.Itoa(n-1) {
			t.Fatalf("n=%d: At(0) = %q, want the newest", n, h.At(0))
		}
		_ = h.Close()
	}
}

func TestGoalHistoryDegradeReportsCloseFailure(t *testing.T) {
	// A close failure inside degrade is the user's only chance to hear about
	// it: Close() afterwards reports nil because the file is already gone.
	root := t.TempDir()
	getenv, _ := historyEnv(t, root)
	var warns []string
	h := newGoalHistory(getenv, root, collectWarnings(&warns))

	// Close the descriptor behind degrade's back so its own Close fails.
	if err := h.file.Close(); err != nil {
		t.Fatal(err)
	}
	h.degrade(errors.New("triggering failure"))

	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want one", warns)
	}
	if !strings.Contains(warns[0], "triggering failure") {
		t.Fatalf("warning %q lost the triggering error", warns[0])
	}
	if !strings.Contains(warns[0], "closing the file failed") {
		t.Fatalf("warning %q dropped the close failure", warns[0])
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close after degrade = %v, want nil", err)
	}
}
