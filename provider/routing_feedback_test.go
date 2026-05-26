package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
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
