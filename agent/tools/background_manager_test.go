package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- deterministic fakes (no sleeps, no wall-clock coordination) ---

// fakeStarter is the deterministic backgroundStarter for manager tests. The
// closure decides what each Start returns and may block on a channel gate to
// hold the spawn window open; coordination is channel-only.
type fakeStarter struct {
	mu    sync.Mutex
	calls int
	fn    func(spec execSpec, stdout, stderr io.Writer) (backgroundProcess, error)
}

func (f *fakeStarter) Start(spec execSpec, stdout, stderr io.Writer) (backgroundProcess, error) {
	f.mu.Lock()
	f.calls++
	fn := f.fn
	f.mu.Unlock()
	return fn(spec, stdout, stderr)
}

func (f *fakeStarter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeProc is a barrier-gated backgroundProcess: Wait blocks until released.
// By default Kill releases the wait (like a real SIGKILL); tests that need the
// stop-vs-reap window open set killReleases = false before handing it out and
// call release() themselves.
type fakeProc struct {
	pid          int
	code         int
	waitErr      error
	killReleases bool

	mu            sync.Mutex
	kills         int
	waitReturned  bool
	managerKilled bool
	released      chan struct{}
	releaseOnce   sync.Once
	waitObserved  chan struct{}
	waitGate      <-chan struct{}
}

func newFakeProc(pid int) *fakeProc {
	return &fakeProc{pid: pid, killReleases: true, released: make(chan struct{})}
}

func (p *fakeProc) PID() int { return p.pid }

func (p *fakeProc) Wait() (int, bool, error) {
	<-p.released
	p.mu.Lock()
	p.waitReturned = true
	code, managerKilled, err := p.code, p.managerKilled, p.waitErr
	observed, gate := p.waitObserved, p.waitGate
	p.mu.Unlock()
	if observed != nil {
		close(observed)
	}
	if gate != nil {
		<-gate
	}
	return code, managerKilled, err
}

func (p *fakeProc) Kill() error {
	p.mu.Lock()
	p.kills++
	if !p.waitReturned {
		p.code = -1
		p.managerKilled = true
	}
	rel := p.killReleases
	p.mu.Unlock()
	if rel {
		p.release()
	}
	return nil
}

func (p *fakeProc) release() { p.releaseOnce.Do(func() { close(p.released) }) }

func (p *fakeProc) killCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.kills
}

func (p *fakeProc) waitDone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitReturned
}

// starterOf returns a fakeStarter handing out the given procs in order.
func starterOf(procs ...*fakeProc) *fakeStarter {
	var mu sync.Mutex
	i := 0
	return &fakeStarter{fn: func(execSpec, io.Writer, io.Writer) (backgroundProcess, error) {
		mu.Lock()
		defer mu.Unlock()
		p := procs[i]
		i++
		return p, nil
	}}
}

// countingRandom yields a fresh deterministic 16-byte pattern per handle read
// (every byte of read n+1 differs from read n), so handles never collide.
type countingRandom struct{ n byte }

func (r *countingRandom) Read(p []byte) (int, error) {
	r.n++
	for i := range p {
		p[i] = r.n
	}
	return len(p), nil
}

// flakyRandom fails its first failsLeft reads, then delegates.
type flakyRandom struct {
	failsLeft int
	src       io.Reader
}

func (r *flakyRandom) Read(p []byte) (int, error) {
	if r.failsLeft > 0 {
		r.failsLeft--
		return 0, errors.New("entropy exhausted")
	}
	return r.src.Read(p)
}

func bgSpec(argv ...string) execSpec {
	if len(argv) == 0 {
		argv = []string{"helper", "arg"}
	}
	return execSpec{Path: "/bin/helper", Argv: argv, Dir: "/work"}
}

// jobDoneChan white-boxes the job's completion-publication channel.
func jobDoneChan(t *testing.T, m *BackgroundManager, handle string) chan struct{} {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[handle]
	if !ok {
		t.Fatalf("no registered job %q", handle)
	}
	return job.done
}

func activeCount(m *BackgroundManager) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// --- Step 4.1: start paths ---

func TestBackgroundManagerStartSuccess(t *testing.T) {
	proc := newFakeProc(4242)
	m := newBackgroundManager(starterOf(proc), &countingRandom{})
	t.Cleanup(m.Shutdown)

	st, err := m.start(context.Background(), bgSpec("cmd", "a", "b"), "~/proj")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if st.PID != 4242 || st.Cwd != "~/proj" || st.State != backgroundStateRunning {
		t.Errorf("status = %+v, want pid 4242, cwd ~/proj, running", st)
	}
	if st.ExitKnown {
		t.Error("ExitKnown = true for a running job")
	}
	if got, want := fmt.Sprintf("%v", st.Argv), "[cmd a b]"; got != want {
		t.Errorf("Argv = %s, want %s", got, want)
	}
	if st.StdoutFloor != 0 || st.StdoutEnd != 0 || st.StderrFloor != 0 || st.StderrEnd != 0 {
		t.Errorf("fresh job stream bounds = %+v, want all zero", st)
	}
	got, ok := m.status(st.Handle)
	if !ok || got.Handle != st.Handle || got.State != backgroundStateRunning {
		t.Errorf("status(%q) = (%+v, %v)", st.Handle, got, ok)
	}
	if list := m.List(); len(list) != 1 || list[0].Handle != st.Handle {
		t.Errorf("List = %+v, want the one running job", list)
	}
}

func TestBackgroundManagerHandleFormat(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	m := newBackgroundManager(starterOf(newFakeProc(1), newFakeProc(2)), bytes.NewReader(seed))
	t.Cleanup(m.Shutdown)

	st1, err := m.start(context.Background(), bgSpec(), "/w")
	if err != nil {
		t.Fatalf("start 1: %v", err)
	}
	st2, err := m.start(context.Background(), bgSpec(), "/w")
	if err != nil {
		t.Fatalf("start 2: %v", err)
	}
	if st1.Handle != "bg-000102030405060708090a0b0c0d0e0f" {
		t.Errorf("handle 1 = %q, want bg- plus 32 lowercase hex chars of the injected bytes", st1.Handle)
	}
	if st2.Handle != "bg-101112131415161718191a1b1c1d1e1f" {
		t.Errorf("handle 2 = %q, want the next 16 injected bytes", st2.Handle)
	}
}

func TestBackgroundManagerRandomFailureReservesNothing(t *testing.T) {
	starter := starterOf(newFakeProc(1), newFakeProc(2), newFakeProc(3), newFakeProc(4))
	m := newBackgroundManager(starter, &flakyRandom{failsLeft: 1, src: &countingRandom{}})
	t.Cleanup(m.Shutdown)

	if _, err := m.start(context.Background(), bgSpec(), "/w"); err == nil {
		t.Fatal("start with failing entropy: want error")
	}
	if starter.callCount() != 0 {
		t.Errorf("starter called %d times after entropy failure, want 0", starter.callCount())
	}
	if activeCount(m) != 0 {
		t.Errorf("active = %d after entropy failure, want 0 (no slot reserved)", activeCount(m))
	}
	// The full cap must still be available: all four slots start fine.
	for i := 0; i < backgroundActiveCap; i++ {
		if _, err := m.start(context.Background(), bgSpec(), "/w"); err != nil {
			t.Fatalf("start %d after entropy failure: %v", i, err)
		}
	}
}

func TestBackgroundManagerHandleCollisionRegenerates(t *testing.T) {
	// Reader yields A, A, B: the second start collides with the first handle
	// and must regenerate under the lock, landing on B.
	a := bytes.Repeat([]byte{0xaa}, 16)
	b := bytes.Repeat([]byte{0xbb}, 16)
	seed := append(append(append([]byte(nil), a...), a...), b...)
	starter := starterOf(newFakeProc(1), newFakeProc(2))
	m := newBackgroundManager(starter, bytes.NewReader(seed))
	t.Cleanup(m.Shutdown)

	st1, err := m.start(context.Background(), bgSpec(), "/w")
	if err != nil {
		t.Fatalf("start 1: %v", err)
	}
	st2, err := m.start(context.Background(), bgSpec(), "/w")
	if err != nil {
		t.Fatalf("start 2: %v", err)
	}
	if st1.Handle != "bg-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("handle 1 = %q", st1.Handle)
	}
	if st2.Handle != "bg-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("handle 2 = %q, want the regenerated non-colliding handle", st2.Handle)
	}
	if starter.callCount() != 2 {
		t.Errorf("starter called %d times, want 2 (collision must not spawn)", starter.callCount())
	}
}

func TestBackgroundManagerCollisionThenRandomFailure(t *testing.T) {
	// Reader yields A, A, error, then a working source: the second start
	// collides, its in-lock regeneration read fails, and start must return
	// the rand error with the mutex released and nothing reserved. A dropped
	// unlock in that branch deadlocks this test (activeCount and the third
	// start both need m.mu).
	a := bytes.Repeat([]byte{0xaa}, 16)
	random := io.MultiReader(
		bytes.NewReader(append(append([]byte(nil), a...), a...)),
		&flakyRandom{failsLeft: 1, src: &countingRandom{}},
	)
	starter := starterOf(newFakeProc(1), newFakeProc(2))
	m := newBackgroundManager(starter, random)
	t.Cleanup(m.Shutdown)

	if _, err := m.start(context.Background(), bgSpec(), "/w"); err != nil {
		t.Fatalf("start 1: %v", err)
	}
	_, err := m.start(context.Background(), bgSpec(), "/w")
	if err == nil || !strings.Contains(err.Error(), "entropy exhausted") {
		t.Fatalf("start 2 = %v, want the propagated rand error", err)
	}
	if starter.callCount() != 1 {
		t.Errorf("starter called %d times after regeneration failure, want 1", starter.callCount())
	}
	if activeCount(m) != 1 {
		t.Errorf("active = %d after regeneration failure, want 1 (failed start reserved nothing)", activeCount(m))
	}
	// The source now works again: a subsequent start must succeed.
	st, err := m.start(context.Background(), bgSpec(), "/w")
	if err != nil {
		t.Fatalf("start 3 after regeneration failure: %v", err)
	}
	if st.Handle == "bg-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("start 3 reused the colliding handle %q", st.Handle)
	}
}

func TestBackgroundManagerSpawnFailureReleasesSlot(t *testing.T) {
	fail := true
	var mu sync.Mutex
	procs := []*fakeProc{newFakeProc(1), newFakeProc(2), newFakeProc(3), newFakeProc(4)}
	i := 0
	starter := &fakeStarter{fn: func(execSpec, io.Writer, io.Writer) (backgroundProcess, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			fail = false
			return nil, errors.New("spawn boom")
		}
		p := procs[i]
		i++
		return p, nil
	}}
	m := newBackgroundManager(starter, &countingRandom{})
	t.Cleanup(m.Shutdown)

	if _, err := m.start(context.Background(), bgSpec(), "/w"); err == nil || !strings.Contains(err.Error(), "spawn boom") {
		t.Fatalf("start = %v, want wrapped spawn error", err)
	}
	if activeCount(m) != 0 {
		t.Errorf("active = %d after spawn failure, want 0", activeCount(m))
	}
	if list := m.List(); len(list) != 0 {
		t.Errorf("List = %+v after spawn failure, want empty", list)
	}
	// All four slots must still be available.
	for j := 0; j < backgroundActiveCap; j++ {
		if _, err := m.start(context.Background(), bgSpec(), "/w"); err != nil {
			t.Fatalf("start %d after spawn failure: %v", j, err)
		}
	}
}

func TestBackgroundManagerCancelBeforeSpawn(t *testing.T) {
	starter := starterOf(newFakeProc(1))
	m := newBackgroundManager(starter, &countingRandom{})
	t.Cleanup(m.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := m.start(ctx, bgSpec(), "/w")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("start = %v, want context.Canceled", err)
	}
	if starter.callCount() != 0 {
		t.Errorf("starter called %d times for a pre-cancelled start, want 0", starter.callCount())
	}
	if activeCount(m) != 0 || len(m.List()) != 0 {
		t.Errorf("active = %d, list = %d, want the reservation fully undone", activeCount(m), len(m.List()))
	}
}

func TestBackgroundManagerCancelAfterGatedSpawn(t *testing.T) {
	proc := newFakeProc(7)
	entered := make(chan struct{})
	gate := make(chan struct{})
	starter := &fakeStarter{fn: func(execSpec, io.Writer, io.Writer) (backgroundProcess, error) {
		close(entered)
		<-gate
		return proc, nil
	}}
	m := newBackgroundManager(starter, &countingRandom{})
	t.Cleanup(m.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := m.start(ctx, bgSpec(), "/w")
		errCh <- err
	}()
	<-entered
	cancel()
	close(gate)
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("start = %v, want context.Canceled", err)
	}
	// The spawned-but-never-registered process must be reaped synchronously
	// before start returns.
	if !proc.waitDone() || proc.killCount() == 0 {
		t.Errorf("late process killed=%d waited=%v, want killed and waited before start returned", proc.killCount(), proc.waitDone())
	}
	if activeCount(m) != 0 || len(m.List()) != 0 {
		t.Errorf("active = %d, list = %d, want the reservation fully undone", activeCount(m), len(m.List()))
	}
}

// --- Step 4.2: shutdown/start barrier ---

func TestBackgroundManagerShutdownStartBarrier(t *testing.T) {
	proc := newFakeProc(9)
	entered := make(chan struct{})
	gate := make(chan struct{})
	starter := &fakeStarter{fn: func(execSpec, io.Writer, io.Writer) (backgroundProcess, error) {
		close(entered)
		<-gate
		return proc, nil
	}}
	m := newBackgroundManager(starter, &countingRandom{})

	startErr := make(chan error, 1)
	go func() {
		_, err := m.start(context.Background(), bgSpec(), "/w")
		startErr <- err
	}()
	<-entered

	shutdownDone := make(chan struct{})
	go func() {
		m.Shutdown()
		close(shutdownDone)
	}()
	// Negative probe: a correct Shutdown provably cannot return here (the
	// admitted start holds startWG until after the gate opens), so the timer
	// can never fail a correct implementation; it exists to catch a mutated
	// Shutdown that skips the in-flight-start wait.
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned while a start was still gated in the starter")
	case <-time.After(50 * time.Millisecond):
	}

	close(gate)
	<-shutdownDone
	// Shutdown's return must already imply the late process was killed and
	// reaped (happens-before via startWG).
	if proc.killCount() == 0 || !proc.waitDone() {
		t.Errorf("late process killed=%d waited=%v at Shutdown return, want both", proc.killCount(), proc.waitDone())
	}
	if err := <-startErr; err == nil || err.Error() != "background manager is shut down" {
		t.Errorf("gated start = %v, want shut-down error and no handle", err)
	}
	if list := m.List(); len(list) != 0 {
		t.Errorf("List = %+v after barriered shutdown, want empty", list)
	}
}

// --- Step 4.3: concurrent double shutdown ---

func TestBackgroundManagerConcurrentDoubleShutdown(t *testing.T) {
	proc := newFakeProc(3)
	proc.killReleases = false // keep the killed job's reaper gated across Shutdown's kill
	m := newBackgroundManager(starterOf(proc), &countingRandom{})

	st, err := m.start(context.Background(), bgSpec(), "/w")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	done := jobDoneChan(t, m, st.Handle)

	results := make(chan bool, 2) // whether completion was published at that caller's return
	for i := 0; i < 2; i++ {
		go func() {
			m.Shutdown()
			select {
			case <-done:
				results <- true
			default:
				results <- false
			}
		}()
	}
	// Negative probe: neither caller can return while the reaper is gated —
	// same reasoning as the barrier test, catches an early-returning second
	// Shutdown deterministically.
	select {
	case r := <-results:
		t.Fatalf("a Shutdown caller returned (published=%v) before the job was reaped", r)
	case <-time.After(50 * time.Millisecond):
	}

	proc.release()
	for i := 0; i < 2; i++ {
		if published := <-results; !published {
			t.Error("a Shutdown caller returned before the job's cleanup postcondition (done closed) was true")
		}
	}
	if proc.killCount() != 1 {
		t.Errorf("kill count = %d across double shutdown, want exactly 1", proc.killCount())
	}
	final, ok := m.status(st.Handle)
	if !ok || final.State != backgroundStateKilled || !final.ExitKnown || final.ExitCode != -1 {
		t.Errorf("final status = (%+v, %v), want killed/-1/known", final, ok)
	}
}

// --- Step 4.4: caps, retention, handles, snapshots ---

func TestBackgroundManagerActiveCap(t *testing.T) {
	procs := []*fakeProc{newFakeProc(1), newFakeProc(2), newFakeProc(3), newFakeProc(4), newFakeProc(5)}
	m := newBackgroundManager(starterOf(procs...), &countingRandom{})
	t.Cleanup(m.Shutdown)

	for i := 0; i < backgroundActiveCap; i++ {
		if _, err := m.start(context.Background(), bgSpec(), "/w"); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	_, err := m.start(context.Background(), bgSpec(), "/w")
	if err == nil || err.Error() != "active background job limit reached (4); stop one first" {
		t.Fatalf("fifth start = %v, want the frozen cap error", err)
	}
}

func TestBackgroundManagerRetentionNewestEight(t *testing.T) {
	var procs []*fakeProc
	for i := 0; i < 10; i++ {
		procs = append(procs, newFakeProc(100+i))
	}
	m := newBackgroundManager(starterOf(procs...), &countingRandom{})
	t.Cleanup(m.Shutdown)

	var handles []string
	for i := 0; i < 10; i++ {
		st, err := m.start(context.Background(), bgSpec(), "/w")
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		done := jobDoneChan(t, m, st.Handle)
		procs[i].release()
		<-done
		handles = append(handles, st.Handle)
	}
	list := m.List()
	if len(list) != backgroundRetainedCap {
		t.Fatalf("List has %d jobs, want %d retained", len(list), backgroundRetainedCap)
	}
	for _, h := range handles[:2] {
		if _, ok := m.status(h); ok {
			t.Errorf("oldest completed job %q still queryable, want evicted", h)
		}
	}
	for i, h := range handles[2:] {
		st, ok := m.status(h)
		if !ok {
			t.Errorf("retained job %q (completion #%d) missing", h, i+3)
			continue
		}
		if st.State != backgroundStateExited || !st.ExitKnown || st.ExitCode != 0 {
			t.Errorf("retained job %q = %+v, want exited/0/known", h, st)
		}
	}
}

func TestBackgroundManagerRunningJobsNeverEvicted(t *testing.T) {
	var procs []*fakeProc
	for i := 0; i < 12; i++ {
		procs = append(procs, newFakeProc(200+i))
	}
	m := newBackgroundManager(starterOf(procs...), &countingRandom{})
	t.Cleanup(m.Shutdown)

	// Three long-running jobs pinned open; the fourth slot cycles nine
	// completions so the completed count crosses the retention cap while the
	// running three stay oldest-registered.
	var running []string
	for i := 0; i < 3; i++ {
		st, err := m.start(context.Background(), bgSpec(), "/w")
		if err != nil {
			t.Fatalf("start running %d: %v", i, err)
		}
		running = append(running, st.Handle)
	}
	for i := 3; i < 12; i++ {
		st, err := m.start(context.Background(), bgSpec(), "/w")
		if err != nil {
			t.Fatalf("start cycling %d: %v", i, err)
		}
		done := jobDoneChan(t, m, st.Handle)
		procs[i].release()
		<-done
	}
	for _, h := range running {
		st, ok := m.status(h)
		if !ok {
			t.Errorf("running job %q was evicted", h)
			continue
		}
		if st.State != backgroundStateRunning {
			t.Errorf("job %q state = %q, want still running", h, st.State)
		}
	}
	if got, want := len(m.List()), 3+backgroundRetainedCap; got != want {
		t.Errorf("List has %d jobs, want %d (3 running + %d retained)", got, want, backgroundRetainedCap)
	}
}

func TestBackgroundManagerUnknownHandle(t *testing.T) {
	m := newBackgroundManager(starterOf(), &countingRandom{})
	t.Cleanup(m.Shutdown)

	const wantErr = `unknown background job handle "bg-nope"`
	if _, ok := m.status("bg-nope"); ok {
		t.Error("status of unknown handle reported ok")
	}
	if _, err := m.tail("bg-nope", "stdout", nil, 100); err == nil || err.Error() != wantErr {
		t.Errorf("tail unknown = %v, want %q", err, wantErr)
	}
	if _, err := m.Stop(context.Background(), "bg-nope"); err == nil || err.Error() != wantErr {
		t.Errorf("Stop unknown = %v, want %q", err, wantErr)
	}
}

func TestBackgroundManagerSnapshotsAreCopies(t *testing.T) {
	m := newBackgroundManager(starterOf(newFakeProc(1)), &countingRandom{})
	t.Cleanup(m.Shutdown)

	argv := []string{"cmd", "arg"}
	st, err := m.start(context.Background(), execSpec{Path: "/bin/cmd", Argv: argv, Dir: "/w"}, "/w")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	argv[1] = "CALLER-MUTATED"
	st.Argv[0] = "RETURN-MUTATED"
	got, _ := m.status(st.Handle)
	if got.Argv[0] != "cmd" || got.Argv[1] != "arg" {
		t.Errorf("Argv = %v, want the registration-time copy unaffected by either mutation", got.Argv)
	}
	got.Argv[1] = "STATUS-MUTATED"
	list := m.List()
	if list[0].Argv[1] != "arg" {
		t.Errorf("List Argv = %v, want unaffected by status-snapshot mutation", list[0].Argv)
	}
	list[0].Argv[0] = "LIST-MUTATED"
	again, _ := m.status(st.Handle)
	if again.Argv[0] != "cmd" {
		t.Errorf("Argv = %v after List mutation, want unaffected", again.Argv)
	}
}

// --- taxonomy: Wait results map to the frozen state contract ---

func TestBackgroundManagerStateTaxonomy(t *testing.T) {
	cases := []struct {
		name      string
		code      int
		waitErr   error
		wantState string
		wantCode  int
		wantKnown bool
	}{
		{"natural exit", 5, nil, backgroundStateExited, 5, true},
		{"outside signal kill", -1, nil, backgroundStateExited, -1, true},
		{"wait delay", 0, exec.ErrWaitDelay, backgroundStateExited, 0, true},
		{"infra failure", -1, errors.New("wait: ECHILD"), backgroundStateExited, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proc := newFakeProc(1)
			proc.code = tc.code
			proc.waitErr = tc.waitErr
			m := newBackgroundManager(starterOf(proc), &countingRandom{})
			t.Cleanup(m.Shutdown)

			st, err := m.start(context.Background(), bgSpec(), "/w")
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			done := jobDoneChan(t, m, st.Handle)
			proc.release()
			<-done
			got, ok := m.status(st.Handle)
			if !ok {
				t.Fatal("job missing after completion")
			}
			if got.State != tc.wantState || got.ExitKnown != tc.wantKnown || (tc.wantKnown && got.ExitCode != tc.wantCode) {
				t.Errorf("status = %+v, want state %q code %d known %v", got, tc.wantState, tc.wantCode, tc.wantKnown)
			}
		})
	}
}

// TestBackgroundManagerSentinelErrors proves the frozen error strings carry
// errors.Is-able sentinels and the two classes never cross-match.
func TestBackgroundManagerSentinelErrors(t *testing.T) {
	m := newBackgroundManager(starterOf(), &countingRandom{})
	if _, err := m.Stop(context.Background(), "bg-nope"); !errors.Is(err, errBackgroundUnknownHandle) || errors.Is(err, errBackgroundShutDown) {
		t.Errorf("Stop unknown = %v, want errBackgroundUnknownHandle and not errBackgroundShutDown", err)
	}
	m.Shutdown()
	if _, err := m.start(context.Background(), bgSpec(), "/w"); !errors.Is(err, errBackgroundShutDown) || errors.Is(err, errBackgroundUnknownHandle) {
		t.Errorf("start after shutdown = %v, want errBackgroundShutDown and not errBackgroundUnknownHandle", err)
	}
}

func TestBackgroundManagerStartAfterShutdown(t *testing.T) {
	starter := starterOf(newFakeProc(1))
	m := newBackgroundManager(starter, &countingRandom{})
	m.Shutdown()
	_, err := m.start(context.Background(), bgSpec(), "/w")
	if err == nil || err.Error() != "background manager is shut down" {
		t.Fatalf("start after Shutdown = %v, want the frozen closed error", err)
	}
	if starter.callCount() != 0 {
		t.Errorf("starter called %d times after shutdown, want 0", starter.callCount())
	}
}

// --- Step 4.5: completion publication releases the slot ---

func TestBackgroundManagerCompletionPublishesSlot(t *testing.T) {
	var procs []*fakeProc
	for i := 0; i < 5; i++ {
		procs = append(procs, newFakeProc(300+i))
	}
	m := newBackgroundManager(starterOf(procs...), &countingRandom{})
	t.Cleanup(m.Shutdown)

	var handles []string
	for i := 0; i < backgroundActiveCap; i++ {
		st, err := m.start(context.Background(), bgSpec(), "/w")
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		handles = append(handles, st.Handle)
	}
	done := jobDoneChan(t, m, handles[1])
	procs[1].release()
	<-done
	// The moment done is observed closed, the released slot must be available
	// with no retry loop, and the completed job already queryable.
	if _, err := m.start(context.Background(), bgSpec(), "/w"); err != nil {
		t.Fatalf("start after completion publication: %v", err)
	}
	final, ok := m.status(handles[1])
	if !ok || final.State != backgroundStateExited || !final.ExitKnown {
		t.Errorf("completed job = (%+v, %v), want queryable exited status", final, ok)
	}
}

// --- Step 4.6: stop paths ---

func TestBackgroundManagerStopIdempotentKill(t *testing.T) {
	proc := newFakeProc(1)
	proc.killReleases = false // hold the stop-vs-reap window open
	m := newBackgroundManager(starterOf(proc), &countingRandom{})
	t.Cleanup(m.Shutdown)

	st, err := m.start(context.Background(), bgSpec(), "/w")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 2; i++ {
		if _, err := m.Stop(expired, st.Handle); !errors.Is(err, context.Canceled) {
			t.Fatalf("Stop %d = %v, want context.Canceled while wait is gated", i, err)
		}
	}
	if proc.killCount() != 1 {
		t.Errorf("kill count = %d after two Stops, want exactly 1", proc.killCount())
	}
	done := jobDoneChan(t, m, st.Handle)
	proc.release()
	<-done
	final, err := m.Stop(context.Background(), st.Handle)
	if err != nil {
		t.Fatalf("Stop after completion: %v", err)
	}
	if final.State != backgroundStateKilled || !final.ExitKnown || final.ExitCode != -1 {
		t.Errorf("final = %+v, want killed/-1/known", final)
	}
	if proc.killCount() != 1 {
		t.Errorf("kill count = %d after post-completion Stop, want still 1 (no kill after wait)", proc.killCount())
	}
}

func TestBackgroundManagerStopAlreadyFinished(t *testing.T) {
	proc := newFakeProc(1)
	m := newBackgroundManager(starterOf(proc), &countingRandom{})
	t.Cleanup(m.Shutdown)

	st, err := m.start(context.Background(), bgSpec(), "/w")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	done := jobDoneChan(t, m, st.Handle)
	proc.release()
	<-done
	got, err := m.Stop(context.Background(), st.Handle)
	if err != nil {
		t.Fatalf("Stop finished job: %v", err)
	}
	if got.State != backgroundStateExited || !got.ExitKnown || got.ExitCode != 0 {
		t.Errorf("Stop finished job = %+v, want exited/0/known no-op", got)
	}
	if proc.killCount() != 0 {
		t.Errorf("kill count = %d for already-finished job, want 0", proc.killCount())
	}
}

func TestBackgroundManagerLateStopDoesNotRewriteNaturalExit(t *testing.T) {
	proc := newFakeProc(1)
	proc.killReleases = false
	proc.waitObserved = make(chan struct{})
	gate := make(chan struct{})
	proc.waitGate = gate
	m := newBackgroundManager(starterOf(proc), &countingRandom{})
	t.Cleanup(m.Shutdown)

	st, err := m.start(context.Background(), bgSpec(), "/w")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	done := jobDoneChan(t, m, st.Handle)
	proc.release()
	<-proc.waitObserved // leader exited; manager publication is still gated

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Stop(expired, st.Handle); !errors.Is(err, context.Canceled) {
		t.Fatalf("late Stop = %v, want context.Canceled while publication is gated", err)
	}
	close(gate)
	<-done
	final, ok := m.status(st.Handle)
	if !ok || final.State != backgroundStateExited || !final.ExitKnown || final.ExitCode != 0 {
		t.Errorf("final = (%+v, %v), want the leader's natural exited/0/known result", final, ok)
	}
}

func TestBackgroundManagerStopContextExpiryWhileWaitGated(t *testing.T) {
	proc := newFakeProc(1)
	proc.killReleases = false
	m := newBackgroundManager(starterOf(proc), &countingRandom{})
	t.Cleanup(m.Shutdown)

	st, err := m.start(context.Background(), bgSpec(), "/w")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := m.Stop(expired, st.Handle)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop = %v, want context.Canceled", err)
	}
	if got.State != backgroundStateRunning {
		t.Errorf("snapshot at expiry = %+v, want still running (reaper gated)", got)
	}
	// Tool-facing stop has returned canceled; the manager must still finish
	// its own cleanup once the wait releases.
	done := jobDoneChan(t, m, st.Handle)
	proc.release()
	<-done
	final, ok := m.status(st.Handle)
	if !ok || final.State != backgroundStateKilled || !final.ExitKnown || final.ExitCode != -1 {
		t.Errorf("final = (%+v, %v), want killed/-1/known after late release", final, ok)
	}
	if activeCount(m) != 0 {
		t.Errorf("active = %d after cleanup, want 0", activeCount(m))
	}
}

// --- tail semantics through the manager ---

func TestBackgroundManagerTailStreamsAndEOF(t *testing.T) {
	proc := newFakeProc(1)
	var stdout, stderr io.Writer
	starter := &fakeStarter{fn: func(_ execSpec, out, errw io.Writer) (backgroundProcess, error) {
		stdout, stderr = out, errw
		return proc, nil
	}}
	m := newBackgroundManager(starter, &countingRandom{})
	t.Cleanup(m.Shutdown)

	st, err := m.start(context.Background(), bgSpec(), "/w")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := m.tail(st.Handle, "both", nil, 100); err == nil {
		t.Error("tail with invalid stream: want error")
	}
	if _, err := stdout.Write([]byte("hello")); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
	if _, err := stderr.Write([]byte("oops")); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
	chunk, err := m.tail(st.Handle, "stdout", nil, 100)
	if err != nil {
		t.Fatalf("tail stdout: %v", err)
	}
	if string(chunk.Data) != "hello" || chunk.NextCursor != 5 || chunk.Dropped != 0 {
		t.Errorf("chunk = %+v, want newest-tail hello with cursor 5", chunk)
	}
	if chunk.EOF {
		t.Error("EOF = true while the job is still running")
	}
	if chunk.Status.State != backgroundStateRunning || chunk.Status.StdoutEnd != 5 || chunk.Status.StderrEnd != 4 {
		t.Errorf("chunk status = %+v, want running with live stream bounds", chunk.Status)
	}
	done := jobDoneChan(t, m, st.Handle)
	proc.release()
	<-done
	cursor := uint64(0)
	chunk, err = m.tail(st.Handle, "stdout", &cursor, 100)
	if err != nil {
		t.Fatalf("tail after done: %v", err)
	}
	if string(chunk.Data) != "hello" || chunk.NextCursor != 5 || !chunk.EOF {
		t.Errorf("chunk after done = %+v, want full replay with EOF", chunk)
	}
	mid := uint64(2)
	chunk, err = m.tail(st.Handle, "stdout", &mid, 2)
	if err != nil {
		t.Fatalf("tail partial: %v", err)
	}
	if string(chunk.Data) != "ll" || chunk.NextCursor != 4 || chunk.EOF {
		t.Errorf("partial chunk = %+v, want ll cursor 4 and no EOF before end", chunk)
	}
	echunk, err := m.tail(st.Handle, "stderr", nil, 100)
	if err != nil {
		t.Fatalf("tail stderr: %v", err)
	}
	if string(echunk.Data) != "oops" || !echunk.EOF {
		t.Errorf("stderr chunk = %+v, want oops with EOF", echunk)
	}
}
