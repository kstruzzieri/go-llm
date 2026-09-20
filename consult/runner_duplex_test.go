//go:build unix

package consult

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const duplexInitialize = `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"go-llm-consult","version":"1"},"capabilities":{"experimentalApi":false,"requestAttestation":false,"optOutNotificationMethods":["item/agentMessage/delta","item/reasoning/textDelta","item/reasoning/summaryTextDelta","item/reasoning/summaryPartAdded","item/plan/delta"]}}}` + "\n"
const duplexInitialized = "{\"method\":\"initialized\"}\n"
const duplexModelList = `{"id":2,"method":"model/list","params":{"includeHidden":true,"limit":100}}` + "\n"
const duplexInitializeReply = `{"id":1,"result":{"userAgent":"fake","codexHome":"/synthetic","platformFamily":"unix","platformOs":"macos"}}` + "\n"
const duplexTerminal = `{"method":"turn/completed","params":{"threadId":"thread-test","turn":{"id":"turn-test","items":[],"itemsView":"notLoaded","status":"completed"}}}` + "\n"
const duplexMetadata = "{\"method\":\"skills/changed\",\"params\":{}}\n"
const duplexLateError = `{"method":"thread/status/changed","params":{"threadId":"thread-test","status":{"type":"systemError"}}}` + "\n"
const duplexTurnStarted = `{"method":"turn/started","params":{"threadId":"thread-test","turn":{"id":"turn-test","items":[],"itemsView":"notLoaded","status":"inProgress"}}}` + "\n"
const duplexInterrupt = `{"id":5,"method":"turn/interrupt","params":{"threadId":"thread-test","turnId":"turn-test"}}` + "\n"

var errDuplexTestRejected = errors.New("test: rejected record")

func duplexBytes(n int) []byte {
	return []byte(`{"text":"` + strings.Repeat("x", n-len("{\"text\":\"\"}\n")) + "\"}\n")
}

// This is deliberately a transport-only exchange, not an App Server admission
// parser: the source-derived records exercise framing and late rejection.
func duplexTestExchange() duplexExchange {
	return duplexExchange{
		start: func(string) (duplexAction, error) {
			return duplexAction{write: []byte(duplexInitialize)}, nil
		},
		receive: func(frame []byte) (duplexAction, error) {
			switch string(frame) + "\n" {
			case duplexInitializeReply:
				return duplexAction{write: []byte(duplexInitialized)}, nil
			case duplexTerminal:
				return duplexAction{closeStdin: true}, nil
			case duplexMetadata:
				return duplexAction{}, nil
			default:
				return duplexAction{closeStdin: true}, errDuplexTestRejected
			}
		},
	}
}

func duplexTestSpec(t *testing.T, mode string) (runSpec, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	dir := realTempDir(t)
	pidFile := filepath.Join(dir, "pid")
	t.Cleanup(func() {
		if raw, err := os.ReadFile(pidFile); err == nil {
			pid, err := strconv.Atoi(string(raw))
			if err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				waitForDeath(t, pidFile)
			}
		}
	})
	return runSpec{adapter: codexAdapter, command: exe, args: []string{"-test.run=^TestDuplexChild$", "--", "546-duplex-" + mode, dir}, timeout: 15 * time.Second, outputCap: 1048576, waitDelay: 150 * time.Millisecond}, dir
}

func assertDuplexCleanup(t *testing.T, out runOutcome, dir string) {
	t.Helper()
	if !out.GroupCleanupOK || !out.CleanupOK {
		t.Fatalf("runDuplex cleanup = group %t, root %t; want both true", out.GroupCleanupOK, out.CleanupOK)
	}
	for _, cwd := range out.Cwds {
		if _, err := os.Stat(cwd); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("runDuplex cwd survived: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "pid")); err == nil {
		waitForDeath(t, filepath.Join(dir, "pid"))
	}
	stack := make([]byte, 1<<20)
	stack = stack[:runtime.Stack(stack, true)]
	if bytes.Contains(stack, []byte("consult.runDuplex.func")) || bytes.Contains(stack, []byte("consult.(*duplexOutput).")) {
		t.Fatal("runDuplex returned with a live transport worker")
	}
}

func TestDuplexRequestDrivenAndFragmented(t *testing.T) {
	spec, dir := duplexTestSpec(t, "flow")
	spec.stdin = "prompt validated separately, never written as unframed stdin"
	exchange := duplexTestExchange()
	var cwd string
	start := exchange.start
	exchange.start = func(privateCwd string) (duplexAction, error) {
		cwd = privateCwd
		return start(privateCwd)
	}
	out, err := runDuplex(context.Background(), spec, exchange)
	if err != nil || out.ExitCode != 0 || out.WaitErrorKind != "none" || string(out.Stdout) != duplexInitializeReply+duplexTerminal+duplexMetadata || out.StderrBytes != 18 {
		t.Fatalf("runDuplex(fragmented) = %+v, %v; want complete exchange and counted stderr", out, err)
	}
	if len(out.Cwds) == 0 || cwd != out.Cwds[0] {
		t.Fatalf("runDuplex start cwd = %q, want private cwd %v", cwd, out.Cwds)
	}
	assertDuplexCleanup(t, out, dir)
}

func TestDuplexRejectsWritesAfterClose(t *testing.T) {
	spec, dir := duplexTestSpec(t, "flow")
	exchange := duplexTestExchange()
	receive := exchange.receive
	exchange.receive = func(frame []byte) (duplexAction, error) {
		if string(frame)+"\n" == duplexMetadata {
			return duplexAction{write: []byte(duplexInitialized)}, nil
		}
		return receive(frame)
	}
	out, err := runDuplex(context.Background(), spec, exchange)
	if !errors.Is(err, errDuplexWrite) {
		t.Fatalf("runDuplex(write after close) = %+v, %v; want rejected write", out, err)
	}
	assertDuplexCleanup(t, out, dir)
}

func TestDuplexVerifiesAfterPreparingInitialRequest(t *testing.T) {
	marker := filepath.Join(realTempDir(t), "launched")
	command := script(t, "touch "+shellQuote(marker))
	spec := runSpec{adapter: codexAdapter, command: command, sha256: sha256File(t, command), timeout: time.Second, outputCap: 4096}
	exchange := duplexTestExchange()
	exchange.start = func(string) (duplexAction, error) {
		if err := os.WriteFile(command, []byte("#!/bin/sh\ntouch "+shellQuote(marker)+"\nexit 3\n"), 0700); err != nil {
			t.Fatal(err)
		}
		return duplexAction{closeStdin: true}, nil
	}
	out, err := runDuplex(context.Background(), spec, exchange)
	if !errors.Is(err, errTargetDrift) || !out.CleanupOK {
		t.Fatalf("runDuplex(target changed while preparing) = %+v, %v; want digest drift before launch", out, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runDuplex launched a changed target: %v", err)
	}
}

func TestDuplexEarlyEOFAndLateRecords(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want error
	}{
		{"empty", errDuplexEOF},
		{"truncated", errDuplexEOF},
		{"late-error", errDuplexTestRejected},
		{"late-action", errDuplexTestRejected},
		{"late-truncated", errDuplexEOF},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			spec, dir := duplexTestSpec(t, tc.mode)
			out, err := runDuplex(context.Background(), spec, duplexTestExchange())
			if !errors.Is(err, tc.want) || out.ExitCode != 0 || out.WaitErrorKind != "none" {
				t.Fatalf("runDuplex(%s) = %+v, %v; want clean exit with %v", tc.mode, out, err, tc.want)
			}
			assertDuplexCleanup(t, out, dir)
		})
	}
}

func TestDuplexNormalShutdownDeadline(t *testing.T) {
	spec, dir := duplexTestSpec(t, "refuse-exit")
	spec.waitDelay = 0 // Exercise the production five-second normal-close timer.
	out, err := runDuplex(context.Background(), spec, duplexTestExchange())
	if err != nil || out.TimedOut || out.Canceled || out.WaitStatus != "signaled(SIGKILL)" || out.WaitErrorKind != "wait-delay" || out.Duration < 5*time.Second || out.Duration > 9*time.Second {
		t.Fatalf("runDuplex(refuse-exit) = %+v, %v; want five-second shutdown kill before consultation timeout", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stdin-eof")); err != nil {
		t.Fatalf("runDuplex(refuse-exit) did not close stdin: %v", err)
	}
	assertDuplexCleanup(t, out, dir)
}

func TestDuplexRejectionStillWritesNegativeReply(t *testing.T) {
	for _, mode := range []string{"reject", "reject-hang"} {
		t.Run(mode, func(t *testing.T) {
			spec, dir := duplexTestSpec(t, mode)
			exchange := duplexTestExchange()
			receive := exchange.receive
			exchange.receive = func(frame []byte) (duplexAction, error) {
				if bytes.Contains(frame, []byte(`"future/action"`)) {
					return duplexAction{write: []byte("{\"id\":47,\"error\":{\"code\":-32000,\"message\":\"unsupported server request\"}}\n"), closeStdin: true}, errDuplexTestRejected
				}
				return receive(frame)
			}
			out, err := runDuplex(context.Background(), spec, exchange)
			wantWait := "none"
			if mode == "reject-hang" {
				wantWait = "wait-delay"
			}
			if !errors.Is(err, errDuplexTestRejected) || out.WaitErrorKind != wantWait || out.Duration > 9*time.Second {
				t.Fatalf("runDuplex(%s) = %+v, %v; want preserved rejection and %s", mode, out, err, wantWait)
			}
			if _, err := os.Stat(filepath.Join(dir, "negative-received")); err != nil {
				t.Fatalf("runDuplex(%s) discarded negative reply: %v", mode, err)
			}
			assertDuplexCleanup(t, out, dir)
		})
	}
}

func TestDuplexOutboundLimit(t *testing.T) {
	for _, n := range []int{1048576, 1048577} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			spec, dir := duplexTestSpec(t, "consume")
			exchange := duplexTestExchange()
			exchange.start = func(string) (duplexAction, error) {
				return duplexAction{write: duplexBytes(n), closeStdin: true}, nil
			}
			out, err := runDuplex(context.Background(), spec, exchange)
			if out.CapExceeded != (n > 1048576) || out.TimedOut || out.Canceled || out.Duration > 3*time.Second {
				t.Fatalf("runDuplex(outbound=%d) = %+v, %v; want cap=%t and bounded cleanup", n, out, err, n > 1048576)
			}
			if n == 1048576 {
				count, readErr := os.ReadFile(filepath.Join(dir, "consumed"))
				if err != nil || out.ExitCode != 0 || readErr != nil || string(count) != "1048576" {
					t.Fatalf("runDuplex(exact outbound cap) = %+v, %v, consumed=%q (%v)", out, err, count, readErr)
				}
			}
			assertDuplexCleanup(t, out, dir)
		})
	}
}

func TestDuplexOutboundLimitIsCumulativeAndPrecedesCancel(t *testing.T) {
	spec, dir := duplexTestSpec(t, "two-writes")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exchange := duplexTestExchange()
	exchange.start = func(string) (duplexAction, error) {
		return duplexAction{write: duplexBytes(1048512)}, nil
	}
	exchange.receive = func([]byte) (duplexAction, error) {
		cancel()
		return duplexAction{write: duplexBytes(65)}, nil
	}
	out, err := runDuplex(ctx, spec, exchange)
	if !out.CapExceeded || !out.Canceled || out.TimedOut || out.Duration > 3*time.Second {
		t.Fatalf("runDuplex(cumulative cap + cancel) = %+v, %v; want cap and caller flags, no deadline", out, err)
	}
	count, readErr := os.ReadFile(filepath.Join(dir, "consumed"))
	if readErr != nil || string(count) != "1048512" {
		t.Fatalf("runDuplex(cumulative cap) wrote past first batch: %q (%v)", count, readErr)
	}
	assertDuplexCleanup(t, out, dir)
}

// A coalesced initialize reply and remote metadata produce two control writes.
// Keep the first write in flight so scheduling cannot hide a too-small queue.
func TestDuplexQueuesCoalescedStartupWhileWriteBlocked(t *testing.T) {
	spec, dir := duplexTestSpec(t, "coalesced-startup")
	exchange := duplexTestExchange()
	exchange.start = func(string) (duplexAction, error) {
		return duplexAction{write: duplexBytes(524288)}, nil
	}
	receive := exchange.receive
	metadata := 0
	exchange.receive = func(frame []byte) (duplexAction, error) {
		if string(frame)+"\n" == duplexMetadata {
			metadata++
			if metadata == 1 {
				return duplexAction{write: []byte(duplexModelList)}, nil
			}
			// This third record is an out-of-band test handshake: both control
			// writes have been enqueued before the child can drain the first.
			err := os.WriteFile(filepath.Join(dir, "drain"), nil, 0600)
			return duplexAction{closeStdin: true}, err
		}
		return receive(frame)
	}
	out, err := runDuplex(context.Background(), spec, exchange)
	if err != nil || metadata != 2 || out.ExitCode != 0 || out.WaitErrorKind != "none" || out.Canceled || out.TimedOut || out.CapExceeded {
		t.Fatalf("runDuplex(coalesced startup, first write blocked) = %+v, %v, metadata=%d; want complete ordered writes and clean success", out, err, metadata)
	}
	assertDuplexCleanup(t, out, dir)
}

func TestDuplexBackpressureDoesNotBlockDispatcher(t *testing.T) {
	spec, dir := duplexTestSpec(t, "backpressure")
	exchange := duplexTestExchange()
	exchange.start = func(string) (duplexAction, error) {
		return duplexAction{write: duplexBytes(262144)}, nil
	}
	exchange.receive = func([]byte) (duplexAction, error) {
		return duplexAction{write: duplexBytes(262144)}, nil
	}
	out, err := runDuplex(context.Background(), spec, exchange)
	if !errors.Is(err, errDuplexBackpressure) || out.TimedOut || out.CapExceeded || out.Duration > 3*time.Second {
		t.Fatalf("runDuplex(saturated handoff) = %+v, %v; want immediate bounded backpressure rejection", out, err)
	}
	assertDuplexCleanup(t, out, dir)
}

func TestDuplexDrainsBothStreamsWhileWriteBlocked(t *testing.T) {
	spec, dir := duplexTestSpec(t, "bursts")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exchange := duplexTestExchange()
	exchange.start = func(string) (duplexAction, error) {
		return duplexAction{write: duplexBytes(524288)}, nil
	}
	seen := 0
	exchange.receive = func(frame []byte) (duplexAction, error) {
		seen++
		if string(frame)+"\n" == duplexTerminal {
			cancel()
		}
		return duplexAction{}, nil
	}
	out, err := runDuplex(ctx, spec, exchange)
	if seen != 257 || out.StderrBytes != 262144 || !out.Canceled || out.TimedOut || out.CapExceeded || out.Duration > 3*time.Second {
		t.Fatalf("runDuplex(concurrent bursts) = %+v, %v, records=%d; want both streams drained while write blocked", out, err, seen)
	}
	assertDuplexCleanup(t, out, dir)
}

func TestDuplexBlockedWriteCloseIsBounded(t *testing.T) {
	spec, dir := duplexTestSpec(t, "blocked")
	exchange := duplexTestExchange()
	exchange.start = func(string) (duplexAction, error) {
		return duplexAction{write: duplexBytes(524288)}, nil
	}
	exchange.receive = func([]byte) (duplexAction, error) {
		return duplexAction{closeStdin: true}, errDuplexTestRejected
	}
	out, err := runDuplex(context.Background(), spec, exchange)
	if !errors.Is(err, errDuplexTestRejected) || out.TimedOut || out.WaitStatus != "signaled(SIGKILL)" || out.Duration < 5*time.Second || out.Duration > 9*time.Second {
		t.Fatalf("runDuplex(rejected, write blocked) = %+v, %v; want bounded close before consultation timeout", out, err)
	}
	assertDuplexCleanup(t, out, dir)
}

func TestDuplexCleanExitAfterCompleteWrites(t *testing.T) {
	spec, dir := duplexTestSpec(t, "complete-exit")
	exchange := duplexTestExchange()
	receive := exchange.receive
	waitFinished := false
	exchange.receive = func(frame []byte) (duplexAction, error) {
		if string(frame)+"\n" == duplexTerminal {
			raw, err := os.ReadFile(filepath.Join(dir, "pid"))
			if err != nil {
				return duplexAction{closeStdin: true}, err
			}
			pid, err := strconv.Atoi(string(raw))
			if err != nil {
				return duplexAction{closeStdin: true}, err
			}
			// Test-only scheduling barrier: keep the writer queue open until
			// the real Wait has reaped the child and closed its StdinPipe.
			// This forces the final writer Close to happen second, without
			// a production hook or a guessed sleep controlling the ordering.
			deadline := time.Now().Add(2 * time.Second)
			stack := make([]byte, 1<<20)
			for time.Now().Before(deadline) {
				if syscall.Kill(pid, 0) == syscall.ESRCH {
					n := runtime.Stack(stack, true)
					if !bytes.Contains(stack[:n], []byte("os/exec.(*Cmd).Wait(")) {
						waitFinished = true
						break
					}
				}
				time.Sleep(time.Millisecond)
			}
		}
		return receive(frame)
	}
	out, err := runDuplex(context.Background(), spec, exchange)
	if !waitFinished || err != nil || out.ExitCode != 0 || out.WaitErrorKind != "none" || out.Canceled || out.TimedOut || out.CapExceeded || string(out.Stdout) != duplexInitializeReply+duplexTerminal {
		t.Fatalf("runDuplex(complete writes, Wait before final close) = %+v, %v, waited=%t; want clean success", out, err, waitFinished)
	}
	assertDuplexCleanup(t, out, dir)
}

func TestDuplexCleanExitCannotHideIncompleteWrite(t *testing.T) {
	spec, dir := duplexTestSpec(t, "terminal-exit")
	exchange := duplexTestExchange()
	exchange.start = func(string) (duplexAction, error) {
		return duplexAction{write: duplexBytes(524288)}, nil
	}
	out, err := runDuplex(context.Background(), spec, exchange)
	// os/exec may report "other" if our write-error cancellation races a zero
	// exit. Both orders must retain the required-write rejection.
	if !errors.Is(err, errDuplexWrite) || out.ExitCode != 0 || (out.WaitErrorKind != "none" && out.WaitErrorKind != "other") || out.TimedOut || out.CapExceeded {
		t.Fatalf("runDuplex(terminal + exit before write completes) = %+v, %v; want rejected incomplete write despite clean process/EOF", out, err)
	}
	assertDuplexCleanup(t, out, dir)
}

func TestDuplexBlockedWriteCancellationAndDeadline(t *testing.T) {
	for _, mode := range []string{"before-turn", "after-turn", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			spec, dir := duplexTestSpec(t, "blocked")
			spec.waitDelay = 5 * time.Second // A missing Cancel must not hide behind WaitDelay's later kill.
			if mode == "after-turn" {
				spec.args[len(spec.args)-2] = "546-duplex-blocked-after-turn"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline" {
				spec.timeout = 500 * time.Millisecond
			}
			exchange := duplexTestExchange()
			exchange.start = func(string) (duplexAction, error) {
				return duplexAction{write: duplexBytes(524288)}, nil
			}
			active, interrupts := false, 0
			exchange.receive = func(frame []byte) (duplexAction, error) {
				active = string(frame)+"\n" == duplexTurnStarted
				if mode != "deadline" {
					cancel()
				}
				return duplexAction{}, nil
			}
			exchange.interrupt = func() []byte {
				if !active {
					return nil
				}
				interrupts++
				return []byte("{\"id\":5,\"method\":\"turn/interrupt\",\"params\":{\"threadId\":\"thread-test\",\"turnId\":\"turn-test\"}}\n")
			}
			out, err := runDuplex(ctx, spec, exchange)
			if out.Canceled != (mode != "deadline") || out.TimedOut != (mode == "deadline") || out.WaitStatus != "signaled(SIGKILL)" || out.Duration > 3*time.Second || interrupts > 1 || (mode != "after-turn" && interrupts != 0) {
				t.Fatalf("runDuplex(%s) = %+v, %v, interrupts=%d; want bounded stop and at most one interrupt after turn ID", mode, out, err, interrupts)
			}
			assertDuplexCleanup(t, out, dir)
		})
	}
}

func TestDuplexCancellationDeliversInterrupt(t *testing.T) {
	for _, mode := range []string{"interrupt-ack", "interrupt-no-ack", "interrupt-deadline", "interrupt-cap"} {
		t.Run(mode, func(t *testing.T) {
			spec, dir := duplexTestSpec(t, mode)
			spec.waitDelay = 5 * time.Second
			if mode == "interrupt-deadline" {
				spec.timeout = time.Second
			}
			if mode == "interrupt-cap" {
				spec.outputCap = 1024
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exchange := duplexTestExchange()
			receive := exchange.receive
			active := false
			exchange.receive = func(frame []byte) (duplexAction, error) {
				if string(frame)+"\n" == duplexTurnStarted {
					active = true
					cancel()
					return duplexAction{}, nil
				}
				return receive(frame)
			}
			exchange.interrupt = func() []byte {
				if active {
					return []byte(duplexInterrupt)
				}
				return nil
			}
			out, err := runDuplex(ctx, spec, exchange)
			got, readErr := os.ReadFile(filepath.Join(dir, "interrupt-received"))
			if readErr != nil || string(got) != duplexInterrupt {
				t.Fatalf("runDuplex(%s) = %+v, %v, interrupt=%q (%v); want exact interrupt before teardown", mode, out, err, got, readErr)
			}
			if _, err := os.Stat(filepath.Join(dir, "stdin-eof")); err != nil {
				t.Fatalf("runDuplex(%s) did not close stdin after interrupt: %v", mode, err)
			}
			if !out.Canceled || out.CapExceeded != (mode == "interrupt-cap") || out.Duration > 9*time.Second {
				t.Fatalf("runDuplex(%s) = %+v, %v; want bounded caller cancellation", mode, out, err)
			}
			if mode == "interrupt-ack" && (out.ExitCode != 0 || out.Duration > 3*time.Second) {
				t.Fatalf("runDuplex(ack) = %+v, %v; want graceful bounded teardown", out, err)
			}
			if mode == "interrupt-no-ack" && (out.WaitStatus != "signaled(SIGKILL)" || out.Duration < 5*time.Second || out.TimedOut) {
				t.Fatalf("runDuplex(no ack) = %+v, %v; want five-second cancellation kill", out, err)
			}
			if mode == "interrupt-deadline" && (!out.TimedOut || out.Duration > 3*time.Second) {
				t.Fatalf("runDuplex(deadline during interrupt) = %+v, %v; want original hard deadline", out, err)
			}
			if mode == "interrupt-cap" && (out.StderrBytes <= 1024 || out.TimedOut || out.Duration > 3*time.Second) {
				t.Fatalf("runDuplex(cap during interrupt) = %+v, %v; want immediate cap abort before cancellation grace", out, err)
			}
			assertDuplexCleanup(t, out, dir)
		})
	}
}

func TestDuplexAppServerEnvironment(t *testing.T) {
	spec, dir := duplexTestSpec(t, "app-env")
	exchange := duplexTestExchange()
	exchange.start = func(string) (duplexAction, error) { return duplexAction{closeStdin: true}, nil }
	exchange.receive = func([]byte) (duplexAction, error) { return duplexAction{}, nil }
	out, err := runDuplex(context.Background(), spec, exchange)
	want := "{\"remoteControlDisabled\":\"1\",\"names\":\"CODEX_EXEC_SERVER_URL,CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED,HOME,LC_ALL,PATH,TMPDIR,USER,XDG_CACHE_HOME,XDG_CONFIG_HOME,XDG_STATE_HOME\"}\n"
	if err != nil || out.ExitCode != 0 || string(out.Stdout) != want {
		t.Fatalf("runDuplex(App Server env) = %q, %v; want exact ten-name profile %q", out.Stdout, err, want)
	}
	assertDuplexCleanup(t, out, dir)
}

func TestDuplexStreamCaps(t *testing.T) {
	for _, mode := range []string{"stdout-overflow", "stderr-overflow"} {
		t.Run(mode, func(t *testing.T) {
			spec, dir := duplexTestSpec(t, mode)
			spec.outputCap = 1024
			out, err := runDuplex(context.Background(), spec, duplexTestExchange())
			if !out.CapExceeded || out.Canceled || out.TimedOut || len(out.Stdout) > 1024 || out.Duration > 3*time.Second {
				t.Fatalf("runDuplex(%s) = %+v, %v; want stream cap and bounded cleanup", mode, out, err)
			}
			if mode == "stderr-overflow" && (len(out.Stdout) != 0 || out.StderrBytes <= 1024) {
				t.Fatalf("runDuplex(stderr overflow) retained diagnostics or missed count: %+v", out)
			}
			assertDuplexCleanup(t, out, dir)
		})
	}
}

func TestDuplexRecordBound(t *testing.T) {
	for _, mode := range []string{"records", "too-many-records"} {
		t.Run(mode, func(t *testing.T) {
			spec, dir := duplexTestSpec(t, mode)
			exchange := duplexTestExchange()
			exchange.start = func(string) (duplexAction, error) { return duplexAction{closeStdin: true}, nil }
			out, err := runDuplex(context.Background(), spec, exchange)
			if (mode == "records" && err != nil) || (mode == "too-many-records" && !errors.Is(err, errDuplexRecords)) || out.ExitCode != 0 || out.WaitErrorKind != "none" {
				t.Fatalf("runDuplex(%s) = %+v, %v; want exact 4096-record bound", mode, out, err)
			}
			assertDuplexCleanup(t, out, dir)
		})
	}
}

func TestDuplexEOFIndependentOfExit(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		for _, code := range []int{0, 3} {
			t.Run(fmt.Sprintf("%s/exit%d", stream, code), func(t *testing.T) {
				dir := realTempDir(t)
				pid := filepath.Join(dir, "pid")
				t.Cleanup(func() {
					if raw, err := os.ReadFile(pid); err == nil {
						if child, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && child > 0 {
							_ = syscall.Kill(child, syscall.SIGKILL)
							waitForDeath(t, pid)
						}
					}
				})
				redirect := "2>/dev/null"
				if stream == "stderr" {
					redirect = ">/dev/null"
				}
				body := "printf '%s' " + shellQuote(duplexTerminal) + "; sleep 30 " + redirect + " & echo $! > " + shellQuote(pid) + "; exit " + strconv.Itoa(code)
				spec := runSpec{adapter: codexAdapter, command: script(t, body), timeout: 10 * time.Second, waitDelay: 100 * time.Millisecond, outputCap: 4096}
				exchange := duplexTestExchange()
				exchange.start = func(string) (duplexAction, error) { return duplexAction{}, nil }
				out, err := runDuplex(context.Background(), spec, exchange)
				if out.ExitCode != code || out.WaitErrorKind != "wait-delay" || out.Canceled || out.TimedOut || out.Duration > 3*time.Second {
					t.Fatalf("runDuplex(held %s, exit %d) = %+v, %v; want independent EOF failure", stream, code, out, err)
				}
				assertDuplexCleanup(t, out, dir)
			})
		}
	}
}

func TestDuplexEscapedPipeHolder(t *testing.T) {
	spec, dir := duplexTestSpec(t, "unused")
	pids, ready := filepath.Join(dir, "pids"), filepath.Join(dir, "ready")
	spec.args = []string{"-test.run=^TestDrainChild$", "--", "546-drain-leader", pids, ready}
	t.Cleanup(func() {
		raw, _ := os.ReadFile(pids)
		leader, holder := 0, 0
		_, _ = fmt.Sscanf(string(raw), "%d %d", &leader, &holder)
		for _, pid := range []int{leader, holder} {
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				file := filepath.Join(dir, "cleanup-"+strconv.Itoa(pid))
				if err := os.WriteFile(file, []byte(strconv.Itoa(pid)), 0600); err != nil {
					t.Fatal(err)
				}
				waitForDeath(t, file)
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		out runOutcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := runDuplex(ctx, spec, duplexTestExchange())
		done <- result{out, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	got := <-done
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("runDuplex escaped holder never became ready: %v", err)
	}
	if !got.out.Canceled || got.out.TimedOut || got.out.WaitErrorKind != "wait-delay" || got.out.WaitStatus != "signaled(SIGKILL)" || got.out.Duration > 3*time.Second {
		t.Fatalf("runDuplex(escaped holder) = %+v, %v; want caller cancellation before independent drain failure", got.out, got.err)
	}
	assertDuplexCleanup(t, got.out, dir)
}

func TestDuplexOutputRequiresEOF(t *testing.T) {
	for _, fail := range []bool{false, true} {
		w := &duplexOutput{cappedWriter: &cappedWriter{cap: 8, retain: true}, chunks: make(chan []byte, 1), stop: make(chan struct{})}
		var r io.Reader = strings.NewReader("abc")
		if fail {
			r = io.MultiReader(r, drainErrorReader{})
		}
		n, err := w.ReadFrom(r)
		if n != 3 || w.complete() == fail || (err != nil) != fail || string(<-w.chunks) != "abc" {
			t.Fatalf("duplexOutput.ReadFrom(error=%t) = %d, %v, complete=%t; want true EOF only", fail, n, err, w.complete())
		}
	}
}

func TestDuplexValidatesBeforeLaunch(t *testing.T) {
	base, dir := duplexTestSpec(t, "empty")
	for _, tc := range []struct {
		name string
		edit func(*runSpec, *duplexExchange)
	}{
		{"oversize-prompt", func(s *runSpec, _ *duplexExchange) { s.stdin = strings.Repeat("x", 65537) }},
		{"invalid-utf8", func(s *runSpec, _ *duplexExchange) { s.stdin = "\xff" }},
		{"wrong-adapter", func(s *runSpec, _ *duplexExchange) { s.adapter = claudeAdapter }},
		{"no-deadline", func(s *runSpec, _ *duplexExchange) { s.timeout = 0 }},
		{"zero-cap", func(s *runSpec, _ *duplexExchange) { s.outputCap = 0 }},
		{"excess-cap", func(s *runSpec, _ *duplexExchange) { s.outputCap = 1048577 }},
		{"no-start", func(_ *runSpec, x *duplexExchange) { x.start = nil }},
		{"no-receiver", func(_ *runSpec, x *duplexExchange) { x.receive = nil }},
		{"drift", func(s *runSpec, _ *duplexExchange) { s.sha256 = strings.Repeat("0", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, exchange := base, duplexTestExchange()
			tc.edit(&spec, &exchange)
			if out, err := runDuplex(context.Background(), spec, exchange); err == nil {
				t.Fatalf("runDuplex(%s) = %+v, nil; want pre-launch rejection", tc.name, out)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runDuplex launched an invalid invocation: %v", err)
	}
}

// TestDuplexChild only executes the test binary. It never touches vendor
// executables, credentials or configuration.
// syscall.Exit skips the race runtimes atexit sleep, keeping fake process exit
// independent of the host shutdown timer without adding child environment keys.
func TestDuplexChild(t *testing.T) {
	if len(os.Args) < 4 {
		return
	}
	mode, dir := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	if !strings.HasPrefix(mode, "546-duplex-") {
		return
	}
	mode = strings.TrimPrefix(mode, "546-duplex-")
	if os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0600) != nil {
		syscall.Exit(2)
	}
	if mode == "empty" {
		syscall.Exit(0)
	}
	if mode == "truncated" {
		_, _ = fmt.Fprint(os.Stdout, `{"id":1`)
		syscall.Exit(0)
	}
	switch mode {
	case "app-env":
		names := make([]string, 0, len(os.Environ()))
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			names = append(names, name)
		}
		sort.Strings(names)
		_, _ = fmt.Fprintf(os.Stdout, "{\"remoteControlDisabled\":%q,\"names\":%q}\n", os.Getenv("CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED"), strings.Join(names, ","))
		_, _ = io.Copy(io.Discard, os.Stdin)
		syscall.Exit(0)
	case "terminal-exit":
		_, _ = fmt.Fprint(os.Stdout, duplexTerminal)
		syscall.Exit(0)
	case "consume":
		n, err := io.Copy(io.Discard, os.Stdin)
		if err != nil || os.WriteFile(filepath.Join(dir, "consumed"), []byte(strconv.FormatInt(n, 10)), 0600) != nil {
			syscall.Exit(2)
		}
		syscall.Exit(0)
	case "two-writes":
		line, err := bufio.NewReader(os.Stdin).ReadBytes('\n')
		if err != nil || os.WriteFile(filepath.Join(dir, "consumed"), []byte(strconv.Itoa(len(line))), 0600) != nil {
			syscall.Exit(2)
		}
		_, _ = fmt.Fprint(os.Stdout, duplexMetadata)
		time.Sleep(30 * time.Second)
		syscall.Exit(0)
	case "coalesced-startup":
		// Reading one byte proves the writer dequeued the large first batch;
		// withholding the remainder keeps that write blocked at the pipe.
		prefix := make([]byte, 1)
		if _, err := io.ReadFull(os.Stdin, prefix); err != nil || prefix[0] != '{' {
			syscall.Exit(2)
		}
		_, _ = fmt.Fprint(os.Stdout, duplexInitializeReply+duplexMetadata+duplexMetadata)
		for {
			if _, err := os.Stat(filepath.Join(dir, "drain")); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		rest, err := io.ReadAll(os.Stdin)
		want := append(duplexBytes(524288)[1:], []byte(duplexInitialized+duplexModelList)...)
		if err != nil || !bytes.Equal(rest, want) {
			syscall.Exit(2)
		}
		_, _ = fmt.Fprint(os.Stdout, duplexTerminal)
		syscall.Exit(0)
	case "backpressure":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat(duplexMetadata, 3))
		time.Sleep(30 * time.Second)
		syscall.Exit(0)
	case "bursts":
		done := make(chan struct{})
		go func() {
			_, _ = fmt.Fprint(os.Stdout, strings.Repeat(duplexMetadata, 256))
			close(done)
		}()
		_, _ = os.Stderr.Write(bytes.Repeat([]byte{'e'}, 262144))
		<-done
		_, _ = fmt.Fprint(os.Stdout, duplexTerminal)
		time.Sleep(30 * time.Second)
		syscall.Exit(0)
	case "blocked", "blocked-after-turn":
		frame := duplexMetadata
		if mode == "blocked-after-turn" {
			frame = duplexTurnStarted
		}
		_, _ = fmt.Fprint(os.Stdout, frame)
		time.Sleep(30 * time.Second)
		syscall.Exit(0)
	case "stdout-overflow", "stderr-overflow":
		w := os.Stdout
		if mode == "stderr-overflow" {
			w = os.Stderr
		}
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, 8192))
		time.Sleep(30 * time.Second)
		syscall.Exit(0)
	case "records", "too-many-records":
		_, _ = io.Copy(io.Discard, os.Stdin)
		n := 4096
		if mode == "too-many-records" {
			n++
		}
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat(duplexMetadata, n))
		syscall.Exit(0)
	}
	in := bufio.NewReader(os.Stdin)
	line, err := in.ReadString('\n')
	if err != nil || line != duplexInitialize {
		syscall.Exit(21)
	}
	for _, part := range []string{duplexInitializeReply[:3], duplexInitializeReply[3:17], duplexInitializeReply[17:]} {
		_, _ = fmt.Fprint(os.Stdout, part)
	}
	line, err = in.ReadString('\n')
	if err != nil || line != duplexInitialized {
		syscall.Exit(22)
	}
	if strings.HasPrefix(mode, "interrupt-") {
		_, _ = fmt.Fprint(os.Stdout, duplexTurnStarted)
		line, err := in.ReadString('\n')
		if err != nil || line != duplexInterrupt || os.WriteFile(filepath.Join(dir, "interrupt-received"), []byte(line), 0600) != nil {
			syscall.Exit(28)
		}
		if rest, err := io.ReadAll(in); err != nil || len(rest) != 0 || os.WriteFile(filepath.Join(dir, "stdin-eof"), nil, 0600) != nil {
			syscall.Exit(29)
		}
		if mode == "interrupt-cap" {
			_, _ = os.Stderr.Write(bytes.Repeat([]byte{'e'}, 8192))
		}
		if mode != "interrupt-ack" {
			time.Sleep(30 * time.Second)
		}
		_, _ = fmt.Fprint(os.Stdout, "{\"id\":5,\"result\":{}}\n")
		syscall.Exit(0)
	}
	if mode == "reject" || mode == "reject-hang" {
		_, _ = fmt.Fprint(os.Stdout, "{\"id\":47,\"method\":\"future/action\",\"params\":{}}\n")
		line, err := in.ReadString('\n')
		if err != nil || line != "{\"id\":47,\"error\":{\"code\":-32000,\"message\":\"unsupported server request\"}}\n" {
			syscall.Exit(26)
		}
		if rest, err := io.ReadAll(in); err != nil || len(rest) != 0 || os.WriteFile(filepath.Join(dir, "negative-received"), nil, 0600) != nil {
			syscall.Exit(27)
		}
		if mode == "reject-hang" {
			time.Sleep(30 * time.Second)
		}
		syscall.Exit(0)
	}
	_, _ = fmt.Fprint(os.Stdout, duplexTerminal)
	if mode == "complete-exit" {
		syscall.Exit(0)
	}
	if rest, err := io.ReadAll(in); err != nil || len(rest) != 0 {
		syscall.Exit(23)
	}
	if os.WriteFile(filepath.Join(dir, "stdin-eof"), nil, 0600) != nil {
		syscall.Exit(24)
	}
	switch mode {
	case "flow":
		fmt.Fprint(os.Stderr, "private diagnostic")
		_, _ = fmt.Fprint(os.Stdout, duplexMetadata)
	case "late-error":
		_, _ = fmt.Fprint(os.Stdout, duplexLateError)
	case "late-action":
		_, _ = fmt.Fprint(os.Stdout, "{\"id\":47,\"method\":\"item/permissions/requestApproval\",\"params\":{}}\n")
	case "late-truncated":
		_, _ = fmt.Fprint(os.Stdout, `{"method":"thread/status/changed"`)
	case "refuse-exit":
		time.Sleep(30 * time.Second)
	default:
		syscall.Exit(25)
	}
	syscall.Exit(0)
}
