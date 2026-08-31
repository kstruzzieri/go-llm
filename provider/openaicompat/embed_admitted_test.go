package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

var _ provider.AdmittedEmbedder = (*Provider)(nil)

const admWait = 2 * time.Second // liveness bound only, never an elapsed-time assertion

// doneObservingCtx proves flight registration deterministically: in
// p.embed, ctx.Done() is first consulted by the outer select, which runs
// only AFTER embedGroup.DoChan has returned — so the observation signal
// means this caller's flight registration (lead or join) is decided. If a
// future refactor consults Done() earlier, these tests fail toward flake,
// loudly, not silently.
type doneObservingCtx struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func newDoneObserving(parent context.Context) *doneObservingCtx {
	return &doneObservingCtx{Context: parent, observed: make(chan struct{})}
}

func (c *doneObservingCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

// countingAdmit is a test AdmitFunc: counts acquires/releases and can
// block acquisition until the test opens gate.
type countingAdmit struct {
	acquires atomic.Int32
	releases atomic.Int32
	invoked  chan struct{} // signalled on each acquire attempt
	gate     chan struct{} // nil = never blocks
	err      error
}

func (c *countingAdmit) fn() provider.AdmitFunc {
	return func(ctx context.Context) (func(), error) {
		select {
		case c.invoked <- struct{}{}:
		default:
		}
		if c.gate != nil {
			// Honor ctx like the real gate does: under the production
			// sharedCtx this never fires (uncancellable), but a mutated
			// implementation that passes the leader's cancellable ctx
			// into admit turns the queued-cancel test red here.
			select {
			case <-c.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if c.err != nil {
			return nil, c.err
		}
		c.acquires.Add(1)
		return func() { c.releases.Add(1) }, nil
	}
}

// gatedEmbedServer serves /v1/embeddings, blocking each request on gate
// (nil = no blocking) and counting requests.
type gatedEmbedServer struct {
	srv      *httptest.Server
	requests atomic.Int32
	inflight chan struct{} // signalled when a request arrives
	gate     chan struct{} // requests block here until closed
	status   int
}

func newGatedEmbedServer(status int) *gatedEmbedServer {
	g := &gatedEmbedServer{
		inflight: make(chan struct{}, 8),
		gate:     make(chan struct{}),
		status:   status,
	}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		g.requests.Add(1)
		g.inflight <- struct{}{}
		<-g.gate
		if g.status != http.StatusOK {
			w.WriteHeader(g.status)
			return
		}
		_ = json.NewEncoder(w).Encode(embedResponse{
			Model: "embed-m",
			Data:  []embeddingDoc{{Index: 0, Embedding: []float64{0.5}}},
			Usage: usage{PromptTokens: 1},
		})
	}))
	return g
}

var embedReq = provider.EmbedRequest{Model: "embed-m", Input: []string{"same"}}

// TestEmbedAdmitted_OnePermitPerFlight is the M-callers-one-permit AC:
// three concurrent identical requests form ONE flight that acquires ONE
// permit and issues ONE backend request.
func TestEmbedAdmitted_OnePermitPerFlight(t *testing.T) {
	server := newGatedEmbedServer(http.StatusOK)
	defer server.srv.Close()
	p := NewProvider(NewClient(server.srv.URL))

	admit := &countingAdmit{invoked: make(chan struct{}, 8), gate: make(chan struct{})}

	type result struct {
		resp *provider.EmbedResponse
		err  error
	}
	results := make(chan result, 3)
	start := func() *doneObservingCtx {
		obs := newDoneObserving(context.Background())
		go func() {
			resp, err := p.EmbedAdmitted(obs, embedReq, admit.fn())
			results <- result{resp, err}
		}()
		return obs
	}

	// (a) leader: registered AND inside the closure blocked on admit.
	leader := start()
	select {
	case <-leader.observed:
	case <-time.After(admWait):
		t.Fatal("leader registration never observed")
	}
	select {
	case <-admit.invoked:
	case <-time.After(admWait):
		t.Fatal("leader never invoked admit")
	}
	// (b) followers: registered while the flight is held open by the
	// blocked admit => they joined it.
	for i := range 2 {
		f := start()
		select {
		case <-f.observed:
		case <-time.After(admWait):
			t.Fatalf("follower %d registration never observed", i)
		}
	}
	// Admit-before-HTTP: nothing hits the backend while admit is blocked.
	if got := server.requests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d before admission, want 0", got)
	}
	// (c) release admission, then the handler gate.
	close(admit.gate)
	select {
	case <-server.inflight:
	case <-time.After(admWait):
		t.Fatal("backend request never arrived after admission")
	}
	close(server.gate)
	// (d) all three callers succeed off the single flight.
	for range 3 {
		select {
		case r := <-results:
			if r.err != nil || r.resp == nil {
				t.Fatalf("caller result = (%v, %v)", r.resp, r.err)
			}
		case <-time.After(admWait):
			t.Fatal("caller never completed")
		}
	}
	if a, rel, reqs := admit.acquires.Load(), admit.releases.Load(), server.requests.Load(); a != 1 || rel != 1 || reqs != 1 {
		t.Fatalf("acquires=%d releases=%d requests=%d, want 1/1/1", a, rel, reqs)
	}
}

// TestEmbedAdmitted_AdmitBeforeHTTP pins the ordering in isolation: the
// backend must see nothing until admit returns.
func TestEmbedAdmitted_AdmitBeforeHTTP(t *testing.T) {
	server := newGatedEmbedServer(http.StatusOK)
	defer server.srv.Close()
	p := NewProvider(NewClient(server.srv.URL))
	admit := &countingAdmit{invoked: make(chan struct{}, 1), gate: make(chan struct{})}

	errC := make(chan error, 1)
	go func() {
		_, err := p.EmbedAdmitted(context.Background(), embedReq, admit.fn())
		errC <- err
	}()
	select {
	case <-admit.invoked:
	case <-time.After(admWait):
		t.Fatal("admit never invoked")
	}
	if got := server.requests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d while admission blocked, want 0", got)
	}
	close(admit.gate)
	select {
	case <-server.inflight:
	case <-time.After(admWait):
		t.Fatal("backend request never arrived")
	}
	close(server.gate)
	if err := <-errC; err != nil {
		t.Fatalf("EmbedAdmitted: %v", err)
	}
}

// TestEmbedAdmitted_AdmitErrorReachesEveryCaller: the flight fails before
// HTTP; all joined callers receive the admission error with the chain
// intact (errors.Is finds the original), and nothing was released.
func TestEmbedAdmitted_AdmitErrorReachesEveryCaller(t *testing.T) {
	server := newGatedEmbedServer(http.StatusOK)
	defer server.srv.Close()
	p := NewProvider(NewClient(server.srv.URL))
	sentinel := errors.New("admission sentinel")
	admit := &countingAdmit{invoked: make(chan struct{}, 8), gate: make(chan struct{}), err: sentinel}

	errC := make(chan error, 3)
	leader := newDoneObserving(context.Background())
	go func() {
		_, err := p.EmbedAdmitted(leader, embedReq, admit.fn())
		errC <- err
	}()
	<-leader.observed
	select {
	case <-admit.invoked:
	case <-time.After(admWait):
		t.Fatal("admit never invoked")
	}
	for range 2 {
		f := newDoneObserving(context.Background())
		go func() {
			_, err := p.EmbedAdmitted(f, embedReq, admit.fn())
			errC <- err
		}()
		select {
		case <-f.observed:
		case <-time.After(admWait):
			t.Fatal("follower registration never observed")
		}
	}
	close(admit.gate) // admit now returns the sentinel error
	for range 3 {
		select {
		case err := <-errC:
			if !errors.Is(err, sentinel) {
				t.Fatalf("caller err = %v, want chain containing the sentinel (%%w contract)", err)
			}
		case <-time.After(admWait):
			t.Fatal("caller never returned")
		}
	}
	if got := server.requests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d after admission failure, want 0", got)
	}
	if got := admit.releases.Load(); got != 0 {
		t.Fatalf("releases = %d after admission failure, want 0", got)
	}
}

// TestEmbedAdmitted_ReleasesExactlyOnceOnHTTPError: permit accounting
// balances even when the backend fails.
func TestEmbedAdmitted_ReleasesExactlyOnceOnHTTPError(t *testing.T) {
	server := newGatedEmbedServer(http.StatusInternalServerError)
	defer server.srv.Close()
	close(server.gate) // no HTTP gating in this test
	p := NewProvider(NewClient(server.srv.URL))
	admit := &countingAdmit{invoked: make(chan struct{}, 1)}

	if _, err := p.EmbedAdmitted(context.Background(), embedReq, admit.fn()); err == nil {
		t.Fatal("expected HTTP 500 to surface as an error")
	}
	if a, rel := admit.acquires.Load(), admit.releases.Load(); a != 1 || rel != 1 {
		t.Fatalf("acquires=%d releases=%d, want 1/1 (released exactly once on error)", a, rel)
	}
}

// TestEmbedAdmitted_QueuedFlightCancelLeavesFollowerServed is the §6
// amended contract's queued case: the leader's caller cancels while the
// flight is still waiting in admit; the leader gets its context error
// immediately, the flight stays queued, and once admitted it serves the
// follower. One acquire, one release, one backend request.
func TestEmbedAdmitted_QueuedFlightCancelLeavesFollowerServed(t *testing.T) {
	server := newGatedEmbedServer(http.StatusOK)
	defer server.srv.Close()
	p := NewProvider(NewClient(server.srv.URL))
	admit := &countingAdmit{invoked: make(chan struct{}, 1), gate: make(chan struct{})}

	leaderParent, cancelLeader := context.WithCancel(context.Background())
	leader := newDoneObserving(leaderParent)
	leaderErr := make(chan error, 1)
	go func() {
		_, err := p.EmbedAdmitted(leader, embedReq, admit.fn())
		leaderErr <- err
	}()
	<-leader.observed
	select {
	case <-admit.invoked:
	case <-time.After(admWait):
		t.Fatal("leader never invoked admit")
	}

	follower := newDoneObserving(context.Background())
	followerRes := make(chan error, 1)
	go func() {
		resp, err := p.EmbedAdmitted(follower, embedReq, admit.fn())
		if err == nil && resp == nil {
			err = errors.New("nil resp without error")
		}
		followerRes <- err
	}()
	select {
	case <-follower.observed:
	case <-time.After(admWait):
		t.Fatal("follower registration never observed")
	}

	cancelLeader()
	select {
	case err := <-leaderErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled leader err = %v, want context.Canceled", err)
		}
	case <-time.After(admWait):
		t.Fatal("cancelled leader caller did not return immediately")
	}
	// Flight still queued: nothing acquired, nothing on the wire.
	if a, reqs := admit.acquires.Load(), server.requests.Load(); a != 0 || reqs != 0 {
		t.Fatalf("acquires=%d requests=%d while flight queued, want 0/0", a, reqs)
	}

	close(admit.gate)
	select {
	case <-server.inflight:
	case <-time.After(admWait):
		t.Fatal("flight never reached the backend after admission")
	}
	close(server.gate)
	select {
	case err := <-followerRes:
		if err != nil {
			t.Fatalf("follower err = %v, want success from the shared flight", err)
		}
	case <-time.After(admWait):
		t.Fatal("follower never completed")
	}
	if a, rel, reqs := admit.acquires.Load(), admit.releases.Load(), server.requests.Load(); a != 1 || rel != 1 || reqs != 1 {
		t.Fatalf("acquires=%d releases=%d requests=%d, want 1/1/1", a, rel, reqs)
	}
}

// TestEmbedAdmitted_InFlightCancelLeavesFollowerServed: same shape, but
// the leader cancels after admission, while the HTTP handler is gated.
func TestEmbedAdmitted_InFlightCancelLeavesFollowerServed(t *testing.T) {
	server := newGatedEmbedServer(http.StatusOK)
	defer server.srv.Close()
	p := NewProvider(NewClient(server.srv.URL))
	admit := &countingAdmit{invoked: make(chan struct{}, 1)}

	leaderParent, cancelLeader := context.WithCancel(context.Background())
	leader := newDoneObserving(leaderParent)
	leaderErr := make(chan error, 1)
	go func() {
		_, err := p.EmbedAdmitted(leader, embedReq, admit.fn())
		leaderErr <- err
	}()
	<-leader.observed
	select {
	case <-server.inflight: // admission passed; request gated in handler
	case <-time.After(admWait):
		t.Fatal("flight never reached the backend")
	}

	follower := newDoneObserving(context.Background())
	followerRes := make(chan error, 1)
	go func() {
		_, err := p.EmbedAdmitted(follower, embedReq, admit.fn())
		followerRes <- err
	}()
	select {
	case <-follower.observed:
	case <-time.After(admWait):
		t.Fatal("follower registration never observed")
	}

	cancelLeader()
	select {
	case err := <-leaderErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled leader err = %v, want context.Canceled", err)
		}
	case <-time.After(admWait):
		t.Fatal("cancelled leader caller did not return immediately")
	}

	close(server.gate)
	select {
	case err := <-followerRes:
		if err != nil {
			t.Fatalf("follower err = %v, want success", err)
		}
	case <-time.After(admWait):
		t.Fatal("follower never completed")
	}
	if rel := admit.releases.Load(); rel != 1 {
		t.Fatalf("releases = %d, want exactly 1", rel)
	}
}
