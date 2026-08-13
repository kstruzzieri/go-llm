package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/feedback"
	"github.com/kstruzzieri/go-llm/rag"
)

const (
	feedbackQueueSize        = 128
	feedbackOperationTimeout = time.Second
	feedbackCloseTimeout     = 2 * time.Second

	dropOverflowNewest     = "overflow-newest"
	dropQueuedAfterDisable = "queued-after-disable"
	dropQueuedAfterTimeout = "queued-after-timeout"
	dropDisabledAdmission  = "disabled-admission"
	dropOperationFailed    = "operation-failed"
)

type feedbackReport struct {
	attempted              int
	completed              int
	dropped                int
	reasons                map[string]int
	presentationDuplicates int
	presentationJoinMisses int
}

type feedbackNotifier struct {
	mu sync.RWMutex
	fn func(string)
}

func newFeedbackNotifier(fn func(string)) *feedbackNotifier {
	if fn == nil {
		fn = func(string) {}
	}
	return &feedbackNotifier{fn: fn}
}

func (n *feedbackNotifier) set(fn func(string)) {
	if fn == nil {
		return
	}
	n.mu.Lock()
	n.fn = fn
	n.mu.Unlock()
}

func (n *feedbackNotifier) notify(line string) {
	n.mu.RLock()
	fn := n.fn
	n.mu.RUnlock()
	fn(line)
}

type feedbackEventKind uint8

const (
	feedbackRetrieve feedbackEventKind = iota
	feedbackPresentation
	feedbackRead
	feedbackFinishRun
)

type feedbackEvent struct {
	kind    feedbackEventKind
	counted bool
	at      time.Time
	runID   string
	step    int
	callID  string
	query   string
	path    string
	sources []agent.RetrievedSource
}

type feedbackJoinKey struct {
	runID, callID string
}

type feedbackPending struct {
	query string
	step  int
}

type feedbackActive struct {
	runID       string
	retrievalID string
	step        int
	credited    map[string]bool
}

type feedbackPathJoin struct {
	retrieval *feedbackActive
	keys      []string
}

type feedbackService struct {
	root      string
	collector *feedback.Collector
	weighter  rag.BehavioralWeighter
	writer    *sql.DB
	reader    *feedback.SQLiteWeightReader

	events chan feedbackEvent
	stop   chan struct{}
	done   chan struct{}
	cancel context.CancelFunc
	now    func() time.Time
	warn   *feedbackNotifier
	ticks  <-chan time.Time

	workerGate    func(string)
	admissionGate func()
	closeTimeout  time.Duration
	closeReader   func() error
	closeWriter   func() error

	admissionMu  sync.Mutex
	stopped      bool
	disabled     bool
	disabledNow  atomic.Bool
	overflowWarn atomic.Bool
	timedOut     atomic.Bool
	report       feedbackReport
	closeAt      time.Time

	closeOnce   sync.Once
	closeReport feedbackReport
	closeErr    error
}

type feedbackObserver struct {
	service *feedbackService
	runID   string
}

var (
	_ agent.Observer                      = (*feedbackObserver)(nil)
	_ agent.ToolResultObserver            = (*feedbackObserver)(nil)
	_ agent.RetrievalPresentationObserver = (*feedbackObserver)(nil)
)

func normalizeFeedbackPath(root, name string) (string, bool) {
	if name == "" || strings.IndexByte(name, 0) >= 0 {
		return "", false
	}
	name = filepath.FromSlash(strings.ReplaceAll(name, `\`, "/"))
	root = filepath.Clean(root)
	full := filepath.Clean(name)
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, full)
	}
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func openConfiguredFeedback(ctx context.Context, enabled bool, root, explicitDB string, getenv func(string) string, warn func(string), open func(context.Context, string, string, func(string)) (*feedbackService, error)) (*feedbackService, string) {
	if !enabled {
		return nil, ""
	}
	dbPath := explicitDB
	var err error
	if dbPath == "" {
		dbPath, err = feedbackDBPathForWorkspace(getenv, root)
	} else {
		err = validatePathOutsideWorkspace(dbPath, root)
	}
	if err != nil {
		return nil, "behavioral feedback disabled: " + err.Error()
	}
	svc, err := open(ctx, root, dbPath, warn)
	if err != nil {
		return nil, "behavioral feedback disabled: " + err.Error()
	}
	return svc, ""
}

func openFeedbackService(ctx context.Context, root, dbPath string, warn func(string)) (*feedbackService, error) {
	if err := prepareDBFile(dbPath); err != nil {
		return nil, err
	}
	writer, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", dbPath, err)
	}
	writer.SetMaxOpenConns(1)
	closeWriter := true
	defer func() {
		if closeWriter {
			_ = writer.Close()
		}
	}()
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=1000"} {
		if _, err := writer.ExecContext(ctx, pragma); err != nil {
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	store, err := feedback.NewSignalStore(ctx, writer)
	if err != nil {
		return nil, err
	}
	// NewSQLiteWeightReader's immutable schema preflight cannot read WAL pages.
	if _, err := writer.ExecContext(ctx, "PRAGMA wal_checkpoint(FULL)"); err != nil {
		return nil, fmt.Errorf("checkpoint feedback migrations: %w", err)
	}
	if err := chmodDBFiles(dbPath); err != nil {
		return nil, err
	}
	collector := feedback.NewManualCollector(store, feedback.DefaultConfig())
	reader, err := feedback.NewSQLiteWeightReader(ctx, dbPath, feedback.DefaultConfig())
	if err != nil {
		collector.DiscardOpen()
		return nil, err
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	s := &feedbackService{
		root: root, collector: collector, weighter: reader, writer: writer, reader: reader,
		events: make(chan feedbackEvent, feedbackQueueSize), stop: make(chan struct{}), done: make(chan struct{}),
		cancel: cancel, now: time.Now, warn: newFeedbackNotifier(warn),
		report: feedbackReport{reasons: make(map[string]int)},
	}
	s.closeReader = reader.Close
	s.closeWriter = writer.Close
	closeWriter = false
	go s.work(workerCtx)
	return s, nil
}

func (s *feedbackService) behavioralWeighter() rag.BehavioralWeighter {
	if s == nil {
		return nil
	}
	return s.weighter
}

func (s *feedbackService) observer(runID string) agent.Observer {
	return &feedbackObserver{service: s, runID: runID}
}

func (s *feedbackService) finishRun(runID string) {
	if s == nil || runID == "" {
		return
	}
	s.admit(feedbackEvent{kind: feedbackFinishRun, runID: runID}, false)
}

func (o *feedbackObserver) OnStep(context.Context, agent.StepEvent) error         { return nil }
func (o *feedbackObserver) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }
func (o *feedbackObserver) OnToken(context.Context, agent.TokenEvent) error       { return nil }

func (o *feedbackObserver) OnToolResult(_ context.Context, e agent.ToolResultEvent) error {
	if o == nil || o.service == nil || o.runID == "" || !e.Invoked || e.Denied || e.Result.IsError {
		return nil
	}
	switch e.Call.Function.Name {
	case "retrieve":
		var args struct {
			Query string `json:"query"`
		}
		if e.Call.ID == "" || json.Unmarshal(e.Call.Function.Arguments, &args) != nil || strings.TrimSpace(args.Query) == "" {
			return nil
		}
		o.service.admit(feedbackEvent{kind: feedbackRetrieve, counted: true, at: o.service.now(), runID: o.runID, step: e.Step, callID: e.Call.ID, query: args.Query}, true)
	case "read_file":
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(e.Call.Function.Arguments, &args) != nil {
			return nil
		}
		path, ok := normalizeFeedbackPath(o.service.root, args.Path)
		if !ok {
			return nil
		}
		o.service.admit(feedbackEvent{kind: feedbackRead, counted: true, at: o.service.now(), runID: o.runID, step: e.Step, callID: e.Call.ID, path: path}, true)
	}
	return nil
}

func (o *feedbackObserver) OnRetrievalPresentation(_ context.Context, e agent.RetrievalPresentationEvent) error {
	if o == nil || o.service == nil || o.runID == "" || e.ToolCallID == "" || len(e.Attribution.Sources) == 0 {
		return nil
	}
	sources := make([]agent.RetrievedSource, 0, len(e.Attribution.Sources))
	for _, src := range e.Attribution.Sources {
		path, ok := normalizeFeedbackPath(o.service.root, src.Source)
		if !ok || strings.TrimSpace(src.StableKey) == "" {
			continue
		}
		src.Source = path
		sources = append(sources, src)
	}
	if len(sources) == 0 {
		return nil
	}
	o.service.admit(feedbackEvent{kind: feedbackPresentation, counted: true, at: o.service.now(), runID: o.runID, step: e.Step, callID: e.ToolCallID, sources: sources}, true)
	return nil
}

func (s *feedbackService) admit(e feedbackEvent, counted bool) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.admissionGate != nil {
		s.admissionGate()
	}
	if s.stopped {
		return
	}
	if counted {
		s.report.attempted++
	}
	if s.disabled {
		if counted {
			s.report.dropped++
			s.report.reasons[dropDisabledAdmission]++
		}
		return
	}
	select {
	case s.events <- e:
	default:
		if counted {
			s.report.dropped++
			s.report.reasons[dropOverflowNewest]++
		}
		s.overflowWarn.CompareAndSwap(false, true)
		s.disabled = true
		s.disabledNow.Store(true)
	}
}

func (s *feedbackService) complete() {
	s.admissionMu.Lock()
	s.report.completed++
	s.admissionMu.Unlock()
}

func (s *feedbackService) drop(reason string) {
	s.admissionMu.Lock()
	s.report.dropped++
	s.report.reasons[reason]++
	s.admissionMu.Unlock()
}

func (s *feedbackService) disable(class string, err error) {
	s.admissionMu.Lock()
	first := !s.disabled
	s.disabled = true
	s.disabledNow.Store(true)
	s.admissionMu.Unlock()
	if first && err != nil {
		if s.workerGate != nil {
			s.workerGate("before-warning")
		}
		s.warn.notify("warning: behavioral feedback recording disabled (" + class + "): " + err.Error())
	}
}

func (s *feedbackService) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, feedbackOperationTimeout)
}

func (s *feedbackService) work(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(feedback.SweepInterval)
	defer ticker.Stop()
	ticks := ticker.C
	if s.ticks != nil {
		ticks = s.ticks
	}
	gate := func(point string) {
		if s.workerGate != nil {
			s.workerGate(point)
		}
	}
	warnOverflow := func() {
		if !s.overflowWarn.Load() {
			return
		}
		s.admissionMu.Lock()
		requested := s.overflowWarn.Swap(false)
		s.admissionMu.Unlock()
		if requested {
			gate("before-warning")
			s.warn.notify("warning: behavioral feedback recording disabled (overflow): event queue full")
		}
	}
	pending := make(map[feedbackJoinKey]feedbackPending)
	presented := make(map[feedbackJoinKey]bool)
	paths := make(map[string][]feedbackPathJoin)

	removeRetrievals := func(ids []string) {
		if len(ids) == 0 {
			return
		}
		removed := make(map[string]bool, len(ids))
		for _, id := range ids {
			removed[id] = true
		}
		for path, joins := range paths {
			kept := joins[:0]
			for _, join := range joins {
				if !removed[join.retrieval.retrievalID] {
					kept = append(kept, join)
				}
			}
			if len(kept) == 0 {
				delete(paths, path)
			} else {
				paths[path] = kept
			}
		}
	}
	discard := func() {
		pending = make(map[feedbackJoinKey]feedbackPending)
		presented = make(map[feedbackJoinKey]bool)
		paths = make(map[string][]feedbackPathJoin)
		s.collector.DiscardOpen()
	}
	abandon := func(reason string) {
		for {
			select {
			case e := <-s.events:
				if e.counted {
					s.drop(reason)
				}
			default:
				discard()
				return
			}
		}
	}
	sweep := func(now time.Time) error {
		opctx, cancel := s.operationContext(ctx)
		ids, err := s.collector.SweepExpired(opctx, now)
		cancel()
		removeRetrievals(ids)
		return err
	}

	handle := func(e feedbackEvent) {
		switch e.kind {
		case feedbackRetrieve:
			pending[feedbackJoinKey{e.runID, e.callID}] = feedbackPending{query: e.query, step: e.step}
			s.complete()
		case feedbackPresentation:
			key := feedbackJoinKey{e.runID, e.callID}
			if presented[key] {
				s.admissionMu.Lock()
				s.report.presentationDuplicates++
				s.admissionMu.Unlock()
				s.complete()
				return
			}
			meta, ok := pending[key]
			if !ok || e.step <= meta.step {
				s.admissionMu.Lock()
				s.report.presentationJoinMisses++
				s.admissionMu.Unlock()
				s.complete()
				return
			}
			seen := make(map[string]bool)
			keys := make([]string, 0, len(e.sources))
			byPath := make(map[string][]string)
			for _, src := range e.sources {
				if !seen[src.StableKey] {
					seen[src.StableKey] = true
					keys = append(keys, src.StableKey)
				}
				if !containsString(byPath[src.Source], src.StableKey) {
					byPath[src.Source] = append(byPath[src.Source], src.StableKey)
				}
			}
			opctx, cancel := s.operationContext(ctx)
			id, err := s.collector.RegisterRetrievalAt(opctx, meta.query, keys, e.at)
			cancel()
			if err != nil {
				s.drop(dropOperationFailed)
				s.disable("write", err)
				return
			}
			delete(pending, key)
			presented[key] = true
			active := &feedbackActive{runID: e.runID, retrievalID: id, step: meta.step, credited: make(map[string]bool)}
			for path, pathKeys := range byPath {
				paths[path] = append(paths[path], feedbackPathJoin{retrieval: active, keys: pathKeys})
			}
			s.complete()
		case feedbackRead:
			joins := paths[e.path]
			for _, join := range joins {
				if join.retrieval.runID != e.runID || e.step <= join.retrieval.step {
					continue
				}
				keys := make([]string, 0, len(join.keys))
				for _, key := range join.keys {
					if !join.retrieval.credited[key] {
						keys = append(keys, key)
					}
				}
				if len(keys) == 0 {
					continue
				}
				opctx, cancel := s.operationContext(ctx)
				committed, err := s.collector.RecordAt(opctx, feedback.Signal{Kind: feedback.SignalFileOpened, RetrievalID: join.retrieval.retrievalID, ChunkKeys: keys, Timestamp: e.at}, e.at)
				cancel()
				if committed {
					for _, key := range keys {
						join.retrieval.credited[key] = true
					}
				}
				if err != nil {
					if committed {
						s.complete()
						s.disable("maintenance", err)
					} else {
						s.drop(dropOperationFailed)
						s.disable("write", err)
					}
					return
				}
			}
			s.complete()
		case feedbackFinishRun:
			for key := range pending {
				if key.runID == e.runID {
					delete(pending, key)
				}
			}
			for key := range presented {
				if key.runID == e.runID {
					delete(presented, key)
				}
			}
			for path, joins := range paths {
				kept := joins[:0]
				for _, join := range joins {
					if join.retrieval.runID != e.runID {
						kept = append(kept, join)
					}
				}
				if len(kept) == 0 {
					delete(paths, path)
				} else {
					paths[path] = kept
				}
			}
		}
	}

	for {
		warnOverflow()
		gate("after-overflow-warning-probe")
		if s.timedOut.Load() {
			abandon(dropQueuedAfterTimeout)
			return
		}
		if s.disabledNow.Load() {
			warnOverflow()
			abandon(dropQueuedAfterDisable)
			select {
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			}
		}
		gate("before-select")
		select {
		case <-ctx.Done():
			warnOverflow()
			abandon(dropQueuedAfterTimeout)
			return
		case <-ticks:
			gate("after-tick")
			s.admissionMu.Lock()
			stopped := s.stopped
			sweepAt := s.now()
			s.admissionMu.Unlock()
			if stopped {
				continue
			}
			gate("before-tick-sweep")
			if err := sweep(sweepAt); err != nil {
				s.disable("maintenance", err)
			}
		case <-s.stop:
			warnOverflow()
			for {
				if s.timedOut.Load() {
					abandon(dropQueuedAfterTimeout)
					return
				}
				gate("after-stop-precheck")
				select {
				case e := <-s.events:
					if s.timedOut.Load() {
						if e.counted {
							s.drop(dropQueuedAfterTimeout)
						}
						abandon(dropQueuedAfterTimeout)
						return
					}
					if s.disabledNow.Load() {
						if e.counted {
							s.drop(dropQueuedAfterDisable)
						}
						continue
					}
					handle(e)
				default:
					if s.disabledNow.Load() {
						discard()
						return
					}
					gate("before-final-sweep")
					if s.timedOut.Load() {
						discard()
						return
					}
					if err := sweep(s.closeAt); err != nil {
						discard()
						s.closeErr = err
						return
					}
					s.collector.DiscardOpen()
					return
				}
			}
		case e := <-s.events:
			gate("after-dequeue")
			if s.timedOut.Load() {
				if e.counted {
					s.drop(dropQueuedAfterTimeout)
				}
				abandon(dropQueuedAfterTimeout)
				return
			}
			if s.disabledNow.Load() {
				warnOverflow()
				if e.counted {
					s.drop(dropQueuedAfterDisable)
				}
				abandon(dropQueuedAfterDisable)
				continue
			}
			gate("before-handle")
			handle(e)
		}
	}
}

func (s *feedbackService) close() (feedbackReport, error) {
	if s == nil {
		return feedbackReport{}, nil
	}
	s.closeOnce.Do(func() {
		s.admissionMu.Lock()
		s.stopped = true
		s.closeAt = s.now()
		s.admissionMu.Unlock()
		close(s.stop)
		timeout := s.closeTimeout
		if timeout <= 0 {
			timeout = feedbackCloseTimeout
		}
		timer := time.NewTimer(timeout)
		select {
		case <-s.done:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			s.timedOut.Store(true)
			s.cancel()
			<-s.done
		}
		s.cancel()
		if s.closeReader != nil {
			if err := s.closeReader(); s.closeErr == nil {
				s.closeErr = err
			}
		}
		if s.closeWriter != nil {
			if err := s.closeWriter(); s.closeErr == nil {
				s.closeErr = err
			}
		}
		s.admissionMu.Lock()
		s.closeReport = s.report
		s.closeReport.reasons = make(map[string]int, len(s.report.reasons))
		for reason, count := range s.report.reasons {
			s.closeReport.reasons[reason] = count
		}
		s.admissionMu.Unlock()
	})
	return s.closeReport, s.closeErr
}

type behavioralWeighterHandle struct {
	weighter rag.BehavioralWeighter
	db       *sql.DB
}

// feedbackDBPathForWorkspace resolves the per-workspace writable behavioral
// feedback DB path (<base>/golem/retrieval-feedback/<key>.db), mirroring
// indexDBPathForWorkspace. It is validated to live outside the workspace so it
// is never indexed or edited.
func feedbackDBPathForWorkspace(getenv func(string) string, root string) (string, error) {
	base, err := dataDirBase(getenv)
	if err != nil {
		return "", err
	}
	key := strings.TrimPrefix(workspaceID(root), "workspace:")
	dbPath := filepath.Join(base, "golem", "retrieval-feedback", key+".db")
	if err := validatePathOutsideWorkspace(dbPath, root); err != nil {
		return "", err
	}
	return dbPath, nil
}

// openBehavioralWeighter best-effort opens the feedback DB and returns a
// consume-only weighter handle. It NEVER returns an error: on any failure it
// returns a nil handle and a human-readable warning, so retrieval proceeds with
// neutral ranking. It may run feedback schema migrations, but it never records
// retrievals or outcomes. Callers own the returned DB handle and must close it
// during normal shutdown.
func openBehavioralWeighter(ctx context.Context, dbPath string) (*behavioralWeighterHandle, string) {
	if err := prepareDBFile(dbPath); err != nil {
		return nil, "behavioral feedback disabled: " + err.Error()
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Sprintf("behavioral feedback disabled: open %q: %v", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Sprintf("behavioral feedback disabled: %s: %v", pragma, err)
		}
	}
	store, err := feedback.NewSignalStore(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Sprintf("behavioral feedback disabled: init %q: %v", dbPath, err)
	}
	if err := chmodDBFiles(dbPath); err != nil {
		_ = db.Close()
		return nil, "behavioral feedback disabled: " + err.Error()
	}
	return &behavioralWeighterHandle{
		weighter: feedback.NewWeightReader(store, feedback.DefaultConfig()),
		db:       db,
	}, ""
}
