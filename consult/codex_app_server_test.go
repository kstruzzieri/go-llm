//go:build unix

package consult

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

var appTestArgs = []string{
	"-c", `approval_policy="never"`, "-c", `approvals_reviewer="user"`,
	"-c", "project_doc_max_bytes=0", "-c", `web_search="disabled"`,
	"-c", "features.hooks=false", "-c", "features.apps=false", "-c", "features.plugins=false", "-c", "features.external_agent_memory_import=false", "-c", "features.memories=false", "-c", "features.goals=false", "-c", "features.image_generation=false", "-c", "features.multi_agent_v2=false", "-c", "features.current_time_reminder=false", "-c", "agents.enabled=false", "-c", "orchestrator.skills.enabled=false", "-c", "skills.bundled.enabled=false", "-c", "orchestrator.mcp.enabled=false", "-c", "tools.update_plan.enabled=false", "-c", "tools.experimental_request_user_input.enabled=false", "app-server", "--listen", "stdio://", "--strict-config",
}

func appTestSpec(t *testing.T, mode string) (runSpec, string) {
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
	command := script(t, "exec "+shellQuote(exe)+" -test.run='^TestAppServerChild$' -- 546-app-server "+shellQuote(mode)+" "+shellQuote(dir)+" \"$@\"")
	return runSpec{adapter: codexAdapter, command: command, sha256: sha256File(t, command), stdin: "Reply OK.", timeout: 5 * time.Second, outputCap: 1048576, waitDelay: 150 * time.Millisecond}, dir
}

func TestAppServerOperationAndHostFailure(t *testing.T) {
	for _, mode := range []string{"success", "late-action", "late-error", "late-truncated", "early-eof", "missing-lf", "nonzero", "approval"} {
		t.Run(mode, func(t *testing.T) {
			spec, dir := appTestSpec(t, mode)
			out, facts, err := runCodexAppServer(context.Background(), spec, "gpt-6-astra")
			if mode == "success" {
				if err != nil || facts.Answer != "OK.\nReady�" || out.WaitErrorKind != "none" {
					t.Fatalf("success = %+v, %+v, %v", out, facts, err)
				}
			} else if err == nil || facts != (appServerResult{}) {
				t.Fatalf("%s admitted: %+v, %+v, %v", mode, out, facts, err)
			}
			if len(out.Stdout) != 0 {
				t.Fatal("raw RPC bytes escaped private operation")
			}
			assertDuplexCleanup(t, out, dir)
			if mode == "nonzero" && out.ExitCode != 7 {
				t.Fatalf("host status lost: %+v", out)
			}
		})
	}
}

func TestAppServerChild(t *testing.T) {
	marker := -1
	for i, a := range os.Args {
		if a == "546-app-server" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	mode, dir := os.Args[marker+1], os.Args[marker+2]
	if !reflect.DeepEqual(os.Args[marker+3:], appTestArgs) {
		syscall.Exit(61)
	}
	cwd, err := os.Getwd()
	if err != nil {
		syscall.Exit(62)
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1048576)
	read := func(want string) {
		if !scanner.Scan() {
			syscall.Exit(63)
		}
		var gotValue, wantValue any
		if json.Unmarshal(scanner.Bytes(), &gotValue) != nil || json.Unmarshal([]byte(want), &wantValue) != nil || !reflect.DeepEqual(gotValue, wantValue) {
			syscall.Exit(64)
		}
	}
	emittedBytes := len(appTestInitReply) + 1
	emit := func(value string) {
		value = strings.ReplaceAll(value, "/synthetic/consult", cwd)
		emittedBytes += len(value) + 1
		if _, err := io.WriteString(os.Stdout, value+"\n"); err != nil {
			syscall.Exit(65)
		}
	}
	read(appTestInitialize)
	if mode == "early-eof" {
		syscall.Exit(0)
	}
	if mode == "missing-lf" {
		_, _ = io.WriteString(os.Stdout, appTestInitReply)
		syscall.Exit(0)
	}
	// One source-valid read may contain initialize response and remote status.
	if mode == "remote-race" {
		_, _ = io.WriteString(os.Stdout, appTestInitReply+"\n"+appTestRemote+"\n")
		emittedBytes += len(appTestRemote) + 1
	} else {
		// Fragment a real protocol record, including the final LF, into byte writes.
		for _, b := range []byte(appTestInitReply + "\n") {
			if _, err := os.Stdout.Write([]byte{b}); err != nil {
				syscall.Exit(65)
			}
		}
	}
	read(`{"method":"initialized"}`)
	if mode != "remote-race" {
		emit(appTestRemote)
	}
	read(`{"id":2,"method":"model/list","params":{"includeHidden":true,"limit":100}}`)
	emit(appTestRecords()[2])
	read(`{"id":3,"method":"thread/start","params":{"model":"gpt-6-astra","cwd":` + strconvQuote(cwd) + `,"approvalPolicy":"never","approvalsReviewer":"user","sandbox":"read-only","ephemeral":true}}`)
	emit(appTestThreadReply)
	emit(appTestThreadStarted)
	read(`{"id":4,"method":"turn/start","params":{"threadId":"thread-one","input":[{"type":"text","text":"Reply OK.","text_elements":[]}]}}`)
	if mode == "cancel-before" {
		emit(`{"method":"skills/changed","params":{}}`)
		if scanner.Scan() {
			_ = os.WriteFile(filepath.Join(dir, "unexpected-interrupt"), scanner.Bytes(), 0600)
		}
		syscall.Exit(0)
	}
	emit(appTestTurnStarted)
	if strings.HasPrefix(mode, "cancel-") {
		read(`{"id":5,"method":"turn/interrupt","params":{"threadId":"thread-one","turnId":"turn-one"}}`)
		_ = os.WriteFile(filepath.Join(dir, "interrupt"), []byte("received"), 0600)
		if mode == "cancel-ack" {
			emit(`{"id":5,"result":{}}`)
		}
		if scanner.Scan() {
			syscall.Exit(66)
		}
		if mode == "cancel-no-ack" {
			time.Sleep(30 * time.Second)
		}
		syscall.Exit(0)
	}
	if mode == "approval" {
		emit(`{"id":4,"method":"item/fileChange/requestApproval","params":{"threadId":"thread-one","turnId":"turn-one","itemId":"request","startedAtMs":1}}`)
		read(`{"id":4,"result":{"decision":"cancel"}}`)
	} else {
		emit(appTestCompleted)
		emit(appTestTerminal)
		emit(appTestTurnReply)
	}
	if scanner.Scan() || scanner.Err() != nil {
		syscall.Exit(66)
	}
	_ = os.WriteFile(filepath.Join(dir, "closed"), []byte("true"), 0600)
	switch mode {
	case "records-exact", "records-over":
		count := 4087
		if mode == "records-over" {
			count++
		}
		for range count {
			emit(`{"method":"skills/changed","params":{}}`)
		}
	case "bytes-exact", "bytes-over":
		limit := 1048576
		if mode == "bytes-over" {
			limit++
		}
		prefix := `{"method":"thread/name/updated","params":{"threadId":"thread-one","threadName":"`
		suffix := `"}}`
		emit(prefix + strings.Repeat("x", limit-emittedBytes-len(prefix)-len(suffix)-1) + suffix)
	case "stderr-over":
		_, _ = io.WriteString(os.Stderr, strings.Repeat("x", 1048577))
	case "late-action":
		emit(`{"method":"item/started","params":{"threadId":"thread-one","turnId":"turn-one","startedAtMs":5,"item":{"type":"commandExecution","id":"forbidden"}}}`)
	case "late-error":
		emit(`{"method":"error","params":{"message":"synthetic error"}}`)
	case "late-truncated":
		_, _ = io.WriteString(os.Stdout, `{"method":`)
	case "nonzero":
		syscall.Exit(7)
	}
	syscall.Exit(0)
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestAppServerOperationCannotLaunchUnsupportedModel(t *testing.T) {
	spec, _ := appTestSpec(t, "success")
	out, facts, err := runCodexAppServer(context.Background(), spec, "unsupported")
	if err == nil || out.WaitErrorKind != "" || facts != (appServerResult{}) {
		t.Fatalf("unsupported model launched: %+v, %+v, %v", out, facts, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("unexpected classification")
	}
}

func TestAppServerOperationTransportBounds(t *testing.T) {
	for _, mode := range []string{"records-exact", "records-over", "bytes-exact", "bytes-over", "stderr-over", "lower-cap"} {
		t.Run(mode, func(t *testing.T) {
			childMode := mode
			if mode == "lower-cap" {
				childMode = "success"
			}
			spec, dir := appTestSpec(t, childMode)
			if mode == "lower-cap" {
				spec.outputCap = 1000
			}
			out, facts, err := runCodexAppServer(context.Background(), spec, "gpt-6-astra")
			if mode == "records-exact" || mode == "bytes-exact" {
				if err != nil || facts.Answer != "OK.\nReady�" {
					t.Fatalf("exact bound = %+v, %+v,%v", out, facts, err)
				}
			} else {
				if err == nil || facts != (appServerResult{}) {
					t.Fatalf("excess admitted = %+v, %+v, %v", out, facts, err)
				}
				if mode == "records-over" {
					if !errors.Is(err, errDuplexRecords) {
						t.Fatalf("record error = %v", err)
					}
				} else if !out.CapExceeded {
					t.Fatalf("byte cap fact missing: %+v", out)
				}
			}
			if out.Stdout != nil {
				t.Fatal("raw bytes escaped")
			}
			assertDuplexCleanup(t, out, dir)
		})
	}
}

func TestAppServerCancellationWithDuplex(t *testing.T) {
	for _, mode := range []string{"cancel-before", "cancel-ack", "cancel-no-ack"} {
		t.Run(mode, func(t *testing.T) {
			spec, dir := appTestSpec(t, mode)
			spec.args = codexAppServerArgs()
			ctx, cancel := context.WithCancel(context.Background())
			s := &appServerStream{model: "gpt-6-astra", prompt: spec.stdin}
			ready := make(chan struct{}, 1)
			type result struct {
				out runOutcome
				err error
			}
			done := make(chan result, 1)
			go func() {
				out, err := runDuplex(ctx, spec, duplexExchange{start: s.start, interrupt: s.interrupt, receive: func(frame []byte) (duplexAction, error) {
					a, err := s.receive(frame)
					want := appTestTurnStarted
					if mode == "cancel-before" {
						want = `{"method":"skills/changed","params":{}}`
					}
					if err == nil && string(frame) == want {
						ready <- struct{}{}
					}
					return a, err
				}})
				done <- result{out, err}
			}()
			joined := false
			t.Cleanup(func() {
				cancel()
				if !joined {
					<-done
				}
			})
			select {
			case <-ready:
			case r := <-done:
				joined = true
				t.Fatalf("child exited before cancellation: %+v,%v", r.out, r.err)
			case <-time.After(3 * time.Second):
				t.Fatal("child did not reach cancellation point")
			}
			started := time.Now()
			cancel()
			r := <-done
			joined = true
			if !r.out.Canceled || time.Since(started) > 2*time.Second {
				t.Fatalf("cancel failed/bound exceeded: %+v,%v", r.out, r.err)
			}
			assertDuplexCleanup(t, r.out, dir)
			if facts, err := s.finish(); err == nil || facts != (appServerResult{}) {
				t.Fatalf("cancellation admitted: %+v,%v", facts, err)
			}
			if mode == "cancel-before" {
				if _, err := os.Stat(filepath.Join(dir, "unexpected-interrupt")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("startup interruption used an unconfirmed ID: %v", err)
				}
			} else if got, err := os.ReadFile(filepath.Join(dir, "interrupt")); err != nil || string(got) != "received" {
				t.Fatalf("confirmed interrupt was not delivered: %q, %v", got, err)
			}
		})
	}
}

func TestAppServerCoalescedInitialize(t *testing.T) {
	spec, dir := appTestSpec(t, "remote-race")
	out, facts, err := runCodexAppServer(context.Background(), spec, "gpt-6-astra")
	if err != nil || facts.Answer != "OK.\nReady�" {
		t.Fatalf("coalesced initialization = %+v, %+v, %v", out, facts, err)
	}
	assertDuplexCleanup(t, out, dir)
}
