package main

import (
	"context"
	"errors"
	"strings"
	"testing"

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
}

func (f *fakePrompt) ask(_ context.Context, prompt string) (bool, error) {
	f.calls++
	f.seen = prompt
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
