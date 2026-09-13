package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

func admDest(t *testing.T, prov, raw string) provider.Destination {
	t.Helper()
	d, err := provider.NewDestination(prov, raw)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// admEdges mirrors the live-config shape: local primary agent with a remote
// fallback, remote-only summarize, local embedding, metadata edges.
func admEdges(t *testing.T) []provider.DestinationEdge {
	t.Helper()
	local := admDest(t, "llamacpp", "http://127.0.0.1:8090")
	remote := admDest(t, "opencode", "https://opencode.ai/zen/go")
	return []provider.DestinationEdge{
		{Purpose: "agent", Destination: local},
		{Purpose: "agent", Destination: remote, IsFallback: true},
		{Purpose: "summarize", Destination: remote},
		{Purpose: "embedding", Destination: local},
		{Purpose: provider.DestinationPurposeModelRefresh, Destination: local},
		{Purpose: provider.DestinationPurposeModelRefresh, Destination: remote},
	}
}

type fakePrompt struct {
	answer bool
	err    error
	calls  int
	seen   string
	// before runs inside the consent question — i.e. while the admission
	// mutex is held and before any publication — so a test can observe the
	// pre-approval world or change it underneath the decision.
	before func(ctx context.Context)
}

func (f *fakePrompt) ask(ctx context.Context, prompt string) (bool, error) {
	f.calls++
	f.seen = prompt
	if f.before != nil {
		f.before(ctx)
	}
	return f.answer, f.err
}

func newTestAdmission(t *testing.T, edges []provider.DestinationEdge, allow []string, interactive bool, p *fakePrompt) (*destinationAdmission, *strings.Builder) {
	t.Helper()
	var out strings.Builder
	adm, err := newDestinationAdmission(destinationAdmissionConfig{
		Gate:        provider.NewDestinationGate(),
		Edges:       edges,
		AllowFlags:  allow,
		Interactive: interactive,
		PromptYN:    p.ask,
		Out:         &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	return adm, &out
}

func TestLineSourcePromptYN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "yes", in: " YeS \r", want: true},
		{name: "no", in: " n \r", want: false},
		{name: "EOF", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEditorFixture(t, editorOpts{in: strings.NewReader(tt.in)})
			got, err := lineSourcePromptYN(f.src)(context.Background(), "allow? ")
			if err != nil {
				t.Fatalf("lineSourcePromptYN(%q) error = %v, want nil", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("lineSourcePromptYN(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestLineSourcePromptYNRejectedAnswerDeniesWithoutConsumingTypeahead(t *testing.T) {
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("bad\xb2\ry\r")})
	got, err := lineSourcePromptYN(f.src)(context.Background(), "allow? ")
	if err != nil {
		t.Fatalf("lineSourcePromptYN(invalid UTF-8) error = %v, want nil", err)
	}
	if got {
		t.Error("lineSourcePromptYN(invalid UTF-8) = true, want false")
	}

	line, ok, err := f.readGoal(t)
	if err != nil || !ok || line != "y" {
		t.Fatalf("ReadGoal after rejected answer = %q ok=%v err=%v, want preserved typeahead \"y\" true nil", line, ok, err)
	}
}

func TestLineSourcePromptYNCtrlCReturnsCanceledWithoutFurtherInput(t *testing.T) {
	g := newGatedReader()
	g.entered = make(chan struct{}, 2)
	f := newEditorFixture(t, editorOpts{in: g})

	type result struct {
		approved bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		approved, err := lineSourcePromptYN(f.src)(context.Background(), "allow? ")
		done <- result{approved: approved, err: err}
	}()
	inputClosed := false
	closeInput := func() {
		if !inputClosed {
			close(g.chunks)
			inputClosed = true
		}
	}
	defer closeInput()
	waitForStop := func() bool {
		select {
		case <-done:
			return true
		case <-time.After(5 * time.Second):
			return false
		}
	}

	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		closeInput()
		if !waitForStop() {
			t.Fatal("lineSourcePromptYN did not stop after its input was closed")
		}
		t.Fatal("lineSourcePromptYN did not start an input read")
	}
	g.chunks <- []byte{'\x03'}
	select {
	case res := <-done:
		if res.approved || !errors.Is(res.err, context.Canceled) {
			t.Fatalf("lineSourcePromptYN(Ctrl-C) = %v, %v, want false, context.Canceled", res.approved, res.err)
		}
	case <-g.entered:
		closeInput()
		if !waitForStop() {
			t.Fatal("lineSourcePromptYN did not stop after its second input read was closed")
		}
		t.Fatal("lineSourcePromptYN(Ctrl-C) started another input read, want an immediate cancellation")
	case <-time.After(5 * time.Second):
		closeInput()
		if !waitForStop() {
			t.Fatal("lineSourcePromptYN did not stop after its input was closed")
		}
		t.Fatal("lineSourcePromptYN(Ctrl-C) did not return")
	}
}

// I11/I4: the manifest renders before any decision, grouped by deduplicated
// destination — one entry per destination however many purposes reach it —
// with every purpose edge visible and primary/fallback marked (D14 fields
// only).
func TestAdmissionRendersDedupedManifestWithEdges(t *testing.T) {
	p := &fakePrompt{answer: true}
	adm, out := newTestAdmission(t, admEdges(t), nil, true, p)
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := out.String()

	if n := strings.Count(got, "https://opencode.ai/zen/go"); n != 1 {
		t.Errorf("remote destination rendered %d times, want exactly 1 (deduped): %s", n, got)
	}
	for _, want := range []string{
		"llamacpp", "opencode", "local", "remote",
		"agent (fallback)", "summarize", "model-refresh",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest missing %q:\n%s", want, got)
		}
	}
}

// I12/I20/M21: an all-local manifest admits with no prompt and no
// interaction — local workflows continue exactly as before.
func TestAdmissionLocalOnlyNeverPrompts(t *testing.T) {
	local := admDest(t, "llamacpp", "http://127.0.0.1:8090")
	edges := []provider.DestinationEdge{
		{Purpose: "agent", Destination: local},
		{Purpose: provider.DestinationPurposeModelRefresh, Destination: local},
	}
	p := &fakePrompt{answer: false} // would deny if consulted
	adm, _ := newTestAdmission(t, edges, nil, true, p)

	if err := adm.ensure(context.Background()); err != nil {
		t.Fatalf("local-only admission: %v", err)
	}
	if p.calls != 0 {
		t.Errorf("prompt consulted %d times for local-only manifest, want 0", p.calls)
	}
	if _, err := adm.gate.Bind(context.Background(), "agent", "llamacpp"); err != nil {
		t.Errorf("local edge not admitted: %v", err)
	}
}

// D11: remote destinations get ONE batch decision. Yes admits exactly the
// manifest's remotes; no leaves the gate deny-all.
func TestAdmissionInteractiveBatchDecision(t *testing.T) {
	t.Run("approved", func(t *testing.T) {
		p := &fakePrompt{answer: true}
		adm, _ := newTestAdmission(t, admEdges(t), nil, true, p)
		if err := adm.ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if p.calls != 1 {
			t.Fatalf("prompt consulted %d times, want exactly 1 (batch)", p.calls)
		}
		if _, err := adm.gate.Bind(context.Background(), "summarize", "opencode"); err != nil {
			t.Errorf("approved remote edge denied: %v", err)
		}
		// D4: the approval grants the EXACT manifest set, never allow-all.
		// This matters beyond hygiene: Narrow carries the policy into later
		// generations, and allow-all would silently admit whatever a future
		// manifest adds.
		unrelated := admDest(t, "elsewhere", "https://elsewhere.example.com")
		if adm.granted.Permits(unrelated) {
			t.Error("granted policy permits an unlisted remote; approval must be the exact set")
		}
	})

	t.Run("prompt error admits nothing", func(t *testing.T) {
		// A Ctrl-C or read failure at the consent question is neither a yes
		// nor a quiet no-op: the error surfaces and the gate stays deny-all.
		p := &fakePrompt{answer: true, err: errors.New("interrupted")}
		adm, _ := newTestAdmission(t, admEdges(t), nil, true, p)
		if err := adm.ensure(context.Background()); err == nil {
			t.Fatal("prompt error swallowed")
		}
		if _, err := adm.gate.Bind(context.Background(), "agent", "llamacpp"); !errors.Is(err, provider.ErrDestinationDenied) {
			t.Error("gate not deny-all after prompt error")
		}
	})

	t.Run("declined", func(t *testing.T) {
		p := &fakePrompt{answer: false}
		adm, _ := newTestAdmission(t, admEdges(t), nil, true, p)
		err := adm.ensure(context.Background())
		if !errors.Is(err, provider.ErrDestinationDenied) {
			t.Fatalf("declined admission = %v, want ErrDestinationDenied", err)
		}
		if _, err := adm.gate.Bind(context.Background(), "agent", "llamacpp"); !errors.Is(err, provider.ErrDestinationDenied) {
			t.Error("gate not deny-all after decline")
		}
	})
}

// I6/M6: noninteractive with an uncovered remote fails closed, naming the
// destination, a use case that reaches it, and the exact flag value to fix
// it. Never a prompt.
func TestAdmissionNoninteractiveFailsClosed(t *testing.T) {
	p := &fakePrompt{answer: true} // must not be consulted
	adm, _ := newTestAdmission(t, admEdges(t), nil, false, p)

	err := adm.ensure(context.Background())
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("noninteractive uncovered remote = %v, want ErrDestinationDenied", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"opencode", "https://opencode.ai/zen/go",
		"agent", // a use case that reaches it
		"-allow-destination", "opencode/https://opencode.ai/zen/go",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnostic missing %q: %s", want, msg)
		}
	}
	if p.calls != 0 {
		t.Errorf("noninteractive path consulted the prompt %d times", p.calls)
	}
}

// The exact allowlist covers noninteractive remotes; equivalent spellings of
// the canonical form count as coverage.
func TestAdmissionAllowlistCoversNoninteractive(t *testing.T) {
	p := &fakePrompt{answer: false} // must not be consulted
	adm, _ := newTestAdmission(t, admEdges(t),
		[]string{"opencode/HTTPS://opencode.ai:443/zen/go/"}, false, p)

	if err := adm.ensure(context.Background()); err != nil {
		t.Fatalf("allowlisted noninteractive admission: %v", err)
	}
	if p.calls != 0 {
		t.Error("allowlisted path consulted the prompt")
	}
	if _, err := adm.gate.Bind(context.Background(), "summarize", "opencode"); err != nil {
		t.Errorf("allowlisted remote edge denied: %v", err)
	}
}

// An allowlist entry that covers SOME remotes does not stand in for the
// rest: the uncovered one is still named.
func TestAdmissionPartialAllowlistStillFails(t *testing.T) {
	other := admDest(t, "otherhost", "https://other.example.com")
	edges := append(admEdges(t), provider.DestinationEdge{Purpose: "code-review", Destination: other})
	p := &fakePrompt{}
	adm, _ := newTestAdmission(t, edges,
		[]string{"opencode/https://opencode.ai/zen/go"}, false, p)

	err := adm.ensure(context.Background())
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("partially covered = %v, want ErrDestinationDenied", err)
	}
	if !strings.Contains(err.Error(), "other.example.com") {
		t.Errorf("diagnostic does not name the uncovered destination: %v", err)
	}
}

// A malformed -allow-destination flag is a construction error naming the
// flag, not a silent no-grant.
func TestAdmissionRejectsMalformedAllowFlag(t *testing.T) {
	_, err := newDestinationAdmission(destinationAdmissionConfig{
		Gate:       provider.NewDestinationGate(),
		Edges:      admEdges(t),
		AllowFlags: []string{"not-a-destination"},
		Out:        &strings.Builder{},
	})
	if err == nil {
		t.Fatal("malformed allow flag accepted")
	}
	if !strings.Contains(err.Error(), "-allow-destination") {
		t.Errorf("error does not name the flag: %v", err)
	}
}

// -allow-destination accepts the canonical "<provider>/<base URL>" grant and
// the deprecated "<provider>=<base URL>" spelling go-llm-mcp historically
// used; both admit the same canonical destination identity.
func TestAdmissionAllowFlagBothForms(t *testing.T) {
	tests := []struct {
		name string
		flag string
	}{
		{name: "canonical", flag: "opencode/HTTPS://opencode.ai:443/zen/go/"},
		// The legacy marker is the historic go-llm-mcp grammar exactly: a
		// lowercase scheme directly after "=". Canonicalization stays
		// case-insensitive past the marker (the canonical row above admits an
		// uppercase scheme), but the marker itself is not.
		{name: "legacy equals", flag: "opencode=https://opencode.ai:443/zen/go/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakePrompt{answer: false} // must not be consulted
			adm, _ := newTestAdmission(t, admEdges(t), []string{tt.flag}, false, p)
			if err := adm.ensure(context.Background()); err != nil {
				t.Fatalf("allowlisted noninteractive admission: %v", err)
			}
			if p.calls != 0 {
				t.Error("allowlisted path consulted the prompt")
			}
			if _, err := adm.gate.Bind(context.Background(), "summarize", "opencode"); err != nil {
				t.Errorf("allowlisted remote edge denied: %v", err)
			}
		})
	}
}

// I15/M16: destination authority and tool grants are separate stores. A tool
// grant confers nothing on the gate, and admission writes nothing into the
// tool-grant store.
func TestAdmissionSeparateFromToolGrants(t *testing.T) {
	grants := newApprovalGrants()
	grants.grant(grantScopeExec, "exec:v3:whatever")

	p := &fakePrompt{answer: false}
	adm, _ := newTestAdmission(t, admEdges(t), nil, false, p)
	_ = adm.ensure(context.Background()) // denied: no coverage

	if _, err := adm.gate.Bind(context.Background(), "agent", "opencode"); !errors.Is(err, provider.ErrDestinationDenied) {
		t.Error("a tool grant leaked into destination authority")
	}
	if got := grants.count(); got != 1 {
		t.Errorf("admission changed the tool-grant store: count %d, want 1", got)
	}
	if got := grantScope("destination"); got != "" {
		t.Errorf("grantScope maps a destination pseudo-tool to %q; destination authority must never ride the tool path", got)
	}
}

// I16/M17: revoke clears the gate atomically and marks re-admission pending;
// ensure() then re-runs the SAME batch gate. Old capabilities stay dead.
func TestAdmissionRevokeThenReensure(t *testing.T) {
	p := &fakePrompt{answer: true}
	adm, _ := newTestAdmission(t, admEdges(t), nil, true, p)
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldCtx, err := adm.gate.Bind(context.Background(), "agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}

	adm.revoke()
	if _, err := adm.gate.Bind(context.Background(), "agent", "opencode"); !errors.Is(err, provider.ErrDestinationDenied) {
		t.Error("gate not deny-all after revoke")
	}

	// Re-ensure prompts again (the same batch gate, not a cached yes).
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatalf("re-admission: %v", err)
	}
	if p.calls != 2 {
		t.Errorf("prompt consulted %d times across revoke/re-admit, want 2", p.calls)
	}
	// Capability from the pre-revoke generation is dead even after re-admit.
	freshCtx, err := adm.gate.Bind(context.Background(), "agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}
	_ = freshCtx
	_ = oldCtx // its snapshot pointer differs; transport-level death is pinned in provider tests
}

// ensure() after a successful admission is a no-op: no second prompt, no
// re-render — a grant covers the session until revoked.
func TestAdmissionEnsureIsIdempotent(t *testing.T) {
	p := &fakePrompt{answer: true}
	adm, out := newTestAdmission(t, admEdges(t), nil, true, p)
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	rendered := out.Len()
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Errorf("idempotent ensure consulted the prompt %d times, want 1", p.calls)
	}
	if out.Len() != rendered {
		t.Error("idempotent ensure re-rendered the manifest")
	}
}

// I19/M20: the rendering and every error carry no credential. The config's
// API key never enters the admission surface by construction (Destination
// excludes it), pinned with a canary in the allow flag position where a raw
// string COULD leak.
func TestAdmissionSurfacesCarryNoSecrets(t *testing.T) {
	const canary = "SECRET-CANARY-55107"
	_, err := newDestinationAdmission(destinationAdmissionConfig{
		Gate:       provider.NewDestinationGate(),
		Edges:      admEdges(t),
		AllowFlags: []string{"opencode/https://user:" + canary + "@opencode.ai/zen/go"},
		Out:        &strings.Builder{},
	})
	if err == nil {
		t.Fatal("userinfo-bearing allow flag accepted")
	}
	if strings.Contains(err.Error(), canary) {
		t.Errorf("allow-flag error leaked the canary: %v", err)
	}
}

// The REPL wiring: /grants clear revokes destinations and the next GOAL
// (not the next slash command) re-runs the batch gate; /new, /clear, and
// /resume leave destination authority alone (D12).
func TestReplGrantsClearRevokesDestinations(t *testing.T) {
	p := &fakePrompt{answer: true}
	adm, _ := newTestAdmission(t, admEdges(t), nil, true, p)
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}

	sess := &replSession{grants: newApprovalGrants(), destAdmission: adm}
	var out strings.Builder

	dispatchSlash(context.Background(), &out, sess, "/grants clear")
	if _, err := adm.gate.Bind(context.Background(), "agent", "opencode"); !errors.Is(err, provider.ErrDestinationDenied) {
		t.Error("/grants clear left destination authority in place")
	}
	if !strings.Contains(out.String(), "destination") {
		t.Errorf("/grants clear output does not mention destinations: %s", out.String())
	}

	// /new and /clear do NOT revoke destination authority (D12): re-admit,
	// then reset the conversation.
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	dispatchSlash(context.Background(), &out, sess, "/new")
	dispatchSlash(context.Background(), &out, sess, "/clear")
	if _, err := adm.gate.Bind(context.Background(), "agent", "opencode"); err != nil {
		t.Errorf("conversation reset revoked destination authority: %v", err)
	}
}

func TestReplGrantsStatusShowsDestinations(t *testing.T) {
	p := &fakePrompt{answer: true}
	adm, _ := newTestAdmission(t, admEdges(t), nil, true, p)
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess := &replSession{grants: newApprovalGrants(), destAdmission: adm}
	var out strings.Builder
	dispatchSlash(context.Background(), &out, sess, "/grants")
	if !strings.Contains(out.String(), "opencode") {
		t.Errorf("/grants status does not show destination grants: %s", out.String())
	}
}

// admCountedDest serves one counted backend and returns the destination that
// names it. Every httptest listener is loopback, so a counted destination is
// always LOCAL; the remotes below are unreachable literals, which is what
// makes consent — not connectivity — the thing under test.
func admCountedDest(t *testing.T, prov string) (provider.Destination, *atomic.Int64) {
	t.Helper()
	reqs := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqs.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return admDest(t, prov, srv.URL), reqs
}

// admGuardedGet issues one request to dest through a guarded client using
// ctx. The guard denies before a byte leaves, so the server's counter tells
// "denied" apart from "served".
func admGuardedGet(ctx context.Context, t *testing.T, gate *provider.DestinationGate, dest provider.Destination) error {
	t.Helper()
	hc, err := provider.GuardHTTPClient(gate, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dest.BaseURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// M2: a candidate that reaches a NEW remote destination takes exactly one
// consent decision and one publication, reports the new grant, and — because
// the publication is additive — leaves a capability bound before it alive.
func TestAdmissionExtendAdmitsNewRemoteWithOneConsent(t *testing.T) {
	base, baseReqs := admCountedDest(t, "llamacpp")
	edges := []provider.DestinationEdge{
		{Purpose: "agent", Destination: base},
		{Purpose: provider.DestinationPurposeModelRefresh, Destination: base},
	}
	p := &fakePrompt{answer: true}
	adm, _ := newTestAdmission(t, edges, nil, true, p)
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 {
		t.Fatalf("local-only startup prompted %d times, want 0", p.calls)
	}
	inflight, err := adm.gate.Bind(context.Background(), "agent", "llamacpp")
	if err != nil {
		t.Fatal(err)
	}

	remote := admDest(t, "cloud", "https://cloud.example.com")
	got, err := adm.extend(context.Background(), []provider.DestinationEdge{
		{Purpose: "agent", Destination: remote, IsFallback: true},
		{Purpose: provider.DestinationPurposeModelRefresh, Destination: remote},
	})
	if err != nil {
		t.Fatalf("extend with a new remote = %v, want nil", err)
	}
	if !got {
		t.Error("extend reported no new grant for a newly consented remote")
	}
	if p.calls != 1 {
		t.Errorf("extend consulted the prompt %d times, want exactly 1 (one decision for the complete proposal)", p.calls)
	}

	// Additive publication: the capability bound BEFORE the switch still
	// authorizes, so an in-flight request is not killed by a model switch.
	if err := admGuardedGet(inflight, t, adm.gate, base); err != nil {
		t.Fatalf("capability bound before extend stopped authorizing: %v", err)
	}
	if n := baseReqs.Load(); n != 1 {
		t.Errorf("pre-extend capability served %d requests, want 1", n)
	}
	if _, err := adm.gate.Bind(context.Background(), "agent", "cloud"); err != nil {
		t.Errorf("candidate edge not admitted: %v", err)
	}
	if _, err := adm.gate.Bind(context.Background(), provider.DestinationPurposeModelRefresh, "llamacpp"); err != nil {
		t.Errorf("retained edge lost by extend: %v", err)
	}
	if !adm.granted.Permits(remote) {
		t.Error("granted policy does not record the newly approved remote")
	}
	if adm.granted.Permits(admDest(t, "elsewhere", "https://elsewhere.example.com")) {
		t.Error("granted policy permits an unlisted remote; approval must be the exact set")
	}
}

// M2: the outcome is false whenever the COMPLETE proposal needs no new or
// renewed remote permission — existing grants, local destinations, an exact
// flag the startup manifest never used, and purpose-only additions. Each row
// still publishes the candidate edges.
func TestAdmissionExtendWithoutNewRemoteConsent(t *testing.T) {
	remote := admDest(t, "opencode", "https://opencode.ai/zen/go")
	unused := admDest(t, "sidekick", "https://sidekick.example.com")
	sidecar := admDest(t, "sidecar", "http://127.0.0.1:8099")
	tests := []struct {
		name  string
		allow []string
		cand  []provider.DestinationEdge
	}{
		{
			name: "purpose-only addition to a granted destination",
			cand: []provider.DestinationEdge{{Purpose: "chat", Destination: remote}},
		},
		{
			name: "local candidate",
			cand: []provider.DestinationEdge{{Purpose: "chat", Destination: sidecar}},
		},
		{
			name:  "exact flag the startup manifest never used",
			allow: []string{"sidekick/https://sidekick.example.com"},
			cand:  []provider.DestinationEdge{{Purpose: "chat", Destination: unused}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakePrompt{answer: true}
			adm, _ := newTestAdmission(t, admEdges(t), tt.allow, true, p)
			if err := adm.ensure(context.Background()); err != nil {
				t.Fatal(err)
			}
			if p.calls != 1 {
				t.Fatalf("startup admission prompted %d times, want 1", p.calls)
			}

			got, err := adm.extend(context.Background(), tt.cand)
			if err != nil {
				t.Fatalf("extend = %v, want nil", err)
			}
			if got {
				t.Error("extend reported a new grant for a proposal the baseline already permits")
			}
			if p.calls != 1 {
				t.Errorf("extend asked for consent again (%d prompts total), want the startup decision only", p.calls)
			}
			for _, e := range tt.cand {
				if _, err := adm.gate.Bind(context.Background(), e.Purpose, e.Destination.Provider()); err != nil {
					t.Errorf("candidate edge %q -> %s not admitted: %v", e.Purpose, e.Destination, err)
				}
			}
		})
	}
}

// M2: denial, a prompt failure, a malformed proposal, and the noninteractive
// path all return (false, err) and leave BOTH the gate and the bookkeeping
// exactly as they were.
func TestAdmissionExtendFailuresChangeNothing(t *testing.T) {
	remote := admDest(t, "cloud", "https://cloud.example.com")
	newEdge := provider.DestinationEdge{Purpose: "chat", Destination: remote}
	tests := []struct {
		name        string
		interactive bool
		prompt      *fakePrompt
		cand        []provider.DestinationEdge
		wantPrompts int
		wantDenied  bool
	}{
		{
			name:        "declined",
			interactive: true,
			prompt:      &fakePrompt{answer: false},
			cand:        []provider.DestinationEdge{newEdge},
			wantPrompts: 1,
			wantDenied:  true,
		},
		{
			name:        "prompt error",
			interactive: true,
			prompt:      &fakePrompt{answer: true, err: errors.New("interrupted")},
			cand:        []provider.DestinationEdge{newEdge},
			wantPrompts: 1,
		},
		{
			name:        "noninteractive",
			interactive: false,
			prompt:      &fakePrompt{answer: true},
			cand:        []provider.DestinationEdge{newEdge},
			wantDenied:  true,
		},
		{
			name:        "malformed proposal retargets a frozen provider",
			interactive: true,
			prompt:      &fakePrompt{answer: true},
			cand: []provider.DestinationEdge{
				{Purpose: "chat", Destination: admDest(t, "opencode", "https://elsewhere.example.com")},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			startup := &fakePrompt{answer: true}
			adm, _ := newTestAdmission(t, admEdges(t), nil, true, startup)
			if err := adm.ensure(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantManifest, wantGranted := adm.manifest, adm.granted
			adm.setPrompt(tt.prompt.ask)
			adm.interactive = tt.interactive

			got, err := adm.extend(context.Background(), tt.cand)
			if err == nil {
				t.Fatal("extend succeeded, want an error")
			}
			if got {
				t.Error("failed extend reported a new grant")
			}
			if tt.wantDenied && !errors.Is(err, provider.ErrDestinationDenied) {
				t.Errorf("extend error = %v, want ErrDestinationDenied", err)
			}
			if tt.prompt.calls != tt.wantPrompts {
				t.Errorf("extend consulted the prompt %d times, want %d", tt.prompt.calls, tt.wantPrompts)
			}
			if adm.manifest != wantManifest {
				t.Error("failed extend replaced the stored manifest")
			}
			if !adm.admitted {
				t.Error("failed extend cleared the admitted flag")
			}
			if adm.granted.Permits(remote) != wantGranted.Permits(remote) {
				t.Error("failed extend changed the granted policy")
			}
			if _, err := adm.gate.Bind(context.Background(), "chat", remote.Provider()); !errors.Is(err, provider.ErrDestinationDenied) {
				t.Errorf("candidate edge bindable after a failed extend: %v", err)
			}
			if _, err := adm.gate.Bind(context.Background(), "agent", "opencode"); err != nil {
				t.Errorf("failed extend revoked the standing generation: %v", err)
			}
		})
	}
}

// The noninteractive diagnostic keeps ensure's shape: the destination, a
// purpose that reaches it, and the exact flag that would cover it.
func TestAdmissionExtendNoninteractiveNamesTheFlag(t *testing.T) {
	startup := &fakePrompt{answer: true}
	adm, _ := newTestAdmission(t, admEdges(t), nil, true, startup)
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := &fakePrompt{answer: true} // must not be consulted
	adm.setPrompt(p.ask)
	adm.interactive = false

	remote := admDest(t, "cloud", "https://cloud.example.com")
	_, err := adm.extend(context.Background(), []provider.DestinationEdge{{Purpose: "chat", Destination: remote}})
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("noninteractive extend = %v, want ErrDestinationDenied", err)
	}
	for _, want := range []string{
		"cloud", "https://cloud.example.com", "chat",
		"-allow-destination", "cloud/https://cloud.example.com",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic missing %q: %v", want, err)
		}
	}
	if p.calls != 0 {
		t.Errorf("noninteractive extend read %d consent answers, want 0", p.calls)
	}
}

// A proposal the installed generation already carries publishes nothing and
// says nothing: no re-render, no second consent question.
func TestAdmissionExtendAlreadyAdmittedIsSilent(t *testing.T) {
	p := &fakePrompt{answer: true}
	adm, out := newTestAdmission(t, admEdges(t), nil, true, p)
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	rendered := out.Len()
	wantManifest := adm.manifest

	got, err := adm.extend(context.Background(), []provider.DestinationEdge{
		{Purpose: "summarize", Destination: admDest(t, "opencode", "https://opencode.ai/zen/go")},
	})
	if err != nil || got {
		t.Fatalf("extend of an already admitted proposal = (%v, %v), want (false, nil)", got, err)
	}
	if p.calls != 1 {
		t.Errorf("extend consulted the prompt %d times, want the startup decision only", p.calls)
	}
	if out.Len() != rendered {
		t.Errorf("extend re-rendered an unchanged manifest:\n%s", out.String()[rendered:])
	}
	if adm.manifest != wantManifest {
		t.Error("extend republished an unchanged manifest")
	}
}

// M2: cancellation observed at the final pre-publication check installs
// nothing, even when the reader answered yes. A prompt that returns yes on a
// cancelled context is exactly the Ctrl-C-then-newline shape.
func TestAdmissionExtendCancelledAfterYesInstallsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startup := &fakePrompt{answer: true}
	adm, _ := newTestAdmission(t, admEdges(t), nil, true, startup)
	if err := adm.ensure(ctx); err != nil {
		t.Fatal(err)
	}
	wantManifest := adm.manifest
	p := &fakePrompt{answer: true, before: func(context.Context) { cancel() }}
	adm.setPrompt(p.ask)

	remote := admDest(t, "cloud", "https://cloud.example.com")
	got, err := adm.extend(ctx, []provider.DestinationEdge{{Purpose: "chat", Destination: remote}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("extend cancelled while answering = %v, want context.Canceled", err)
	}
	if got {
		t.Error("cancelled extend reported a new grant")
	}
	if adm.manifest != wantManifest {
		t.Error("cancelled extend replaced the stored manifest")
	}
	if adm.granted.Permits(remote) {
		t.Error("cancelled extend recorded the grant it never published")
	}
	if _, err := adm.gate.Bind(context.Background(), "chat", "cloud"); !errors.Is(err, provider.ErrDestinationDenied) {
		t.Errorf("cancelled extend admitted the candidate edge: %v", err)
	}
	if _, err := adm.gate.Bind(context.Background(), "agent", "opencode"); err != nil {
		t.Errorf("cancelled extend disturbed the standing generation: %v", err)
	}
}

// M2: a publication that loses to a generation change made while consent was
// being collected fails — with no retry, and without resurrecting the state
// the concurrent writer left behind.
func TestAdmissionExtendLostPublicationKeepsTheWinner(t *testing.T) {
	rogue := admDest(t, "rogue", "http://127.0.0.1:8098")

	tests := []struct {
		name  string
		under func(adm *destinationAdmission)
		check func(t *testing.T, adm *destinationAdmission)
	}{
		{
			name:  "gate cleared while the question was open",
			under: func(adm *destinationAdmission) { adm.gate.Clear() },
			check: func(t *testing.T, adm *destinationAdmission) {
				if _, err := adm.gate.Bind(context.Background(), "agent", "opencode"); !errors.Is(err, provider.ErrDestinationDenied) {
					t.Errorf("lost extend resurrected the revoked generation: %v", err)
				}
			},
		},
		{
			name: "competing generation installed while the question was open",
			under: func(adm *destinationAdmission) {
				m, err := provider.NewDestinationManifest(provider.DestinationEdge{Purpose: "rogue", Destination: rogue})
				if err != nil {
					panic(err)
				}
				var loopbackOnly provider.DestinationPolicy
				if err := adm.gate.Install(loopbackOnly, m); err != nil {
					panic(err)
				}
			},
			check: func(t *testing.T, adm *destinationAdmission) {
				if _, err := adm.gate.Bind(context.Background(), "rogue", "rogue"); err != nil {
					t.Errorf("lost extend overwrote the winning generation: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			startup := &fakePrompt{answer: true}
			adm, _ := newTestAdmission(t, admEdges(t), nil, true, startup)
			if err := adm.ensure(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantManifest := adm.manifest
			p := &fakePrompt{answer: true, before: func(context.Context) { tt.under(adm) }}
			adm.setPrompt(p.ask)

			remote := admDest(t, "cloud", "https://cloud.example.com")
			got, err := adm.extend(context.Background(), []provider.DestinationEdge{{Purpose: "chat", Destination: remote}})
			if err == nil {
				t.Fatal("extend published over a concurrent generation change")
			}
			if got {
				t.Error("lost extend reported a new grant")
			}
			if p.calls != 1 {
				t.Errorf("extend consulted the prompt %d times, want 1 (no retry with the same consent)", p.calls)
			}
			if adm.manifest != wantManifest {
				t.Error("lost extend replaced the stored manifest")
			}
			if _, err := adm.gate.Bind(context.Background(), "chat", "cloud"); !errors.Is(err, provider.ErrDestinationDenied) {
				t.Errorf("lost extend admitted the candidate edge: %v", err)
			}
			tt.check(t, adm)
		})
	}
}

// M2 after /grants clear: the retained edges are still known but no longer
// authorized, so ONE decision covers the complete old-plus-candidate
// proposal. The stale granted policy is never the baseline — a remote it
// still names must be asked for again, and re-approving it is a new grant
// even for a purely LOCAL candidate.
func TestAdmissionExtendAfterRevoke(t *testing.T) {
	sidecar := admDest(t, "sidecar", "http://127.0.0.1:8099")
	localCandidate := []provider.DestinationEdge{{Purpose: "chat", Destination: sidecar}}

	t.Run("approved renews the retained remote in one decision", func(t *testing.T) {
		p := &fakePrompt{answer: true}
		adm, out := newTestAdmission(t, admEdges(t), nil, true, p)
		if err := adm.ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		adm.revoke()

		got, err := adm.extend(context.Background(), localCandidate)
		if err != nil {
			t.Fatalf("extend after revoke = %v, want nil", err)
		}
		if !got {
			t.Error("extend reported no new grant although the retained remote needed renewed consent")
		}
		if p.calls != 2 {
			t.Errorf("prompt consulted %d times across startup and re-admission, want 2 (one decision each)", p.calls)
		}
		if n := strings.Count(out.String(), "destinations:"); n != 2 {
			t.Errorf("manifest rendered %d times, want 2 (one per publication, not one per stage):\n%s", n, out.String())
		}
		for _, e := range append(admEdges(t), localCandidate...) {
			if _, err := adm.gate.Bind(context.Background(), e.Purpose, e.Destination.Provider()); err != nil {
				t.Errorf("edge %q -> %s not admitted by the re-admission: %v", e.Purpose, e.Destination, err)
			}
		}
	})

	t.Run("refused proposals leave the gate revoked", func(t *testing.T) {
		refusals := []struct {
			name   string
			prompt func(adm *destinationAdmission, cancel context.CancelFunc) *fakePrompt
			want   error
		}{
			{
				name:   "declined",
				prompt: func(*destinationAdmission, context.CancelFunc) *fakePrompt { return &fakePrompt{answer: false} },
				want:   provider.ErrDestinationDenied,
			},
			{
				name: "cancelled while answering",
				prompt: func(_ *destinationAdmission, cancel context.CancelFunc) *fakePrompt {
					return &fakePrompt{answer: true, before: func(context.Context) { cancel() }}
				},
				want: context.Canceled,
			},
		}
		for _, tt := range refusals {
			t.Run(tt.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				startup := &fakePrompt{answer: true}
				adm, _ := newTestAdmission(t, admEdges(t), nil, true, startup)
				if err := adm.ensure(ctx); err != nil {
					t.Fatal(err)
				}
				adm.revoke()
				wantManifest := adm.manifest
				adm.setPrompt(tt.prompt(adm, cancel).ask)

				got, err := adm.extend(ctx, localCandidate)
				if !errors.Is(err, tt.want) {
					t.Fatalf("extend after revoke = %v, want %v", err, tt.want)
				}
				if got {
					t.Error("refused extend reported a new grant")
				}
				if adm.admitted {
					t.Error("refused extend marked the session admitted")
				}
				if adm.manifest != wantManifest {
					t.Error("refused extend replaced the stored manifest")
				}
				for _, e := range append(admEdges(t), localCandidate...) {
					if _, err := adm.gate.Bind(context.Background(), e.Purpose, e.Destination.Provider()); !errors.Is(err, provider.ErrDestinationDenied) {
						t.Errorf("edge %q -> %s authorized after a refused re-admission: %v", e.Purpose, e.Destination, err)
					}
				}
			})
		}
	})

	t.Run("flags-only baseline re-admits without a prompt", func(t *testing.T) {
		p := &fakePrompt{answer: false} // must not be consulted
		adm, _ := newTestAdmission(t, admEdges(t),
			[]string{"opencode/https://opencode.ai/zen/go"}, true, p)
		if err := adm.ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		adm.revoke()

		got, err := adm.extend(context.Background(), localCandidate)
		if err != nil {
			t.Fatalf("flag-covered re-admission = %v, want nil", err)
		}
		if got {
			t.Error("flag-covered re-admission reported a new grant")
		}
		if p.calls != 0 {
			t.Errorf("flag-covered re-admission consulted the prompt %d times, want 0", p.calls)
		}
		if _, err := adm.gate.Bind(context.Background(), "summarize", "opencode"); err != nil {
			t.Errorf("flag-covered remote not re-admitted: %v", err)
		}
	})
}

// M2: no request may precede admission. Through the whole consent question
// the candidate destination is unreachable and has received nothing; only
// after publication does a guarded client get through.
func TestAdmissionExtendSendsNothingBeforeApproval(t *testing.T) {
	base, _ := admCountedDest(t, "llamacpp")
	candidate, candidateReqs := admCountedDest(t, "sidecar")
	remote := admDest(t, "cloud", "https://cloud.example.com")

	p := &fakePrompt{answer: true}
	adm, _ := newTestAdmission(t, []provider.DestinationEdge{{Purpose: "agent", Destination: base}}, nil, true, p)
	p.before = func(ctx context.Context) {
		if _, err := adm.gate.Bind(ctx, "chat", "sidecar"); !errors.Is(err, provider.ErrDestinationDenied) {
			t.Errorf("candidate edge bindable before consent: %v", err)
		}
		if err := admGuardedGet(ctx, t, adm.gate, candidate); !errors.Is(err, provider.ErrDestinationDenied) {
			t.Errorf("guarded request to an unadmitted candidate = %v, want a destination denial", err)
		}
		if n := candidateReqs.Load(); n != 0 {
			t.Errorf("candidate destination served %d requests before approval, want 0", n)
		}
	}
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := adm.extend(context.Background(), []provider.DestinationEdge{
		{Purpose: "chat", Destination: candidate},
		{Purpose: "chat", Destination: remote, IsFallback: true},
	})
	if err != nil || !got {
		t.Fatalf("extend = (%v, %v), want (true, nil)", got, err)
	}
	if p.calls != 1 {
		t.Fatalf("prompt consulted %d times, want 1", p.calls)
	}
	bound, err := adm.gate.Bind(context.Background(), "chat", "sidecar")
	if err != nil {
		t.Fatalf("candidate edge not admitted: %v", err)
	}
	if err := admGuardedGet(bound, t, adm.gate, candidate); err != nil {
		t.Fatalf("admitted candidate request denied: %v", err)
	}
	if n := candidateReqs.Load(); n != 1 {
		t.Errorf("candidate destination served %d requests after approval, want 1", n)
	}
}

// D12 over the widened manifest: one /grants clear revokes everything the
// session admitted, candidate edges included.
func TestAdmissionRevokeAfterExtendRevokesEveryEdge(t *testing.T) {
	p := &fakePrompt{answer: true}
	adm, _ := newTestAdmission(t, admEdges(t), nil, true, p)
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.extend(context.Background(), []provider.DestinationEdge{
		{Purpose: "chat", Destination: admDest(t, "cloud", "https://cloud.example.com")},
		{Purpose: "chat", Destination: admDest(t, "sidecar", "http://127.0.0.1:8099")},
	}); err != nil {
		t.Fatal(err)
	}

	adm.revoke()
	edges := adm.manifest.Edges()
	if len(edges) != 8 {
		t.Fatalf("widened manifest holds %d edges, want the 6 startup edges plus 2 candidates", len(edges))
	}
	for _, e := range edges {
		if _, err := adm.gate.Bind(context.Background(), e.Purpose, e.Destination.Provider()); !errors.Is(err, provider.ErrDestinationDenied) {
			t.Errorf("edge %q -> %s survived revoke: %v", e.Purpose, e.Destination, err)
		}
	}
	if adm.admitted {
		t.Error("revoke left the session marked admitted")
	}
}

// The admission mutex serializes extend against revoke: revoke cannot land
// between the publication and the bookkeeping that records it, so the two
// always agree afterwards. Barriers, not sleeps.
func TestAdmissionExtendSerializesWithRevoke(t *testing.T) {
	startup := &fakePrompt{answer: true}
	adm, _ := newTestAdmission(t, admEdges(t), nil, true, startup)
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	p := &fakePrompt{answer: true, before: func(context.Context) {
		close(entered)
		<-release
	}}
	adm.setPrompt(p.ask)

	type outcome struct {
		granted bool
		err     error
	}
	extended := make(chan outcome, 1)
	go func() {
		granted, err := adm.extend(context.Background(), []provider.DestinationEdge{
			{Purpose: "chat", Destination: admDest(t, "cloud", "https://cloud.example.com")},
		})
		extended <- outcome{granted: granted, err: err}
	}()

	<-entered // extend holds the mutex, mid-decision
	revoked := make(chan struct{})
	go func() {
		adm.revoke()
		close(revoked)
	}()
	close(release)

	res := <-extended
	<-revoked
	if res.err != nil || !res.granted {
		t.Fatalf("extend = (%v, %v), want (true, nil)", res.granted, res.err)
	}
	if adm.admitted {
		t.Error("bookkeeping says admitted while revoke cleared the gate")
	}
	for _, e := range adm.manifest.Edges() {
		if _, err := adm.gate.Bind(context.Background(), e.Purpose, e.Destination.Provider()); !errors.Is(err, provider.ErrDestinationDenied) {
			t.Errorf("edge %q -> %s authorized after revoke: %v", e.Purpose, e.Destination, err)
		}
	}
}
