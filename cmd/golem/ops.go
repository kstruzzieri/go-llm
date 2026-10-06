package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/internal/opsbackend"
	"github.com/kstruzzieri/go-llm/internal/opsview"
)

// opsInterval is the -watch and serve collection cadence.
const opsInterval = 2 * time.Second

// runOps implements `golem ops`: a read-only view of what local backends
// report about every configured model (spec §6.1). It exits 0 whenever a
// snapshot rendered: an unreachable backend is an observation, not a failure.
func runOps(ctx context.Context, args []string, stdin io.Reader, out, errOut io.Writer) error {
	var (
		configPath string
		jsonOut    bool
		watch      bool
	)
	fs := flag.NewFlagSet("golem ops", flag.ContinueOnError)
	fs.StringVar(&configPath, "config", "", "path to models.json (default: auto-discover)")
	fs.BoolVar(&jsonOut, "json", false, "emit the opsview v1 snapshot as JSON")
	fs.BoolVar(&watch, "watch", false, "redraw every second in the terminal (requires a TTY)")
	if err := parseQuietly(fs, args, errOut); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("golem ops: %w", err)
	}
	if fs.NArg() > 0 {
		return errors.New("golem ops: unexpected argument (run with -help for usage)")
	}
	if jsonOut && watch {
		return errors.New("golem ops: -json cannot be combined with -watch")
	}
	if watch {
		fd, ok := terminalFd(out)
		if !ok {
			return errors.New("golem ops: -watch needs a terminal on stdout")
		}
		src, err := newOpsSource(configPath, opsInterval)
		if err != nil {
			return opsLoadError("golem ops", err)
		}
		// SIGTERM too: its default action would exit with the alternate
		// screen active and the cursor hidden.
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runOpsWatch(ctx, src, out, fd, realTermOps{}, os.Getenv)
	}
	src, err := newOpsSource(configPath, 0)
	if err != nil {
		return opsLoadError("golem ops", err)
	}
	view := src.view(src.tick(ctx), opsview.ModeOnce)
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}
	width := 0
	if fd, ok := terminalFd(out); ok {
		if w, _, err := (realTermOps{}).GetSize(fd); err == nil {
			width = w
		}
	}
	return renderOpsTable(out, view, 0, width)
}

// opsLoadError reports a configuration failure by its bounded diagnostic
// only: config errors can quote a raw base_url (url.ParseRequestURI), and
// golem ops never echoes configuration text.
func opsLoadError(prefix string, err error) error {
	if d, ok := config.DiagnosticOf(err); ok {
		msg := string(d.Code)
		if d.Subject != "" { // already control-free and bounded by config
			msg += " " + d.Subject
		}
		return fmt.Errorf("%s: models.json not loaded: %s", prefix, msg)
	}
	return fmt.Errorf("%s: models.json not loaded", prefix)
}

// terminalFd returns w's descriptor when w is a terminal.
func terminalFd(w io.Writer) (int, bool) {
	f, ok := w.(*os.File)
	if !ok || !(realTermOps{}).IsTerminal(int(f.Fd())) {
		return 0, false
	}
	return int(f.Fd()), true
}

// opsSource joins one loaded configuration to a collector.
type opsSource struct {
	cfg        configview.Snapshot
	configured []string
	revision   string
	interval   time.Duration
	clock      opsbackend.Clock
	collector  *opsbackend.Collector
}

// newOpsSource loads configuration and builds the collector without I/O to
// any backend. A configless run (auto-discovery found nothing) observes
// nothing and projects configview's not-ready snapshot.
func newOpsSource(configPath string, interval time.Duration) (*opsSource, error) {
	doc, err := loadDocumentFor(configPath)
	if err != nil {
		return nil, err
	}
	s := &opsSource{
		cfg:      configview.Build(modelsJSONInput(doc, configview.Inventory{})),
		interval: interval,
		clock:    opsbackend.SystemClock(),
	}
	var specs []opsbackend.BackendSpec
	if doc != nil {
		cfg := doc.Config()
		s.revision = doc.Revision()
		set := map[string]bool{}
		for _, m := range cfg.Models {
			set[m.Provider+"/"+m.Name] = true
		}
		s.configured = slices.Sorted(maps.Keys(set))
		for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
			p := cfg.Providers[name]
			specs = append(specs, opsbackend.BackendSpec{Provider: name, BaseURL: p.BaseURL, APIFormat: p.APIFormat, APIKey: p.APIKey})
		}
	}
	s.collector = opsbackend.NewCollector(specs, opsbackend.Options{Interval: interval, Clock: s.clock})
	return s, nil
}

func (s *opsSource) tick(ctx context.Context) opsbackend.Observations { return s.collector.Tick(ctx) }

// view projects obs at the current instant. Renderers call it on every frame
// or request, so staleness and attention are judged against now, not against
// the moment of collection. A clock jump since collection drops every
// reading first.
func (s *opsSource) view(obs opsbackend.Observations, mode opsview.Mode) opsview.Snapshot {
	if obs.SkewSince(s.clock) > time.Second {
		obs = obs.Dropped()
	}
	return opsview.Build(opsview.Input{
		Config: s.cfg, Configured: s.configured, Revision: s.revision, Observations: obs,
		Now: s.clock.Wall(), NowMono: s.clock.Mono(), Mode: mode, Interval: s.interval,
	})
}
