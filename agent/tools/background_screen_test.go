//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bgScreenTools is the background tool set over a real manager and a
// tolerant workspace, driven only through the model tools.
type bgScreenTools struct {
	sc     *StartCommand
	status *CommandStatus
	tail   *CommandTail
	stop   *StopCommand
	root   string
}

func newBGScreenTools(t *testing.T, files map[string]string) *bgScreenTools {
	t.Helper()
	ws, root, _ := tolerantFixture(t, files, nil)
	m := NewBackgroundManager()
	t.Cleanup(m.Shutdown)
	return &bgScreenTools{
		sc: NewStartCommand(ws, m), status: NewCommandStatus(m),
		tail: NewCommandTail(m), stop: NewStopCommand(m), root: root,
	}
}

// start runs one start_command call and returns the job handle.
func (b *bgScreenTools) start(t *testing.T, label, script string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"argv": []string{"sh", "-c", script}})
	if err != nil {
		t.Fatal(err)
	}
	mustPlan(t, b.sc, raw)
	res := mustInvoke(t, b.sc, raw)
	first, _, _ := strings.Cut(res.Content, "\n")
	handle, ok := strings.CutPrefix(first, "handle: ")
	if res.IsError || !ok {
		t.Fatalf("%s: start failed (len %d)", label, len(res.Content))
	}
	return handle
}

// statusOf returns the command_status content for handle.
func (b *bgScreenTools) statusOf(t *testing.T, handle string) string {
	t.Helper()
	res := mustInvoke(t, b.status, json.RawMessage(fmt.Sprintf(`{"handle":%q}`, handle)))
	if res.IsError {
		t.Fatalf("status failed (len %d)", len(res.Content))
	}
	return res.Content
}

// waitState polls command_status until the job reports state.
func (b *bgScreenTools) waitState(t *testing.T, label, handle, state string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(b.statusOf(t, handle), "\nstate: "+state+"\n") {
		if time.Now().After(deadline) {
			t.Fatalf("%s: job never reached state %s", label, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// tailAt runs one command_tail read of stream from cursor.
func (b *bgScreenTools) tailAt(t *testing.T, label, handle, stream string, cursor uint64, maxBytes int) (payload string, next uint64, eof bool) {
	t.Helper()
	raw := fmt.Sprintf(`{"handle":%q,"stream":%q,"cursor":%d,"max_bytes":%d}`, handle, stream, cursor, maxBytes)
	res := mustInvoke(t, b.tail, json.RawMessage(raw))
	if res.IsError {
		t.Fatalf("%s: tail failed (len %d)", label, len(res.Content))
	}
	head, payload, ok := strings.Cut(res.Content, "--- output ---\n")
	if !ok {
		t.Fatalf("%s: no output delimiter (len %d)", label, len(res.Content))
	}
	for _, line := range strings.Split(head, "\n") {
		if v, ok := strings.CutPrefix(line, "next_cursor: "); ok {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				t.Fatalf("%s: bad next_cursor", label)
			}
			next = n
		}
		if line == "eof: true" {
			eof = true
		}
	}
	return payload, next, eof
}

// readToEOF reads stream from cursor 0, following each returned cursor,
// until eof, and returns the joined payloads.
func (b *bgScreenTools) readToEOF(t *testing.T, label, handle, stream string, maxBytes int) string {
	t.Helper()
	var all strings.Builder
	cursor := uint64(0)
	deadline := time.Now().Add(10 * time.Second)
	for {
		payload, next, eof := b.tailAt(t, label, handle, stream, cursor, maxBytes)
		all.WriteString(payload)
		cursor = next
		if eof {
			return all.String()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: no eof (read %d bytes)", label, all.Len())
		}
		if payload == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// checkScreened fails unless out holds the sentinel and the span marker and
// not the value.
func checkScreened(t *testing.T, label, out string) {
	t.Helper()
	switch {
	case strings.Contains(out, screenEnvValue):
		t.Fatalf("%s: the value reached the tail", label)
	case !strings.Contains(out, screenSentinel), !strings.Contains(out, tM):
		t.Fatalf("%s: sentinel %t, marker %t (len %d)", label, strings.Contains(out, screenSentinel), strings.Contains(out, tM), len(out))
	}
}

var bgScreenTree = map[string]string{".env": screenEnvFile, "marker.txt": screenSentinel + "\n"}

func TestBackgroundTailScreened(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	rows := []struct{ label, script, stream, other string }{
		{"stdout", "cat .e* marker.txt", "stdout", "stderr"},
		{"stderr", "cat .e* marker.txt >&2", "stderr", "stdout"},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			b := newBGScreenTools(t, bgScreenTree)
			h := b.start(t, r.label, r.script)
			b.waitState(t, r.label, h, "exited")
			whole := b.readToEOF(t, r.label, h, r.stream, bgTailMaxBytes)
			checkScreened(t, r.label, whole)
			// "NAME=" is 5 bytes, so a 10-byte read ends inside the marker.
			split := b.readToEOF(t, r.label, h, r.stream, 10)
			if split != whole {
				t.Fatalf("%s: split joined bytes differ (len %d, want %d)", r.label, len(split), len(whole))
			}
			if other := b.readToEOF(t, r.label, h, r.other, bgTailMaxBytes); other != "" {
				t.Fatalf("%s: the other stream is not empty (len %d)", r.label, len(other))
			}
		})
	}
}

func TestBackgroundCutStopsTail(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	const body = "yes 0123456789012345678901234567890123456789012345678901234567890123456789 | head -n 1024; " +
		"printf 'diff --git a/.env b/.env\\n'; echo content-line; while :; do echo more; sleep 0.05; done"
	rows := []struct{ label, script, stream string }{
		{"stdout", body, "stdout"},
		{"stderr", "{ " + body + "; } >&2", "stderr"},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) { checkCutStopsTail(t, r.label, r.script, r.stream) })
	}
}

func checkCutStopsTail(t *testing.T, label, script, stream string) {
	b := newBGScreenTools(t, nil)
	h := b.start(t, label, script)
	var all strings.Builder
	cursor := uint64(0)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.HasSuffix(all.String(), tP+"\n") {
		if time.Now().After(deadline) {
			t.Fatalf("%s: never ended with the patch marker (read %d bytes)", label, all.Len())
		}
		payload, next, _ := b.tailAt(t, label, h, stream, cursor, bgTailMaxBytes)
		all.WriteString(payload)
		cursor = next
		if payload == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	for i := 0; i < 2; i++ {
		time.Sleep(200 * time.Millisecond)
		payload, next, _ := b.tailAt(t, label, h, stream, cursor, bgTailMaxBytes)
		if payload != "" || next != cursor {
			t.Fatalf("%s after %d: payload len %d, next %d, want 0 and %d", label, i, len(payload), next, cursor)
		}
	}
	if !strings.Contains(b.statusOf(t, h), "\nstate: running\n") {
		t.Fatalf("%s: the job stopped printing", label)
	}
}

func TestBackgroundTailOneByte(t *testing.T) {
	b := newBGScreenTools(t, bgScreenTree)
	h := b.start(t, "one", "cat .e* marker.txt")
	b.waitState(t, "one", h, "exited")
	checkScreened(t, "one", b.readToEOF(t, "one", h, "stdout", 1))
}

func TestBackgroundCarriageReturnBeforeExit(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	b := newBGScreenTools(t, nil)
	h := b.start(t, "cr", `printf "partial\r"; sleep 30`)
	deadline := time.Now().Add(2 * time.Second)
	for {
		payload, _, _ := b.tailAt(t, "cr", h, "stdout", 0, bgTailMaxBytes)
		if strings.Contains(payload, "partial") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cr: no partial within two seconds (len %d)", len(payload))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(b.statusOf(t, h), "\nstate: running\n") {
		t.Fatal("cr: job not running")
	}
}

func TestBackgroundUnterminatedHeldUntilStop(t *testing.T) {
	b := newBGScreenTools(t, nil)
	h := b.start(t, "held", "printf ready; touch printed.txt; sleep 30")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(b.root, "printed.txt")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("held: printed.txt never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if payload, _, _ := b.tailAt(t, "held", h, "stdout", 0, bgTailMaxBytes); payload != "" {
		t.Fatalf("held: unterminated record delivered early (len %d)", len(payload))
	}
	raw := json.RawMessage(fmt.Sprintf(`{"handle":%q}`, h))
	mustPlan(t, b.stop, raw)
	if res := mustInvoke(t, b.stop, raw); res.IsError || !strings.Contains(res.Content, "state: killed\n") {
		t.Fatalf("held: stop failed (len %d)", len(res.Content))
	}
	if payload, _, _ := b.tailAt(t, "stopped", h, "stdout", 0, bgTailMaxBytes); payload != "ready" {
		t.Fatalf("stopped: payload len %d, want the 5 held bytes", len(payload))
	}
}

func TestBackgroundStatusCarriesScreenNote(t *testing.T) {
	b := newBGScreenTools(t, bgScreenTree)
	h := b.start(t, "note", "cat .e* marker.txt")
	b.waitState(t, "note", h, "exited")
	if c := b.statusOf(t, h); !strings.Contains(c, screenMatchNote) {
		t.Fatalf("note: status has no match note (len %d)", len(c))
	}
}

func TestBackgroundNoScreenFails(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	starter := starterOf(newFakeProc(1))
	m := newBackgroundManager(starter, &countingRandom{})
	t.Cleanup(m.Shutdown)
	spec := bgSpec()
	spec.Screen = nil
	if _, err := m.start(context.Background(), spec, "/w"); !errors.Is(err, errNoOutputScreen) {
		t.Fatalf("err = %v, want errNoOutputScreen", err)
	}
	if n := len(m.List()); n != 0 || starter.callCount() != 0 || activeCount(m) != 0 {
		t.Fatalf("list %d, starts %d, active %d, want all 0", n, starter.callCount(), activeCount(m))
	}
}

// keyedScreen is a screen holding one placeholder key.
func keyedScreen() *outputScreen { return newOutputScreen([][]byte{[]byte(screenEnvValue)}) }

// assertReleased fails unless s no longer holds a key.
func assertReleased(t *testing.T, label string, s *outputScreen) {
	t.Helper()
	if keys, _, released := s.snapshot(); keys != nil || !released {
		t.Fatalf("%s: screen not released (keys %d)", label, len(keys))
	}
}

func TestBackgroundAbandonedStartReleasesScreen(t *testing.T) {
	gated := func(proc *fakeProc, cancel context.CancelFunc) *fakeStarter {
		return &fakeStarter{fn: func(execSpec, io.Writer, io.Writer) (backgroundProcess, error) {
			cancel()
			return proc, nil
		}}
	}
	rows := []struct {
		label string
		setup func(cancel context.CancelFunc) backgroundStarter
	}{
		{"canceled after spawn", func(cancel context.CancelFunc) backgroundStarter {
			return gated(newFakeProc(7), cancel)
		}},
		{"spawn failure", func(context.CancelFunc) backgroundStarter {
			return failingStarter{}
		}},
		{"canceled before spawn", func(cancel context.CancelFunc) backgroundStarter {
			cancel()
			return starterOf(newFakeProc(7))
		}},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m := newBackgroundManager(r.setup(cancel), &countingRandom{})
			t.Cleanup(m.Shutdown)
			spec := bgSpec()
			spec.Screen = keyedScreen()
			if _, err := m.start(ctx, spec, "/w"); err == nil {
				t.Fatalf("%s: start succeeded", r.label)
			}
			assertReleased(t, r.label, spec.Screen)
		})
	}

	t.Run("shut down", func(t *testing.T) {
		m := newBackgroundManager(starterOf(), &countingRandom{})
		m.Shutdown()
		spec := bgSpec()
		spec.Screen = keyedScreen()
		if _, err := m.start(context.Background(), spec, "/w"); !errors.Is(err, errBackgroundShutDown) {
			t.Fatalf("shut down: err = %v", err)
		}
		assertReleased(t, "shut down", spec.Screen)
	})

	t.Run("job limit", func(t *testing.T) {
		var procs []*fakeProc
		for i := 0; i < backgroundActiveCap; i++ {
			procs = append(procs, newFakeProc(10+i))
		}
		m := newBackgroundManager(starterOf(procs...), &countingRandom{})
		t.Cleanup(m.Shutdown)
		for i := 0; i < backgroundActiveCap; i++ {
			if _, err := m.start(context.Background(), bgSpec(), "/w"); err != nil {
				t.Fatal(err)
			}
		}
		spec := bgSpec()
		spec.Screen = keyedScreen()
		if _, err := m.start(context.Background(), spec, "/w"); err == nil {
			t.Fatal("job limit: start succeeded")
		}
		assertReleased(t, "job limit", spec.Screen)
	})

	t.Run("entropy failure", func(t *testing.T) {
		m := newBackgroundManager(starterOf(newFakeProc(1)), &flakyRandom{failsLeft: 1, src: &countingRandom{}})
		t.Cleanup(m.Shutdown)
		spec := bgSpec()
		spec.Screen = keyedScreen()
		if _, err := m.start(context.Background(), spec, "/w"); err == nil {
			t.Fatal("entropy failure: start succeeded")
		}
		assertReleased(t, "entropy failure", spec.Screen)
	})
}

func TestBackgroundReapReleasesScreen(t *testing.T) {
	proc := newFakeProc(1)
	m := newBackgroundManager(starterOf(proc), &countingRandom{})
	t.Cleanup(m.Shutdown)
	spec := bgSpec()
	spec.Screen = keyedScreen()
	st, err := m.start(context.Background(), spec, "/w")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, released := spec.Screen.snapshot(); released {
		t.Fatal("running: screen released before exit")
	}
	done := jobDoneChan(t, m, st.Handle)
	proc.release()
	<-done
	assertReleased(t, "exited", spec.Screen)
}

func TestBackgroundScratchSetupFailureReleasesScreen(t *testing.T) {
	root := buildIsolationWorkspace(t)
	m := NewBackgroundManager()
	t.Cleanup(m.Shutdown)
	sc, _ := scratchBackgroundTools(t, root, ScratchConfig{Enabled: true}, m)
	// A root other than the runtime's fails beginScratchSession at once.
	spec := execSpec{WorkspaceRoot: root + "-elsewhere", Screen: keyedScreen()}
	res, err := sc.invokeScratched(context.Background(), execPending{}, spec)
	if err != nil || !res.IsError {
		t.Fatalf("scratch setup: want an error result (error %t, err %v)", res.IsError, err)
	}
	assertReleased(t, "scratch setup", spec.Screen)
}
