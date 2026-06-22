package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
)

type flags struct {
	configPath    string
	root          string
	ollamaURL     string
	ragDB         string
	maxSteps      int
	inputCeiling  int
	outputReserve int
	noColor       bool
	noSession     bool
	fresh         bool
	sessionID     string
	allowWrite    bool
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
	fs.StringVar(&f.sessionID, "session", "", "explicit session id to resume or create (default: per-workspace)")
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
	return nil
}

type startupInfo struct {
	workspace         string
	useRecommend      bool
	bootstrapWarns    []error
	preflightWarns    []string
	retrieveOmitted   bool
	retrieveRequested bool // -rag-db was given; suppress the generic no-index notice
	sessionLine       string
}

// startupNotices renders the human-facing startup lines (written to stderr).
func startupNotices(info startupInfo) []string {
	var out []string
	out = append(out, "workspace: "+info.workspace)
	if info.sessionLine != "" {
		out = append(out, info.sessionLine)
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
		_, _ = fmt.Fprintf(os.Stderr, "golem: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin *os.File, stdout, stderr *os.File) error {
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

	warns, err := preflightToolCapable(ctx, bundle.Models, plan.chain)
	if err != nil {
		return err
	}

	var retrieve agent.Tool
	if t, rerr := resolveRetriever(ctx, bundle.Config, bundle.Router, f.ragDB); rerr != nil {
		// -rag-db was given but retrieve could not be enabled: surface why
		// instead of swallowing it behind the generic "no RAG index" notice.
		warns = append(warns, "retrieve disabled: "+rerr.Error())
	} else {
		retrieve = t
	}
	tools, err := buildTools(root, retrieve)
	if err != nil {
		return err
	}
	retrieveOmitted := retrieve == nil

	var journal *mutationJournal
	if f.allowWrite {
		wt, j, werr := buildWriteTools(root)
		if werr != nil {
			return werr
		}
		tools = append(tools, wt...)
		journal = j
	}

	baseSystem := golemSystemPrompt
	if f.allowWrite {
		baseSystem = golemWriteSystemPrompt
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

	for _, line := range startupNotices(startupInfo{
		workspace:         root,
		useRecommend:      plan.useRecommend,
		bootstrapWarns:    bundle.Warnings,
		preflightWarns:    warns,
		retrieveOmitted:   retrieveOmitted,
		retrieveRequested: f.ragDB != "",
		sessionLine:       sessionLine,
	}) {
		_, _ = fmt.Fprintln(stderr, line)
	}

	caller := newRouterChainCaller(bundle.Router, plan.chain)
	sess := &replSession{
		orch:            agent.New(caller, agent.ContextManager{}),
		tools:           tools,
		baseSystem:      baseSystem,
		maxSteps:        f.maxSteps,
		budget:          agent.Budget{InputCeiling: f.inputCeiling, OutputReserve: f.outputReserve},
		color:           !f.noColor,
		retrieveOmitted: retrieveOmitted,
		session:         sessn,
		journal:         journal,
		allowWrite:      f.allowWrite,
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
