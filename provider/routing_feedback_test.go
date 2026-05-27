package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
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
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
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
	out := rp.buildOutcome(0, nil)
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

type routeIDShortReader struct {
	read bool
}

func (r *routeIDShortReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, errRouteIDForcedFailure
	}
	r.read = true
	return copy(p, []byte{1, 2, 3, 4}), nil
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

func TestNewRouteIDReturnsEmptyOnShortRandRead(t *testing.T) {
	orig := routeIDRand
	routeIDRand = &routeIDShortReader{}
	t.Cleanup(func() { routeIDRand = orig })

	if got := newRouteID(); got != "" {
		t.Fatalf("newRouteID() = %q, want empty string on short RNG read", got)
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

func TestNewMemoryStoreRejectsInvalidLimits(t *testing.T) {
	cases := []struct {
		name string
		cfg  MemoryStoreConfig
	}{
		{"retained-less-than-negative-one", MemoryStoreConfig{MaxRetainedSamples: -2}},
		{"negative-meta-keys", MemoryStoreConfig{MaxMetaKeys: -1}},
		{"negative-meta-value-bytes", MemoryStoreConfig{MaxMetaValueBytes: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewMemoryStore(tc.cfg); err == nil {
				t.Fatal("NewMemoryStore returned nil error")
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

func mustStore(t *testing.T, cfg MemoryStoreConfig) *MemoryStore {
	t.Helper()
	s, err := NewMemoryStore(cfg)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	return s
}

// mustRecord is a helper that records a well-formed signal of the given
// kind and fails the test on error. ErrorClass and LatencyMs are
// auto-applied based on kind so payload validation is satisfied.
func mustRecord(t *testing.T, s *MemoryStore, kind RoutingSignalKind, errorClass string, latencyMs int64) {
	t.Helper()
	sig := FeedbackSignal{Kind: kind}
	switch kind {
	case RoutingSignalFailure:
		if errorClass == "" {
			errorClass = "5xx"
		}
		sig.ErrorClass = errorClass
	case RoutingSignalLatency:
		if latencyMs == 0 {
			latencyMs = 100
		}
		sig.LatencyMs = latencyMs
	}
	if err := s.Record(context.Background(), validKey(), sig); err != nil {
		t.Fatalf("Record(%s): %v", kind, err)
	}
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

func TestSignalsDeepCopiesStrengthAndMeta(t *testing.T) {
	s, _ := NewMemoryStore(MemoryStoreConfig{})
	key := validKey()
	strength := +0.9
	if err := s.Record(context.Background(), key, FeedbackSignal{
		Kind:     RoutingSignalSuccess,
		Strength: &strength,
		Meta:     map[string]string{"key": "v1"},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	snapshot := s.Signals(key)
	if len(snapshot) != 1 {
		t.Fatalf("Signals len = %d, want 1", len(snapshot))
	}
	if snapshot[0].Strength == nil {
		t.Fatal("snapshot Strength is nil")
	}
	*snapshot[0].Strength = -1.0
	snapshot[0].Meta["key"] = "redacted"
	snapshot[0].Meta["extra"] = "caller-only"

	next := s.Signals(key)
	if len(next) != 1 {
		t.Fatalf("Signals after mutation len = %d, want 1", len(next))
	}
	if next[0].Strength == nil {
		t.Fatal("stored Strength is nil")
	}
	if got := *next[0].Strength; got != 0.9 {
		t.Fatalf("stored Strength = %v, want 0.9", got)
	}
	if got := next[0].Meta["key"]; got != "v1" {
		t.Fatalf("stored Meta[key] = %q, want \"v1\"", got)
	}
	if _, ok := next[0].Meta["extra"]; ok {
		t.Fatal("stored Meta unexpectedly saw caller-only key")
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

func TestGetSingleSuccess(t *testing.T) {
	s := mustStore(t, MemoryStoreConfig{})
	if err := s.Record(context.Background(), validKey(),
		FeedbackSignal{Kind: RoutingSignalSuccess}); err != nil {
		t.Fatal(err)
	}
	agg, _ := s.Get(context.Background(), validKey())
	if agg.Score != 0.75 {
		t.Fatalf("Score = %v, want 0.75", agg.Score)
	}
	if agg.SampleCount != 1 || agg.ScoredCount != 1 {
		t.Fatalf("counts = (sample=%d, scored=%d), want (1,1)", agg.SampleCount, agg.ScoredCount)
	}
}

func TestGetSingleFailure(t *testing.T) {
	s := mustStore(t, MemoryStoreConfig{})
	if err := s.Record(context.Background(), validKey(),
		FeedbackSignal{Kind: RoutingSignalFailure, ErrorClass: "5xx"}); err != nil {
		t.Fatal(err)
	}
	agg, _ := s.Get(context.Background(), validKey())
	// 0.5 + 0.5*(-0.7) = 0.15; allow 1 ULP of float64 rounding.
	const want = 0.15
	if math.Abs(agg.Score-want) > 1e-15 {
		t.Fatalf("Score = %v, want %v", agg.Score, want)
	}
	if agg.ScoredCount != 1 {
		t.Fatalf("ScoredCount = %d, want 1", agg.ScoredCount)
	}
}

func TestGetLatencyDoesNotShiftScore(t *testing.T) {
	s := mustStore(t, MemoryStoreConfig{})
	mustRecord(t, s, RoutingSignalSuccess, "", 0)
	mustRecord(t, s, RoutingSignalLatency, "", 820)
	agg, _ := s.Get(context.Background(), validKey())
	if agg.Score != 0.75 {
		t.Fatalf("Score = %v, want 0.75 (Latency must not dilute)", agg.Score)
	}
	if agg.SampleCount != 2 || agg.ScoredCount != 1 {
		t.Fatalf("counts = (sample=%d, scored=%d), want (2,1)", agg.SampleCount, agg.ScoredCount)
	}
}

func TestGetAllLatencyIsNeutralWithSampleCount(t *testing.T) {
	s := mustStore(t, MemoryStoreConfig{})
	for i := 0; i < 3; i++ {
		mustRecord(t, s, RoutingSignalLatency, "", 100)
	}
	agg, _ := s.Get(context.Background(), validKey())
	if agg.Score != 0.5 {
		t.Fatalf("Score = %v, want 0.5", agg.Score)
	}
	if agg.SampleCount != 3 || agg.ScoredCount != 0 {
		t.Fatalf("counts = (sample=%d, scored=%d), want (3,0)", agg.SampleCount, agg.ScoredCount)
	}
}

func TestGetExplicitNeutralStrengthCountsAsSampleOnly(t *testing.T) {
	s := mustStore(t, MemoryStoreConfig{})
	zero := 0.0
	if err := s.Record(context.Background(), validKey(),
		FeedbackSignal{Kind: RoutingSignalSuccess, Strength: &zero}); err != nil {
		t.Fatal(err)
	}
	agg, _ := s.Get(context.Background(), validKey())
	if agg.Score != 0.5 {
		t.Fatalf("Score = %v, want 0.5 (explicit-neutral Success is sample-only)", agg.Score)
	}
	if agg.SampleCount != 1 || agg.ScoredCount != 0 {
		t.Fatalf("counts = (sample=%d, scored=%d), want (1,0)", agg.SampleCount, agg.ScoredCount)
	}
}

func TestGetStrengthClipping(t *testing.T) {
	s := mustStore(t, MemoryStoreConfig{})
	too := -5.0
	if err := s.Record(context.Background(), validKey(),
		FeedbackSignal{Kind: RoutingSignalSuccess, Strength: &too}); err != nil {
		t.Fatal(err)
	}
	agg, _ := s.Get(context.Background(), validKey())
	// Clipping at -1.0 → mean = -1.0 → score = 0.0.
	if agg.Score != 0.0 {
		t.Fatalf("Score = %v, want 0.0 (clipped at -1)", agg.Score)
	}
}

func TestGetMixedSuccessAndFailure(t *testing.T) {
	// Exercises the multi-scored division path: two score-bearing signals
	// with opposite signs. Without this case, the formula could regress
	// into scored==1 special-casing without any TestGet* failure.
	s := mustStore(t, MemoryStoreConfig{})
	mustRecord(t, s, RoutingSignalSuccess, "", 0) // +0.5
	mustRecord(t, s, RoutingSignalFailure, "", 0) // -0.7 (5xx)
	agg, _ := s.Get(context.Background(), validKey())
	// mean = (0.5 + (-0.7)) / 2 = -0.1; score = 0.5 + 0.5 * -0.1 = 0.45.
	const want = 0.45
	if math.Abs(agg.Score-want) > 1e-15 {
		t.Fatalf("Score = %v, want %v (within 1e-15)", agg.Score, want)
	}
	if agg.SampleCount != 2 || agg.ScoredCount != 2 {
		t.Fatalf("counts = (sample=%d, scored=%d), want (2,2)", agg.SampleCount, agg.ScoredCount)
	}
}

func TestRecordBatchAppliesValidItems(t *testing.T) {
	s := mustStore(t, MemoryStoreConfig{})
	items := []FeedbackItem{
		{Key: validKey(), Signal: FeedbackSignal{Kind: RoutingSignalSuccess}},
		{Key: validKey(), Signal: FeedbackSignal{Kind: RoutingSignalLatency, LatencyMs: 100}},
	}
	if err := s.RecordBatch(context.Background(), items); err != nil {
		t.Fatalf("RecordBatch: %v", err)
	}
	agg, _ := s.Get(context.Background(), validKey())
	if agg.SampleCount != 2 || agg.ScoredCount != 1 {
		t.Fatalf("counts = (sample=%d, scored=%d), want (2,1)", agg.SampleCount, agg.ScoredCount)
	}
}

func TestRecordBatchRejectsAnyInvalidItemAtomically(t *testing.T) {
	s := mustStore(t, MemoryStoreConfig{})
	items := []FeedbackItem{
		{Key: validKey(), Signal: FeedbackSignal{Kind: RoutingSignalSuccess}},
		{Key: FeedbackKey{}, Signal: FeedbackSignal{Kind: RoutingSignalSuccess}}, // invalid key
	}
	if err := s.RecordBatch(context.Background(), items); !errors.Is(err, ErrInvalidFeedbackKey) {
		t.Fatalf("err = %v, want ErrInvalidFeedbackKey", err)
	}
	agg, _ := s.Get(context.Background(), validKey())
	if agg.SampleCount != 0 {
		t.Fatalf("SampleCount = %d, want 0 (no item should have been persisted)", agg.SampleCount)
	}
}

func TestRecordBatchEmptyIsNoOp(t *testing.T) {
	s := mustStore(t, MemoryStoreConfig{})
	if err := s.RecordBatch(context.Background(), nil); err != nil {
		t.Fatalf("nil items: %v", err)
	}
	if err := s.RecordBatch(context.Background(), []FeedbackItem{}); err != nil {
		t.Fatalf("empty items: %v", err)
	}
}

func TestRecordBatchSharesTimestampDefault(t *testing.T) {
	s := mustStore(t, MemoryStoreConfig{})
	items := []FeedbackItem{
		{Key: validKey(), Signal: FeedbackSignal{Kind: RoutingSignalSuccess}},
		{Key: validKey(), Signal: FeedbackSignal{Kind: RoutingSignalLatency, LatencyMs: 100}},
	}
	before := time.Now()
	if err := s.RecordBatch(context.Background(), items); err != nil {
		t.Fatal(err)
	}
	after := time.Now()

	// The "shared" property is the one that matters: both signals in the
	// batch must carry the same `At`. UpdatedAt alone could pass even if
	// RecordBatch internally re-sampled time.Now() between items.
	s.mu.Lock()
	sigs := s.signals[validKey()]
	s.mu.Unlock()
	if len(sigs) != 2 {
		t.Fatalf("stored signals = %d, want 2", len(sigs))
	}
	if !sigs[0].At.Equal(sigs[1].At) {
		t.Fatalf("signals[0].At = %v, signals[1].At = %v; want equal (shared now)", sigs[0].At, sigs[1].At)
	}
	if sigs[0].At.Before(before) || sigs[0].At.After(after) {
		t.Fatalf("At %v outside [%v, %v]", sigs[0].At, before, after)
	}
}

func TestRecordBatchDistinctKeys(t *testing.T) {
	// Exercises the per-key map-insertion path that single-key batches do
	// not — RecordOutcome (Task 9) produces batches keyed by attempt.Key,
	// so distinct keys are the primary use case.
	s := mustStore(t, MemoryStoreConfig{})
	keyA := FeedbackKey{Provider: "a", Model: "m", UseCase: "c"}
	keyB := FeedbackKey{Provider: "b", Model: "m", UseCase: "c"}
	items := []FeedbackItem{
		{Key: keyA, Signal: FeedbackSignal{Kind: RoutingSignalSuccess}},
		{Key: keyB, Signal: FeedbackSignal{Kind: RoutingSignalFailure, ErrorClass: "timeout"}},
	}
	if err := s.RecordBatch(context.Background(), items); err != nil {
		t.Fatalf("RecordBatch: %v", err)
	}
	for _, k := range []FeedbackKey{keyA, keyB} {
		agg, _ := s.Get(context.Background(), k)
		if agg.SampleCount != 1 {
			t.Fatalf("key %+v SampleCount = %d, want 1", k, agg.SampleCount)
		}
	}
}

func TestRecordBatchTinyCapKeepsAtomicity(t *testing.T) {
	s := mustStore(t, MemoryStoreConfig{MaxRetainedSamples: 1})
	items := []FeedbackItem{
		{Key: validKey(), Signal: FeedbackSignal{Kind: RoutingSignalSuccess}},
		{Key: validKey(), Signal: FeedbackSignal{Kind: RoutingSignalLatency, LatencyMs: 100}},
	}
	if err := s.RecordBatch(context.Background(), items); err != nil {
		t.Fatal(err)
	}
	agg, _ := s.Get(context.Background(), validKey())
	if agg.SampleCount != 1 {
		t.Fatalf("SampleCount = %d, want 1 (tiny cap retains newest only)", agg.SampleCount)
	}
}

func TestRoutingFeedbackScoreDelegates(t *testing.T) {
	store := mustStore(t, MemoryStoreConfig{})
	rf := NewRoutingFeedback(store)

	agg, err := rf.Score(context.Background(), validKey())
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if agg.Score != DefaultNeutralScore {
		t.Fatalf("Score = %v, want %v", agg.Score, DefaultNeutralScore)
	}

	if err := rf.Record(context.Background(), validKey(),
		FeedbackSignal{Kind: RoutingSignalSuccess}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	agg, _ = rf.Score(context.Background(), validKey())
	if agg.Score != 0.75 {
		t.Fatalf("after Record Score = %v, want 0.75", agg.Score)
	}
}

func TestRoutingFeedbackRecordPropagatesValidationErrors(t *testing.T) {
	rf := NewRoutingFeedback(mustStore(t, MemoryStoreConfig{}))
	err := rf.Record(context.Background(), FeedbackKey{},
		FeedbackSignal{Kind: RoutingSignalSuccess})
	if !errors.Is(err, ErrInvalidFeedbackKey) {
		t.Fatalf("err = %v, want ErrInvalidFeedbackKey", err)
	}
}

func TestRoutingFeedbackRejectsNilStore(t *testing.T) {
	rf := NewRoutingFeedback(nil)
	if _, err := rf.Score(context.Background(), validKey()); !errors.Is(err, ErrNilRoutingFeedbackStore) {
		t.Fatalf("Score err = %v, want ErrNilRoutingFeedbackStore", err)
	}
	if err := rf.Record(context.Background(), validKey(),
		FeedbackSignal{Kind: RoutingSignalSuccess}); !errors.Is(err, ErrNilRoutingFeedbackStore) {
		t.Fatalf("Record err = %v, want ErrNilRoutingFeedbackStore", err)
	}
	if err := rf.RecordOutcome(context.Background(), "chat", RouteOutcome{}); !errors.Is(err, ErrNilRoutingFeedbackStore) {
		t.Fatalf("RecordOutcome err = %v, want ErrNilRoutingFeedbackStore", err)
	}
}

func TestRoutingFeedbackRejectsTypedNilStore(t *testing.T) {
	var store *MemoryStore
	rf := NewRoutingFeedback(store)
	if _, err := rf.Score(context.Background(), validKey()); !errors.Is(err, ErrNilRoutingFeedbackStore) {
		t.Fatalf("Score err = %v, want ErrNilRoutingFeedbackStore", err)
	}
}

func TestRoutingFeedbackRejectsNilReceiver(t *testing.T) {
	var rf *RoutingFeedback
	if _, err := rf.Score(context.Background(), validKey()); !errors.Is(err, ErrNilRoutingFeedbackStore) {
		t.Fatalf("Score err = %v, want ErrNilRoutingFeedbackStore", err)
	}
}

func mkAttempt(provider, model string, status AttemptStatus, latencyMs int64, errClass string) RouteAttempt {
	return RouteAttempt{
		Key:        ModelKey{Provider: provider, Model: model},
		Status:     status,
		LatencyMs:  latencyMs,
		ErrorClass: errClass,
	}
}

func TestRecordOutcomeNilAttemptsIsNoOp(t *testing.T) {
	rf := NewRoutingFeedback(mustStore(t, MemoryStoreConfig{}))
	if err := rf.RecordOutcome(context.Background(), "chat", RouteOutcome{}); err != nil {
		t.Fatalf("nil Attempts: %v", err)
	}
	if err := rf.RecordOutcome(context.Background(), "", RouteOutcome{}); err != nil {
		t.Fatalf("nil Attempts with empty useCase: %v", err)
	}
}

func TestRecordOutcomeAllUnknownIsNoOp(t *testing.T) {
	store := mustStore(t, MemoryStoreConfig{})
	rf := NewRoutingFeedback(store)
	out := RouteOutcome{
		Attempts: []RouteAttempt{
			mkAttempt("p", "m", AttemptStatusUnknown, 100, ""),
		},
	}
	if err := rf.RecordOutcome(context.Background(), "chat", out); err != nil {
		t.Fatalf("all-Unknown: %v", err)
	}
	agg, _ := store.Get(context.Background(), FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"})
	if agg.SampleCount != 0 {
		t.Fatalf("SampleCount = %d, want 0", agg.SampleCount)
	}
}

func TestRecordOutcomeRejectsEmptyUseCase(t *testing.T) {
	rf := NewRoutingFeedback(mustStore(t, MemoryStoreConfig{}))
	out := RouteOutcome{
		Attempts: []RouteAttempt{mkAttempt("p", "m", AttemptStatusSucceeded, 100, "")},
	}
	err := rf.RecordOutcome(context.Background(), "", out)
	if !errors.Is(err, ErrInvalidFeedbackKey) {
		t.Fatalf("err = %v, want ErrInvalidFeedbackKey", err)
	}
}

func TestRecordOutcomeRejectsInvalidAttemptStatusAtomically(t *testing.T) {
	store := mustStore(t, MemoryStoreConfig{})
	rf := NewRoutingFeedback(store)
	out := RouteOutcome{
		Attempts: []RouteAttempt{
			mkAttempt("p", "m", AttemptStatusSucceeded, 100, ""),
			mkAttempt("p2", "m2", AttemptStatus(99), 100, ""),
		},
	}
	err := rf.RecordOutcome(context.Background(), "chat", out)
	if !errors.Is(err, ErrUnknownAttemptStatus) {
		t.Fatalf("err = %v, want ErrUnknownAttemptStatus", err)
	}
	// Neither attempt should have produced signals — atomic rejection.
	for _, k := range []FeedbackKey{
		{Provider: "p", Model: "m", UseCase: "chat"},
		{Provider: "p2", Model: "m2", UseCase: "chat"},
	} {
		agg, _ := store.Get(context.Background(), k)
		if agg.SampleCount != 0 {
			t.Fatalf("key %+v SampleCount = %d, want 0", k, agg.SampleCount)
		}
	}
}

func TestRecordOutcomeSuccessOnlyEmitsSuccessAndLatency(t *testing.T) {
	store := mustStore(t, MemoryStoreConfig{})
	rf := NewRoutingFeedback(store)
	out := RouteOutcome{
		Attempts: []RouteAttempt{mkAttempt("p", "m", AttemptStatusSucceeded, 820, "")},
		RouteID:  "route-1",
	}
	if err := rf.RecordOutcome(context.Background(), "chat", out); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	key := FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.SampleCount != 2 {
		t.Fatalf("SampleCount = %d, want 2", agg.SampleCount)
	}
	if agg.ScoredCount != 1 {
		t.Fatalf("ScoredCount = %d, want 1", agg.ScoredCount)
	}
	// Verify RouteID propagation: every emitted signal must carry the
	// outcome's RouteID. Aggregate has no RouteID field, so we inspect
	// store.signals directly.
	store.mu.Lock()
	sigs := store.signals[key]
	store.mu.Unlock()
	if len(sigs) != 2 {
		t.Fatalf("stored signals = %d, want 2", len(sigs))
	}
	for i, sig := range sigs {
		if sig.RouteID != "route-1" {
			t.Errorf("sigs[%d].RouteID = %q, want \"route-1\"", i, sig.RouteID)
		}
	}
}

func TestRecordOutcomeFailureCarriesErrorClass(t *testing.T) {
	store := mustStore(t, MemoryStoreConfig{})
	rf := NewRoutingFeedback(store)
	out := RouteOutcome{
		Attempts: []RouteAttempt{mkAttempt("p", "m", AttemptStatusFailed, 30000, "timeout")},
	}
	if err := rf.RecordOutcome(context.Background(), "chat", out); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	key := FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	// Failure (-0.7) + Latency (0): scoredCount = 1, mean = -0.7, score = 0.15.
	if math.Abs(agg.Score-0.15) > 1e-15 {
		t.Fatalf("Score = %v, want 0.15", agg.Score)
	}
}

func TestRecordOutcomeFailedAttemptEmptyErrorClassNormalizesToUnknown(t *testing.T) {
	store := mustStore(t, MemoryStoreConfig{})
	rf := NewRoutingFeedback(store)
	out := RouteOutcome{
		Attempts: []RouteAttempt{mkAttempt("p", "m", AttemptStatusFailed, 100, "")},
	}
	if err := rf.RecordOutcome(context.Background(), "chat", out); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	// Decomposition emitted Failure + Latency at the same key. Failure
	// must have been recorded with ErrorClass="unknown" (otherwise
	// validateSignal would have rejected it).
	key := FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.SampleCount != 2 || agg.ScoredCount != 1 {
		t.Fatalf("counts = (sample=%d, scored=%d), want (2,1)", agg.SampleCount, agg.ScoredCount)
	}
}

func TestRecordOutcomePrimaryFailFallbackSucceedKeysSeparately(t *testing.T) {
	// This is the design-review fix test: a primary failure followed by a
	// fallback success debits the primary and credits the fallback —
	// rather than crediting the planned model for a rescued request.
	store := mustStore(t, MemoryStoreConfig{})
	rf := NewRoutingFeedback(store)
	out := RouteOutcome{
		Attempts: []RouteAttempt{
			mkAttempt("ollama-a", "qwen3:8b", AttemptStatusFailed, 200, "5xx"),
			mkAttempt("ollama-b", "qwen3:8b", AttemptStatusSucceeded, 900, ""),
		},
	}
	if err := rf.RecordOutcome(context.Background(), "chat", out); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	primary := FeedbackKey{Provider: "ollama-a", Model: "qwen3:8b", UseCase: "chat"}
	fallback := FeedbackKey{Provider: "ollama-b", Model: "qwen3:8b", UseCase: "chat"}

	primaryAgg, _ := store.Get(context.Background(), primary)
	if math.Abs(primaryAgg.Score-0.15) > 1e-15 {
		t.Errorf("primary.Score = %v, want 0.15 (Failure)", primaryAgg.Score)
	}
	if primaryAgg.SampleCount != 2 {
		t.Errorf("primary.SampleCount = %d, want 2", primaryAgg.SampleCount)
	}

	fallbackAgg, _ := store.Get(context.Background(), fallback)
	if fallbackAgg.Score != 0.75 {
		t.Errorf("fallback.Score = %v, want 0.75 (Success)", fallbackAgg.Score)
	}
	if fallbackAgg.SampleCount != 2 {
		t.Errorf("fallback.SampleCount = %d, want 2", fallbackAgg.SampleCount)
	}
}

func TestRecordOutcomeLatencyOmittedWhenNotMeasured(t *testing.T) {
	store := mustStore(t, MemoryStoreConfig{})
	rf := NewRoutingFeedback(store)
	out := RouteOutcome{
		Attempts: []RouteAttempt{mkAttempt("p", "m", AttemptStatusSucceeded, 0, "")},
	}
	if err := rf.RecordOutcome(context.Background(), "chat", out); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	key := FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.SampleCount != 1 {
		t.Fatalf("SampleCount = %d, want 1 (no Latency emitted when LatencyMs <= 0)", agg.SampleCount)
	}
}

func TestRecordOutcomeRejectsNegativeLatencyAtomically(t *testing.T) {
	store := mustStore(t, MemoryStoreConfig{})
	rf := NewRoutingFeedback(store)
	out := RouteOutcome{
		Attempts: []RouteAttempt{
			mkAttempt("p", "m", AttemptStatusSucceeded, 100, ""),
			mkAttempt("p2", "m2", AttemptStatusSucceeded, -1, ""),
		},
	}
	err := rf.RecordOutcome(context.Background(), "chat", out)
	if !errors.Is(err, ErrInvalidSignalPayload) {
		t.Fatalf("err = %v, want ErrInvalidSignalPayload", err)
	}
	// Atomicity: neither attempt's signals should have been persisted,
	// even though the first attempt was valid.
	for _, k := range []FeedbackKey{
		{Provider: "p", Model: "m", UseCase: "chat"},
		{Provider: "p2", Model: "m2", UseCase: "chat"},
	} {
		agg, _ := store.Get(context.Background(), k)
		if agg.SampleCount != 0 {
			t.Fatalf("key %+v SampleCount = %d, want 0 (atomic rejection)", k, agg.SampleCount)
		}
	}
}

func TestMemoryStoreConcurrentRecordAndGet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency stress under -short")
	}
	store := mustStore(t, MemoryStoreConfig{MaxRetainedSamples: -1})
	const goroutines = 8
	const iterations = 200

	keys := []FeedbackKey{
		{Provider: "p1", Model: "m1", UseCase: "chat"},
		{Provider: "p1", Model: "m2", UseCase: "chat"},
		{Provider: "p2", Model: "m1", UseCase: "fim"},
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				k := keys[(gid+i)%len(keys)]
				switch i % 3 {
				case 0:
					_ = store.Record(context.Background(), k,
						FeedbackSignal{Kind: RoutingSignalSuccess})
				case 1:
					_ = store.RecordBatch(context.Background(), []FeedbackItem{
						{Key: k, Signal: FeedbackSignal{Kind: RoutingSignalSuccess}},
						{Key: k, Signal: FeedbackSignal{Kind: RoutingSignalLatency, LatencyMs: 100}},
					})
				case 2:
					_, _ = store.Get(context.Background(), k)
				}
			}
		}(g)
	}
	wg.Wait()

	// Final invariants: every Record adds exactly 1 sample; every
	// RecordBatch with 2 items adds exactly 2 samples; Get adds 0. So
	// total samples across all keys == sum over goroutines of:
	//   (iterations/3 + 1 if iterations%3 > 0)  *  1     (Record)
	// + (iterations/3 + 1 if iterations%3 > 1)  *  2     (RecordBatch)
	// + (iterations/3)                          *  0     (Get)
	// For iterations=200: 200%3 == 2, so:
	//   record_calls = 67, batch_calls = 67, get_calls = 66.
	// Per goroutine samples = 67 + 67*2 = 201.
	// Total = 8 * 201 = 1608.
	const wantTotal = 1608
	var got int
	for _, k := range keys {
		agg, _ := store.Get(context.Background(), k)
		got += agg.SampleCount
	}
	if got != wantTotal {
		t.Fatalf("total samples = %d, want %d (key distribution may have drifted)", got, wantTotal)
	}
}
