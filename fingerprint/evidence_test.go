package fingerprint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	evEndpoint = "http://127.0.0.1:8080"
	evAdapter  = "openai-compat"
	evModel    = "m"
	evDigest   = "sha256:aaa"
	evDigest2  = "sha256:bbb"
	evDayLen   = 24 * time.Hour
)

// evNow is a fixed instant on a whole millisecond, the stored resolution.
var evNow = time.UnixMilli(1_800_000_000_000)

func evDay(n int) time.Time { return evNow.Add(time.Duration(n) * evDayLen) }

// evRow is a chat-stream-tools probe answer of yes for tool_call, the shape most
// tests want, with mutators applied in order.
func evRow(id int64, at time.Time, mutators ...func(*EvidenceRow)) EvidenceRow {
	r := EvidenceRow{
		ID: id, KeyVersion: EvidenceKeyVersion,
		Endpoint: evEndpoint, Adapter: AdapterOpenAICompat, Model: evModel, Digest: evDigest,
		Capability: "tool_call", Source: EvidenceSourceProbe, Kind: EvidenceKindAnswer, State: CapProbeYes,
		Method: MethodChatStreamTools, MethodVersion: 1, ObservedAt: at,
	}
	for _, mutate := range mutators {
		mutate(&r)
	}
	return r
}

func asNo(r *EvidenceRow)       { r.State = CapProbeNo }
func asAttempt(r *EvidenceRow)  { r.Kind, r.State = EvidenceKindAttempt, CapProbeInconclusive }
func asObserved(r *EvidenceRow) { r.Source = EvidenceSourceObserved }
func asProvider(r *EvidenceRow) { r.Source = EvidenceSourceProvider }
func nameOnly(r *EvidenceRow)   { r.Digest = "" }

func asFlag(r *EvidenceRow) {
	r.Kind, r.State, r.Detail, r.HTTPStatus = EvidenceKindFlag, CapProbeFlagged, "run_refused", 400
}

func asOllamaShow(r *EvidenceRow) {
	r.Adapter, r.Method, r.Source = AdapterOllama, MethodOllamaShow, EvidenceSourceProvider
}

func withDigest(d string) func(*EvidenceRow) { return func(r *EvidenceRow) { r.Digest = d } }
func withVersion(v int) func(*EvidenceRow)   { return func(r *EvidenceRow) { r.MethodVersion = v } }
func withCap(c string) func(*EvidenceRow)    { return func(r *EvidenceRow) { r.Capability = c } }
func withMethod(m string) func(*EvidenceRow) { return func(r *EvidenceRow) { r.Method = m } }
func expiring(at time.Time) func(*EvidenceRow) {
	return func(r *EvidenceRow) { r.ExpiresAt = at }
}

// byTheRules applies the expiry table, as a writer does.
func byTheRules(ttl time.Duration) func(*EvidenceRow) {
	return func(r *EvidenceRow) { r.FreshUntil, r.ExpiresAt = ExpiryFor(*r, ttl) }
}

// readingIDs is one reading reduced to which row fills each slot. Comparing the
// whole value pins every slot, including those that must stay empty.
type readingIDs struct{ probe, observed, provider, attempt, flag, other int64 }

func idsOfReading(c CapabilityReading) readingIDs {
	id := func(r *EvidenceRow) int64 {
		if r == nil {
			return 0
		}
		return r.ID
	}
	return readingIDs{id(c.Probe), id(c.Observed), id(c.Provider), id(c.Attempt), id(c.Flag), id(c.OtherAdapter)}
}

func idsOfReadings(m map[string]CapabilityReading) map[string]readingIDs {
	out := make(map[string]readingIDs, len(m))
	for capability, reading := range m {
		out[capability] = idsOfReading(reading)
	}
	return out
}

func toolIDs(rows []EvidenceRow, adapter, digest string, now time.Time) readingIDs {
	return idsOfReading(ReadCapabilities(rows, adapter, digest, now)["tool_call"])
}

// evInvalid lists rows no reader may honor, whatever their timestamps.
var evInvalid = []struct {
	name   string
	mutate func(*EvidenceRow)
}{
	{"unknown state", func(r *EvidenceRow) { r.State = "maybe" }},
	{"unknown kind", func(r *EvidenceRow) { r.Kind = "x" }},
	{"unknown source", func(r *EvidenceRow) { r.Source = "x" }},
	{"unknown method", withMethod("nope")},
	{"chat-tools method", withMethod("chat-tools")},
	{"older method version", withVersion(0)},
	{"newer method version", withVersion(2)},
	{"newer key version", func(r *EvidenceRow) { r.KeyVersion = EvidenceKeyVersion + 1 }},
	{"inconclusive answer", func(r *EvidenceRow) { r.Kind, r.State = EvidenceKindAnswer, CapProbeInconclusive }},
	{"flagged answer", func(r *EvidenceRow) { r.Kind, r.State = EvidenceKindAnswer, CapProbeFlagged }},
	{"definitive attempt", func(r *EvidenceRow) { r.Kind, r.State = EvidenceKindAttempt, CapProbeYes }},
	{"definitive flag", func(r *EvidenceRow) { r.Kind, r.State = EvidenceKindFlag, CapProbeNo }},
	{"unknown adapter", func(r *EvidenceRow) { r.Adapter = "x" }},
	{"adapter the method does not use", func(r *EvidenceRow) { r.Adapter = AdapterOllama }},
	{"capability the method cannot answer", withMethod(MethodChatStream)},
	{"zero observation time", func(r *EvidenceRow) { r.ObservedAt = time.Time{} }},
}

// --- Reader ---

func TestReadCapabilities_InconclusiveLeavesDefinitive(t *testing.T) {
	rows := []EvidenceRow{evRow(1, evDay(-2)), evRow(2, evDay(-1), asAttempt)}
	got := ReadCapabilities(rows, AdapterOpenAICompat, evDigest, evNow)["tool_call"]
	if ids := idsOfReading(got); ids != (readingIDs{probe: 1, attempt: 2}) {
		t.Fatalf("reading = %+v", ids)
	}
	if got.Capability != "tool_call" || got.Probe.State != CapProbeYes || got.Attempt.State != CapProbeInconclusive {
		t.Fatalf("reading = %+v, probe %+v, attempt %+v", got, *got.Probe, *got.Attempt)
	}
}

// The newest attempt stays visible after its expiry, unlike an answer: whoever
// honors the cooldown reads ExpiresAt from it.
func TestReadCapabilities_ExpiredAttemptIsStillNewest(t *testing.T) {
	rows := []EvidenceRow{
		evRow(1, evDay(-5), asAttempt, byTheRules(0)),
		evRow(2, evDay(-3), asAttempt, byTheRules(0)),
	}
	got := ReadCapabilities(rows, AdapterOpenAICompat, evDigest, evNow)["tool_call"]
	if ids := idsOfReading(got); ids != (readingIDs{attempt: 2}) {
		t.Fatalf("reading = %+v", ids)
	}
	if !got.Attempt.ExpiresAt.Equal(evDay(-2)) {
		t.Errorf("attempt expires at %v, want %v", got.Attempt.ExpiresAt, evDay(-2))
	}
}

func TestReadCapabilities_NewerMethodNotShadowed(t *testing.T) {
	for _, other := range []int{0, 2} {
		rows := []EvidenceRow{evRow(1, evDay(-2)), evRow(2, evDay(-1), asNo, withVersion(other))}
		if got := toolIDs(rows, AdapterOpenAICompat, evDigest, evNow); got != (readingIDs{probe: 1}) {
			t.Errorf("method version %d row shadowed the current one: %+v", other, got)
		}
	}
}

func TestReadCapabilities_SlowOlderWriteChangesNothing(t *testing.T) {
	// The later-inserted row, with the higher ID, was observed first.
	rows := []EvidenceRow{evRow(1, evDay(-1), asNo), evRow(2, evDay(-2))}
	if got := toolIDs(rows, AdapterOpenAICompat, evDigest, evNow); got != (readingIDs{probe: 1}) {
		t.Errorf("slow older write won: %+v", got)
	}
	// Only an exact tie falls to the ID, whichever order the rows arrive in.
	tied := []EvidenceRow{evRow(3, evDay(-1), asNo), evRow(4, evDay(-1))}
	for _, rows := range [][]EvidenceRow{tied, {tied[1], tied[0]}} {
		if got := toolIDs(rows, AdapterOpenAICompat, evDigest, evNow); got != (readingIDs{probe: 4}) {
			t.Errorf("tie = %+v, want the higher ID", got)
		}
	}
}

func TestReadCapabilities_OtherAdapterNeverCounts(t *testing.T) {
	show := evRow(7, evDay(-1), asOllamaShow)
	tests := []struct {
		name            string
		rows            []EvidenceRow
		adapter, digest string
		want            readingIDs
	}{
		{"name-only openai-compat route", []EvidenceRow{show}, AdapterOpenAICompat, "", readingIDs{other: 7}},
		{"same digest does not make it count", []EvidenceRow{show}, AdapterOpenAICompat, evDigest, readingIDs{other: 7}},
		{"its own route counts it", []EvidenceRow{show}, AdapterOllama, evDigest, readingIDs{provider: 7}},
		{"each route counts its own", []EvidenceRow{show, evRow(8, evDay(-2))}, AdapterOpenAICompat, evDigest, readingIDs{probe: 8, other: 7}},
		{"the other way round", []EvidenceRow{show, evRow(8, evDay(-2))}, AdapterOllama, evDigest, readingIDs{provider: 7, other: 8}},
		{"newest other answer", []EvidenceRow{show, evRow(9, evDay(-3), asOllamaShow, asNo)}, AdapterOpenAICompat, "", readingIDs{other: 7}},
		{"an attempt is not an answer", []EvidenceRow{evRow(9, evDay(-1), asOllamaShow, asAttempt)}, AdapterOpenAICompat, "", readingIDs{}},
		{"a flag is not an answer", []EvidenceRow{evRow(9, evDay(-1), asOllamaShow, asFlag)}, AdapterOpenAICompat, "", readingIDs{}},
		{"an expired answer is gone", []EvidenceRow{evRow(9, evDay(-2), asOllamaShow, asNo, expiring(evDay(-1)))}, AdapterOpenAICompat, "", readingIDs{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolIDs(tt.rows, tt.adapter, tt.digest, evNow); got != tt.want {
				t.Fatalf("reading = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestReadCapabilities_DigestMismatchIgnored(t *testing.T) {
	for _, tt := range []struct{ name, stored, current string }{
		{"new weights", evDigest, evDigest2},
		{"name-only row, digest route", "", evDigest},
		{"digest row, name-only route", evDigest, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows := []EvidenceRow{
				evRow(1, evDay(-3), withDigest(tt.stored)),
				evRow(2, evDay(-2), withDigest(tt.stored), asAttempt),
				evRow(3, evDay(-1), withDigest(tt.stored), asFlag),
			}
			if got := ReadCapabilities(rows, AdapterOpenAICompat, tt.current, evNow); len(got) != 0 {
				t.Fatalf("readings = %+v, want none", idsOfReadings(got))
			}
		})
	}
}

func TestReadCapabilities_FlagClearedByLaterDefinitive(t *testing.T) {
	t1, t2 := evDay(-2), evDay(-1)
	tests := []struct {
		name string
		rows []EvidenceRow
		want readingIDs
	}{
		{"later yes clears", []EvidenceRow{evRow(1, t1, asFlag), evRow(2, t2)}, readingIDs{probe: 2}},
		{"later flag stands", []EvidenceRow{evRow(1, t1), evRow(2, t2, asFlag)}, readingIDs{probe: 1, flag: 2}},
		{"later no clears too", []EvidenceRow{evRow(1, t1, asFlag), evRow(2, t2, asNo)}, readingIDs{probe: 2}},
		{"later observed answer clears", []EvidenceRow{evRow(1, t1, asFlag), evRow(2, t2, asObserved)}, readingIDs{observed: 2}},
		{"must beat every direct answer", []EvidenceRow{evRow(1, t1, asObserved), evRow(2, t2.Add(-time.Hour), asFlag), evRow(3, t2)}, readingIDs{probe: 3, observed: 1}},
		{"a provider answer is not direct", []EvidenceRow{evRow(1, t1, asFlag), evRow(2, t2, asProvider)}, readingIDs{provider: 2, flag: 1}},
		{"exact tie, flag inserted last", []EvidenceRow{evRow(1, t1), evRow(2, t1, asFlag)}, readingIDs{probe: 1, flag: 2}},
		{"exact tie, answer inserted last", []EvidenceRow{evRow(1, t1, asFlag), evRow(2, t1)}, readingIDs{probe: 2}},
		{"an expired answer no longer clears", []EvidenceRow{evRow(1, t1, asFlag), evRow(2, t2, asNo, expiring(evNow.Add(-time.Hour)))}, readingIDs{flag: 1}},
		{"a future answer does not clear yet", []EvidenceRow{evRow(1, t1, asFlag), evRow(2, evNow.Add(2*time.Hour))}, readingIDs{flag: 1}},
		{"another digest does not clear", []EvidenceRow{evRow(1, t1, asFlag), evRow(2, t2, withDigest(evDigest2))}, readingIDs{flag: 1}},
		{"newest flag", []EvidenceRow{evRow(1, t1, asFlag), evRow(2, t2, asFlag)}, readingIDs{flag: 2}},
		{"an expired flag is gone", []EvidenceRow{evRow(1, t1, asFlag, expiring(evNow.Add(-time.Hour)))}, readingIDs{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolIDs(tt.rows, AdapterOpenAICompat, evDigest, evNow); got != tt.want {
				t.Fatalf("reading = %+v, want %+v", got, tt.want)
			}
		})
	}

	flag := ReadCapabilities([]EvidenceRow{evRow(1, t1, asFlag)}, AdapterOpenAICompat, evDigest, evNow)["tool_call"].Flag
	if flag == nil || flag.Detail != "run_refused" || flag.HTTPStatus != 400 || !flag.ObservedAt.Equal(t1) {
		t.Fatalf("flag lost its cause, status or time: %+v", flag)
	}
}

func TestReadCapabilities_FutureDatedOrUnknownIgnored(t *testing.T) {
	type bad struct {
		name   string
		mutate func(*EvidenceRow)
	}
	cases := []bad{
		{"two hours ahead", func(r *EvidenceRow) { r.ObservedAt = evNow.Add(2 * time.Hour) }},
		{"just past the skew allowance", func(r *EvidenceRow) { r.ObservedAt = evNow.Add(time.Hour + time.Millisecond) }},
	}
	for _, c := range evInvalid {
		cases = append(cases, bad{c.name, c.mutate})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kinds := map[string]func(*EvidenceRow){"as answer": func(*EvidenceRow) {}, "as attempt": asAttempt, "as flag": asFlag}
			for kind, as := range kinds {
				row := evRow(2, evDay(-1), as, c.mutate)
				if got := ReadCapabilities([]EvidenceRow{row}, AdapterOpenAICompat, evDigest, evNow); len(got) != 0 {
					t.Errorf("%s alone = %+v, want no reading", kind, idsOfReadings(got))
				}
				older := evRow(1, evDay(-2))
				if got := toolIDs([]EvidenceRow{older, row}, AdapterOpenAICompat, evDigest, evNow); got != (readingIDs{probe: 1}) {
					t.Errorf("%s beside a valid answer = %+v, want only the answer", kind, got)
				}
			}
		})
	}

	// The allowance itself is honored, inclusive, and a future row is admitted
	// once the clock comes within it.
	onEdge := evRow(2, evNow.Add(time.Hour), asNo)
	if got := toolIDs([]EvidenceRow{evRow(1, evDay(-1)), onEdge}, AdapterOpenAICompat, evDigest, evNow); got != (readingIDs{probe: 2}) {
		t.Errorf("row exactly one hour ahead = %+v, want it to count", got)
	}
	ahead := evRow(2, evNow.Add(2*time.Hour), asNo)
	if got := toolIDs([]EvidenceRow{evRow(1, evDay(-1)), ahead}, AdapterOpenAICompat, evDigest, evNow.Add(time.Hour)); got != (readingIDs{probe: 2}) {
		t.Errorf("future row within the allowance = %+v, want it admitted", got)
	}
}

func TestReadCapabilities_ChatToolsNeverCounts(t *testing.T) {
	rows := []EvidenceRow{evRow(1, evDay(-1), withMethod("chat-tools"))}
	if got := ReadCapabilities(rows, AdapterOpenAICompat, evDigest, evNow); len(got) != 0 {
		t.Fatalf("readings = %+v, want none", idsOfReadings(got))
	}
}

func TestReadCapabilities_NameOnlyYesAfter30DaysValidNotFresh(t *testing.T) {
	at := evDay(-31)
	rows := []EvidenceRow{evRow(1, at, nameOnly, byTheRules(0))}
	got := ReadCapabilities(rows, AdapterOpenAICompat, "", evNow)["tool_call"]
	if ids := idsOfReading(got); ids != (readingIDs{probe: 1}) {
		t.Fatalf("reading = %+v, want the stale yes", ids)
	}
	if got.Probe.Fresh(evNow) {
		t.Errorf("31-day-old name-only yes reads as fresh")
	}
	if !got.Probe.Fresh(at.Add(30 * evDayLen)) {
		t.Errorf("name-only yes not fresh at exactly 30 days")
	}
	if got.Probe.Fresh(at.Add(30*evDayLen + time.Millisecond)) {
		t.Errorf("name-only yes still fresh past 30 days")
	}
}

func TestReadCapabilities_ExpiredAnswerRevealsOlderValid(t *testing.T) {
	rows := []EvidenceRow{
		evRow(1, evDay(0), nameOnly, byTheRules(0)),
		evRow(2, evDay(100), nameOnly, asNo, byTheRules(0)),
	}
	for _, tt := range []struct {
		name string
		at   time.Time
		want int64
	}{
		{"before the newer answer", evDay(50), 1},
		{"while the no holds", evDay(101), 2},
		{"at its expiry", evDay(107), 2},
		{"just past it", evDay(107).Add(time.Millisecond), 1},
		{"long after", evDay(1000), 1},
	} {
		if got := toolIDs(rows, AdapterOpenAICompat, "", tt.at); got != (readingIDs{probe: tt.want}) {
			t.Errorf("%s: reading = %+v, want probe %d", tt.name, got, tt.want)
		}
	}
}

func TestEvidenceRow_Fresh(t *testing.T) {
	tests := []struct {
		name string
		row  EvidenceRow
		at   time.Time
		want bool
	}{
		{"unbounded yes", evRow(1, evDay(-500)), evNow, true},
		{"unexpired no", evRow(1, evDay(-1), asNo, expiring(evDay(1))), evNow, true},
		{"no at its expiry", evRow(1, evDay(-1), asNo, expiring(evNow)), evNow, true},
		{"no past its expiry", evRow(1, evDay(-1), asNo, expiring(evNow)), evNow.Add(time.Millisecond), false},
		{"attempt", evRow(1, evDay(-1), asAttempt, expiring(evDay(1))), evNow, false},
		{"flag", evRow(1, evDay(-1), asFlag), evNow, false},
		{"unknown state", evRow(1, evDay(-1), func(r *EvidenceRow) { r.State = "maybe" }), evNow, false},
		{"future-dated", evRow(1, evNow.Add(2*time.Hour)), evNow, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.row.Fresh(tt.at); got != tt.want {
				t.Fatalf("Fresh = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExpiryFor(t *testing.T) {
	const (
		day   = 24 * time.Hour
		month = 30 * day
	)
	at := evNow
	tests := []struct {
		name               string
		row                EvidenceRow
		ttl                time.Duration
		wantFresh, wantExp time.Time
	}{
		{name: "yes with digest", row: evRow(1, at)},
		{name: "yes with digest ignores the prober", row: evRow(1, at), ttl: 7 * day},
		{name: "yes name-only", row: evRow(1, at, nameOnly), wantFresh: at.Add(month)},
		{name: "no name-only", row: evRow(1, at, nameOnly, asNo), wantExp: at.Add(7 * day)},
		{name: "no name-only ignores the prober", row: evRow(1, at, nameOnly, asNo), ttl: time.Hour, wantExp: at.Add(7 * day)},
		{name: "no with digest, as the prober chose", row: evRow(1, at, asNo), ttl: 48 * time.Hour, wantExp: at.Add(48 * time.Hour)},
		{name: "no with digest, prober chose none", row: evRow(1, at, asNo)},
		{name: "no with digest, negative ttl is none", row: evRow(1, at, asNo), ttl: -time.Hour},
		{name: "inconclusive with digest", row: evRow(1, at, asAttempt), wantExp: at.Add(day)},
		{name: "inconclusive name-only", row: evRow(1, at, asAttempt, nameOnly), ttl: 7 * day, wantExp: at.Add(day)},
		{name: "flag", row: evRow(1, at, asFlag), ttl: day},
		{name: "unknown state", row: evRow(1, at, func(r *EvidenceRow) { r.State = "maybe" }), ttl: day},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fresh, exp := ExpiryFor(tt.row, tt.ttl)
			if !fresh.Equal(tt.wantFresh) || !exp.Equal(tt.wantExp) {
				t.Fatalf("ExpiryFor = (%v, %v), want (%v, %v)", fresh, exp, tt.wantFresh, tt.wantExp)
			}
		})
	}
}

// --- Pruning, through the store ---

func appendEv(t *testing.T, s *SQLiteStore, rows ...EvidenceRow) {
	t.Helper()
	if err := s.AppendEvidence(t.Context(), rows...); err != nil {
		t.Fatalf("AppendEvidence: %v", err)
	}
}

// appendForeignEv stores rows the way another binary would, past this binary's
// write check: a newer build writes its own key version into the shared file.
func appendForeignEv(t *testing.T, s *SQLiteStore, rows ...EvidenceRow) {
	t.Helper()
	err := s.writeTx(t.Context(), func(tx *sql.Tx) error { return insertEvidence(t.Context(), tx, rows) })
	if err != nil {
		t.Fatalf("store foreign rows: %v", err)
	}
}

func pruneEv(t *testing.T, s *SQLiteStore, now time.Time) {
	t.Helper()
	if err := s.Prune(t.Context(), now); err != nil {
		t.Fatalf("Prune: %v", err)
	}
}

func storedEv(t *testing.T, s *SQLiteStore) []EvidenceRow {
	t.Helper()
	rows, err := s.EvidenceRows(t.Context(), evEndpoint, evModel)
	if err != nil {
		t.Fatalf("EvidenceRows: %v", err)
	}
	return rows
}

// wantStoredIDs reads the table directly, so it also sees rows of other key
// versions that EvidenceRows filters out.
func wantStoredIDs(t *testing.T, s *SQLiteStore, want ...int64) {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), `SELECT id FROM capability_evidence ORDER BY id`)
	if err != nil {
		t.Fatalf("query ids: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read ids: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("stored IDs = %v, want %v", got, want)
	}
}

func wantGeneration(t *testing.T, s *SQLiteStore, want int64) {
	t.Helper()
	got, err := s.Generation(t.Context())
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}
	if got != want {
		t.Fatalf("generation = %d, want %d", got, want)
	}
}

func TestPrune_KeepsNewestPerMethodVersion(t *testing.T) {
	s := newTestStore(t)
	const digest3 = "sha256:ccc"
	appendEv(t, s,
		evRow(1, evDay(-400)), // behind 2 in its partition: the one row that may go
		evRow(2, evDay(-300)),
		evRow(3, evDay(-200), withVersion(2)),                                 // a newer version evicts nothing
		evRow(4, evDay(-500), withVersion(0)),                                 // an older version is not compacted into the current one
		evRow(5, evDay(-500), withDigest(evDigest2)),                          // another digest
		evRow(6, evDay(-500), asObserved),                                     // another source
		evRow(7, evDay(-500), asAttempt),                                      // another kind
		evRow(8, evDay(-500), asOllamaShow),                                   // another adapter and method
		evRow(9, evDay(-500), withCap("chat")),                                // another capability, older than 10
		evRow(10, evDay(-400), withCap("chat"), withMethod(MethodChatStream)), // another method than 9
		evRow(11, evDay(-500), withDigest(digest3)),                           // only a newer version above it
		evRow(12, evDay(-200), withDigest(digest3), withVersion(2)),
		evRow(13, evDay(-500), func(r *EvidenceRow) { r.Model = "other" }),                // another model
		evRow(14, evDay(-500), func(r *EvidenceRow) { r.Endpoint = "http://other:8080" }), // another endpoint
	)
	pruneEv(t, s, evNow)
	wantStoredIDs(t, s, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14)
}

func TestPrune_ExpiredAttemptsBeforeWeeklyCheck(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for week := range 52 {
		at := evDay(-364 + week*7)
		pruneEv(t, s, at) // OpenSharedStore prunes before the next check writes.
		appendEv(t, s, evRow(0, at, asAttempt, byTheRules(0)))
	}
	pruneEv(t, s, evNow)
	wantStoredIDs(t, s, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52)
	if got := toolIDs(storedEv(t, s), AdapterOpenAICompat, evDigest, evNow); got != (readingIDs{attempt: 52}) {
		t.Errorf("ReadCapabilities(after weekly pruning) = %+v, want attempt 52", got)
	}

	pruneEv(t, s, evDay(100))
	wantStoredIDs(t, s, 52) // The newest attempt survives even after all history ages out.
}

func TestPrune_AttemptDominanceIgnoresExpiry(t *testing.T) {
	for _, tt := range []struct {
		name        string
		newerAt     time.Time
		olderExpiry time.Time
		want        []int64
	}{
		{"older has no expiry", evDay(-100), time.Time{}, []int64{2}},
		{"older expires later", evDay(-100), evDay(1), []int64{2}},
		{"future attempt cannot evict", evNow.Add(time.Millisecond), time.Time{}, []int64{1, 2}},
		{"attempt dated now can evict", evNow, time.Time{}, []int64{2}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			appendEv(t, s,
				evRow(1, evDay(-200), asAttempt, expiring(tt.olderExpiry)),
				evRow(2, tt.newerAt, asAttempt, byTheRules(0)),
			)
			before := storedEv(t, s)
			pruneEv(t, s, evNow)
			wantStoredIDs(t, s, tt.want...)
			wantSameDecisions(t, tt.name, before, storedEv(t, s), pruneDecisionClocks(before))
		})
	}
}

func TestPrune_FiniteNoPreservesOlderUnboundedYes(t *testing.T) {
	s := newTestStore(t)
	appendEv(t, s,
		evRow(1, evDay(0), nameOnly, byTheRules(0)),
		evRow(2, evDay(100), nameOnly, asNo, byTheRules(0)),
	)
	read := func(at time.Time) *EvidenceRow {
		return ReadCapabilities(storedEv(t, s), AdapterOpenAICompat, "", at)["tool_call"].Probe
	}
	wantStaleYes := func() {
		t.Helper()
		got := read(evDay(108))
		if got == nil || got.ID != 1 || got.State != CapProbeYes || got.Fresh(evDay(108)) {
			t.Fatalf("day 108 reading = %+v, want the stale yes", got)
		}
	}
	wantStaleYes()
	if got := read(evDay(101)); got == nil || got.ID != 2 {
		t.Fatalf("day 101 reading = %+v, want the no", got)
	}

	for _, day := range []int{101, 108, 400} { // the no alive, just expired, long gone
		pruneEv(t, s, evDay(day))
		wantStoredIDs(t, s, 1, 2)
		wantStaleYes()
	}
}

func TestPrune_RetainsFiniteThatOutlivesNewer(t *testing.T) {
	s := newTestStore(t)
	appendEv(t, s,
		evRow(1, evDay(-400), asNo, expiring(evDay(300))), // outlives 2: a winner again after day 100
		evRow(2, evDay(-300), asNo, expiring(evDay(100))),
		evRow(3, evDay(-400), asNo, expiring(evDay(50)), withDigest(evDigest2)), // covered by 4
		evRow(4, evDay(-300), asNo, expiring(evDay(50)), withDigest(evDigest2)),
		evRow(5, evDay(-400), asNo, expiring(evDay(300)), asObserved), // covered by 6, which never expires
		evRow(6, evDay(-300), asObserved),
		evRow(7, evDay(-400), asNo, expiring(evDay(-200)), withCap("chat")), // already over; 8 is alive
		evRow(8, evDay(-300), asNo, expiring(evDay(1)), withCap("chat")),
		evRow(9, evDay(-400), asNo, expiring(evDay(-300)), withCap("stream")), // over, but 10 is over too
		evRow(10, evDay(-300), asNo, expiring(evDay(-200)), withCap("stream")),
	)
	pruneEv(t, s, evNow)
	wantStoredIDs(t, s, 1, 2, 4, 6, 8, 9, 10)

	rows := storedEv(t, s)
	if got := toolIDs(rows, AdapterOpenAICompat, evDigest, evDay(50)); got != (readingIDs{probe: 2, observed: 6}) {
		t.Errorf("day 50 = %+v", got)
	}
	if got := toolIDs(rows, AdapterOpenAICompat, evDigest, evDay(101)); got != (readingIDs{probe: 1, observed: 6}) {
		t.Errorf("day 101 = %+v, want the older no back", got)
	}
}

func TestPrune_UnknownOrFutureRowCannotEvictUsableAnswer(t *testing.T) {
	s := newTestStore(t)
	strange := func(r *EvidenceRow) { r.State = "maybe" }
	appendEv(t, s,
		evRow(1, evDay(-400), strange), // unknown shape, with a valid newer sibling: kept for other binaries
		evRow(2, evDay(-300)),          // the usable answer
		evRow(3, evDay(-200), strange), // unknown shape, newer: authorizes nothing
		evRow(4, evDay(-200), func(r *EvidenceRow) { r.Kind = "x" }),
		evRow(5, evNow.Add(2*time.Hour)),          // future-dated, same partition: authorizes nothing
		evRow(6, evDay(-500), withMethod("nope")), // unknown method
		evRow(7, evDay(-400), withMethod("nope")), // unknown method, newer sibling: not compacted either
		evRow(8, evDay(-500), withVersion(9)),     // unknown version
		evRow(9, evDay(-400), withVersion(9)),
	)
	appendForeignEv(t, s,
		evRow(10, evDay(-500), func(r *EvidenceRow) { r.KeyVersion = EvidenceKeyVersion + 1 }),
		evRow(11, evDay(-400), func(r *EvidenceRow) { r.KeyVersion = EvidenceKeyVersion + 1 }),
	)
	pruneEv(t, s, evNow)
	wantStoredIDs(t, s, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11)
	if got := toolIDs(storedEv(t, s), AdapterOpenAICompat, evDigest, evNow); got != (readingIDs{probe: 2}) {
		t.Fatalf("reading = %+v, want the usable answer", got)
	}

	// Once the future row is honored and past 90 days it does dominate.
	pruneEv(t, s, evDay(100))
	got := toolIDs(storedEv(t, s), AdapterOpenAICompat, evDigest, evDay(100))
	if got != (readingIDs{probe: 5}) {
		t.Fatalf("reading after the future row arrived = %+v", got)
	}
	wantStoredIDs(t, s, 1, 3, 4, 5, 6, 7, 8, 9, 10, 11)

	// A row inside the skew allowance counts for a reader at the pruner's clock
	// but not for one slightly behind it, so it authorizes nothing until its date
	// has passed.
	for _, tt := range []struct {
		name string
		at   time.Time
		want []int64
	}{
		{"30 minutes ahead", evNow.Add(30 * time.Minute), []int64{1, 2}},
		{"at the skew allowance", evNow.Add(time.Hour), []int64{1, 2}},
		{"1 ms ahead", evNow.Add(time.Millisecond), []int64{1, 2}},
		{"dated exactly now", evNow, []int64{2}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			appendEv(t, s, evRow(1, evDay(-300)), evRow(2, tt.at))
			pruneEv(t, s, evNow)
			wantStoredIDs(t, s, tt.want...)
		})
	}
}

func TestPrune_DominatedOldPositiveCanBeRemoved(t *testing.T) {
	for _, tt := range []struct {
		name   string
		digest func(*EvidenceRow)
		read   string
	}{
		{"content digest", withDigest(evDigest), evDigest},
		{"name-only, freshness window set", nameOnly, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			appendEv(t, s,
				evRow(1, evDay(-300), tt.digest, byTheRules(0)),
				evRow(2, evDay(-200), tt.digest, byTheRules(0)),
			)
			wantGeneration(t, s, 1)
			pruneEv(t, s, evNow)
			wantStoredIDs(t, s, 2)
			wantGeneration(t, s, 2)
			if got := toolIDs(storedEv(t, s), AdapterOpenAICompat, tt.read, evNow); got != (readingIDs{probe: 2}) {
				t.Fatalf("reading = %+v", got)
			}
		})
	}
}

func TestPrune_KeepsEveryRowWithin90Days(t *testing.T) {
	s := newTestStore(t)
	cutoff := evNow.Add(-90 * 24 * time.Hour)
	appendEv(t, s,
		evRow(1, cutoff.Add(-time.Millisecond)), // just outside
		evRow(2, cutoff),                        // on the line: within
		evRow(3, cutoff.Add(time.Millisecond)),
		evRow(4, evNow),
	)
	pruneEv(t, s, evNow)
	wantStoredIDs(t, s, 2, 3, 4)
}

func TestPrune_NothingToPruneLeavesGeneration(t *testing.T) {
	s := newTestStore(t)
	pruneEv(t, s, evNow) // an empty log
	wantGeneration(t, s, 0)

	appendEv(t, s, evRow(1, evDay(-300)), evRow(2, evDay(-200)))
	wantGeneration(t, s, 1)
	pruneEv(t, s, evDay(-250)) // nothing is old yet
	wantGeneration(t, s, 1)
	pruneEv(t, s, evNow)
	wantGeneration(t, s, 2)
	pruneEv(t, s, evNow) // nothing left to do
	wantGeneration(t, s, 2)
	wantStoredIDs(t, s, 2)
}

func TestPrune_CanceledDeletesNothing(t *testing.T) {
	s := newTestStore(t)
	appendEv(t, s, evRow(1, evDay(-300)), evRow(2, evDay(-200)))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Prune(ctx, evNow); !errors.Is(err, context.Canceled) {
		t.Fatalf("Prune(canceled) = %v, want context.Canceled", err)
	}
	wantStoredIDs(t, s, 1, 2)
	wantGeneration(t, s, 1)
}

// --- Invariants over generated logs ---

// Offsets and lifetimes sit on a coarse grid so rows tie, overlap and straddle
// the retention line and the skew allowance.
var (
	genObserved = []time.Duration{
		-400 * evDayLen, -300 * evDayLen, -200 * evDayLen, -150 * evDayLen, -120 * evDayLen, -91 * evDayLen,
		-90*evDayLen - time.Millisecond, -90 * evDayLen, -89 * evDayLen, -30 * evDayLen, -evDayLen, 0,
		30 * time.Minute, time.Hour, time.Hour + time.Millisecond, 2 * time.Hour, 5 * evDayLen,
	}
	genLifetimes = []time.Duration{evDayLen, 7 * evDayLen, 100 * evDayLen, 200 * evDayLen, 300 * evDayLen, 420 * evDayLen, 500 * evDayLen, 1000 * evDayLen}
	genTTLs      = []time.Duration{0, 7 * evDayLen, 200 * evDayLen, 500 * evDayLen}
)

func genPick[T any](rng *rand.Rand, options ...T) T { return options[rng.IntN(len(options))] }

// genEvidenceLog builds a log around one to three identities, so rows collide in
// partitions and also compete across methods and sources for one reading. IDs
// are a permutation, unrelated to observation time, because the reader must
// treat them only as the last tie-breaker.
func genEvidenceLog(rng *rand.Rand) []EvidenceRow {
	identities := make([]EvidenceRow, 1+rng.IntN(3))
	for i := range identities {
		identities[i] = genEvidenceIdentity(rng)
	}
	n := 2 + rng.IntN(19)
	rows := make([]EvidenceRow, 0, n)
	for _, id := range rng.Perm(n) {
		r := genPick(rng, identities...)
		r.ID = int64(id) + 1
		genEvidenceClaim(rng, &r)
		rows = append(rows, r)
	}
	return rows
}

func genEvidenceIdentity(rng *rand.Rand) EvidenceRow {
	r := evRow(0, evNow)
	switch rng.IntN(5) {
	case 0:
		r.Method = MethodChatStream // with tool_call, a capability it cannot answer
	case 1:
		r.Adapter, r.Method = AdapterOllama, MethodOllamaShow
	}
	r.Digest = genPick(rng, "", evDigest)
	r.Capability = genPick(rng, "chat", "chat", "tool_call")
	r.Source = genPick(rng, EvidenceSourceProbe, EvidenceSourceProbe, EvidenceSourceObserved, EvidenceSourceProvider)
	return r
}

// genEvidenceClaim fills in everything that varies within a partition.
func genEvidenceClaim(rng *rand.Rand, r *EvidenceRow) {
	r.ObservedAt = evNow.Add(genPick(rng, genObserved...))
	switch rng.IntN(10) {
	case 0, 1:
		asAttempt(r)
	case 2:
		asFlag(r)
	default:
		r.State = genPick(rng, CapProbeYes, CapProbeNo)
	}
	r.FreshUntil, r.ExpiresAt = ExpiryFor(*r, genPick(rng, genTTLs...))
	// Lifetimes the rules rarely produce, to overlap in every way.
	switch rng.IntN(6) {
	case 0, 1:
		r.ExpiresAt = r.ObservedAt.Add(genPick(rng, genLifetimes...))
	case 2:
		r.ExpiresAt = time.Time{}
	}
	// Rows another binary wrote.
	switch rng.IntN(14) {
	case 0:
		r.MethodVersion = 2
	case 1:
		r.State = "maybe"
	case 2:
		r.Method = "nope"
	case 3:
		r.KeyVersion = EvidenceKeyVersion + 1
	}
}

// evidenceClocks returns the instants at which any reading could change: each
// expiry, freshness and skew-admission boundary and a millisecond either side,
// from the given instant on.
func evidenceClocks(rows []EvidenceRow, from time.Time) []time.Time {
	seen := map[int64]bool{from.UnixMilli(): true, from.Add(5000 * evDayLen).UnixMilli(): true}
	add := func(b time.Time) {
		for _, ms := range []int64{b.UnixMilli() - 1, b.UnixMilli(), b.UnixMilli() + 1} {
			if ms >= from.UnixMilli() {
				seen[ms] = true
			}
		}
	}
	for _, r := range rows {
		if !r.ExpiresAt.IsZero() {
			add(r.ExpiresAt)
		}
		if !r.FreshUntil.IsZero() {
			add(r.FreshUntil)
		}
		if !r.ObservedAt.IsZero() {
			add(r.ObservedAt)
			add(r.ObservedAt.Add(-time.Hour))
		}
	}
	clocks := make([]time.Time, 0, len(seen))
	for _, ms := range slices.Sorted(maps.Keys(seen)) {
		clocks = append(clocks, time.UnixMilli(ms))
	}
	return clocks
}

// pruneDecisionClocks is every clock a pruned log must answer for: the prune
// instant and everything after it, plus readers behind the pruner by up to the
// skew allowance, the most this package tolerates between two clocks.
func pruneDecisionClocks(rows []EvidenceRow) []time.Time {
	return append(evidenceClocks(rows, evNow.Add(-time.Hour)), evNow.Add(-time.Millisecond), evNow)
}

// asReaderVersion shows the log as a binary reading method version v would see
// it: its version counts and every other is unknown. The fixed table only has
// version 1, so a future reader is simulated by renumbering the rows it reads.
func asReaderVersion(rows []EvidenceRow, v int) []EvidenceRow {
	out := slices.Clone(rows)
	for i := range out {
		if out[i].MethodVersion == v {
			out[i].MethodVersion = MethodVersion(out[i].Method)
		} else {
			out[i].MethodVersion = -1
		}
	}
	return out
}

// wantSameDecisions compares both logs for every reader version, route adapter
// and digest at every clock. Equal IDs mean equal rows, freshness included.
func wantSameDecisions(t *testing.T, label string, full, pruned []EvidenceRow, clocks []time.Time) {
	t.Helper()
	for _, version := range []int{1, 2} {
		f, p := asReaderVersion(full, version), asReaderVersion(pruned, version)
		for _, adapter := range []string{AdapterOpenAICompat, AdapterOllama} {
			for _, digest := range []string{"", evDigest, evDigest2} {
				for _, clock := range clocks {
					a := idsOfReadings(ReadCapabilities(f, adapter, digest, clock))
					b := idsOfReadings(ReadCapabilities(p, adapter, digest, clock))
					if !maps.Equal(a, b) {
						t.Fatalf("%s: reader v%d, %s, digest %q, clock %v:\n full   %+v\n pruned %+v\nlog: %s",
							label, version, adapter, digest, clock.Sub(evNow), a, b, describeLog(full))
					}
				}
			}
		}
	}
}

func describeLog(rows []EvidenceRow) string {
	var out string
	for _, r := range slices.SortedFunc(slices.Values(rows), func(a, b EvidenceRow) int { return int(a.ID - b.ID) }) {
		out += fmt.Sprintf("\n  #%d %s/%s/%s v%d digest=%q %s %s %s observed=%v fresh=%v expires=%v",
			r.ID, r.Adapter, r.Method, r.Capability, r.MethodVersion, r.Digest, r.Source, r.Kind, r.State,
			r.ObservedAt.Sub(evNow), offsetOrNone(r.FreshUntil), offsetOrNone(r.ExpiresAt))
	}
	return out
}

func offsetOrNone(at time.Time) string {
	if at.IsZero() {
		return "none"
	}
	return at.Sub(evNow).String()
}

func withoutIDs(rows []EvidenceRow, ids []int64) []EvidenceRow {
	return slices.DeleteFunc(slices.Clone(rows), func(r EvidenceRow) bool { return slices.Contains(ids, r.ID) })
}

// evNewestPerPartition recomputes the issue's retention rule independently of
// the production grouping: the newest row of each partition and kind.
func evNewestPerPartition(rows []EvidenceRow) map[string]EvidenceRow {
	newest := map[string]EvidenceRow{}
	for _, r := range rows {
		key := fmt.Sprint(r.KeyVersion, r.Endpoint, r.Adapter, r.Model, r.Capability, r.Source, r.Method, r.MethodVersion, r.Digest, r.Kind)
		cur, ok := newest[key]
		if !ok || r.ObservedAt.After(cur.ObservedAt) || (r.ObservedAt.Equal(cur.ObservedAt) && r.ID > cur.ID) {
			newest[key] = r
		}
	}
	return newest
}

func TestEvidenceInvariants_PrunePreservesFutureDecisions(t *testing.T) {
	cutoff := evNow.Add(-90 * evDayLen)
	var deleted, keptOld int
	for seed := range uint64(250) {
		rng := rand.New(rand.NewPCG(seed, 734))
		full := genEvidenceLog(rng)
		ids := prunableEvidenceIDs(full, evNow)
		pruned := withoutIDs(full, ids)

		for _, r := range full {
			gone := slices.Contains(ids, r.ID)
			if gone && !r.ObservedAt.Before(cutoff) {
				t.Fatalf("seed %d: deleted row %d inside the 90-day window: %s", seed, r.ID, describeLog(full))
			}
			foreign := r.MethodVersion != 1 || r.State == "maybe" || r.Method == "nope" || r.KeyVersion != EvidenceKeyVersion ||
				(r.Method == MethodChatStream && r.Capability == "tool_call")
			if gone && foreign {
				t.Fatalf("seed %d: deleted row %d this binary does not understand: %s", seed, r.ID, describeLog(full))
			}
			if !gone && r.ObservedAt.Before(cutoff) {
				keptOld++
			}
		}
		for _, head := range evNewestPerPartition(full) {
			if slices.Contains(ids, head.ID) {
				t.Fatalf("seed %d: deleted row %d, the newest of its partition: %s", seed, head.ID, describeLog(full))
			}
		}
		deleted += len(ids)
		wantSameDecisions(t, fmt.Sprintf("seed %d", seed), full, pruned, pruneDecisionClocks(full))
	}
	// Guard against a generator that stopped exercising the decision.
	t.Logf("%d rows deleted, %d old rows kept", deleted, keptOld)
	if deleted < 300 || keptOld < 300 {
		t.Fatalf("generated logs exercised too little: %d deleted, %d old rows kept", deleted, keptOld)
	}
}

// The same property through the store: Prune applies the reducer's decision to
// the stored rows and nothing else.
func TestEvidenceInvariants_StoredPruneMatchesReducer(t *testing.T) {
	for seed := range uint64(25) {
		rng := rand.New(rand.NewPCG(seed, 736))
		rows := genEvidenceLog(rng)
		slices.SortFunc(rows, func(a, b EvidenceRow) int { return int(a.ID - b.ID) }) // rowids then match IDs

		s := newTestStore(t)
		appendForeignEv(t, s, rows...) // the log holds rows other binaries wrote
		full := storedEv(t, s)
		ids := prunableEvidenceIDs(rows, evNow)
		pruneEv(t, s, evNow)
		pruned := storedEv(t, s)

		var want []int64
		for _, r := range withoutIDs(rows, ids) {
			want = append(want, r.ID)
		}
		wantStoredIDs(t, s, want...)
		wantSameDecisions(t, fmt.Sprintf("seed %d", seed), full, pruned, pruneDecisionClocks(full))
	}
}

func TestEvidenceInvariants_OrderIndependent(t *testing.T) {
	for seed := range uint64(60) {
		rng := rand.New(rand.NewPCG(seed, 735))
		rows := genEvidenceLog(rng)
		clocks := evidenceClocks(rows, evNow)
		wantIDs := prunableEvidenceIDs(rows, evNow)
		for range 4 {
			shuffled := slices.Clone(rows)
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			if got := prunableEvidenceIDs(shuffled, evNow); !slices.Equal(got, wantIDs) {
				t.Fatalf("seed %d: prunable IDs %v, want %v", seed, got, wantIDs)
			}
			for _, adapter := range []string{AdapterOpenAICompat, AdapterOllama} {
				for _, digest := range []string{"", evDigest} {
					for _, clock := range clocks {
						a := idsOfReadings(ReadCapabilities(rows, adapter, digest, clock))
						b := idsOfReadings(ReadCapabilities(shuffled, adapter, digest, clock))
						if !maps.Equal(a, b) {
							t.Fatalf("seed %d, %s, digest %q, clock %v: %+v vs %+v", seed, adapter, digest, clock.Sub(evNow), a, b)
						}
					}
				}
			}
		}
	}
}

func TestEvidenceInvariants_InvalidRowsAreInert(t *testing.T) {
	base := []EvidenceRow{
		evRow(1, evDay(-300)), // behind 2: the one prunable row
		evRow(2, evDay(-200)), // a newer invalid row must not evict or shadow it, nor clear the flag
		evRow(3, evDay(-200), asObserved),
		evRow(4, evDay(-100), asAttempt, byTheRules(0)),
		evRow(5, evDay(-50), asFlag),
		evRow(6, evDay(-40), asOllamaShow),
	}
	clocks := evidenceClocks(base, evNow)
	baseIDs := prunableEvidenceIDs(base, evNow)
	if !slices.Equal(baseIDs, []int64{1}) {
		t.Fatalf("base prunable IDs = %v, want [1]", baseIDs)
	}
	for _, c := range evInvalid {
		for _, offset := range []int{-400, -1} { // older than everything, newer than everything
			t.Run(fmt.Sprintf("%s/day%d", c.name, offset), func(t *testing.T) {
				log := append(slices.Clone(base), evRow(100, evDay(offset), c.mutate))
				for _, digest := range []string{evDigest, ""} {
					for _, adapter := range []string{AdapterOpenAICompat, AdapterOllama} {
						for _, clock := range clocks {
							a := idsOfReadings(ReadCapabilities(base, adapter, digest, clock))
							b := idsOfReadings(ReadCapabilities(log, adapter, digest, clock))
							if !maps.Equal(a, b) {
								t.Fatalf("%s, digest %q, clock %v: %+v vs %+v", adapter, digest, clock.Sub(evNow), a, b)
							}
						}
					}
				}
				if got := prunableEvidenceIDs(log, evNow); !slices.Equal(got, baseIDs) {
					t.Fatalf("prunable IDs = %v, want %v", got, baseIDs)
				}
			})
		}
	}
}

// --- Store ---

func TestEvidenceRows_RoundTripAndScope(t *testing.T) {
	s := newTestStore(t)
	full := EvidenceRow{
		ID: 1, KeyVersion: EvidenceKeyVersion,
		Endpoint: evEndpoint, Adapter: AdapterOllama, Model: evModel, Digest: evDigest,
		Capability: "tool_call", Source: EvidenceSourceProvider, Kind: EvidenceKindFlag, State: CapProbeFlagged,
		Method: MethodOllamaShow, MethodVersion: 1, Detail: "run_refused", HTTPStatus: 422,
		ObservedAt: evDay(-3), FreshUntil: evDay(1), ExpiresAt: evDay(2),
	}
	bare := evRow(2, evDay(-1))
	appendEv(t, s, full, bare,
		evRow(3, evDay(-1), func(r *EvidenceRow) { r.Endpoint = "http://other:8080" }),
		evRow(4, evDay(-1), func(r *EvidenceRow) { r.Model = "other" }),
	)
	appendForeignEv(t, s, evRow(5, evDay(-1), func(r *EvidenceRow) { r.KeyVersion = EvidenceKeyVersion + 1 }))
	got := storedEv(t, s)
	if want := []EvidenceRow{full, bare}; !reflect.DeepEqual(got, want) {
		t.Fatalf("EvidenceRows =\n %+v\nwant\n %+v", got, want)
	}
}

func TestAppendEvidence_OneTransactionBumpsGenerationOnce(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "append.db"))
	s, err := NewStore(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	wantGeneration(t, s, 0)

	appendEv(t, s, evRow(1, evNow, withCap("chat")), evRow(2, evNow, withCap("stream")), evRow(3, evNow))
	wantGeneration(t, s, 1)
	wantStoredIDs(t, s, 1, 2, 3)

	appendEv(t, s) // nothing to write, nothing to tell readers
	wantGeneration(t, s, 1)

	// A failure on a later row takes the earlier rows and the bump with it.
	execMigrationSQL(t, db, `CREATE TRIGGER refuse_stream BEFORE INSERT ON capability_evidence
		WHEN NEW.capability = 'stream' BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	err = s.AppendEvidence(t.Context(), evRow(4, evNow, withCap("chat")), evRow(5, evNow, withCap("stream")))
	if err == nil {
		t.Fatal("append through the refusing trigger succeeded")
	}
	wantStoredIDs(t, s, 1, 2, 3)
	wantGeneration(t, s, 1)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.AppendEvidence(ctx, evRow(4, evNow)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled append = %v", err)
	}
	wantStoredIDs(t, s, 1, 2, 3)
	wantGeneration(t, s, 1)
}

// evMarked is a row of marker values, so a test can see any of them leak into an
// error. Its method, state, kind and source are unknown, which AppendEvidence
// must accept: readers ignore such rows and other binaries may write them.
func evMarked(mutators ...func(*EvidenceRow)) EvidenceRow {
	r := EvidenceRow{
		KeyVersion: EvidenceKeyVersion,
		Endpoint:   "http://endpoint-marker:8080", Adapter: "adapter-marker", Model: "model-marker", Digest: "digest-marker",
		Capability: "capability-marker", Source: "source-marker", Kind: "kind-marker", State: "state-marker",
		Method: "method-marker", MethodVersion: 9, Detail: "detail-marker", ObservedAt: evNow,
	}
	for _, mutate := range mutators {
		mutate(&r)
	}
	return r
}

func TestAppendEvidence_RejectsRowsItCouldNotReadBack(t *testing.T) {
	noModel := func(r *EvidenceRow) { r.Model = "" }
	tests := []struct {
		name  string
		field string // the field the error must name
		rows  []EvidenceRow
	}{
		{"unset key version", "KeyVersion", []EvidenceRow{evMarked(func(r *EvidenceRow) { r.KeyVersion = 0 })}},
		{"another key version", "KeyVersion", []EvidenceRow{evMarked(func(r *EvidenceRow) { r.KeyVersion = EvidenceKeyVersion + 1 })}},
		{"empty endpoint", "Endpoint", []EvidenceRow{evMarked(func(r *EvidenceRow) { r.Endpoint = "" })}},
		{"empty adapter", "Adapter", []EvidenceRow{evMarked(func(r *EvidenceRow) { r.Adapter = "" })}},
		{"empty model", "Model", []EvidenceRow{evMarked(noModel)}},
		{"empty capability", "Capability", []EvidenceRow{evMarked(func(r *EvidenceRow) { r.Capability = "" })}},
		{"empty method", "Method", []EvidenceRow{evMarked(func(r *EvidenceRow) { r.Method = "" })}},
		{"zero observation time", "ObservedAt", []EvidenceRow{evMarked(func(r *EvidenceRow) { r.ObservedAt = time.Time{} })}},
		{"good row, then a bad one", "Model", []EvidenceRow{evMarked(), evMarked(noModel)}},
		{"bad row, then a good one", "Model", []EvidenceRow{evMarked(noModel), evMarked()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			wantRejected := func(err error) {
				t.Helper()
				if err == nil {
					t.Fatal("AppendEvidence accepted the batch")
				}
				if msg := err.Error(); !strings.Contains(msg, tt.field) || strings.Contains(msg, "marker") {
					t.Errorf("error = %q, want it to name %s and carry no row value", msg, tt.field)
				}
			}
			wantRejected(s.AppendEvidence(t.Context(), tt.rows...))

			// Each row's own key, or a row written under its bad value would not show.
			for _, r := range append([]EvidenceRow{evMarked()}, tt.rows...) {
				got, err := s.EvidenceRows(t.Context(), r.Endpoint, r.Model)
				if err != nil {
					t.Fatalf("EvidenceRows: %v", err)
				}
				if len(got) != 0 {
					t.Errorf("EvidenceRows(%q, %q) returned %d rows, want none", r.Endpoint, r.Model, len(got))
				}
			}
			wantStoredIDs(t, s) // EvidenceRows' key-version filter would hide a KeyVersion row
			wantGeneration(t, s, 0)

			// A canceled context fails BeginTx, so seeing the rejection instead
			// proves the check ran before any transaction opened.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err := s.AppendEvidence(ctx, tt.rows...)
			wantRejected(err)
			if errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want the rejection before the transaction", err)
			}
		})
	}
}

func TestAppendEvidence_AcceptsRowsOtherBinariesMayWrite(t *testing.T) {
	s := newTestStore(t)
	appendEv(t, s, evMarked(), evMarked(func(r *EvidenceRow) { r.Digest = "" })) // name-only is a real identity
	wantGeneration(t, s, 1)
	got, err := s.EvidenceRows(t.Context(), evMarked().Endpoint, evMarked().Model)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Method != "method-marker" || got[0].State != "state-marker" || got[1].Digest != "" {
		t.Fatalf("EvidenceRows = %+v", got)
	}
	if readings := ReadCapabilities(got, "adapter-marker", "digest-marker", evNow); len(readings) != 0 {
		t.Fatalf("readings = %+v, want the reader to ignore these rows", idsOfReadings(readings))
	}
}

func TestAppendEvidence_WaitsForWriterInsteadOfFailing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writers.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s, err := NewStore(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	// The competitor commits a write, so a transaction that read before writing
	// would be left on a stale snapshot and fail at once instead of waiting.
	execMigrationSQL(t, b, "BEGIN IMMEDIATE")
	execMigrationSQL(t, b, "UPDATE store_meta SET generation = generation + 1")
	done := make(chan error, 1)
	go func() { done <- s.AppendEvidence(ctx, evRow(1, evNow)) }()
	for a.Stats().InUse == 0 {
		select {
		case err := <-done:
			t.Fatalf("append returned while the writer held the lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(200 * time.Millisecond)
	execMigrationSQL(t, b, "COMMIT")
	if err := migrationResult(t, ctx, done); err != nil {
		t.Fatalf("append behind a writer: %v", err)
	}
	wantGeneration(t, s, 2)
	wantStoredIDs(t, s, 1)
}

func TestAppendEvidence_BusyBeyondTimeoutReturnsLockError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	s, err := NewStore(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	execMigrationSQL(t, a, "PRAGMA busy_timeout=50")
	execMigrationSQL(t, b, "BEGIN IMMEDIATE")
	err = s.AppendEvidence(t.Context(), evRow(1, evNow))
	execMigrationSQL(t, b, "ROLLBACK")
	assertMigrationSQLiteError(t, err, 5)
	wantStoredIDs(t, s)
	wantGeneration(t, s, 0)
}

func TestAppendEvidence_TwoHandlesAppendAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const perHandle = 25
	results := make(chan error, 2)
	stores := []*SQLiteStore{mustEvidenceStore(t, ctx, path), mustEvidenceStore(t, ctx, path)}
	for _, s := range stores {
		go func() {
			for range perHandle {
				if err := s.AppendEvidence(ctx, evRow(0, evNow)); err != nil {
					results <- err
					return
				}
				if _, err := s.EvidenceRows(ctx, evEndpoint, evModel); err != nil {
					results <- err
					return
				}
			}
			results <- nil
		}()
	}
	for range stores {
		if err := migrationResult(t, ctx, results); err != nil {
			t.Fatalf("shared file: %v", err)
		}
	}
	wantGeneration(t, stores[0], 2*perHandle)
	if got := len(storedEv(t, stores[1])); got != 2*perHandle {
		t.Fatalf("stored %d rows, want %d", got, 2*perHandle)
	}
}

func mustEvidenceStore(t *testing.T, ctx context.Context, path string) *SQLiteStore {
	t.Helper()
	s, err := NewStore(ctx, openMigrationDB(t, path))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Prune decides outside its transaction, and SQLite may reuse a deleted row's id
// for a later insert. The trigger runs on the generation bump, Prune's first
// statement in the transaction, and swaps the doomed row for a fresh one under
// the same id. The fresh row is not the one Prune decided on, so it stays.
func TestPrune_ReusedIDIsNotDeleted(t *testing.T) {
	s := newTestStore(t)
	appendEv(t, s, evRow(0, evDay(-300), byTheRules(0)), evRow(0, evDay(-200), byTheRules(0)))
	if ids := prunableEvidenceIDs(storedEv(t, s), evNow); !slices.Equal(ids, []int64{1}) {
		t.Fatalf("prunable = %v, want [1]: the test needs row 1 doomed", ids)
	}
	_, err := s.db.ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER reuse_id AFTER UPDATE ON store_meta BEGIN
		DELETE FROM capability_evidence WHERE id = 1;
		INSERT INTO capability_evidence (id, key_version, endpoint, adapter, model_name, model_digest,
			capability, evidence, kind, state, method, method_version, observed_at)
		SELECT 1, key_version, endpoint, adapter, model_name, model_digest,
			capability, evidence, kind, state, method, method_version, %d
		FROM capability_evidence WHERE id = 2;
	END`, evNow.UnixMilli()))
	if err != nil {
		t.Fatal(err)
	}
	pruneEv(t, s, evNow)
	wantStoredIDs(t, s, 1, 2)
	if got := storedEv(t, s)[0].ObservedAt; !got.Equal(evNow) {
		t.Fatalf("row 1 observed at %v, want the fresh row's %v: the trigger did not run", got, evNow)
	}
}

// A statement that runs out of space makes SQLite roll the whole transaction
// back itself, so the deferred rollback finds none. That is not a second failure.
func TestWriteTx_FullDiskReportsOnlyTheFullDisk(t *testing.T) {
	ctx := t.Context()
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "full.db"))
	s, err := NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := s.Generation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pages int
	if err := db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	execMigrationSQL(t, db, fmt.Sprintf(`PRAGMA max_page_count = %d`, pages+2))
	row := evRow(0, time.Now(), func(r *EvidenceRow) { r.Detail = strings.Repeat("x", 4096) })
	err = s.AppendEvidence(ctx, slices.Repeat([]EvidenceRow{row}, 64)...)
	if err == nil {
		t.Fatal("append past max_page_count succeeded")
	}
	if !strings.Contains(err.Error(), "disk is full") {
		t.Fatalf("err = %q, want the full disk", err)
	}
	if strings.Contains(err.Error(), "rollback") {
		t.Fatalf("err = %q, want no rollback failure beside the full disk", err)
	}
	wantGeneration(t, s, gen)
	wantStoredIDs(t, s)
}
