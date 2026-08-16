package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// testWait bounds every join in these tests so a regression hangs a test for
// seconds, never a CI run.
const testWait = 5 * time.Second

// waitOn receives one value within testWait or fails the test.
func waitOn[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// gate is an idempotently releasable barrier: the test flow and the
// unconditional cleanup release can both call release without a double-close.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate() *gate     { return &gate{ch: make(chan struct{})} }
func (g *gate) release() { g.once.Do(func() { close(g.ch) }) }

// gatedTransport blocks the dial until its gate releases, signalling `started`
// first, so tests can observe exactly which dials are in flight. Both channel
// operations are ctx-guarded so an aborted test never strands a worker.
type gatedTransport struct {
	inner   gomcp.Transport
	alias   string
	started chan<- string // buffered cap >= len(servers): lossless
	gate    <-chan struct{}
}

func (g *gatedTransport) Connect(ctx context.Context) (gomcp.Connection, error) {
	select {
	case g.started <- g.alias:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-g.gate:
		return g.inner.Connect(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// makeGatedFakes builds one real in-memory SDK server per alias (distinct
// Implementation.Name "server-<alias>", one tool "pad_<alias>" whose over-long
// description yields exactly one deterministic truncation warning per server)
// behind gated transports. Server lifecycles run on their own context,
// separate from the Connect parent, and are joined in cleanup.
func makeGatedFakes(t *testing.T, aliases []string, started chan string) ([]Server, []*gate) {
	t.Helper()
	n := len(aliases)
	servers := make([]Server, n)
	gates := make([]*gate, n)
	serverCtx, cancelServers := context.WithCancel(context.Background())
	runDones := make([]chan struct{}, n)
	for i, alias := range aliases {
		srv := gomcp.NewServer(&gomcp.Implementation{Name: "server-" + alias, Version: "0.0.1"}, nil)
		gomcp.AddTool(srv, &gomcp.Tool{
			Name:        "pad_" + alias,
			Description: strings.Repeat("x", maxDescBytes+50),
		}, func(_ context.Context, _ *gomcp.CallToolRequest, _ echoIn) (*gomcp.CallToolResult, any, error) {
			return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "ok"}}}, nil, nil
		})
		serverTr, clientTr := gomcp.NewInMemoryTransports()
		done := make(chan struct{})
		runDones[i] = done
		go func() { defer close(done); _ = srv.Run(serverCtx, serverTr) }()
		gates[i] = newGate()
		servers[i] = Server{
			Alias: alias,
			tr: &gatedTransport{
				inner:   clientTr,
				alias:   alias,
				started: started,
				gate:    gates[i].ch,
			},
		}
	}
	// Registered before the caller's cleanup, so LIFO runs it last: the driver
	// is joined and every Manager closed before server lifecycles are torn down.
	t.Cleanup(func() {
		cancelServers()
		for i, done := range runDones {
			select {
			case <-done:
			case <-time.After(testWait):
				t.Errorf("fake server %d Run goroutine leaked", i)
			}
		}
	})
	return servers, gates
}

// connectDriver runs connectWithHooks on its own goroutine, exposing the
// result via shared fields and a closed channel so any number of waiters
// (assertions and cleanup) can join without a second-receive deadlock.
type connectDriver struct {
	m     *Manager
	warns []error
	err   error
	done  chan struct{}
}

func startConnectDriver(t *testing.T, ctx context.Context, servers []Server, h *connectHooks, gates []*gate) *connectDriver {
	t.Helper()
	d := &connectDriver{done: make(chan struct{})}
	go func() {
		defer close(d.done)
		d.m, d.warns, d.err = connectWithHooks(ctx, Implementation{Name: "golem", Version: "test"}, servers, h)
	}()
	t.Cleanup(func() {
		for _, g := range gates {
			g.release()
		}
		select {
		case <-d.done:
		case <-time.After(testWait):
			t.Errorf("Connect driver goroutine leaked")
			return
		}
		if d.m != nil {
			_ = d.m.Close()
		}
	})
	return d
}

func (d *connectDriver) join(t *testing.T) {
	t.Helper()
	select {
	case <-d.done:
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for Connect to return")
	}
}

func TestConnectOverlapsDialsBoundsAndOrders(t *testing.T) {
	const n = maxConcurrentConnects + 1
	aliases := make([]string, n)
	for i := range n {
		aliases[i] = fmt.Sprintf("s%d", i)
	}
	started := make(chan string, n)
	launched := make(chan int, n)
	published := make(chan int, n)
	servers, gates := makeGatedFakes(t, aliases, started)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := startConnectDriver(t, ctx, servers, &connectHooks{
		launched:  func(i int) { launched <- i },
		published: func(i int) { published <- i },
	}, gates)

	// Overlap: with every gate closed, maxConcurrentConnects dials must be in
	// flight simultaneously.
	for i := 0; i < maxConcurrentConnects; i++ {
		waitOn(t, started, "concurrent dial start")
	}
	// All n launch attempts are observed even though the n-th g.Go is blocked
	// by the limit (launched fires on the launcher goroutine before g.Go).
	for i := 0; i < n; i++ {
		waitOn(t, launched, "launch attempt")
	}
	// Bound: no further dial may start while all gates are held. Absence check
	// is scheduler-bounded: under a deleted limit the extra worker's lossless
	// started send lands well within the window; a correct impl can never send.
	select {
	case alias := <-started:
		t.Fatalf("dial %q started above the concurrency limit", alias)
	case <-time.After(500 * time.Millisecond):
	}

	// Order: force completion order 3,4,2,1,0 (worker 4 is admitted only after
	// 3's permit frees). Each release waits for that server's publication, so
	// completion order is exact, not probabilistic.
	for _, idx := range []int{3, 4, 2, 1, 0} {
		gates[idx].release()
		for waitOn(t, published, fmt.Sprintf("publication of server %d", idx)) != idx {
		}
	}

	d.join(t)
	if d.err != nil {
		t.Fatalf("Connect fatal error: %v", d.err)
	}

	// Aggregate state must be in config order regardless of completion order.
	tools := d.m.Tools()
	if len(tools) != n {
		t.Fatalf("got %d tools, want %d", len(tools), n)
	}
	for i, tl := range tools {
		if want := fmt.Sprintf("mcp__s%d__pad_s%d", i, i); tl.Spec().Name != want {
			t.Fatalf("tools[%d] = %q, want %q (config order violated)", i, tl.Spec().Name, want)
		}
	}
	if len(d.m.sessions) != n {
		t.Fatalf("got %d sessions, want %d", len(d.m.sessions), n)
	}
	for i, sess := range d.m.sessions {
		if want := fmt.Sprintf("server-s%d", i); sess.InitializeResult().ServerInfo.Name != want {
			t.Fatalf("sessions[%d] = %q, want %q (config order violated)", i, sess.InitializeResult().ServerInfo.Name, want)
		}
	}
	if len(d.warns) != n {
		t.Fatalf("got %d warnings, want %d: %v", len(d.warns), n, d.warns)
	}
	for i, w := range d.warns {
		want := fmt.Sprintf("server %q: tool %q description truncated to %d bytes",
			fmt.Sprintf("s%d", i), fmt.Sprintf("pad_s%d", i), maxDescBytes)
		if w.Error() != want {
			t.Fatalf("warns[%d] = %q, want %q (config order violated)", i, w.Error(), want)
		}
	}
}

// failingTransport fails every dial immediately with a fixed error.
type failingTransport struct{ err error }

func (f *failingTransport) Connect(context.Context) (gomcp.Connection, error) {
	return nil, f.err
}

func TestConnectPartialFailureExactOutputs(t *testing.T) {
	started := make(chan string, 3)
	fakes, gates := makeGatedFakes(t, []string{"a", "c"}, started)
	for _, g := range gates {
		g.release() // no gating needed: this test is about outputs, not timing
	}
	servers := []Server{
		fakes[0],
		{Alias: "b", tr: &failingTransport{err: errors.New("boom")}},
		fakes[1],
	}

	m, warns, err := Connect(context.Background(), Implementation{Name: "golem", Version: "test"}, servers)
	if err != nil {
		t.Fatalf("partial failure must not be fatal: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	wantWarns := []string{
		fmt.Sprintf(`server "a": tool "pad_a" description truncated to %d bytes`, maxDescBytes),
		`server "b": connect: boom`,
		fmt.Sprintf(`server "c": tool "pad_c" description truncated to %d bytes`, maxDescBytes),
	}
	if len(warns) != len(wantWarns) {
		t.Fatalf("got %d warnings, want %d: %v", len(warns), len(wantWarns), warns)
	}
	for i, w := range warns {
		if w.Error() != wantWarns[i] {
			t.Fatalf("warns[%d] = %q, want %q", i, w.Error(), wantWarns[i])
		}
	}
	wantTools := []string{"mcp__a__pad_a", "mcp__c__pad_c"}
	tools := m.Tools()
	if len(tools) != len(wantTools) {
		t.Fatalf("got %d tools, want %d", len(tools), len(wantTools))
	}
	for i, tl := range tools {
		if tl.Spec().Name != wantTools[i] {
			t.Fatalf("tools[%d] = %q, want %q", i, tl.Spec().Name, wantTools[i])
		}
	}
	if len(m.sessions) != 2 {
		t.Fatalf("got %d sessions, want 2 (failed server contributes none)", len(m.sessions))
	}
	for i, want := range []string{"server-a", "server-c"} {
		if got := m.sessions[i].InitializeResult().ServerInfo.Name; got != want {
			t.Fatalf("sessions[%d] = %q, want %q", i, got, want)
		}
	}
}

func TestConnectParentCancelDrainsCleanly(t *testing.T) {
	const n = maxConcurrentConnects + 1
	aliases := make([]string, n)
	for i := range n {
		aliases[i] = fmt.Sprintf("s%d", i)
	}
	started := make(chan string, n)
	launched := make(chan int, n)
	servers, gates := makeGatedFakes(t, aliases, started)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := startConnectDriver(t, ctx, servers, &connectHooks{
		launched: func(i int) { launched <- i },
	}, gates)

	// Cancel only once maxConcurrentConnects dials are gated in flight and the
	// final worker is provably queued behind the limit (all launch attempts
	// observed), so the drain covers both in-flight and queued workers.
	for i := 0; i < maxConcurrentConnects; i++ {
		waitOn(t, started, "concurrent dial start")
	}
	for i := 0; i < n; i++ {
		waitOn(t, launched, "launch attempt")
	}
	cancel()

	d.join(t)
	if d.err != nil {
		t.Fatalf("cancellation must not be fatal: %v", d.err)
	}
	if len(d.m.sessions) != 0 || len(d.m.Tools()) != 0 {
		t.Fatalf("cancelled connect aggregated %d sessions / %d tools, want none",
			len(d.m.sessions), len(d.m.Tools()))
	}
	if len(d.warns) != n {
		t.Fatalf("got %d warnings, want one per server (%d): %v", len(d.warns), n, d.warns)
	}
	for i, w := range d.warns {
		prefix := fmt.Sprintf("server %q: connect: ", fmt.Sprintf("s%d", i))
		if !strings.HasPrefix(w.Error(), prefix) {
			t.Fatalf("warns[%d] = %q, want prefix %q (config order violated)", i, w.Error(), prefix)
		}
		if !strings.Contains(w.Error(), context.Canceled.Error()) {
			t.Fatalf("warns[%d] = %q, want it to wrap %q", i, w.Error(), context.Canceled.Error())
		}
	}
}
