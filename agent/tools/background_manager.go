package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// Background-exec policy constants (#346), the bounded retained-set policy:
// at most 4 concurrently running jobs, the newest 8 completed jobs retained
// for inspection, and a 64 KiB tail ring per stream per job.
const (
	backgroundActiveCap   = 4
	backgroundRetainedCap = 8
	backgroundRingCap     = 64 * 1024 // per stream per job
)

// Job lifecycle states as surfaced in JobStatus.State.
const (
	backgroundStateRunning = "running"
	backgroundStateExited  = "exited"
	backgroundStateKilled  = "killed"
)

// Sentinel errors for the manager's frozen error classes, so tools can
// errors.Is-classify without string matching. The rendered text is frozen;
// do not reword.
var (
	errBackgroundShutDown      = errors.New("background manager is shut down")
	errBackgroundUnknownHandle = errors.New("unknown background job handle")
)

// errUnknownJobHandle wraps errBackgroundUnknownHandle with the offending
// handle, rendering the frozen "unknown background job handle %q" text.
func errUnknownJobHandle(handle string) error {
	return fmt.Errorf("%w %q", errBackgroundUnknownHandle, handle)
}

// JobStatus is a point-in-time snapshot of one background job. Every snapshot
// is a value copy: Argv never aliases manager state, and no process object or
// buffer reference escapes.
type JobStatus struct {
	Handle    string
	PID       int
	Argv      []string // copied; never aliases internal state
	Cwd       string   // display source; tools render it quoted
	State     string   // "running" | "exited" | "killed"
	ExitCode  int      // meaningful only when ExitKnown
	ExitKnown bool

	StdoutFloor, StdoutEnd uint64
	StderrFloor, StderrEnd uint64
}

// tailChunk is one tail read through the manager: the stream bytes plus the
// job snapshot the reader needs to render them.
type tailChunk struct {
	Status     JobStatus
	Data       []byte
	NextCursor uint64
	Dropped    uint64
	EOF        bool
}

// backgroundJob is the manager's record of one started process. proc, the
// rings, and done are set at registration and immutable after; the remaining
// mutable fields are guarded by the manager mutex. done closes only after the
// job's exit, retention, and active-cap bookkeeping are published.
type backgroundJob struct {
	handle string
	pid    int
	argv   []string
	cwd    string
	proc   backgroundProcess
	stdout *tailRing
	stderr *tailRing
	done   chan struct{}

	running       bool
	killRequested bool
	state         string
	exitCode      int
	exitKnown     bool
}

// BackgroundManager owns every detached background command for one host:
// admission against the active cap, opaque 128-bit handles, per-job output
// rings, reaping, retention of completed jobs, and a serialized shutdown that
// leaves no process behind. Model tools use the package-private start/status/
// tail operations; hosts use the exported List, Stop, and Shutdown.
type BackgroundManager struct {
	backend resolvedExecBackend
	random  io.Reader
	randMu  sync.Mutex

	mu        sync.Mutex
	closed    bool
	active    int // running jobs + reserved in-flight starts
	pending   map[string]struct{}
	jobs      map[string]*backgroundJob
	order     []*backgroundJob // registration order, for stable listings
	completed []*backgroundJob // completion order, oldest first

	startWG      sync.WaitGroup
	shutdownOnce sync.Once
}

// NewBackgroundManager returns a production manager wired to the host
// platform's process starter and crypto/rand entropy — the host sandbox
// runtime. Use NewSandboxedBackgroundManager to select another runtime.
func NewBackgroundManager() *BackgroundManager {
	return newBackgroundManager(newPlatformStarter(), rand.Reader)
}

// NewSandboxedBackgroundManager resolves cfg through the runtime dispatch
// point (#440) and returns a manager whose background starter, foreground
// runner, and approval metadata all derive from that one resolved backend,
// so the exec tool set built over it cannot split runtimes between paths.
// Unimplemented or invalid configs fail closed.
func NewSandboxedBackgroundManager(cfg SandboxConfig) (*BackgroundManager, error) {
	backend, err := newExecBackend(cfg)
	if err != nil {
		return nil, fmt.Errorf("tools: build background manager: %w", err)
	}
	return newBackgroundManagerWithBackend(backend, rand.Reader), nil
}

// newBackgroundManager remains the starter-injection test seam: it wraps the
// supplied starter in a host-policy paired backend. Production runtime
// selection goes through newExecBackend.
func newBackgroundManager(starter backgroundStarter, random io.Reader) *BackgroundManager {
	return newBackgroundManagerWithBackend(newHostExecBackend(starter), random)
}

func newBackgroundManagerWithBackend(backend resolvedExecBackend, random io.Reader) *BackgroundManager {
	return &BackgroundManager{
		backend: backend,
		random:  random,
		pending: map[string]struct{}{},
		jobs:    map[string]*backgroundJob{},
	}
}

// readHandle draws 16 bytes from the injected entropy source and formats the
// opaque job handle. A read failure reserves nothing.
func (m *BackgroundManager) readHandle() (string, error) {
	var b [16]byte
	m.randMu.Lock()
	_, err := io.ReadFull(m.random, b[:])
	m.randMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("generate background job handle: %w", err)
	}
	return "bg-" + hex.EncodeToString(b[:]), nil
}

func (m *BackgroundManager) handleTaken(handle string) bool {
	if _, ok := m.pending[handle]; ok {
		return true
	}
	_, ok := m.jobs[handle]
	return ok
}

// releaseReservation undoes one admitted-but-unregistered start. startWG.Done
// comes last so Shutdown's in-flight-start barrier cannot pass until any late
// process this path reaped is fully gone.
func (m *BackgroundManager) releaseReservation(handle string) {
	m.mu.Lock()
	m.active--
	delete(m.pending, handle)
	m.mu.Unlock()
	m.startWG.Done()
}

// start admits and spawns one background command. Successful registration
// under the mutex is the linearization point: from then on the manager owns
// the process and cancellation no longer revokes ownership. Cancellation or
// closure observed before registration kills and reaps the process
// synchronously and returns no handle.
func (m *BackgroundManager) start(ctx context.Context, spec execSpec, cwdDisplay string) (JobStatus, error) {
	return m.startWrapped(ctx, spec, cwdDisplay, nil)
}

// startWrapped is start with a generic post-start process decorator (#443):
// wrap, when non-nil, is applied to the spawned process immediately after
// backend.Start and before registration, so the wrapped Wait owns per-job
// resource cleanup on every path — including the spawned-but-unregistered
// abandonment below. The manager stays ignorant of what the wrapper does.
func (m *BackgroundManager) startWrapped(ctx context.Context, spec execSpec, cwdDisplay string, wrap func(backgroundProcess) backgroundProcess) (JobStatus, error) {
	handle, err := m.readHandle()
	if err != nil {
		return JobStatus{}, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return JobStatus{}, errBackgroundShutDown
	}
	if m.active >= backgroundActiveCap {
		m.mu.Unlock()
		return JobStatus{}, fmt.Errorf("active background job limit reached (%d); stop one first", backgroundActiveCap)
	}
	for m.handleTaken(handle) {
		if handle, err = m.readHandle(); err != nil {
			m.mu.Unlock()
			return JobStatus{}, err
		}
	}
	m.active++
	m.pending[handle] = struct{}{}
	m.startWG.Add(1)
	m.mu.Unlock()

	if err := ctx.Err(); err != nil {
		m.releaseReservation(handle)
		return JobStatus{}, err
	}

	stdout := newTailRing(backgroundRingCap)
	stderr := newTailRing(backgroundRingCap)
	proc, err := m.backend.Start(spec, stdout, stderr)
	if err != nil {
		m.releaseReservation(handle)
		return JobStatus{}, fmt.Errorf("start background command: %w", err)
	}
	if wrap != nil {
		proc = wrap(proc)
	}

	job := &backgroundJob{
		handle:  handle,
		pid:     proc.PID(),
		argv:    append([]string(nil), spec.Argv...),
		cwd:     cwdDisplay,
		proc:    proc,
		stdout:  stdout,
		stderr:  stderr,
		done:    make(chan struct{}),
		running: true,
		state:   backgroundStateRunning,
	}

	m.mu.Lock()
	if m.closed || ctx.Err() != nil {
		closed := m.closed
		m.mu.Unlock()
		// Spawned but never registered: reap synchronously so no orphan
		// survives, then undo the reservation (which releases startWG and
		// with it any Shutdown blocked on this start).
		_ = proc.Kill()
		_, _, _ = proc.Wait()
		m.releaseReservation(handle)
		if closed {
			return JobStatus{}, errBackgroundShutDown
		}
		return JobStatus{}, ctx.Err()
	}
	delete(m.pending, handle)
	m.jobs[handle] = job
	m.order = append(m.order, job)
	st := m.snapshotLocked(job)
	m.mu.Unlock()
	m.startWG.Done()
	go m.reap(job)
	return st, nil
}

// reap waits for the job's process, then publishes exit state, releases the
// active slot, and applies completed-job retention — all before done closes.
// Nothing that only presents or notifies may ever be added before the close.
func (m *BackgroundManager) reap(job *backgroundJob) {
	code, managerKilled, waitErr := job.proc.Wait()

	m.mu.Lock()
	switch {
	case managerKilled:
		// Manager-initiated kill (Stop or Shutdown).
		job.state = backgroundStateKilled
		job.exitCode = -1
		job.exitKnown = true
	case waitErr == nil || errors.Is(waitErr, exec.ErrWaitDelay):
		// Normal completion, including outside signal-kill (-1) and the
		// exited-with-abandoned-pipes case — never infra.
		job.state = backgroundStateExited
		job.exitCode = code
		job.exitKnown = true
	default:
		// Infra failure: no trustworthy exit code exists.
		job.state = backgroundStateExited
		job.exitCode = -1
		job.exitKnown = false
	}
	job.running = false
	m.active--
	m.completed = append(m.completed, job)
	for len(m.completed) > backgroundRetainedCap {
		oldest := m.completed[0]
		copy(m.completed, m.completed[1:])
		m.completed = m.completed[:len(m.completed)-1]
		delete(m.jobs, oldest.handle)
		for i, j := range m.order {
			if j == oldest {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
	}
	m.mu.Unlock()
	close(job.done)
}

func (m *BackgroundManager) snapshotLocked(job *backgroundJob) JobStatus {
	outFloor, outEnd := job.stdout.Bounds()
	errFloor, errEnd := job.stderr.Bounds()
	return JobStatus{
		Handle:      job.handle,
		PID:         job.pid,
		Argv:        append([]string(nil), job.argv...),
		Cwd:         job.cwd,
		State:       job.state,
		ExitCode:    job.exitCode,
		ExitKnown:   job.exitKnown,
		StdoutFloor: outFloor,
		StdoutEnd:   outEnd,
		StderrFloor: errFloor,
		StderrEnd:   errEnd,
	}
}

// status returns a snapshot of one job, reporting false for handles that were
// never registered or have been evicted from retention.
func (m *BackgroundManager) status(handle string) (JobStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[handle]
	if !ok {
		return JobStatus{}, false
	}
	return m.snapshotLocked(job), true
}

// tail reads one output stream through the job's tail ring. A nil cursor is
// the newest-tail mode; EOF is reported only once completion bookkeeping has
// been published AND the read reached the stream's end. maxBytes validation
// is the calling tool's job; the ring's >= 1 contract applies.
func (m *BackgroundManager) tail(handle, stream string, cursor *uint64, maxBytes int) (tailChunk, error) {
	m.mu.Lock()
	job, ok := m.jobs[handle]
	m.mu.Unlock()
	if !ok {
		return tailChunk{}, errUnknownJobHandle(handle)
	}
	var ring *tailRing
	switch stream {
	case "stdout":
		ring = job.stdout
	case "stderr":
		ring = job.stderr
	default:
		return tailChunk{}, fmt.Errorf("unknown stream %q (want %q or %q)", stream, "stdout", "stderr")
	}
	// Observe publication BEFORE reading the ring: once done is closed the
	// stream can no longer grow, so next == end below really is the end.
	published := false
	select {
	case <-job.done:
		published = true
	default:
	}
	data, next, dropped := ring.Read(cursor, maxBytes)
	m.mu.Lock()
	st := m.snapshotLocked(job)
	m.mu.Unlock()
	_, end := ring.Bounds()
	return tailChunk{
		Status:     st,
		Data:       data,
		NextCursor: next,
		Dropped:    dropped,
		EOF:        published && next == end,
	}, nil
}

// List returns snapshots of every known job — running and retained completed
// — in registration order.
func (m *BackgroundManager) List() []JobStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]JobStatus, 0, len(m.order))
	for _, job := range m.order {
		out = append(out, m.snapshotLocked(job))
	}
	return out
}

// Stop kills one running job and waits for its completion to be published. A
// job that already finished is a no-op returning its final status. If ctx
// expires while the process is still being reaped, Stop returns the current
// snapshot with the context error; the reaper keeps running and manager
// cleanup still completes. The kill is issued at most once per job across all
// Stop and Shutdown calls.
func (m *BackgroundManager) Stop(ctx context.Context, handle string) (JobStatus, error) {
	m.mu.Lock()
	job, ok := m.jobs[handle]
	if !ok {
		m.mu.Unlock()
		return JobStatus{}, errUnknownJobHandle(handle)
	}
	if !job.running {
		st := m.snapshotLocked(job)
		m.mu.Unlock()
		return st, nil
	}
	needKill := !job.killRequested
	job.killRequested = true
	m.mu.Unlock()
	if needKill {
		_ = job.proc.Kill()
	}
	select {
	case <-job.done:
		m.mu.Lock()
		st := m.snapshotLocked(job)
		m.mu.Unlock()
		return st, nil
	case <-ctx.Done():
		m.mu.Lock()
		st := m.snapshotLocked(job)
		m.mu.Unlock()
		return st, ctx.Err()
	}
}

// Shutdown closes the manager: no new starts are admitted, every in-flight
// start is barriered out, every running job is killed, and every known job's
// completion is awaited. All concurrent callers block until the single
// shutdown body has finished.
func (m *BackgroundManager) Shutdown() {
	m.shutdownOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		// Barrier: admitted starts either register their job or kill and reap
		// their late process before releasing the group.
		m.startWG.Wait()
		m.mu.Lock()
		jobs := make([]*backgroundJob, 0, len(m.jobs))
		var toKill []*backgroundJob
		for _, job := range m.jobs {
			jobs = append(jobs, job)
			// Kill only jobs whose completion is not published, and only if no
			// Stop already issued the kill. The process guard makes a call that
			// races completed Wait cleanup a no-op.
			if job.running && !job.killRequested {
				job.killRequested = true
				toKill = append(toKill, job)
			}
		}
		m.mu.Unlock()
		for _, job := range toKill {
			_ = job.proc.Kill()
		}
		// Wait for every snapshot job — including a reaper already between
		// clearing running and closing done.
		for _, job := range jobs {
			<-job.done
		}
	})
}
