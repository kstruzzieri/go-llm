package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
)

type flags struct {
	configPath    string
	root          string
	ollamaURL     string
	maxSteps      int
	inputCeiling  int
	outputReserve int
	noColor       bool
}

func parseFlags(args []string) (flags, error) {
	var f flags
	fs := flag.NewFlagSet("golem", flag.ContinueOnError)
	fs.StringVar(&f.configPath, "config", "", "path to models.json (default: auto-discover)")
	fs.StringVar(&f.root, "root", ".", "workspace root the tools are scoped to")
	fs.StringVar(&f.ollamaURL, "ollama-url", "", "override Ollama base URL")
	fs.IntVar(&f.maxSteps, "max-steps", 0, "max agent steps per prompt (0 => default 16)")
	fs.IntVar(&f.inputCeiling, "input-ceiling", 0, "token input ceiling (0 => default)")
	fs.IntVar(&f.outputReserve, "output-reserve", 0, "token output reserve")
	fs.BoolVar(&f.noColor, "no-color", false, "disable dim ANSI footers")
	if err := fs.Parse(args); err != nil {
		return flags{}, err
	}
	return f, nil
}

type startupInfo struct {
	workspace       string
	useRecommend    bool
	bootstrapWarns  []error
	preflightWarns  []string
	retrieveOmitted bool
}

// startupNotices renders the human-facing startup lines (written to stderr).
func startupNotices(info startupInfo) []string {
	var out []string
	out = append(out, "workspace: "+info.workspace)
	if info.useRecommend {
		out = append(out, "no defaults.agent configured; using model recommendation (run will route to the recommended model)")
	}
	for _, w := range info.bootstrapWarns {
		out = append(out, "warning: "+w.Error())
	}
	for _, w := range info.preflightWarns {
		out = append(out, "warning: "+w)
	}
	if info.retrieveOmitted {
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

	root, err := filepath.Abs(f.root)
	if err != nil {
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

	// retrieve resolution is Task 9; until then it is always omitted.
	tools, err := buildTools(root, nil)
	if err != nil {
		return err
	}
	retrieveOmitted := true

	for _, line := range startupNotices(startupInfo{
		workspace:       root,
		useRecommend:    plan.useRecommend,
		bootstrapWarns:  bundle.Warnings,
		preflightWarns:  warns,
		retrieveOmitted: retrieveOmitted,
	}) {
		_, _ = fmt.Fprintln(stderr, line)
	}

	caller := newRouterChainCaller(bundle.Router, plan.chain)
	sess := &replSession{
		orch:            agent.New(caller, agent.ContextManager{}),
		tools:           tools,
		baseSystem:      golemSystemPrompt,
		maxSteps:        f.maxSteps,
		budget:          agent.Budget{InputCeiling: f.inputCeiling, OutputReserve: f.outputReserve},
		color:           !f.noColor,
		retrieveOmitted: retrieveOmitted,
	}
	if sess.maxSteps == 0 {
		sess.maxSteps = 16 // mirror agent defaultMaxSteps so the footer's k/max is accurate
	}

	interrupts := make(chan struct{}, 1)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
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
