package configio

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestProbeToolCall_InvalidKey(t *testing.T) {
	r := &fakeResolver{state: fingerprint.CapProbeYes}
	for _, k := range []struct{ prov, model string }{{"", "m"}, {"p", ""}, {"", ""}} {
		out, err := ProbeToolCall(context.Background(), r, key(k.prov, k.model))
		if err == nil {
			t.Fatalf("ProbeToolCall(%q/%q) error = nil; want invalid_argument", k.prov, k.model)
		}
		if code, ok := CodeOf(err); !ok || code != CodeInvalidArgument {
			t.Fatalf("CodeOf = %q, %v; want %q", code, ok, CodeInvalidArgument)
		}
		if out != (ProbeOutcome{}) {
			t.Fatalf("outcome = %+v; want zero on error", out)
		}
	}
	if r.calls != 0 {
		t.Fatalf("resolver called %d times for invalid keys; want 0", r.calls)
	}
}

func TestProbeToolCall_FastPathAnswersSurviveUnwiredRegistry(t *testing.T) {
	// ResolveToolCall answers authoritatively from explicit overrides or
	// merged profiles BEFORE checking probe wiring; the op forwards such
	// answers (resolve-first). Explicit-source answers are durable.
	for _, want := range []fingerprint.CapProbeState{fingerprint.CapProbeYes, fingerprint.CapProbeNo} {
		r := &fakeResolver{state: want, exp: provider.ToolCallExplanation{Source: "explicit"}}
		out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
		if err != nil {
			t.Fatalf("ProbeToolCall() error: %v; a fast-path %q answer must pass through", err, want)
		}
		if out.State != want {
			t.Fatalf("state = %q; want %q", out.State, want)
		}
		if !out.Persisted {
			t.Fatalf("Persisted = false for explicit-source %q; declared knowledge is durable", want)
		}
	}
}

func TestProbeToolCall_EmptyStateNilErrorIsUnavailable(t *testing.T) {
	r := &fakeResolver{state: ""}
	out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
	if err == nil {
		t.Fatal("ProbeToolCall() error = nil for unresolvable model; explicit probe must be loud")
	}
	if code, ok := CodeOf(err); !ok || code != CodeProbeUnavailable {
		t.Fatalf("CodeOf = %q, %v; want %q", code, ok, CodeProbeUnavailable)
	}
	if out != (ProbeOutcome{}) {
		t.Fatalf("outcome = %+v; want zero", out)
	}
}

func TestProbeToolCall_PersistedTrueRequiresValidRow(t *testing.T) {
	// The row-match durability branch demands exp.Valid: ExplainToolCall
	// reports stale-row State separately from validity, and a stale
	// same-state row will NOT validate next session.
	valid := &fakeResolver{
		state: fingerprint.CapProbeNo,
		exp:   provider.ToolCallExplanation{Source: "probe", State: fingerprint.CapProbeNo, Valid: true},
	}
	out, err := ProbeToolCall(context.Background(), valid, key("p", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v", err)
	}
	if out.State != fingerprint.CapProbeNo || !out.Persisted {
		t.Fatalf("outcome = %+v; want {no, Persisted:true} for a VALID matching row", out)
	}

	// Same state, STALE row (save failed; an old same-verdict row remains):
	// must NOT claim durability.
	stale := &fakeResolver{
		state: fingerprint.CapProbeNo,
		exp:   provider.ToolCallExplanation{Source: "probe", State: fingerprint.CapProbeNo, Valid: false},
	}
	out, err = ProbeToolCall(context.Background(), stale, key("p", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v", err)
	}
	if out.State != fingerprint.CapProbeNo || out.Persisted {
		t.Fatalf("outcome = %+v; want {no, Persisted:false} — a stale same-state row is not durable", out)
	}
}

func TestProbeToolCall_PersistedFalseWhenRowLost(t *testing.T) {
	r := &fakeResolver{
		state: fingerprint.CapProbeNo,
		exp:   provider.ToolCallExplanation{Source: "unknown"},
	}
	out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v; lost persistence is a warning, not a failure", err)
	}
	if out.State != fingerprint.CapProbeNo || out.Persisted {
		t.Fatalf("outcome = %+v; want {no, Persisted:false}", out)
	}
}

func TestProbeToolCall_PersistedTrueWhenProfileCarriesYes(t *testing.T) {
	r := &fakeResolver{
		state: fingerprint.CapProbeYes,
		exp:   provider.ToolCallExplanation{Source: "runtime", Has: true},
	}
	out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v", err)
	}
	if !out.Persisted {
		t.Fatalf("outcome = %+v; profile-carried yes re-derives next session — durable", out)
	}
}

func TestProbeToolCall_ExplainFailureMeansPersistedFalseNotError(t *testing.T) {
	r := &fakeResolver{
		state:  fingerprint.CapProbeYes,
		expErr: errors.New("store read failed"),
	}
	out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v; the verdict stands, persistence unknown", err)
	}
	if out.State != fingerprint.CapProbeYes || out.Persisted {
		t.Fatalf("outcome = %+v; want {yes, Persisted:false} (conservative)", out)
	}
}

func TestProbeToolCall_TransientFailure(t *testing.T) {
	cause := errors.New("401 unauthorized")
	r := &fakeResolver{err: cause}
	out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
	if err == nil {
		t.Fatal("ProbeToolCall() error = nil; want probe_failed")
	}
	if code, ok := CodeOf(err); !ok || code != CodeProbeFailed {
		t.Fatalf("CodeOf = %q, %v; want %q", code, ok, CodeProbeFailed)
	}
	if !errors.Is(err, cause) {
		t.Fatal("cause lost from the error chain")
	}
	if !strings.Contains(err.Error(), "p/m") {
		t.Fatalf("error %q does not name the model key", err)
	}
	if out != (ProbeOutcome{}) {
		t.Fatalf("outcome = %+v; want zero", out)
	}
}

func TestProbeToolCall_PreCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &fakeResolver{state: fingerprint.CapProbeYes}
	out, err := ProbeToolCall(ctx, r, key("p", "m"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
	if code, ok := CodeOf(err); ok {
		t.Fatalf("cancellation classified as %q; must stay unclassified", code)
	}
	if out != (ProbeOutcome{}) || r.calls != 0 {
		t.Fatalf("outcome=%+v calls=%d; pre-cancelled probe must do and publish nothing", out, r.calls)
	}
}

func TestProbeToolCall_CancelledCallerGetsNothingEvenWithADeliveredResult(t *testing.T) {
	// Singleflight followers receive the leader's completed result even
	// when their own context died. The POST-barrier withholds it: the
	// cancelled CALLER receives nothing (the #456 promise).
	ctx, cancel := context.WithCancel(context.Background())
	r := &fakeResolver{state: fingerprint.CapProbeYes}
	r.onCall = func(context.Context) { cancel() }
	out, err := ProbeToolCall(ctx, r, key("p", "m"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled (post-barrier)", err)
	}
	if code, ok := CodeOf(err); ok {
		t.Fatalf("cancellation classified as %q; must stay unclassified", code)
	}
	if out != (ProbeOutcome{}) {
		t.Fatalf("outcome = %+v; a cancelled caller must receive nothing", out)
	}
}
