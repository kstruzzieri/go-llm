package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestCompactValidationAndHelp(t *testing.T) {
	for _, tt := range []struct {
		name, command string
		sess          *replSession
		want          string
	}{
		{"arguments", "/compact extra", &replSession{}, "usage: /compact\n"},
		{"session disabled", "/compact", &replSession{}, "session disabled (--no-session)\n"},
		{"runtime absent", "/compact", &replSession{session: &session{id: "user:test"}}, "compact: runtime unavailable\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			forced, exit := dispatchSlash(context.Background(), &out, tt.sess, tt.command)
			if out.String() != tt.want || forced != "" || exit {
				t.Fatalf("dispatchSlash(%q) = %q, %q, %v; want %q, empty, false", tt.command, out.String(), forced, exit, tt.want)
			}
		})
	}
	if !strings.Contains(golemHelp, "  /compact       compact the active session's history\n") {
		t.Fatalf("help missing /compact: %s", golemHelp)
	}
}

func newCompactSession(t *testing.T, exchanges int, summarize conversation.Summarizer) (*replSession, *thinkTurnCaller) {
	t.Helper()
	root := t.TempDir()
	caller := &thinkTurnCaller{captureCaller: captureCaller{answer: "answer"}}
	sess := newSessionedTestSession(t, caller, root, "user:compact")
	sess.root = root
	current := conversation.Conversation{ID: sess.session.id, Title: "Original title"}
	for i := range exchanges {
		current.Messages = append(current.Messages,
			conversation.Message{Role: "user", Content: strings.Repeat(string(rune('a'+i)), 40)},
			conversation.Message{Role: "assistant", Content: strings.Repeat(string(rune('A'+i)), 40)})
	}
	if err := sess.session.store.Save(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.session.switchTo(context.Background(), current.ID); err != nil {
		t.Fatal(err)
	}
	sess.session.store = &compactTestStore{Store: sess.session.store}
	installCompactRuntime(t, sess, golemruntime.Options{Summarizer: summarize})
	return sess, caller
}

func installCompactRuntime(t *testing.T, sess *replSession, opts golemruntime.Options) {
	t.Helper()
	if err := sess.runtime.Close(); err != nil {
		t.Fatal(err)
	}
	opts.Root, opts.System, opts.Orchestrator = sess.root, sess.baseSystem, sess.orch
	if opts.SessionStore == nil {
		opts.SessionStore = sess.session.store
	}
	runtime, err := golemruntime.New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	sess.runtime = runtime
	t.Cleanup(func() { _ = runtime.Close() })
}

func TestCompactReportAndRefresh(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exchanges int
		want      string
		calls     int
	}{
		{"changed", 5, "compact: history token estimate 100 -> 121 (changed)\n", 1},
		{"floor", 4, "compact: history token estimate 80 -> 80 (unchanged)\n", 0},
		{"empty", 0, "compact: history token estimate 0 -> 0 (unchanged)\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			sess, _ := newCompactSession(t, tc.exchanges, func(_ context.Context, prior string, old []conversation.Message) (string, error) {
				calls++
				want := []conversation.Message{{Role: "user", Content: strings.Repeat("a", 40)}, {Role: "assistant", Content: strings.Repeat("A", 40)}}
				if prior != "" || !reflect.DeepEqual(old, want) {
					t.Errorf("summary inputs = %q, %+v; want empty prior, oldest pair", prior, old)
				}
				return "SUM", nil
			})
			var out strings.Builder
			_, _ = dispatchSlash(context.Background(), &out, sess, "/compact")
			if out.String() != tc.want || calls != tc.calls {
				t.Fatalf("/compact = %q, calls %d; want %q, %d", out.String(), calls, tc.want, tc.calls)
			}
			if saves := sess.session.store.(*compactTestStore).saves; saves != tc.calls {
				t.Errorf("saves=%d, want %d", saves, tc.calls)
			}
			stored, err := sess.session.store.Load(context.Background(), sess.session.id)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sess.session.msgs, stored.Messages) || !reflect.DeepEqual(sess.session.summary, stored.DurableSummary) {
				t.Fatalf("cache differs from committed snapshot: %+v / %+v", sess.session, stored)
			}
			if sess.session.id != "user:compact" || stored.Title != "Original title" {
				t.Fatalf("identity/title changed: %+v", stored)
			}
		})
	}
}

func TestInterruptContextCleanup(t *testing.T) {
	for _, nonnil := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil", true: "channel"}[nonnil], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, parentCancel := context.WithCancel(context.Background())
				defer parentCancel()
				var interrupts chan struct{}
				if nonnil {
					interrupts = make(chan struct{}, 1)
					interrupts <- struct{}{}
				}
				child, cleanup := interruptContext(parent, interrupts)
				synctest.Wait()
				if child.Err() != nil {
					t.Fatalf("stale interrupt canceled operation: %v", child.Err())
				}
				cleanup()
				cleanup()
				var wg sync.WaitGroup
				for range 8 {
					wg.Go(cleanup)
				}
				wg.Wait()
				if !errors.Is(child.Err(), context.Canceled) {
					t.Fatalf("cleanup context = %v, want canceled", child.Err())
				}
				if nonnil {
					interrupts <- struct{}{}
					synctest.Wait()
					if len(interrupts) != 1 {
						t.Fatal("retired watcher consumed next interrupt")
					}
				}
				next, nextCleanup := interruptContext(parent, interrupts)
				defer nextCleanup()
				parentCancel()
				synctest.Wait()
				if !errors.Is(next.Err(), context.Canceled) {
					t.Fatalf("parent cancellation = %v", next.Err())
				}
			})
		})
	}
}

func TestCompactDisabledSkipsWork(t *testing.T) {
	for _, mode := range []string{"startup flag", "runtime disabled", "missing summarizer"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			summarize := func(context.Context, string, []conversation.Message) (string, error) { calls++; return "SUM", nil }
			sess, _ := newCompactSession(t, 5, summarize)
			prompt := &fakePrompt{answer: true}
			sess.destAdmission, _ = newTestAdmission(t, admEdges(t), nil, true, prompt)
			switch mode {
			case "startup flag":
				sess.noCompress = true
			case "runtime disabled":
				installCompactRuntime(t, sess, golemruntime.Options{Summarizer: summarize, DisableCompression: true})
				sess.destAdmission = nil
			case "missing summarizer":
				installCompactRuntime(t, sess, golemruntime.Options{})
				sess.destAdmission = nil
			}
			var out strings.Builder
			_, _ = dispatchSlash(context.Background(), &out, sess, "/compact")
			if saves := sess.session.store.(*compactTestStore).saves; saves != 0 {
				t.Errorf("disabled compact saved %d times", saves)
			}
			if out.String() != "compact: compression unavailable\n" || calls != 0 || prompt.calls != 0 {
				t.Fatalf("disabled compact = %q, model %d, admission %d", out.String(), calls, prompt.calls)
			}
		})
	}
}

func TestCompactStartupWiresDisableFlag(t *testing.T) {
	configPath, root := writeRunLifecycleConfig(t)
	stdin, stdout, stderr := runTestFiles(t)
	stop := errors.New("stop after compact wiring")
	err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-compress", "-no-memory", "-no-rag", "-no-project-context", "-no-auto-index"}, stdin, stdout, stderr, runHooks{
		startAutoIndex: func() func() { return func() {} },
		afterSessionReady: func(sess *replSession) error {
			if !sess.noCompress {
				t.Error("startup did not retain -no-compress")
			}
			var out strings.Builder
			_, _ = dispatchSlash(context.Background(), &out, sess, "/compact")
			if out.String() != "compact: compression unavailable\n" {
				t.Errorf("startup /compact = %q", out.String())
			}
			return stop
		}})
	if !errors.Is(err, stop) {
		t.Fatalf("run = %v; stderr=%s", err, readRunTestFile(t, stderr))
	}
}

// compactTestStore wraps the fixture SQLite store, retaining its real transaction
// while allowing deterministic failure/commit barriers at the public store seam.
type compactTestStore struct {
	conversation.Store
	saves int
	save  func(context.Context, conversation.Conversation) error
	load  func(context.Context, string) (*conversation.Conversation, error)
}

func (s *compactTestStore) Save(ctx context.Context, c conversation.Conversation) error {
	s.saves++
	if s.save != nil {
		return s.save(ctx, c)
	}
	return s.Store.Save(ctx, c)
}
func (s *compactTestStore) Load(ctx context.Context, id string) (*conversation.Conversation, error) {
	if s.load != nil {
		return s.load(ctx, id)
	}
	return s.Store.Load(ctx, id)
}

func TestCompactConsentAfterGrantsClear(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer bool
		err    error
		want   string
		calls  int
	}{
		{"yes", true, nil, "compact: history token estimate 100 -> 121 (changed)\n", 1},
		{"no", false, nil, "compact failed: provider: destination opencode/https://opencode.ai/zen/go not admitted for agent\n", 0},
		{"canceled", false, context.Canceled, "compact: canceled; session unchanged\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := &fakePrompt{answer: true}
			adm, _ := newTestAdmission(t, admEdges(t), nil, true, p)
			if err := adm.ensure(context.Background()); err != nil {
				t.Fatal(err)
			}
			summarize := func(context.Context, string, []conversation.Message) (string, error) {
				calls++
				if p.calls != 2 || !adm.admitted {
					t.Errorf("summary before reapproval: prompts=%d admitted=%v", p.calls, adm.admitted)
				}
				return "SUM", nil
			}
			sess, _ := newCompactSession(t, 5, summarize)
			store := &compactTestStore{Store: sess.session.store}
			installCompactRuntime(t, sess, golemruntime.Options{SessionStore: store, Summarizer: summarize})
			sess.destAdmission = adm
			var out strings.Builder
			_, _ = dispatchSlash(context.Background(), &out, sess, "/grants clear")
			out.Reset()
			p.answer, p.err = tc.answer, tc.err
			_, _ = dispatchSlash(context.Background(), &out, sess, "/compact")
			if got := out.String(); got != tc.want {
				t.Errorf("compact %s = %q, want %q", tc.name, got, tc.want)
			}
			if p.calls != 2 || calls != tc.calls || store.saves != tc.calls {
				t.Errorf("compact %s prompts/model/saves=%d/%d/%d; want 2/%d/%d", tc.name, p.calls, calls, store.saves, tc.calls, tc.calls)
			}
		})
	}
}

func TestCompactFailureAndRefreshWarning(t *testing.T) {
	blocked := &agent.BlockedError{Findings: []agent.Finding{{Interceptor: "secrets", Verdict: agent.VerdictBlock, Detail: "private summary text"}}}
	for _, tc := range []struct {
		name    string
		refresh bool
		err     error
		want    string
	}{
		{"summary failure", false, blocked, "compact failed: run blocked: sensitive content detected\n"},
		{"refresh failure", true, blocked, "compact: history token estimate 100 -> 121 (changed)\nwarning: session state not refreshed: run blocked: sensitive content detected\n"},
		{"parent canceled after commit", true, context.Canceled, "compact: history token estimate 100 -> 121 (changed)\nwarning: session state not refreshed: context canceled\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sess, _ := newCompactSession(t, 5, func(context.Context, string, []conversation.Message) (string, error) {
				if !tc.refresh {
					return "", tc.err
				}
				return "SUM", nil
			})
			before := append([]conversation.Message(nil), sess.session.msgs...)
			backing := sess.session.store
			if tc.refresh {
				sess.session.store = &compactTestStore{Store: backing, load: func(ctx context.Context, _ string) (*conversation.Conversation, error) {
					if tc.err == context.Canceled {
						cancel()
						return nil, ctx.Err()
					}
					return nil, tc.err
				}}
			}
			var out strings.Builder
			_, _ = dispatchSlash(ctx, &out, sess, "/compact")
			if got := out.String(); got != tc.want {
				t.Errorf("compact %s = %q, want %q", tc.name, got, tc.want)
			}
			saved, err := backing.Load(context.Background(), sess.session.id)
			if err != nil {
				t.Fatal(err)
			}
			want := before
			if tc.refresh {
				want = before[2:]
			}
			if !reflect.DeepEqual(saved.Messages, want) || !reflect.DeepEqual(sess.session.msgs, before) {
				t.Errorf("failure snapshot/cache wrong: saved=%+v cache=%+v", saved, sess.session.msgs)
			}
		})
	}
}

func TestCompactREPLInterruptDuringSummary(t *testing.T) {
	started := make(chan struct{})
	sess, caller := newCompactSession(t, 5, func(ctx context.Context, _ string, _ []conversation.Message) (string, error) {
		close(started)
		<-ctx.Done()
		return "SUM", nil
	})
	before := append([]conversation.Message(nil), sess.session.msgs...)
	interrupts := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var out strings.Builder
	src := &recordingSource{scannerSource: newScannerSource(strings.NewReader("/compact\nnext goal\n"), &out)}
	caller.before = func(ctx context.Context) {
		if ctx.Err() != nil {
			t.Errorf("next goal context = %v, want live", ctx.Err())
		}
	}
	go func() { done <- runREPL(ctx, src, &out, interrupts, sess) }()
	waitThink(t, started)
	interrupts <- struct{}{}
	if err := waitThink(t, done); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatalf("REPL timed out: %v", ctx.Err())
	}
	if !strings.Contains(out.String(), "compact: canceled; session unchanged\n") || strings.Contains(out.String(), "compact failed") || strings.Contains(out.String(), "(changed)") {
		t.Fatalf("interrupt output = %q", out.String())
	}
	if len(caller.requests) != 1 || !reflect.DeepEqual(src.recorded, []string{"next goal"}) {
		t.Fatalf("next goal calls/history = %d/%q", len(caller.requests), src.recorded)
	}
	saved, err := sess.session.store.Load(context.Background(), sess.session.id)
	if err != nil {
		t.Fatal(err)
	}
	want := append(before, conversation.Message{Role: "user", Content: "next goal"}, conversation.Message{Role: "assistant", Content: "answer"})
	if !reflect.DeepEqual(saved.Messages, want) || saved.DurableSummary != nil {
		t.Fatalf("cancellation changed old history: %+v", saved)
	}
}

func TestCompactREPLCancellationAfterCommit(t *testing.T) {
	summarize := func(context.Context, string, []conversation.Message) (string, error) { return "SUM", nil }
	sess, caller := newCompactSession(t, 5, summarize)
	original := append([]conversation.Message(nil), sess.session.msgs...)
	backing := sess.session.store
	committed := make(chan struct{})
	store := &compactTestStore{Store: backing}
	store.save = func(ctx context.Context, c conversation.Conversation) error {
		if err := backing.Save(ctx, c); err != nil {
			return err
		}
		if store.saves == 1 {
			close(committed)
			<-ctx.Done()
		}
		return nil
	}
	installCompactRuntime(t, sess, golemruntime.Options{SessionStore: store, Summarizer: summarize})
	interrupts := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var out strings.Builder
	src := &recordingSource{scannerSource: newScannerSource(strings.NewReader("/compact\nnext goal\n"), &out)}
	caller.before = func(ctx context.Context) {
		if ctx.Err() != nil {
			t.Errorf("next goal context = %v", ctx.Err())
		}
		if sess.session.historySummary() != "SUM" || !reflect.DeepEqual(sess.session.msgs, original[2:]) {
			t.Errorf("cache after committed Ctrl-C = %q/%+v", sess.session.historySummary(), sess.session.msgs)
		}
	}
	go func() { done <- runREPL(ctx, src, &out, interrupts, sess) }()
	waitThink(t, committed)
	interrupts <- struct{}{}
	if err := waitThink(t, done); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatalf("parent canceled: %v", ctx.Err())
	}
	if !strings.Contains(out.String(), "compact: history token estimate 100 -> 121 (changed)\n") || strings.Contains(out.String(), "canceled") || strings.Contains(out.String(), "failed") || strings.Contains(out.String(), "not refreshed") {
		t.Fatalf("post-commit output = %q", out.String())
	}
	if len(caller.requests) != 1 || !reflect.DeepEqual(src.recorded, []string{"next goal"}) || sess.session.id != "user:compact" {
		t.Fatalf("next prompt/identity failed: calls=%d recorded=%q id=%s", len(caller.requests), src.recorded, sess.session.id)
	}
}

func TestCompactREPLHistoryResumeAndSettings(t *testing.T) {
	calls := 0
	sess, caller := newCompactSession(t, 5, func(context.Context, string, []conversation.Message) (string, error) { calls++; return "SUM", nil })
	original := append([]conversation.Message(nil), sess.session.msgs...)
	sess.grants = newApprovalGrants()
	sess.grants.grant(grantScopeExec, "exec:test")
	options := provider.ModelOptions{Think: provider.Ptr(true), ThinkEffort: "high", NumPredict: 99}
	if err := sess.runtime.Replace(sess.baseSystem, nil, options); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	src := &recordingSource{scannerSource: newScannerSource(strings.NewReader("/compact\n/compact\nnext goal\n/resume user:compact\n"), &out)}
	caller.before = func(context.Context) {
		if sess.grants.count() != 1 {
			t.Errorf("compact cleared grants: %d", sess.grants.count())
		}
	}
	if err := runREPL(context.Background(), src, &out, nil, sess); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(caller.requests) != 1 || !reflect.DeepEqual(src.recorded, []string{"next goal"}) {
		t.Fatalf("calls = summary %d model %d history %q", calls, len(caller.requests), src.recorded)
	}
	if !strings.Contains(out.String(), "compact: history token estimate 100 -> 121 (changed)\n") || !strings.Contains(out.String(), "compact: history token estimate 121 -> 121 (unchanged)\n") {
		t.Fatalf("reports = %q", out.String())
	}
	want := []provider.ChatMessage{{Role: "system", Content: sess.baseSystem + "\n\n" + agent.ToolTrustContract}, {Role: "system", Content: agent.DurableSummaryPrompt("SUM")}}
	for _, m := range original[2:] {
		want = append(want, provider.ChatMessage{Role: m.Role, Content: m.Content})
	}
	want = append(want, provider.ChatMessage{Role: "user", Content: "next goal"})
	if !reflect.DeepEqual(caller.requests[0].Messages, want) {
		t.Errorf("next request = %+v, want %+v", caller.requests[0].Messages, want)
	}
	if !reflect.DeepEqual(sess.runtime.ModelOptions(), options) {
		t.Errorf("options changed: %+v", sess.runtime.ModelOptions())
	}
	saved, err := sess.session.store.Load(context.Background(), sess.session.id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.session.id != "user:compact" || sess.session.historySummary() != "SUM" || !reflect.DeepEqual(sess.session.msgs, saved.Messages) {
		t.Errorf("resume cache = %+v, stored %+v", sess.session, saved)
	}
}

func TestCompactEditedTextIsForcedGoal(t *testing.T) {
	sess, caller := newCompactSession(t, 0, nil)
	sess.goalEditor = &fakeGoalEditor{available: true, text: "/compact"}
	var out strings.Builder
	src := &recordingSource{scannerSource: newScannerSource(strings.NewReader("/edit\n"), &out)}
	if err := runREPL(context.Background(), src, &out, nil, sess); err != nil {
		t.Fatal(err)
	}
	if len(caller.requests) != 1 || !reflect.DeepEqual(src.recorded, []string{"/compact"}) {
		t.Fatalf("edited goal = %d calls, %q history", len(caller.requests), src.recorded)
	}
	msgs := caller.requests[0].Messages
	if msgs[len(msgs)-1].Content != "/compact" || strings.Contains(out.String(), "compact:") {
		t.Fatalf("edit incorrectly dispatched: %+v / %s", msgs, out.String())
	}
}

func TestCompactEditorConsentInterrupt(t *testing.T) {
	calls := 0
	sess, _ := newCompactSession(t, 5, func(context.Context, string, []conversation.Message) (string, error) { calls++; return "SUM", nil })
	g := newGatedReader()
	g.entered = make(chan struct{}, 2)
	f := newEditorFixture(t, editorOpts{in: g})
	defer close(g.chunks)
	p := &fakePrompt{answer: true}
	adm, _ := newTestAdmission(t, admEdges(t), nil, true, p)
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess.destAdmission = adm
	_, _ = dispatchSlash(context.Background(), io.Discard, sess, "/grants clear")
	adm.promptYN = lineSourcePromptYN(f.src)
	done := make(chan string, 1)
	go func() {
		var out strings.Builder
		_, _ = dispatchSlash(context.Background(), &out, sess, "/compact")
		done <- out.String()
	}()
	waitThink(t, g.entered)
	g.chunks <- []byte{'\x03'}
	select {
	case out := <-done:
		if out != "compact: canceled; session unchanged\n" || calls != 0 || sess.session.store.(*compactTestStore).saves != 0 {
			t.Fatalf("editor interrupt=%q model=%d saves=%d", out, calls, sess.session.store.(*compactTestStore).saves)
		}
	case <-g.entered:
		t.Fatal("consent resumed terminal read after Ctrl-C")
	case <-time.After(5 * time.Second):
		t.Fatal("consent did not return after Ctrl-C")
	}
}

func TestCompactStaleInterruptAndREPLEntryReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		sess, _ := newCompactSession(t, 5, func(ctx context.Context, _ string, _ []conversation.Message) (string, error) {
			calls++
			synctest.Wait()
			if ctx.Err() != nil {
				t.Errorf("stale interrupt canceled compact: %v", ctx.Err())
			}
			return "SUM", nil
		})
		interrupts := make(chan struct{}, 1)
		interrupts <- struct{}{}
		var out strings.Builder
		if err := runREPL(context.Background(), newScannerSource(strings.NewReader("/compact\n"), &out), &out, interrupts, sess); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || !strings.Contains(out.String(), "100 -> 121 (changed)\n") {
			t.Fatalf("stale interrupt compact = %q, calls=%d", out.String(), calls)
		}
		if sess.interrupts != interrupts {
			t.Fatal("REPL did not retain its interrupt channel")
		}
		if err := runREPL(context.Background(), newScannerSource(strings.NewReader(""), &out), &out, nil, sess); err != nil {
			t.Fatal(err)
		}
		if sess.interrupts != nil {
			t.Fatal("REPL entry did not clear the previous interrupt channel")
		}
	})
}

func TestCompactSaveFailurePreservesHistory(t *testing.T) {
	summarize := func(context.Context, string, []conversation.Message) (string, error) { return "SUM", nil }
	sess, _ := newCompactSession(t, 5, summarize)
	before, err := sess.session.store.Load(context.Background(), sess.session.id)
	if err != nil {
		t.Fatal(err)
	}
	store := &compactTestStore{Store: sess.session.store, save: func(context.Context, conversation.Conversation) error { return errors.New("disk full") }}
	installCompactRuntime(t, sess, golemruntime.Options{SessionStore: store, Summarizer: summarize})
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/compact")
	if out.String() != "compact failed: golem: session persistence failed: compact thread \"user:compact\": disk full\n" {
		t.Errorf("save failure = %q", out.String())
	}
	after, err := sess.session.store.Load(context.Background(), sess.session.id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(sess.session.msgs, before.Messages) || store.saves != 1 {
		t.Fatalf("failed save changed history: before=%+v after=%+v cache=%+v saves=%d", before, after, sess.session.msgs, store.saves)
	}
}

func TestInterruptContextCleanupJoinsBeforeNextInterrupt(t *testing.T) {
	// Keep a newly created watcher pending until cleanup must join it. Repeating
	// covers both select outcomes if a broken cleanup returns without joining:
	// its late watcher can then choose the fresh interrupt over cancellation.
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for i := range 64 {
		interrupts := make(chan struct{}, 1)
		_, cleanup := interruptContext(context.Background(), interrupts)
		cleanup()
		interrupts <- struct{}{}
		runtime.Gosched()
		if len(interrupts) != 1 {
			t.Fatalf("cleanup iteration %d: retired watcher consumed the next operation's interrupt", i)
		}
	}
}

func TestCompactNoopDoesNotReload(t *testing.T) {
	sess, _ := newCompactSession(t, 4, func(context.Context, string, []conversation.Message) (string, error) {
		t.Error("no-op called summarizer")
		return "SUM", nil
	})
	sess.session.store = &compactTestStore{Store: sess.session.store, load: func(context.Context, string) (*conversation.Conversation, error) {
		t.Error("no-op reloaded CLI cache")
		return nil, errors.New("unexpected reload")
	}}
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/compact")
	if out.String() != "compact: history token estimate 80 -> 80 (unchanged)\n" {
		t.Fatalf("no-op output = %q", out.String())
	}
}
