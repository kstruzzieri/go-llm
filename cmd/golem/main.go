package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/mcpclient"
	"github.com/kstruzzieri/go-llm/memory"
)

type flags struct {
	configPath       string
	root             string
	ollamaURL        string
	ragDB            string
	maxSteps         int
	inputCeiling     int
	outputReserve    int
	noColor          bool
	noSession        bool
	fresh            bool
	sessionID        string
	allowWrite       bool
	allowExec        bool
	mcpStdio         stringSliceFlag
	mcpHTTP          stringSliceFlag
	noRag            bool
	noProjectContext bool
	noCompress       bool
	noMemory         bool
	trace            bool
	telemetry        bool
	pressureWarn     int
}

func parseFlags(args []string) (flags, error) {
	var f flags
	fs := flag.NewFlagSet("golem", flag.ContinueOnError)
	fs.StringVar(&f.configPath, "config", "", "path to models.json (default: auto-discover)")
	fs.StringVar(&f.root, "root", ".", "workspace root the tools are scoped to")
	fs.StringVar(&f.ollamaURL, "ollama-url", "", "override Ollama base URL")
	fs.StringVar(&f.ragDB, "rag-db", "", "path to a prebuilt RAG SQLite DB to enable the retrieve tool")
	fs.IntVar(&f.maxSteps, "max-steps", 0, "max agent steps per prompt (0 => default 16)")
	fs.IntVar(&f.inputCeiling, "input-ceiling", 0, "token input ceiling (0 => default)")
	fs.IntVar(&f.outputReserve, "output-reserve", 0, "token output reserve")
	fs.BoolVar(&f.noColor, "no-color", false, "disable dim ANSI footers")
	fs.BoolVar(&f.noSession, "no-session", false, "disable persistent session memory")
	fs.BoolVar(&f.fresh, "fresh", false, "start a new persistent session instead of resuming this workspace")
	fs.BoolVar(&f.allowWrite, "allow-write", false, "enable approval-gated write_file/edit_file tools")
	fs.BoolVar(&f.allowExec, "allow-exec", false, "enable the approval-gated run_command exec tool")
	fs.Var(&f.mcpStdio, "mcp-stdio", "attach an MCP server over stdio: \"[alias=]command args...\" (repeatable; use `env KEY=val cmd` for env vars)")
	fs.Var(&f.mcpHTTP, "mcp-http", "attach an MCP server over streamable HTTP: \"[alias=]https://endpoint\" (repeatable)")
	fs.BoolVar(&f.noRag, "no-rag", false, "disable the retrieve tool entirely (ignore any auto index)")
	fs.BoolVar(&f.noProjectContext, "no-project-context", false, "do not load AGENTS.md project-context files into the system prompt")
	fs.BoolVar(&f.noCompress, "no-compress", false, "disable post-turn conversation compression into a durable summary")
	fs.BoolVar(&f.noMemory, "no-memory", false, "disable explicit local memories (/remember, /memories, memory_search)")
	fs.StringVar(&f.sessionID, "session", "", "explicit session id to resume or create (default: per-workspace)")
	// -trace persists a full per-run trace (may contain workspace/user content; for replay/eval).
	// -telemetry appends content-light run metrics only (timings, route, usage; no prompt or output).
	fs.BoolVar(&f.trace, "trace", false, "persist a content-full run trace per turn (outside the workspace; may contain workspace/user content)")
	fs.BoolVar(&f.telemetry, "telemetry", false, "append content-light run telemetry (timings, route, usage; no prompt/output)")
	fs.IntVar(&f.pressureWarn, "pressure-warn", 75, "context-pressure warn threshold percent 1-100 (0 disables the warning line)")
	if err := fs.Parse(args); err != nil {
		return flags{}, err
	}
	return f, nil
}

// validateFlags rejects flag values flag.Parse cannot police.
func validateFlags(f flags) error {
	if f.fresh && f.sessionID != "" {
		return fmt.Errorf("golem: -fresh and -session are mutually exclusive")
	}
	if f.noRag && f.ragDB != "" {
		return fmt.Errorf("golem: -no-rag and -rag-db are mutually exclusive")
	}
	if f.pressureWarn < 0 || f.pressureWarn > 100 {
		return fmt.Errorf("golem: -pressure-warn must be between 0 and 100")
	}
	return nil
}

type startupInfo struct {
	workspace          string
	useRecommend       bool
	bootstrapWarns     []error
	preflightWarns     []string
	retrieveLine       string
	retrieveOmitted    bool
	retrieveRequested  bool // -rag-db, -no-rag, or auto suppress the generic no-index notice
	sessionLine        string
	projectContextLine string
	memoryLine         string
	mcpLine            string
}

// startupNotices renders the human-facing startup lines (written to stderr).
func startupNotices(info startupInfo) []string {
	var out []string
	out = append(out, "workspace: "+info.workspace)
	if info.sessionLine != "" {
		out = append(out, info.sessionLine)
	}
	if info.memoryLine != "" {
		out = append(out, info.memoryLine)
	}
	if info.mcpLine != "" {
		out = append(out, info.mcpLine)
	}
	if info.projectContextLine != "" {
		out = append(out, info.projectContextLine)
	}
	if info.retrieveLine != "" {
		out = append(out, info.retrieveLine)
	}
	if info.useRecommend {
		out = append(out, "no defaults.agent configured; using model recommendation (run will route to the recommended model)")
	}
	for _, w := range info.bootstrapWarns {
		out = append(out, "warning: "+w.Error())
	}
	for _, w := range info.preflightWarns {
		out = append(out, "warning: "+w)
	}
	// The generic no-index notice is only meaningful when retrieve was NOT
	// requested. When -rag-db was given but failed, a specific "retrieve
	// disabled: ..." warning above already explains why.
	if info.retrieveOmitted && !info.retrieveRequested {
		out = append(out, "retrieve unavailable: no RAG index configured; using file/search tools")
	}
	return out
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		// -h / -help: the flag package already printed usage to stderr; help
		// is not a failure, so exit cleanly without a spurious "golem:" line.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		// runIndex already rendered its own output; just exit non-zero.
		if errors.Is(err, errIndexFailed) {
			os.Exit(1)
		}
		_, _ = fmt.Fprintf(os.Stderr, "golem: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin *os.File, stdout, stderr *os.File) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "index":
			return runIndex(context.Background(), args[1:], stdout, stderr)
		default:
			return fmt.Errorf("unknown command %q (did you mean \"index\"?)", args[0])
		}
	}

	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	if err := validateFlags(f); err != nil {
		return err
	}

	root, err := filepath.Abs(f.root)
	if err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}

	cfg, err := loadConfig(f.configPath)
	if err != nil {
		return err
	}

	ctx := context.Background()
	bundle, err := providerbootstrap.New(ctx, providerbootstrap.Options{
		Config:            cfg, // nil => default Ollama provider
		OllamaURLOverride: f.ollamaURL,
	})
	if err != nil {
		return fmt.Errorf("bootstrap providers: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	plan, err := resolveAgentChain(bundle.Config)
	if err != nil {
		return err
	}

	resolveEndpoint := newPreflightEndpointResolver(bundle.Config, f.ollamaURL)
	warns, err := preflightToolCapable(ctx, bundle.Models, plan.chain, resolveEndpoint)
	if err != nil {
		return err
	}

	autoDBPath, autoWorkspaceID, autoErr := indexDBPathForWorkspace(os.Getenv, root)
	autoSidecar := ""
	if autoErr == nil {
		autoSidecar = sidecarPath(autoDBPath)
	} else if !f.noRag && f.ragDB == "" {
		warns = append(warns, "retrieve auto-index disabled: "+autoErr.Error())
	}
	rr := enableRetrieve(ctx, bundle.Config, bundle.Router, retrieveOpts{
		noRag:           f.noRag,
		ragDB:           f.ragDB,
		autoDBPath:      autoDBPath,
		autoSidecarPath: autoSidecar,
		workspaceID:     autoWorkspaceID,
	})
	retrieve := rr.tool
	warns = append(warns, rr.warns...)
	retrieveLine := rr.line
	tools, err := buildTools(root, retrieve)
	if err != nil {
		return err
	}
	retrieveOmitted := retrieve == nil

	var memStore *memory.SQLiteStore
	var memDB *sql.DB
	var memDBPath string
	memoryEnabled := false
	if !f.noMemory {
		if dbPath, derr := memoryDBPathForWorkspace(os.Getenv, root); derr != nil {
			warns = append(warns, "memory disabled: "+derr.Error())
		} else if store, db, oerr := openMemoryStore(ctx, dbPath); oerr != nil {
			warns = append(warns, "memory disabled: "+oerr.Error())
		} else {
			memStore, memDB, memDBPath, memoryEnabled = store, db, dbPath, true
			tools = append(tools, agenttools.MemorySearch{S: store, WorkspaceID: workspaceID(root), Limit: 8})
		}
	}
	defer func() {
		if memDB != nil {
			_ = memDB.Close()
		}
	}()

	var journal *mutationJournal
	if f.allowWrite {
		wt, j, werr := buildWriteTools(root)
		if werr != nil {
			return werr
		}
		tools = append(tools, wt...)
		journal = j
	}

	if f.allowExec {
		et, eerr := buildExecTools(root)
		if eerr != nil {
			return eerr
		}
		tools = append(tools, et...)
	}

	var mcpManager *mcpclient.Manager
	mcpAttached := false
	mcpLine := ""
	if servers, perr := parseMCPServers(f.mcpStdio, f.mcpHTTP); perr != nil {
		return perr // fatal: bad flag config / explicit duplicate alias
	} else if len(servers) > 0 {
		mgr, mcpWarns, cerr := mcpclient.Connect(ctx, mcpClientImpl(), servers)
		if cerr != nil {
			return cerr // fatal: invalid / duplicate alias
		}
		for _, w := range mcpWarns {
			warns = append(warns, "mcp: "+w.Error())
		}
		mcpTools := mgr.Tools()
		tools = append(tools, mcpTools...)
		mcpManager = mgr
		mcpAttached = len(mcpTools) > 0
		// Positive confirmation so a silently-failed server attach is visible:
		// attached-tool count against the configured-server count (failures and
		// skipped tools appear as the "mcp: ..." warnings above).
		mcpLine = fmt.Sprintf("mcp: attached %d tool(s) from %d configured server(s)", len(mcpTools), len(servers))
	}
	defer func() {
		if mcpManager != nil {
			_ = mcpManager.Close()
		}
	}()

	baseSystem := buildSystemPrompt(f.allowWrite, f.allowExec)
	baseSystem += memorySystemFragment(memoryEnabled)
	projectContextLine := ""
	if !f.noProjectContext {
		if block, n, perr := loadProjectContext(ctx, root, os.Getenv); perr != nil {
			warns = append(warns, "project context disabled: "+perr.Error())
		} else if block != "" {
			baseSystem = baseSystem + "\n\n" + block
			projectContextLine = fmt.Sprintf("project context: loaded %d file(s)", n)
		}
	}

	var sessn *session
	var sessionLine string
	if !f.noSession {
		switch dbPath, derr := sessionDBPathForWorkspace(os.Getenv, root); {
		case derr != nil:
			warns = append(warns, "session disabled: "+derr.Error())
		default:
			if id, ierr := resolveSessionID(sessionIDOpts{fresh: f.fresh, explicit: f.sessionID, root: root}); ierr != nil {
				warns = append(warns, "session disabled: "+ierr.Error())
			} else if s, info, oerr := openSession(ctx, dbPath, id); oerr != nil {
				warns = append(warns, "session disabled: "+oerr.Error())
			} else {
				sessn = s
				sessionLine = info.line()
			}
		}
	}
	defer func() { _ = sessn.Close() }() // nil-safe

	memoryLine := ""
	if memoryEnabled {
		memoryLine = "memory: enabled"
	}
	for _, line := range startupNotices(startupInfo{
		workspace:          root,
		useRecommend:       plan.useRecommend,
		bootstrapWarns:     bundle.Warnings,
		preflightWarns:     warns,
		retrieveLine:       retrieveLine,
		retrieveOmitted:    retrieveOmitted,
		retrieveRequested:  f.ragDB != "" || f.noRag || rr.suppressNotice,
		sessionLine:        sessionLine,
		projectContextLine: projectContextLine,
		memoryLine:         memoryLine,
		mcpLine:            mcpLine,
	}) {
		_, _ = fmt.Fprintln(stderr, line)
	}

	maxHistoryTokens := f.inputCeiling
	if maxHistoryTokens <= 0 {
		maxHistoryTokens = agent.DefaultInputCeiling
	}
	maxHistoryTokens /= 2
	summarizeChain, err := resolveSummarizeChain(bundle.Config)
	if err != nil {
		return err
	}

	caller := newRouterChainCaller(bundle.Router, plan.chain)

	obsv, err := newObserv(os.Getenv, root, f.trace, f.telemetry, time.Now)
	if err != nil {
		return fmt.Errorf("golem: observability setup: %w", err)
	}

	budget := agent.Budget{InputCeiling: f.inputCeiling, OutputReserve: f.outputReserve}
	if f.pressureWarn > 0 {
		w := float64(f.pressureWarn) / 100
		// Keep the triplet monotonic for any chosen warn so normalize() does not
		// fall back to defaults: Watch <= Warn <= Critical.
		budget.Pressure = agent.PressureThresholds{
			Watch:    math.Min(0.60, w),
			Warn:     w,
			Critical: math.Max(0.90, w),
		}
	}
	sess := &replSession{
		orch:            agent.New(caller, agent.ContextManager{}),
		tools:           tools,
		baseSystem:      baseSystem,
		maxSteps:        f.maxSteps,
		budget:          budget,
		color:           !f.noColor,
		retrieveOmitted: retrieveOmitted,
		session:         sessn,
		compress: compressPolicy{
			summarize:          agent.NewRouterSummarizer(bundle.Router, summarizeChain),
			estimate:           conversation.CharRatioEstimator(4.0),
			maxHistoryTokens:   maxHistoryTokens,
			minRecentExchanges: 4,
			enabled:            !f.noCompress,
		},
		journal:      journal,
		allowWrite:   f.allowWrite,
		allowExec:    f.allowExec,
		mcpAttached:  mcpAttached,
		memory:       memStore,
		memoryDBPath: memDBPath,
		workspaceID:  workspaceID(root),
		obs:          obsv,
		pressureWarn: f.pressureWarn > 0,
	}
	if sess.maxSteps == 0 {
		sess.maxSteps = 16 // mirror agent defaultMaxSteps so the footer's k/max is accurate
	}

	interrupts := make(chan struct{}, 1)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, interruptSignals()...)
	defer signal.Stop(sigCh)
	go func() {
		for range sigCh {
			select {
			case interrupts <- struct{}{}:
			default:
			}
		}
	}()

	return runREPL(ctx, stdin, stdout, interrupts, sess)
}

func interruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
