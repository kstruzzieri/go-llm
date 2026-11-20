package fingerprint

import (
	"maps"
	"math/rand/v2"
	"testing"
	"time"
)

func TestNextReadingTransition(t *testing.T) {
	const after = time.Nanosecond // an end is inclusive: the change is the next instant
	future := evNow.Add(2 * time.Hour)
	admits := evNow.Add(time.Hour) // future minus the one-hour skew allowance
	tests := []struct {
		name  string
		rows  []EvidenceRow
		named bool      // a name-only route; otherwise the route's digest is evDigest
		now   time.Time // default evNow
		want  time.Time // zero: no future transition
	}{
		{name: "no rows"},
		{
			name: "yes with a digest never changes",
			rows: []EvidenceRow{evRow(1, evDay(-1), byTheRules(0))},
		},
		{
			name:  "name-only yes turns stale",
			rows:  []EvidenceRow{evRow(1, evDay(-1), nameOnly, byTheRules(0))},
			named: true,
			want:  evDay(29).Add(after),
		},
		{
			name:  "name-only yes on its last fresh instant",
			rows:  []EvidenceRow{evRow(1, evDay(-1), nameOnly, byTheRules(0))},
			named: true,
			now:   evDay(29),
			want:  evDay(29).Add(after),
		},
		{
			name:  "name-only yes already stale",
			rows:  []EvidenceRow{evRow(1, evDay(-1), nameOnly, byTheRules(0))},
			named: true,
			now:   evDay(29).Add(after),
		},
		{
			name:  "name-only no expires",
			rows:  []EvidenceRow{evRow(1, evDay(-1), asNo, nameOnly, byTheRules(0))},
			named: true,
			want:  evDay(6).Add(after),
		},
		{
			name:  "name-only no on its last live instant",
			rows:  []EvidenceRow{evRow(1, evDay(-1), asNo, nameOnly, byTheRules(0))},
			named: true,
			now:   evDay(6),
			want:  evDay(6).Add(after),
		},
		{
			name:  "name-only no already expired",
			rows:  []EvidenceRow{evRow(1, evDay(-1), asNo, nameOnly, byTheRules(0))},
			named: true,
			now:   evDay(6).Add(after),
		},
		{
			name: "no with a prober TTL expires",
			rows: []EvidenceRow{evRow(1, evDay(-1), asNo, byTheRules(3*evDayLen))},
			want: evDay(2).Add(after),
		},
		{
			name: "no with a digest and no TTL never changes",
			rows: []EvidenceRow{evRow(1, evDay(-1), asNo, byTheRules(0))},
		},
		{
			name: "future row is admitted at observed minus the skew",
			rows: []EvidenceRow{evRow(1, future)},
			want: admits,
		},
		{
			name: "future row one instant before admission",
			rows: []EvidenceRow{evRow(1, future)},
			now:  admits.Add(-time.Nanosecond),
			want: admits,
		},
		{
			name: "future row on its admission instant counts already",
			rows: []EvidenceRow{evRow(1, future)},
			now:  admits,
		},
		{
			name: "row inside the skew allowance counts already",
			rows: []EvidenceRow{evRow(1, evNow.Add(30*time.Minute))},
		},
		{
			name: "future attempt is admitted too",
			rows: []EvidenceRow{evRow(1, future, asAttempt, byTheRules(0))},
			want: admits,
		},
		{
			name: "attempt reads the same past its expiry",
			rows: []EvidenceRow{evRow(1, evDay(-1).Add(time.Hour), asAttempt, byTheRules(0))},
		},
		{
			name: "flag expires",
			rows: []EvidenceRow{evRow(1, evDay(-1), asFlag, expiring(evDay(4)))},
			want: evDay(4).Add(after),
		},
		{
			name: "another adapter's expiry never counts for this route",
			rows: []EvidenceRow{evRow(1, evDay(-1), asOllamaShow, asNo, expiring(evDay(4)))},
		},
		{
			name: "another adapter's future row never counts for this route",
			rows: []EvidenceRow{evRow(1, future, asOllamaShow)},
		},
		{
			name: "another digest never counts",
			rows: []EvidenceRow{evRow(1, evDay(-1), asNo, withDigest(evDigest2), expiring(evDay(4)))},
		},
		{
			name: "a row this binary does not understand never counts",
			rows: []EvidenceRow{
				evRow(1, evDay(-1), asNo, withVersion(2), expiring(evDay(4))),
				evRow(2, future, withMethod("nope")),
			},
		},
		{
			name: "expiry of a row a newer one outranks changes nothing",
			rows: []EvidenceRow{
				evRow(1, evDay(-2), asNo, expiring(evDay(4))),
				evRow(2, evDay(-1)),
			},
		},
		{
			name: "expiry of a newer no brings back the older yes",
			rows: []EvidenceRow{
				evRow(1, evDay(-2)),
				evRow(2, evDay(-1), asNo, expiring(evDay(4))),
			},
			want: evDay(4).Add(after),
		},
		{
			name: "a row that expires before it is admitted changes nothing",
			rows: []EvidenceRow{evRow(1, future, asNo, expiring(evNow.Add(30*time.Minute)))},
		},
		{
			name: "earliest across capabilities",
			rows: []EvidenceRow{
				evRow(1, evDay(-1), asNo, expiring(evDay(6))),
				evRow(2, evDay(-1), asNo, withCap("chat"), expiring(evDay(3))),
			},
			want: evDay(3).Add(after),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := tt.now
			if now.IsZero() {
				now = evNow
			}
			digest := evDigest
			if tt.named {
				digest = ""
			}
			got := NextReadingTransition(tt.rows, AdapterOpenAICompat, digest, now)
			if !got.Equal(tt.want) {
				t.Fatalf("NextReadingTransition() = %v; want %v", got, tt.want)
			}
		})
	}
}

// routeView is a reading as the route sees it: which row fills each slot and
// whether each answer is fresh. OtherAdapter is left out because it never counts.
type routeView struct {
	ids                                      readingIDs
	probeFresh, observedFresh, providerFresh bool
}

func observeRoute(rows []EvidenceRow, adapter, digest string, at time.Time) map[string]routeView {
	fresh := func(r *EvidenceRow) bool { return r != nil && r.Fresh(at) }
	out := make(map[string]routeView)
	for capability, c := range ReadCapabilities(rows, adapter, digest, at) {
		c.OtherAdapter = nil
		v := routeView{idsOfReading(c), fresh(c.Probe), fresh(c.Observed), fresh(c.Provider)}
		if v != (routeView{}) {
			out[capability] = v
		}
	}
	return out
}

// Walks each generated log from one transition to the next. Every instant it
// names is a real change to what ReadCapabilities reports, and no instant any row
// defines changes the reading without being named. Both directions matter: one
// keeps a cached decision from rebuilding for nothing, the other keeps it from
// going stale.
func TestNextReadingTransition_AgreesWithReadCapabilities(t *testing.T) {
	routes := []struct{ adapter, digest string }{
		{AdapterOpenAICompat, evDigest}, {AdapterOpenAICompat, ""},
		{AdapterOllama, evDigest}, {AdapterOllama, ""},
	}
	var named int
	for seed := range uint64(250) {
		rows := genEvidenceLog(rand.New(rand.NewPCG(seed, 738)))
		start := evNow.Add(-1500 * evDayLen)
		clocks := evidenceClocks(rows, start)
		for _, route := range routes {
			at := start
			for {
				next := NextReadingTransition(rows, route.adapter, route.digest, at)
				if !next.IsZero() && !next.After(at) {
					t.Fatalf("seed %d %v: transition %v is not after %v: %s", seed, route, next, at, describeLog(rows))
				}
				want := observeRoute(rows, route.adapter, route.digest, at)
				for _, c := range clocks {
					if c.After(at) && (next.IsZero() || c.Before(next)) &&
						!maps.Equal(observeRoute(rows, route.adapter, route.digest, c), want) {
						t.Fatalf("seed %d %v: the reading changed by %v, before the named transition %v, from %v: %s",
							seed, route, c, next, at, describeLog(rows))
					}
				}
				if next.IsZero() {
					break
				}
				if maps.Equal(observeRoute(rows, route.adapter, route.digest, next), want) {
					t.Fatalf("seed %d %v: nothing changes at the named transition %v: %s", seed, route, next, describeLog(rows))
				}
				if !maps.Equal(observeRoute(rows, route.adapter, route.digest, next.Add(-time.Nanosecond)), want) {
					t.Fatalf("seed %d %v: the reading had already changed before %v: %s", seed, route, next, describeLog(rows))
				}
				named++
				at = next
			}
		}
	}
	// Guard against a generator that stopped producing transitions.
	t.Logf("%d transitions named", named)
	if named < 500 {
		t.Fatalf("generated logs exercised too little: %d transitions", named)
	}
}
