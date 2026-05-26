package provider

import (
	"encoding/json"
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
		s    AttemptStatus
		want string
	}{
		{AttemptStatusUnknown, "unknown"},
		{AttemptStatusSucceeded, "succeeded"},
		{AttemptStatusFailed, "failed"},
		{AttemptStatus(99), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
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
	if got := s; got == "" {
		t.Fatalf("empty marshal")
	}
	// Forward-compatible expectation: zero-value Attempts/RouteID must be
	// absent from the JSON output so existing consumers see no new keys.
	for _, key := range []string{`"attempts"`, `"route_id"`} {
		if contains(s, key) {
			t.Errorf("RouteOutcome JSON %s unexpectedly contains %s", s, key)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
