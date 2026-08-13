package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/rag"
)

func TestParseFlagsAllowWrite(t *testing.T) {
	f, err := parseFlags([]string{"-allow-write"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.allowWrite {
		t.Fatal("-allow-write should set allowWrite")
	}
	f2, _ := parseFlags(nil)
	if f2.allowWrite {
		t.Fatal("allowWrite must default to false")
	}
}

func TestColorPermittedTruthTable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		noColor bool
		env     map[string]string
		want    bool
	}{
		{name: "default", env: map[string]string{"NO_COLOR": "", "TERM": "xterm-256color"}, want: true},
		{name: "no-color flag", noColor: true, env: map[string]string{"NO_COLOR": "", "TERM": "xterm-256color"}, want: false},
		{name: "NO_COLOR env", env: map[string]string{"NO_COLOR": "1", "TERM": "xterm-256color"}, want: false},
		{name: "TERM dumb", env: map[string]string{"NO_COLOR": "", "TERM": "dumb"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := colorPermitted(tc.noColor); got != tc.want {
				t.Fatalf("colorPermitted(%v) = %v, want %v", tc.noColor, got, tc.want)
			}
		})
	}
}

func TestColorEnabledHonorsTerminalAndEnvironment(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	if colorEnabled(null, false) {
		t.Fatal("character device without a terminal must disable ANSI")
	}

	if runtime.GOOS != "windows" {
		t.Run("pty", func(t *testing.T) {
			terminal := openTestTerminal(t)

			if !colorEnabled(terminal, false) {
				t.Fatal("PTY should enable ANSI by default")
			}
			if colorEnabled(terminal, true) {
				t.Fatal("-no-color must disable ANSI")
			}
			t.Setenv("NO_COLOR", "1")
			if colorEnabled(terminal, false) {
				t.Fatal("NO_COLOR must disable ANSI")
			}
			t.Setenv("NO_COLOR", "")
			t.Setenv("TERM", "dumb")
			if colorEnabled(terminal, false) {
				t.Fatal("TERM=dumb must disable ANSI")
			}
		})
	}

	regular, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = regular.Close() }()
	t.Setenv("TERM", "xterm-256color")
	if colorEnabled(regular, false) {
		t.Fatal("non-terminal output must disable ANSI")
	}
}

// openTestTerminal opens the controlling terminal, skipping in non-interactive
// runs. Legacy /dev/pty?? nodes are dead on modern Darwin (open returns
// EAGAIN), so only interactive runs exercise the TTY-true branch; the flag and
// environment gates are pinned terminal-free by TestColorPermittedTruthTable.
func openTestTerminal(t *testing.T) *os.File {
	t.Helper()
	terminal, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("open terminal: %v", err)
	}
	t.Cleanup(func() { _ = terminal.Close() })
	return terminal
}

func TestStartupNotices(t *testing.T) {
	got := startupNotices(startupInfo{
		workspace:       "/abs/root",
		useRecommend:    true,
		bootstrapWarns:  []error{errors.New("provider lab: refresh models failed")},
		preflightWarns:  []string{`agent fallback "ollama/m1" is not tool-capable (chat|stream|tool_call)`},
		retrieveOmitted: true,
	})
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"workspace: /abs/root",
		"no defaults.agent configured; using model recommendation",
		"warning: provider lab: refresh models failed",
		"ollama/m1",
		"retrieve unavailable: no RAG index configured; using file/search tools",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("notices missing %q in:\n%s", want, joined)
		}
	}
}

func TestStartupNotices_RequestedRetrieveSuppressesGenericLine(t *testing.T) {
	got := startupNotices(startupInfo{
		workspace:         "/r",
		retrieveOmitted:   true,
		retrieveRequested: true,
		preflightWarns:    []string{`retrieve disabled: rag-db "/x.db": no such file or directory`},
	})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "retrieve disabled:") {
		t.Errorf("expected the specific retrieve-disabled reason, got:\n%s", joined)
	}
	if strings.Contains(joined, "no RAG index configured") {
		t.Errorf("generic no-index line must be suppressed when -rag-db was requested, got:\n%s", joined)
	}
}

func TestParseFlags_RejectsPositionalArguments(t *testing.T) {
	// A stray positional stops flag.Parse, silently dropping every later flag
	// (`golem -agentflow-status status -json` would emit human output with
	// machine exit codes), so leftovers are an error rather than ignored.
	if _, err := parseFlags([]string{"-agentflow-status", "status", "-json"}); err == nil || !strings.Contains(err.Error(), "positional") {
		t.Fatalf("parseFlags = %v, want positional-argument rejection", err)
	}
}

func TestParseFlags_Defaults(t *testing.T) {
	f, err := parseFlags([]string{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.root != "." {
		t.Errorf("root = %q, want \".\"", f.root)
	}
}

func TestMainFeedbackConstructionGate(t *testing.T) {
	root := t.TempDir()
	paths, opens := 0, 0
	getenv := func(string) string { paths++; return t.TempDir() }
	opener := func(context.Context, string, string, func(string)) (*feedbackService, error) {
		opens++
		return nil, errors.New("open failed")
	}

	service, warning := openConfiguredFeedback(context.Background(), false, root, "", getenv, func(string) {}, opener)
	if service != nil || warning != "" || paths != 0 || opens != 0 {
		t.Fatalf("flag-off gate: service=%v warning=%q paths=%d opens=%d", service, warning, paths, opens)
	}
	service, warning = openConfiguredFeedback(context.Background(), true, root, filepath.Join(t.TempDir(), "feedback.db"), getenv, func(string) {}, opener)
	if service != nil || opens != 1 || strings.Count(warning, "behavioral feedback disabled") != 1 {
		t.Fatalf("open failure: service=%v warning=%q opens=%d", service, warning, opens)
	}
}

func TestRunFeedbackConstructionGate(t *testing.T) {
	configPath, root := writeRunLifecycleConfig(t)
	baseArgs := []string{"-config", configPath, "-root", root, "-p", "done", "-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-project-context"}
	errStop := errors.New("stop after startup")

	for _, tc := range []struct {
		name         string
		args         []string
		wantOpens    int
		wantWarnings int
		openSucceeds bool
	}{
		{name: "flag off", wantOpens: 0},
		{name: "no rag", args: []string{"-feedback", "-no-rag"}, wantOpens: 0},
		{name: "open failure", args: []string{"-feedback"}, wantOpens: 1, wantWarnings: 1},
		{name: "empty report", args: []string{"-feedback"}, wantOpens: 1, openSucceeds: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdin, stdout, stderr := runTestFiles(t)
			opens := 0
			var svc *feedbackService
			if tc.openSucceeds {
				svc = feedbackServiceForStore(root, &feedbackBlockingStore{}, feedbackTestWarn(t))
			}
			err := run(append(append([]string{}, baseArgs...), tc.args...), stdin, stdout, stderr, runHooks{
				openFeedback: func(context.Context, string, string, func(string)) (*feedbackService, error) {
					opens++
					if svc != nil {
						return svc, nil
					}
					return nil, errors.New("open failed")
				},
				startAutoIndex: func() func() { return func() {} },
				afterAutoIndexStart: func(lineSourceMode, agent.Tool, *feedbackService) error {
					return errStop
				},
			})
			if !errors.Is(err, errStop) {
				t.Fatalf("run error = %v, want test stop", err)
			}
			if opens != tc.wantOpens {
				t.Fatalf("feedback opens = %d, want %d", opens, tc.wantOpens)
			}
			warningText := readRunTestFile(t, stderr)
			if got := strings.Count(warningText, "behavioral feedback disabled"); got != tc.wantWarnings {
				t.Fatalf("feedback warnings = %d, want %d; stderr:\n%s", got, tc.wantWarnings, warningText)
			}
			if strings.Contains(warningText, "behavioral feedback: attempted=") {
				t.Fatalf("empty or disabled feedback printed a shutdown report:\n%s", warningText)
			}
		})
	}
}

func TestRunStopsAutoIndexInEveryDispatchBranch(t *testing.T) {
	configPath, root := writeRunLifecycleConfig(t)
	baseArgs := []string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-project-context", "-no-auto-index"}
	errStop := errors.New("stop after auto-index start")

	for _, tc := range []struct {
		name string
		args []string
		mode lineSourceMode
	}{
		{name: "answer only", args: []string{"-goal", "plan it"}, mode: sourceAnswerOnly},
		{name: "noninteractive", args: []string{"-p", "done"}, mode: sourceNone},
		{name: "repl", mode: sourceREPL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdin, stdout, stderr := runTestFiles(t)
			var events []string
			err := run(append(append([]string{}, baseArgs...), tc.args...), stdin, stdout, stderr, runHooks{
				startAutoIndex: func() func() {
					return func() { events = append(events, "auto-index") }
				},
				afterAutoIndexStart: func(got lineSourceMode, _ agent.Tool, _ *feedbackService) error {
					if got != tc.mode {
						t.Fatalf("dispatch mode = %v, want %v", got, tc.mode)
					}
					return errStop
				},
				closed: func(name string) { events = append(events, name) },
			})
			if !errors.Is(err, errStop) {
				t.Fatalf("run error = %v, want test stop", err)
			}
			if len(events) == 0 || events[0] != "auto-index" {
				t.Fatalf("shutdown events = %v, want branch-local auto-index stop first", events)
			}
			if got := strings.Count(strings.Join(events, ","), "auto-index"); got != 1 {
				t.Fatalf("auto-index stop count = %d, events = %v", got, events)
			}
		})
	}
}

func TestRunRendersCumulativeFeedbackShutdownReport(t *testing.T) {
	configPath, root := writeRunLifecycleConfig(t)
	stdin, stdout, stderr := runTestFiles(t)
	svc := feedbackServiceForStore(root, &feedbackBlockingStore{}, feedbackTestWarn(t))
	svc.report = feedbackReport{
		attempted:              9,
		completed:              4,
		dropped:                5,
		reasons:                map[string]int{dropOverflowNewest: 3, dropOperationFailed: 2},
		presentationDuplicates: 6,
		presentationJoinMisses: 7,
	}
	closeErr := errors.New("close failed")
	svc.closeWriter = func() error { return closeErr }
	errStop := errors.New("stop after startup")

	err := run([]string{"-config", configPath, "-root", root, "-feedback", "-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-project-context", "-no-auto-index"}, stdin, stdout, stderr, runHooks{
		openFeedback: func(context.Context, string, string, func(string)) (*feedbackService, error) {
			return svc, nil
		},
		startAutoIndex: func() func() { return func() {} },
		afterAutoIndexStart: func(lineSourceMode, agent.Tool, *feedbackService) error {
			return errStop
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run error = %v, want test stop", err)
	}
	got := readRunTestFile(t, stderr)
	want := "behavioral feedback: attempted=9 completed=4 dropped=5 drop_reasons=operation-failed:2,overflow-newest:3 duplicates=6 join_misses=7\n"
	if !strings.Contains(got, want) {
		t.Fatalf("stderr missing cumulative report %q:\n%s", want, got)
	}
	if count := strings.Count(got, "behavioral feedback close failed: close failed"); count != 1 {
		t.Fatalf("close error count = %d, want 1; stderr:\n%s", count, got)
	}
}

func TestRunShutdownDrainsBorrowedWeighterBeforeFeedbackClose(t *testing.T) {
	for _, current := range []bool{false, true} {
		name := "ready generation"
		if current {
			name = "current explicit generation"
		}
		t.Run(name, func(t *testing.T) {
			configPath, root := writeRunLifecycleConfig(t)
			stdin, stdout, stderr := runTestFiles(t)
			errStop := errors.New("stop with retrieval active")
			weighter := &runBlockingWeighter{entered: make(chan struct{}), release: make(chan struct{})}
			svc := feedbackServiceForStore(root, &feedbackBlockingStore{}, feedbackTestWarn(t))
			svc.weighter = weighter
			args := []string{"-config", configPath, "-root", root, "-feedback", "-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-project-context"}
			if current {
				dbPath := filepath.Join(t.TempDir(), "explicit.db")
				seedIndex(t, dbPath, workspaceID(root), "test/qwen3-embedding:8b")
				args = append(args, "-rag-db", dbPath)
			}

			var mu sync.Mutex
			var events []string
			runtimeClosed := make(chan struct{})
			feedbackClosed := make(chan struct{})
			var runtimeOnce, feedbackOnce sync.Once
			closed := func(name string) {
				mu.Lock()
				events = append(events, name)
				mu.Unlock()
				switch name {
				case "runtime":
					runtimeOnce.Do(func() { close(runtimeClosed) })
				case "feedback":
					feedbackOnce.Do(func() { close(feedbackClosed) })
				}
			}

			releaseResult := make(chan error, 1)
			err := run(args, stdin, stdout, stderr, runHooks{
				openFeedback: func(context.Context, string, string, func(string)) (*feedbackService, error) {
					return svc, nil
				},
				startAutoIndex: func() func() {
					return func() {
						mu.Lock()
						events = append(events, "auto-index")
						mu.Unlock()
					}
				},
				afterAutoIndexStart: func(mode lineSourceMode, retrieve agent.Tool, gotSvc *feedbackService) error {
					if mode != sourceREPL || gotSvc != svc {
						t.Fatalf("startup hook mode/service = %v/%p, want REPL/%p", mode, gotSvc, svc)
					}
					active := retrieve
					if !current {
						ready, ok := retrieve.(*readyRetrieve)
						if !ok {
							t.Fatalf("retrieve tool = %T, want *readyRetrieve", retrieve)
						}
						ready.install(newRetrievalReader(runBorrowingTool{weighter: gotSvc.behavioralWeighter()}, func() error { return nil }), "ready")
						active = ready
					}
					type invokeOutcome struct {
						result agent.ToolResult
						err    error
					}
					invokeResult := make(chan invokeOutcome, 1)
					go func() {
						result, invokeErr := active.Invoke(context.Background(), json.RawMessage(`{"query":"active"}`))
						invokeResult <- invokeOutcome{result: result, err: invokeErr}
					}()
					select {
					case <-weighter.entered:
					case outcome := <-invokeResult:
						t.Fatalf("retrieval returned before borrowing the feedback weighter: result=%+v err=%v", outcome.result, outcome.err)
					case <-time.After(time.Second):
						t.Fatal("retrieval did not borrow the feedback weighter")
					}
					go func() {
						select {
						case <-runtimeClosed:
						case <-time.After(time.Second):
							close(weighter.release)
							releaseResult <- errors.New("retrieval drain ran before runtime close")
							return
						}
						select {
						case <-feedbackClosed:
							close(weighter.release)
							releaseResult <- errors.New("feedback closed while retrieval still borrowed its weighter")
						default:
							close(weighter.release)
							releaseResult <- nil
						}
					}()
					return errStop
				},
				closed: closed,
			})
			if !errors.Is(err, errStop) {
				t.Fatalf("run error = %v, want test stop", err)
			}
			if releaseErr := <-releaseResult; releaseErr != nil {
				t.Fatal(releaseErr)
			}
			mu.Lock()
			gotEvents := append([]string(nil), events...)
			mu.Unlock()
			wantEvents := []string{"auto-index", "runtime", "retrieval", "feedback"}
			if fmt.Sprint(gotEvents) != fmt.Sprint(wantEvents) {
				t.Fatalf("shutdown events = %v, want %v", gotEvents, wantEvents)
			}
		})
	}
}

type runBlockingWeighter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *runBlockingWeighter) WeightsBatch(context.Context, []string) (map[string]float64, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return map[string]float64{}, nil
}

type runBorrowingTool struct{ weighter rag.BehavioralWeighter }

func (runBorrowingTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "retrieve", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (runBorrowingTool) Effect() agent.Effect { return agent.Effect{Class: agent.Read} }
func (t runBorrowingTool) Invoke(ctx context.Context, _ json.RawMessage) (agent.ToolResult, error) {
	_, err := t.weighter.WeightsBatch(ctx, []string{"chunk"})
	return agent.ToolResult{Content: "done"}, err
}

func writeRunLifecycleConfig(t *testing.T) (string, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"agent-model"},{"id":"qwen3-embedding:8b"}]}`)
		case "/v1/embeddings":
			embedding := make([]float64, 768)
			embedding[0] = 1
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data":  []any{map[string]any{"embedding": embedding, "index": 0}},
				"model": "qwen3-embedding:8b",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
  "providers": {"test": {"base_url": %q, "api_format": "openai-compat", "timeout": "2s"}},
  "models": {
    "agent": {"name": "agent-model", "provider": "test", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "stream", "tool_call"]},
	"embedding": {"name": "qwen3-embedding:8b", "provider": "test", "type": "embedding", "dimensions": 768,
      "capabilities": ["embed"]}
  },
  "defaults": {"agent": "agent", "embedding": "embedding"}
}`, server.URL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, root
}

func runTestFiles(t *testing.T) (*os.File, *os.File, *os.File) {
	t.Helper()
	files := make([]*os.File, 3)
	for i := range files {
		file, err := os.CreateTemp(t.TempDir(), "run-file")
		if err != nil {
			t.Fatal(err)
		}
		files[i] = file
		t.Cleanup(func() { _ = file.Close() })
	}
	return files[0], files[1], files[2]
}

func readRunTestFile(t *testing.T, file *os.File) string {
	t.Helper()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func TestParseFlags_FeedbackHelpDescribesCollectionAndRanking(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	t.Cleanup(func() { os.Stderr = original })
	os.Stderr = w
	_, parseErr := parseFlags([]string{"-help"})
	os.Stderr = original
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	help, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(parseErr, flag.ErrHelp) || !strings.Contains(string(help), "enable local behavioral feedback collection and retrieval ranking") {
		t.Fatalf("parse error=%v help=%q", parseErr, help)
	}
}

func TestParseFlags_Overrides(t *testing.T) {
	f, err := parseFlags([]string{"-root", "/x", "-config", "/c/models.json", "-no-color", "-max-steps", "8"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.root != "/x" || f.configPath != "/c/models.json" || !f.noColor || f.maxSteps != 8 {
		t.Errorf("flags = %+v, unexpected", f)
	}
}

func TestInterruptSignalsExcludeSIGTERM(t *testing.T) {
	for _, sig := range interruptSignals() {
		if sig == syscall.SIGTERM {
			t.Fatal("SIGTERM must keep Go's default process-termination behavior")
		}
	}
}

func TestParseFlags_SessionDefaults(t *testing.T) {
	f, err := parseFlags([]string{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.noSession || f.fresh || f.sessionID != "" {
		t.Errorf("session flag defaults wrong: %+v", f)
	}
}

func TestParseFlags_SessionOverrides(t *testing.T) {
	f, err := parseFlags([]string{"-no-session", "-fresh", "-session", "mychat"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.noSession || !f.fresh || f.sessionID != "mychat" {
		t.Errorf("session overrides wrong: %+v", f)
	}
}

func TestValidateFlags(t *testing.T) {
	if err := validateFlags(flags{fresh: true, sessionID: "x"}); err == nil {
		t.Error("-fresh with -session must error (mutually exclusive)")
	}
	if err := validateFlags(flags{fresh: true}); err != nil {
		t.Errorf("-fresh alone must be allowed, got %v", err)
	}
	if err := validateFlags(flags{sessionID: "x"}); err != nil {
		t.Errorf("-session alone must be allowed, got %v", err)
	}
}

func TestStartupNotices_SessionLine(t *testing.T) {
	got := startupNotices(startupInfo{
		workspace:   "/r",
		sessionLine: "session: workspace:abcd (new)",
	})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "session: workspace:abcd (new)") {
		t.Errorf("session line missing in:\n%s", joined)
	}
}

func TestStartupNotices_MCPLine(t *testing.T) {
	got := startupNotices(startupInfo{
		workspace: "/r",
		mcpLine:   "mcp: attached 3 tool(s) from 2 configured server(s)",
	})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "mcp: attached 3 tool(s) from 2 configured server(s)") {
		t.Errorf("mcp line missing in:\n%s", joined)
	}
}

func TestParseFlags_MCPServersRepeatable(t *testing.T) {
	f, err := parseFlags([]string{"-mcp-stdio", "npx a", "-mcp-stdio", "npx b", "-mcp-http", "https://h/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.mcpStdio) != 2 {
		t.Errorf("mcpStdio = %v, want 2 entries", f.mcpStdio)
	}
	if len(f.mcpHTTP) != 1 {
		t.Errorf("mcpHTTP = %v, want 1 entry", f.mcpHTTP)
	}
}

func TestValidateFlags_NoRagWithRagDBExclusive(t *testing.T) {
	err := validateFlags(flags{noRag: true, ragDB: "/x.db"})
	if err == nil {
		t.Fatal("want error when -no-rag and -rag-db are both set")
	}
}

func TestValidateFlags_FeedbackDBRequiresFeedback(t *testing.T) {
	if err := validateFlags(flags{feedbackDB: "/x.db"}); err == nil {
		t.Fatal("want error when -feedback-db is set without -feedback")
	}
	if err := validateFlags(flags{feedback: true, feedbackDB: "/x.db"}); err != nil {
		t.Fatalf("want no error when -feedback and -feedback-db both set: %v", err)
	}
}

func TestParseFlags_NoRag(t *testing.T) {
	f, err := parseFlags([]string{"-no-rag"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.noRag {
		t.Error("noRag = false, want true")
	}
}

func TestParseFlagsProjectContextOptOut(t *testing.T) {
	f, err := parseFlags([]string{"-no-project-context"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.noProjectContext {
		t.Fatal("-no-project-context should set noProjectContext")
	}
	f2, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags defaults: %v", err)
	}
	if f2.noProjectContext {
		t.Fatal("noProjectContext must default to false")
	}
}

func TestParseFlagsAllowExec(t *testing.T) {
	f, err := parseFlags([]string{"-allow-exec"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.allowExec {
		t.Error("allowExec should be true")
	}
}

func TestStartupNoticesProjectContextLineIsInformational(t *testing.T) {
	got := startupNotices(startupInfo{
		workspace:          "/abs/root",
		projectContextLine: "project context: loaded 2 file(s)",
	})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "project context: loaded 2 file(s)") {
		t.Fatalf("startup notices missing project context line:\n%s", joined)
	}
	if strings.Contains(joined, "warning: project context") {
		t.Fatalf("project context loaded line must not be rendered as a warning:\n%s", joined)
	}
}

func TestParseFlagsNoMemory(t *testing.T) {
	f, err := parseFlags([]string{"-no-memory"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !f.noMemory {
		t.Error("-no-memory not parsed")
	}
	def, _ := parseFlags(nil)
	if def.noMemory {
		t.Error("noMemory should default false")
	}
}

func TestParsePressureWarnFlag(t *testing.T) {
	f, err := parseFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.pressureWarn != 75 {
		t.Fatalf("default pressureWarn = %d, want 75", f.pressureWarn)
	}
	f, err = parseFlags([]string{"-pressure-warn", "0"})
	if err != nil || f.pressureWarn != 0 {
		t.Fatalf("explicit 0: f=%d err=%v", f.pressureWarn, err)
	}
}

func TestValidatePressureWarnRange(t *testing.T) {
	if err := validateFlags(flags{pressureWarn: 75}); err != nil {
		t.Fatalf("75 should be valid: %v", err)
	}
	if err := validateFlags(flags{pressureWarn: -1}); err == nil {
		t.Fatal("negative pressureWarn should be rejected")
	}
	if err := validateFlags(flags{pressureWarn: 101}); err == nil {
		t.Fatal(">100 pressureWarn should be rejected")
	}
}

func TestAgentMemoryFlag(t *testing.T) {
	f, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.agentMemory {
		t.Error("agent memory must default to disabled (opt-in)")
	}
	f, err = parseFlags([]string{"-agent-memory"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !f.agentMemory {
		t.Error("-agent-memory not parsed")
	}
}

func TestStartupNoticesAgentMemoryLine(t *testing.T) {
	lines := startupNotices(startupInfo{workspace: "/w", agentMemoryLine: "agent memory: enabled"})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "agent memory: enabled") {
		t.Errorf("notices missing agent memory line: %v", lines)
	}
}

func TestStartupNotices_DelegateLine(t *testing.T) {
	lines := startupNotices(startupInfo{workspace: "/w", delegateLine: "delegate: enabled -> local/coder"})
	found := false
	for _, l := range lines {
		if l == "delegate: enabled -> local/coder" {
			found = true
		}
	}
	if !found {
		t.Fatalf("delegate notice not surfaced: %v", lines)
	}
}

func TestParseFlags_NoAutoIndex(t *testing.T) {
	f, err := parseFlags([]string{"-no-auto-index"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.noAutoIndex {
		t.Error("-no-auto-index not parsed")
	}
	f, err = parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.noAutoIndex {
		t.Error("noAutoIndex must default to false")
	}
}

func TestParseFlags_ProgressiveIsOptIn(t *testing.T) {
	f, err := parseFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.progressive {
		t.Fatal("progressive summaries must default to disabled")
	}
	f, err = parseFlags([]string{"-progressive"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.progressive {
		t.Fatal("-progressive did not enable progressive summaries")
	}
}

func TestParseFlags_NoAutoIndexNoConflicts(t *testing.T) {
	for _, args := range [][]string{
		{"-no-auto-index", "-rag-db", "x.db"},
		{"-no-auto-index", "-no-rag"},
	} {
		f, err := parseFlags(args)
		if err != nil {
			t.Fatalf("parseFlags(%v): %v", args, err)
		}
		if err := validateFlags(f); err != nil {
			t.Errorf("validateFlags(%v): %v", args, err)
		}
	}
}

func TestShouldStartAutoIndex(t *testing.T) {
	tests := []struct {
		name string
		f    flags
		want bool
	}{
		{name: "default", f: flags{}, want: true},
		{name: "one-shot -p", f: flags{prompt: "hi", promptSet: true}, want: false},
		{name: "planning -goal", f: flags{goalSet: true}, want: false},
		{name: "-no-auto-index", f: flags{noAutoIndex: true}, want: false},
		{name: "-no-rag", f: flags{noRag: true}, want: false},
		{name: "-rag-db", f: flags{ragDB: "x.db"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldStartAutoIndex(tt.f); got != tt.want {
				t.Errorf("shouldStartAutoIndex = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAutoIndexEnabled(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name     string
		f        flags
		autoErr  error
		chainErr error
		want     bool
	}{
		{name: "default", want: true},
		{name: "one-shot -p", f: flags{prompt: "hi", promptSet: true}, want: false},
		{name: "-no-auto-index", f: flags{noAutoIndex: true}, want: false},
		{name: "-no-rag", f: flags{noRag: true}, want: false},
		{name: "-rag-db", f: flags{ragDB: "x.db"}, want: false},
		{name: "auto path error", autoErr: boom, want: false},
		{name: "embed chain error", chainErr: boom, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := autoIndexEnabled(tt.f, tt.autoErr, tt.chainErr); got != tt.want {
				t.Errorf("autoIndexEnabled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStartupNotices_AutoWarmingSuppressesGenericLine(t *testing.T) {
	got := startupNotices(startupInfo{
		workspace:         "/abs/root",
		retrieveLine:      "retrieve: auto-index warming in background",
		retrieveOmitted:   false,
		retrieveRequested: true,
	})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "retrieve: auto-index warming in background") {
		t.Errorf("missing warming line in %q", joined)
	}
	if strings.Contains(joined, "retrieve unavailable") {
		t.Errorf("generic no-index line must not print in auto mode: %q", joined)
	}
}

func TestParseFlags_PromptSkipsAutoIndex(t *testing.T) {
	f, err := parseFlags([]string{"-p", "hello"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if err := validateFlags(f); err != nil {
		t.Fatalf("validateFlags: %v", err)
	}
	if shouldStartAutoIndex(f) {
		t.Error("one-shot -p must not start the background auto-index")
	}
}

func TestParseFlags_TaskMode(t *testing.T) {
	f, err := parseFlags([]string{"-plan", "plan.json", "-approve-plan-edits", "-approve-plan-gates"})
	if err != nil {
		t.Fatal(err)
	}
	if f.planPath != "plan.json" || !f.approveEdits || !f.approveGates {
		t.Fatalf("flags = %+v", f)
	}
}

func TestParseFlags_AgentflowStatus(t *testing.T) {
	f, err := parseFlags([]string{"-agentflow-status", "-json", "-root", "/work", "-agentflow-src", "/src"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.agentflowStatus || !f.jsonOutput || f.agentflowResume {
		t.Fatalf("flags = %+v", f)
	}
	if err := validateFlags(f); err != nil {
		t.Fatal(err)
	}
}

func TestParseFlags_AgentflowResume(t *testing.T) {
	args := []string{
		"-agentflow-resume", "-plan", "plan.json", "-plan-workers", "1",
		"-approve-plan-edits", "-approve-plan-gates", "-task-brief", "brief.json", "-workflow-handoff", "route.json",
	}
	f, err := parseFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	if !f.agentflowResume || f.agentflowStatus || f.planWorkers != 1 {
		t.Fatalf("flags = %+v", f)
	}
	if err := validateFlags(f); err != nil {
		t.Fatal(err)
	}
}

func TestValidateFlags_AgentflowRecovery(t *testing.T) {
	validResume := []string{"-agentflow-resume", "-plan", "plan.json", "-approve-plan-edits", "-approve-plan-gates"}
	tests := [][]string{
		{"-agentflow-status", "-agentflow-resume"},
		{"-json"},
		{"-agentflow-resume"},
		{"-agentflow-resume", "-plan", "plan.json", "-plan-workers", "2", "-approve-plan-edits", "-approve-plan-gates"},
	}
	conflicts := [][]string{
		{"-p", "do it"},
		{"-goal", "plan it"},
		{"-review-manifest", "review.json"},
		{"-evidence", "evidence.json"},
		{"-workflow-profile", "high-risk", "-workflow-reason", "override"},
		{"-rag-db", "rag.db"},
		{"-delegate"},
		{"-mcp-stdio", "server"},
		{"-allow-write"},
		{"-allow-exec"},
		{"-approve-plan-lock"},
	}
	for _, conflict := range conflicts {
		tests = append(tests, append([]string{"-agentflow-status"}, conflict...))
		tests = append(tests, append(append([]string(nil), validResume...), conflict...))
	}
	for _, args := range tests {
		f, err := parseFlags(args)
		if err != nil {
			t.Fatalf("parseFlags(%v): %v", args, err)
		}
		if err := validateFlags(f); err == nil {
			t.Errorf("validateFlags(%v) unexpectedly succeeded", args)
		}
	}
}

func writeFakeAgentflow(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agentflow")
	script := "#!/bin/sh\nprintf '%s' \"$GOLEM_AGENTFLOW_STATUS_PAYLOAD\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRun_AgentflowStatusDispatchesBeforeConfig(t *testing.T) {
	root := t.TempDir()
	payload := []byte("{\"state\":\"uninitialized\"}\n")
	t.Setenv("PATH", writeFakeAgentflow(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOLEM_AGENTFLOW_STATUS_PAYLOAD", string(payload))
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	errOut, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = errOut.Close() }()

	err = run([]string{
		"-agentflow-status", "-json", "-root", root,
		"-config", filepath.Join(root, "missing-models.json"),
	}, os.Stdin, out, errOut)
	var statusErr *agentflowStatusExit
	if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 {
		t.Fatalf("run error = %v", err)
	}
	if _, err := out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("stdout = %q, want %q", got, payload)
	}
}

func TestAgentflowStatusExitHelper(t *testing.T) {
	if os.Getenv("GOLEM_AGENTFLOW_STATUS_EXIT_HELPER") != "1" {
		return
	}
	os.Args = []string{
		"golem", "-agentflow-status", "-json", "-root", os.Getenv("GOLEM_AGENTFLOW_STATUS_ROOT"),
		"-config", filepath.Join(os.Getenv("GOLEM_AGENTFLOW_STATUS_ROOT"), "missing-models.json"),
	}
	main()
	os.Exit(0)
}

func TestAgentflowStatusExitCodesDoNotPrintGenericErrors(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, ".agent")
	if err := os.Mkdir(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	plan := `{"steps":[{"id":"P1","gates":[{"kind":"command","run":["true"]}]}]}`
	if err := os.WriteFile(filepath.Join(agentDir, "plan.lock.json"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "proof-pack.json"), []byte(`{"checks":[{"status":"passed"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", writeFakeAgentflow(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, test := range []struct {
		state string
		exit  int
	}{
		{state: "complete", exit: 0},
		{state: "step_unclaimed", exit: 2},
		{state: "uninitialized", exit: 3},
	} {
		t.Run(test.state, func(t *testing.T) {
			payload := fmt.Sprintf("{\"state\":%q}\n", test.state)
			switch test.state {
			case "complete":
				payload = `{"state":"complete","resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem","step":null,"attempt":null,"lease":{"policy":"advisory","ttl_minutes":30,"grace_seconds":0,"expires_at":null,"state":"not_applicable","exclusive":false},"gates":[],"recovery_actions":[],"diagnostics":[]}}` + "\n"
			case "step_unclaimed":
				payload = `{"state":"step_unclaimed","step_id":"P1","resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem","step":{"id":"P1","state":"pending","completed":false},"attempt":null,"lease":{"policy":"advisory","ttl_minutes":30,"grace_seconds":0,"expires_at":null,"state":"not_applicable","exclusive":false},"gates":[],"recovery_actions":[{"action":"claim","allowed":true,"automatic":true,"break_glass":false,"reason":"eligible"}],"diagnostics":[]}}` + "\n"
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestAgentflowStatusExitHelper$")
			cmd.Env = append(os.Environ(),
				"GOLEM_AGENTFLOW_STATUS_EXIT_HELPER=1",
				"GOLEM_AGENTFLOW_STATUS_ROOT="+root,
				"GOLEM_AGENTFLOW_STATUS_PAYLOAD="+payload,
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			gotExit := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatal(err)
				}
				gotExit = exitErr.ExitCode()
			}
			if gotExit != test.exit {
				t.Fatalf("exit = %d, want %d; stderr=%q", gotExit, test.exit, stderr.String())
			}
			if strings.Contains(stderr.String(), "golem:") {
				t.Fatalf("generic error leaked to stderr: %q", stderr.String())
			}
			if stdout.String() != payload {
				t.Fatalf("stdout = %q, want %q", stdout.String(), payload)
			}
		})
	}
}

func TestAgentflowStartupHintOnlyInOrdinaryInteractiveMode(t *testing.T) {
	for _, test := range []struct {
		name string
		f    flags
		want bool
	}{
		{name: "interactive", want: true},
		{name: "one shot", f: flags{promptSet: true}},
		{name: "task", f: flags{planPath: "plan.json"}},
		{name: "planning", f: flags{goalSet: true}},
		{name: "status", f: flags{agentflowStatus: true}},
		{name: "resume", f: flags{agentflowResume: true, planPath: "plan.json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldShowAgentflowHint(test.f); got != test.want {
				t.Fatalf("shouldShowAgentflowHint() = %t, want %t", got, test.want)
			}
		})
	}

	lines := startupNotices(startupInfo{workspace: "/work", agentflowState: true})
	want := "agentflow state detected: inspect with -agentflow-status; resume with -agentflow-resume -plan <plan>"
	if !strings.Contains(strings.Join(lines, "\n"), want) {
		t.Fatalf("startup notices missing %q: %v", want, lines)
	}
}

func TestPlanWorkers(t *testing.T) {
	f, err := parseFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.planWorkers != 1 || f.planWorkersSet {
		t.Fatalf("default plan workers = %d, set=%t; want 1, false", f.planWorkers, f.planWorkersSet)
	}

	for _, workers := range []string{"1", "2"} {
		f, err = parseFlags([]string{"-plan", "plan.json", "-plan-workers", workers, "-approve-plan-edits", "-approve-plan-gates"})
		if err != nil {
			t.Fatalf("parse task workers=%s: %v", workers, err)
		}
		if err := validateFlags(f); err != nil {
			t.Fatalf("validate task workers=%s: %v", workers, err)
		}
	}

	for _, args := range [][]string{
		{"-plan-workers", "0"},
		{"-plan-workers", "-1"},
		{"-plan-workers", "1"},
		{"-plan-workers", "2"},
	} {
		f, err = parseFlags(args)
		if err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		if err := validateFlags(f); err == nil {
			t.Fatalf("validateFlags(%v) unexpectedly succeeded", args)
		}
	}

	for _, f := range []flags{{planWorkers: -1}, {planWorkers: 2}} {
		if err := validateFlags(f); err == nil {
			t.Fatalf("direct flags %+v unexpectedly succeeded", f)
		}
	}
}

func TestParseFlags_TaskModeReviewManifest(t *testing.T) {
	f, err := parseFlags([]string{"-plan", "plan.json", "-review-manifest", "review.json", "-approve-plan-edits", "-approve-plan-gates"})
	if err != nil {
		t.Fatal(err)
	}
	if f.reviewManifest != "review.json" {
		t.Fatalf("reviewManifest = %q", f.reviewManifest)
	}
	if err := validateFlags(f); err != nil {
		t.Fatalf("review manifest with task mode rejected: %v", err)
	}
}

func TestValidateFlags_ReviewManifestRequiresPlan(t *testing.T) {
	err := validateFlags(flags{reviewManifest: "review.json"})
	if err == nil || !strings.Contains(err.Error(), "-review-manifest") || !strings.Contains(err.Error(), "-plan") {
		t.Fatalf("err = %v, want task-mode-only validation", err)
	}
}

func TestValidateFlags_PlanIncompatibleWithP(t *testing.T) {
	f := flags{planPath: "plan.json", promptSet: true, prompt: "hi"}
	if err := validateFlags(f); err == nil {
		t.Fatal("expected -plan + -p to be rejected")
	}
}

func TestValidateFlags_PlanRejectsAmbientToolFlags(t *testing.T) {
	for _, f := range []flags{
		{planPath: "plan.json", allowExec: true},
		{planPath: "plan.json", allowWrite: true},
		{planPath: "plan.json", ragDB: "/tmp/rag.db"},
		{planPath: "plan.json", delegate: true},
		{planPath: "plan.json", mcpStdio: stringSliceFlag{"server"}},
		{planPath: "plan.json", mcpHTTP: stringSliceFlag{"https://example.invalid/mcp"}},
	} {
		if err := validateFlags(f); err == nil {
			t.Fatalf("expected proof-mode ambient tool flag to be rejected: %+v", f)
		}
	}
}

func TestApplyTaskMode_DisablesPersistentAndAmbientState(t *testing.T) {
	f, _ := applyTaskMode(flags{planPath: "plan.json", agentMemory: true})
	if !f.noSession || !f.noCompress || !f.noMemory || !f.noAutoIndex || !f.noRag || f.agentMemory {
		t.Fatalf("task-mode defaults not applied: %+v", f)
	}
}

func TestValidateFlags_PlanRequiresBothApprovals(t *testing.T) {
	if err := validateFlags(flags{planPath: "plan.json", approveEdits: false, approveGates: true}); err == nil {
		t.Fatal("expected error when -approve-plan-edits is missing")
	} else if !strings.Contains(err.Error(), "-approve-plan-edits") || !strings.Contains(err.Error(), "-approve-plan-gates") {
		t.Fatalf("error should mention both approval flags, got %v", err)
	}
	if err := validateFlags(flags{planPath: "plan.json", approveEdits: true, approveGates: false}); err == nil {
		t.Fatal("expected error when -approve-plan-gates is missing")
	} else if !strings.Contains(err.Error(), "-approve-plan-edits") || !strings.Contains(err.Error(), "-approve-plan-gates") {
		t.Fatalf("error should mention both approval flags, got %v", err)
	}
	if err := validateFlags(flags{planPath: "plan.json", approveEdits: true, approveGates: true}); err != nil {
		t.Fatalf("both approvals set should be allowed, got %v", err)
	}
}

func TestApplyTaskMode_WarnsOnIgnoredFlags(t *testing.T) {
	f, warns := applyTaskMode(flags{planPath: "plan.json", trace: true})
	if len(warns) == 0 {
		t.Fatal("expected a warning when -trace is set in task mode")
	}
	if !f.noSession {
		t.Fatalf("task-mode defaults should still apply: %+v", f)
	}

	_, warns = applyTaskMode(flags{planPath: "plan.json"})
	if len(warns) != 0 {
		t.Fatalf("expected no warnings when no ignored flags are set, got %v", warns)
	}
}

func TestParseFlags_GoalMutualExclusions(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"goal+prompt", []string{"-goal", "x", "-p", "hi"}},
		{"goal+plan", []string{"-goal", "x", "-plan", "p.json"}},
		{"goal+allow-write", []string{"-goal", "x", "-allow-write"}},
		{"goal+allow-exec", []string{"-goal", "x", "-allow-exec"}},
		{"goal+rag", []string{"-goal", "x", "-rag-db", "r.db"}},
		{"goal+delegate", []string{"-goal", "x", "-delegate"}},
		{"goal+approve-edits", []string{"-goal", "x", "-approve-plan-edits"}},
		{"goal+approve-gates", []string{"-goal", "x", "-approve-plan-gates"}},
		{"goal+evidence", []string{"-goal", "x", "-evidence", "e.json"}},
		{"goal+mcp", []string{"-goal", "x", "-mcp-stdio", "server"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := parseFlags(c.args)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateFlags(f); err == nil {
				t.Errorf("expected mutual-exclusion error for %v", c.args)
			}
		})
	}
}

func TestParseFlags_GoalAlone(t *testing.T) {
	f, err := parseFlags([]string{"-goal", "add a healthz endpoint"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateFlags(f); err != nil {
		t.Fatal(err)
	}
	if !f.goalSet || f.goal != "add a healthz endpoint" {
		t.Errorf("goalSet=%v goal=%q", f.goalSet, f.goal)
	}
}

func TestParseFlags_GoalRejectsBlankValue(t *testing.T) {
	f, err := parseFlags([]string{"-goal", "   "})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateFlags(f); err == nil {
		t.Fatal("explicit blank -goal must be rejected")
	}
}

func TestParseFlags_ApprovePlanLock(t *testing.T) {
	f, err := parseFlags([]string{"-goal", "x", "-approve-plan-lock"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateFlags(f); err != nil {
		t.Fatal(err)
	}
	if !f.approvePlanLock {
		t.Error("approvePlanLock not set")
	}

	for _, args := range [][]string{
		{"-approve-plan-lock"},
		{"-plan", "p.json", "-approve-plan-edits", "-approve-plan-gates", "-approve-plan-lock"},
	} {
		f, err := parseFlags(args)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateFlags(f); err == nil || !strings.Contains(err.Error(), "-approve-plan-lock") {
			t.Errorf("args %v: want -approve-plan-lock scope error, got %v", args, err)
		}
	}
}

func TestParseFlags_AgentflowWorkflowRouting(t *testing.T) {
	valid := [][]string{
		{"-plan", "plan.json", "-task-brief", "brief.json", "-approve-plan-edits", "-approve-plan-gates"},
		{"-plan", "plan.json", "-task-brief", "brief.json", "-workflow-handoff", "route.json", "-approve-plan-edits", "-approve-plan-gates"},
		{"-plan", "plan.json", "-workflow-profile", "high-risk", "-workflow-reason", "security review required", "-approve-plan-edits", "-approve-plan-gates"},
		{"-goal", "ship it", "-workflow-profile", "high-risk", "-workflow-reason", "security review required"},
	}
	for _, args := range valid {
		f, err := parseFlags(args)
		if err != nil {
			t.Fatalf("parseFlags(%v): %v", args, err)
		}
		if err := validateFlags(f); err != nil {
			t.Errorf("validateFlags(%v): %v", args, err)
		}
	}

	invalid := [][]string{
		{"-task-brief", "brief.json"},
		{"-workflow-handoff", "route.json"},
		{"-goal", "ship it", "-workflow-handoff", "route.json"},
		{"-plan", "plan.json", "-workflow-handoff", "route.json", "-workflow-profile", "high-risk", "-workflow-reason", "reason"},
		{"-goal", "ship it", "-task-brief", "brief.json"},
		{"-workflow-profile", "high-risk", "-workflow-reason", "security review required"},
		{"-goal", "ship it", "-workflow-profile", "high-risk"},
		{"-goal", "ship it", "-workflow-reason", "security review required"},
		{"-goal", "ship it", "-workflow-profile", "high-risk", "-workflow-reason", "   "},
	}
	for _, args := range invalid {
		f, err := parseFlags(args)
		if err != nil {
			t.Fatalf("parseFlags(%v): %v", args, err)
		}
		if err := validateFlags(f); err == nil {
			t.Errorf("validateFlags(%v) unexpectedly succeeded", args)
		}
	}

	f, err := parseFlags(valid[0])
	if err != nil {
		t.Fatal(err)
	}
	if f.taskBriefPath != "brief.json" {
		t.Fatalf("task brief path = %q", f.taskBriefPath)
	}
	f, err = parseFlags(valid[1])
	if err != nil {
		t.Fatal(err)
	}
	if f.workflowHandoffPath != "route.json" {
		t.Fatalf("workflow handoff path = %q", f.workflowHandoffPath)
	}
	f, err = parseFlags(valid[3])
	if err != nil {
		t.Fatal(err)
	}
	if f.workflowProfile != "high-risk" || f.workflowReason != "security review required" {
		t.Fatalf("workflow override = %q / %q", f.workflowProfile, f.workflowReason)
	}
}

func TestApplyGoalMode_DisablesAmbientStateButKeepsProjectContext(t *testing.T) {
	f, _ := applyGoalMode(flags{goalSet: true, agentMemory: true, feedback: true, feedbackDB: "x.db"})
	if !f.noSession || !f.noCompress || !f.noMemory || !f.noAutoIndex || !f.noRag || f.agentMemory {
		t.Fatalf("planning-mode ambient shutdown not applied: %+v", f)
	}
	if f.feedback || f.feedbackDB != "" {
		t.Fatalf("planning mode should disable feedback: %+v", f)
	}
	if f.noProjectContext {
		t.Fatal("planning mode must leave project context enabled")
	}
	// Non-goal flags flow through untouched.
	g, warns := applyGoalMode(flags{})
	if g.noSession || g.noRag || len(warns) != 0 {
		t.Fatalf("applyGoalMode must be a no-op without -goal: %+v warns=%v", g, warns)
	}
}

func TestApplyGoalMode_WarnsOnIgnoredFlags(t *testing.T) {
	_, warns := applyGoalMode(flags{goalSet: true, sessionID: "chat"})
	if len(warns) == 0 {
		t.Fatal("expected a warning when -session is set in planning mode")
	}
	_, warns = applyGoalMode(flags{goalSet: true, trace: true})
	if len(warns) == 0 {
		t.Fatal("expected a warning when -trace is set in planning mode")
	}
	_, warns = applyGoalMode(flags{goalSet: true})
	if len(warns) != 0 {
		t.Fatalf("expected no warnings when no ignored flags are set, got %v", warns)
	}
}
