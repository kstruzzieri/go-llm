package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestDefaultStrength(t *testing.T) {
	cases := []struct {
		kind RoutingSignalKind
		want float64
	}{
		{RoutingSignalSuccess, +0.5},
		{RoutingSignalFailure, -0.7},
		{RoutingSignalLatency, 0.0},
		{RoutingSignalKind("unknown"), 0.0},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			if got := DefaultStrength(tc.kind); got != tc.want {
				t.Fatalf("DefaultStrength(%q) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

func TestAttemptStatusString(t *testing.T) {
	cases := []struct {
		name string
		s    AttemptStatus
		want string
	}{
		{"unknown", AttemptStatusUnknown, "unknown"},
		{"succeeded", AttemptStatusSucceeded, "succeeded"},
		{"failed", AttemptStatusFailed, "failed"},
		{"out_of_range", AttemptStatus(99), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAttemptStatusJSON(t *testing.T) {
	type wrap struct {
		S AttemptStatus `json:"s"`
	}
	cases := []struct {
		in   AttemptStatus
		want string
	}{
		{AttemptStatusUnknown, `{"s":"unknown"}`},
		{AttemptStatusSucceeded, `{"s":"succeeded"}`},
		{AttemptStatusFailed, `{"s":"failed"}`},
	}
	for _, tc := range cases {
		t.Run(tc.in.String(), func(t *testing.T) {
			got, err := json.Marshal(wrap{S: tc.in})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRouteOutcomeJSONOmitsEmptyNewFields(t *testing.T) {
	out := RouteOutcome{} // zero-valued
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(b)
	if s == "" {
		t.Fatalf("empty marshal")
	}
	// Forward-compatible expectation: zero-value Attempts/RouteID must be
	// absent from the JSON output so existing consumers see no new keys.
	for _, key := range []string{`"attempts"`, `"route_id"`} {
		if strings.Contains(s, key) {
			t.Errorf("RouteOutcome JSON %s unexpectedly contains %s", s, key)
		}
	}
}

func TestNewRouteIDProducesDistinct32CharHex(t *testing.T) {
	id1 := newRouteID()
	id2 := newRouteID()
	if len(id1) != 32 {
		t.Fatalf("newRouteID() length = %d, want 32", len(id1))
	}
	if id1 == id2 {
		t.Fatalf("two consecutive newRouteID() values collided: %q", id1)
	}
	for _, c := range id1 {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("newRouteID() returned non-hex char %q in %q", c, id1)
		}
	}
}

func TestBuildOutcomePopulatesRouteID(t *testing.T) {
	rp := &RoutePlan{
		Profile: &ModelProfile{Key: ModelKey{Provider: "p", Model: "m"}},
		Score:   0.42,
		Reason:  "test",
	}
	out := rp.buildOutcome(0)
	if out == nil {
		t.Fatal("buildOutcome returned nil")
	}
	if out.RouteID == "" {
		t.Fatal("buildOutcome did not set RouteID")
	}
	if len(out.RouteID) != 32 {
		t.Fatalf("RouteID length = %d, want 32", len(out.RouteID))
	}
	if len(out.Attempts) != 0 {
		t.Fatalf("Attempts populated by buildOutcome (len=%d); PR2 owns this", len(out.Attempts))
	}
}

// routeIDFailingReader is an io.Reader stub for TestNewRouteIDReturnsEmptyOnRandFailure;
// every Read returns an error so newRouteID exercises its empty-string-on-error
// branch deterministically.
type routeIDFailingReader struct{}

func (routeIDFailingReader) Read(_ []byte) (int, error) {
	return 0, errRouteIDForcedFailure
}

var errRouteIDForcedFailure = errors.New("forced route id read failure")

func TestNewRouteIDReturnsEmptyOnRandFailure(t *testing.T) {
	orig := routeIDRand
	routeIDRand = routeIDFailingReader{}
	t.Cleanup(func() { routeIDRand = orig })

	if got := newRouteID(); got != "" {
		t.Fatalf("newRouteID() = %q, want empty string on RNG failure", got)
	}
}

func TestNewMemoryStoreAppliesDefaults(t *testing.T) {
	s, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	if s == nil {
		t.Fatal("NewMemoryStore returned nil store")
	}
	// Zero values must resolve to the documented defaults. We verify
	// indirectly through Get on an empty key (Score == DefaultNeutralScore).
	agg, err := s.Get(context.Background(), FeedbackKey{Provider: "p", Model: "m", UseCase: "c"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if agg.Score != DefaultNeutralScore {
		t.Fatalf("Score = %v, want %v", agg.Score, DefaultNeutralScore)
	}
}

func TestNewMemoryStoreRejectsOutOfRangeNeutralScore(t *testing.T) {
	cases := []float64{-0.01, 1.01, -1.0, 2.0}
	for _, n := range cases {
		t.Run("", func(t *testing.T) {
			_, err := NewMemoryStore(MemoryStoreConfig{NeutralScore: n})
			if err == nil {
				t.Fatalf("NewMemoryStore(NeutralScore=%v) returned nil error", n)
			}
		})
	}
}

func TestNewMemoryStoreCustomNeutralScore(t *testing.T) {
	s, err := NewMemoryStore(MemoryStoreConfig{NeutralScore: 0.42})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	agg, err := s.Get(context.Background(), FeedbackKey{Provider: "p", Model: "m", UseCase: "c"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if agg.Score != 0.42 {
		t.Fatalf("Score = %v, want 0.42", agg.Score)
	}
}

func validKey() FeedbackKey {
	return FeedbackKey{Provider: "p", Model: "m", UseCase: "c"}
}

func TestRecordRejectsInvalidKeyFields(t *testing.T) {
	s, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	cases := []FeedbackKey{
		{Provider: "", Model: "m", UseCase: "c"},
		{Provider: "p", Model: "", UseCase: "c"},
		{Provider: "p", Model: "m", UseCase: ""},
	}
	for _, k := range cases {
		t.Run("", func(t *testing.T) {
			err := s.Record(context.Background(), k, FeedbackSignal{Kind: RoutingSignalSuccess})
			if !errors.Is(err, ErrInvalidFeedbackKey) {
				t.Fatalf("Record err = %v, want ErrInvalidFeedbackKey", err)
			}
		})
	}
}

func TestRecordRejectsUnknownKind(t *testing.T) {
	s, _ := NewMemoryStore(MemoryStoreConfig{})
	err := s.Record(context.Background(), validKey(), FeedbackSignal{Kind: RoutingSignalKind("nope")})
	if !errors.Is(err, ErrUnknownSignalKind) {
		t.Fatalf("err = %v, want ErrUnknownSignalKind", err)
	}
}

func TestRecordRejectsBadStrength(t *testing.T) {
	s, _ := NewMemoryStore(MemoryStoreConfig{})
	nan := math.NaN()
	posInf := math.Inf(1)
	negInf := math.Inf(-1)
	cases := []*float64{&nan, &posInf, &negInf}
	for _, sp := range cases {
		err := s.Record(context.Background(), validKey(),
			FeedbackSignal{Kind: RoutingSignalSuccess, Strength: sp})
		if !errors.Is(err, ErrInvalidSignalStrength) {
			t.Fatalf("Strength=%v err = %v, want ErrInvalidSignalStrength", *sp, err)
		}
	}
}

func TestRecordRejectsBadPayload(t *testing.T) {
	s, _ := NewMemoryStore(MemoryStoreConfig{})
	cases := []struct {
		name string
		sig  FeedbackSignal
	}{
		{"latency-without-positive-ms", FeedbackSignal{Kind: RoutingSignalLatency, LatencyMs: 0}},
		{"latency-with-negative-ms", FeedbackSignal{Kind: RoutingSignalLatency, LatencyMs: -1}},
		{"success-with-latency-ms", FeedbackSignal{Kind: RoutingSignalSuccess, LatencyMs: 100}},
		{"failure-with-latency-ms", FeedbackSignal{Kind: RoutingSignalFailure, ErrorClass: "5xx", LatencyMs: 100}},
		{"failure-without-class", FeedbackSignal{Kind: RoutingSignalFailure}},
		{"success-with-error-class", FeedbackSignal{Kind: RoutingSignalSuccess, ErrorClass: "5xx"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Record(context.Background(), validKey(), tc.sig)
			if !errors.Is(err, ErrInvalidSignalPayload) {
				t.Fatalf("err = %v, want ErrInvalidSignalPayload", err)
			}
		})
	}
}

func TestRecordRejectsOversizedMeta(t *testing.T) {
	s, _ := NewMemoryStore(MemoryStoreConfig{MaxMetaKeys: 2, MaxMetaValueBytes: 3})
	too := map[string]string{"a": "1", "b": "2", "c": "3"}
	if err := s.Record(context.Background(), validKey(),
		FeedbackSignal{Kind: RoutingSignalSuccess, Meta: too}); !errors.Is(err, ErrMetaTooLarge) {
		t.Fatalf("too-many-keys err = %v, want ErrMetaTooLarge", err)
	}
	big := map[string]string{"a": strings.Repeat("x", 4)}
	if err := s.Record(context.Background(), validKey(),
		FeedbackSignal{Kind: RoutingSignalSuccess, Meta: big}); !errors.Is(err, ErrMetaTooLarge) {
		t.Fatalf("oversize-value err = %v, want ErrMetaTooLarge", err)
	}
}

func TestRecordAcceptsValidSignalAndDefaultsAt(t *testing.T) {
	s, _ := NewMemoryStore(MemoryStoreConfig{})
	before := time.Now()
	if err := s.Record(context.Background(), validKey(),
		FeedbackSignal{Kind: RoutingSignalSuccess}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Add a small slack to upper bound for CI clock skew.
	after := time.Now().Add(time.Second)
	agg, _ := s.Get(context.Background(), validKey())
	if agg.SampleCount != 1 {
		t.Fatalf("SampleCount = %d, want 1", agg.SampleCount)
	}
	if agg.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt is zero; Record should have defaulted At to time.Now()")
	}
	if agg.UpdatedAt.Before(before) || agg.UpdatedAt.After(after) {
		t.Fatalf("UpdatedAt %v outside [%v, %v]", agg.UpdatedAt, before, after)
	}
}

func TestRecordDefensiveCopiesStrengthAndMeta(t *testing.T) {
	s, _ := NewMemoryStore(MemoryStoreConfig{})
	strength := +0.9
	meta := map[string]string{"key": "v1"}
	sig := FeedbackSignal{
		Kind:     RoutingSignalSuccess,
		Strength: &strength,
		Meta:     meta,
	}
	if err := s.Record(context.Background(), validKey(), sig); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Mutate caller-owned values after Record returns; stored data must
	// not change. The store is an in-package neighbor, so we can lock
	// and inspect s.signals directly — that is what actually proves the
	// deep-copy worked.
	strength = -1.0
	meta["key"] = "v2"
	meta["extra"] = "v3"

	s.mu.Lock()
	storedSignals := s.signals[validKey()]
	if len(storedSignals) != 1 {
		s.mu.Unlock()
		t.Fatalf("stored signals = %d, want 1", len(storedSignals))
	}
	stored := storedSignals[0]
	if stored.Strength == nil {
		s.mu.Unlock()
		t.Fatal("stored Strength is nil")
	}
	storedStrength := *stored.Strength
	storedMetaKey := stored.Meta["key"]
	_, storedMetaHasExtra := stored.Meta["extra"]
	s.mu.Unlock()

	if storedStrength != 0.9 {
		t.Errorf("stored Strength = %v, want 0.9", storedStrength)
	}
	if storedMetaKey != "v1" {
		t.Errorf("stored Meta[key] = %q, want \"v1\"", storedMetaKey)
	}
	if storedMetaHasExtra {
		t.Error("stored Meta unexpectedly saw caller mutation (extra key present)")
	}
}

func TestRecordFIFOBound(t *testing.T) {
	s, _ := NewMemoryStore(MemoryStoreConfig{MaxRetainedSamples: 3})
	for i := 0; i < 5; i++ {
		if err := s.Record(context.Background(), validKey(),
			FeedbackSignal{Kind: RoutingSignalSuccess}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	agg, _ := s.Get(context.Background(), validKey())
	if agg.SampleCount != 3 {
		t.Fatalf("SampleCount = %d, want 3 (FIFO-capped)", agg.SampleCount)
	}
}

func TestRecordUnboundedMode(t *testing.T) {
	s, _ := NewMemoryStore(MemoryStoreConfig{MaxRetainedSamples: -1})
	for i := 0; i < 50; i++ {
		if err := s.Record(context.Background(), validKey(),
			FeedbackSignal{Kind: RoutingSignalSuccess}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	agg, _ := s.Get(context.Background(), validKey())
	if agg.SampleCount != 50 {
		t.Fatalf("SampleCount = %d, want 50", agg.SampleCount)
	}
}
