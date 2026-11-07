package fingerprint

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	keyA = "sk-live-AAAA-0123456789abcdef-raw-secret-value"
	keyB = "sk-live-BBBB-fedcba9876543210-other-secret-val"
)

// lsNow and lsDay date rows from the real clock: the store checks dates against
// it, and evNow (2027) is ahead of it. lsNow is 30 days back, so lsDay(n) is a
// past date for n < 30; tests that read through a getter using the real clock
// date their rows from time.Now() instead.
var lsNow = time.Now().Add(-30 * evDayLen).Truncate(time.Millisecond)

func lsDay(n int) time.Time { return lsNow.Add(time.Duration(n) * evDayLen) }

func publish(t *testing.T, s *SQLiteStore, pub ListingPublish) {
	t.Helper()
	if err := s.PublishListings(t.Context(), []ListingPublish{pub}); err != nil {
		t.Fatalf("PublishListings: %v", err)
	}
}

func listingOf(endpoint, key string, at time.Time, models ...string) ListingPublish {
	p := ListingPublish{Endpoint: endpoint, Adapter: evAdapter, APIKey: key, StartedAt: at}
	for _, m := range models {
		p.Members = append(p.Members, ListingMember{Model: m, OwnedBy: "o-" + m, Digest: "d-" + m})
	}
	return p
}

func modelsOf(l *ListingSnapshot) []string {
	var out []string
	for _, m := range l.Members {
		out = append(out, m.Model)
	}
	return out
}

func count(t *testing.T, s *SQLiteStore, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func wantModels(t *testing.T, got *ListingSnapshot, want ...string) {
	t.Helper()
	if got == nil {
		t.Fatalf("listing = nil, want models %v", want)
	}
	if g := modelsOf(got); !slices.Equal(g, want) {
		t.Fatalf("listing models = %v, want %v", g, want)
	}
}

// failInserts makes every insert into table fail inside the transaction.
func failInserts(t *testing.T, s *SQLiteStore, table string) {
	t.Helper()
	q := fmt.Sprintf(`CREATE TRIGGER boom_%s BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT, 'injected'); END`, table, table)
	if _, err := s.db.ExecContext(t.Context(), q); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}
}

func TestAccount_TwoKeysOneEndpointSeparate(t *testing.T) {
	s := newTestStore(t)
	publish(t, s, listingOf(evEndpoint, keyA, lsNow, "only-a"))
	if err := s.RecordModelAccess(t.Context(), evEndpoint, keyA, "gone", AccessRefused, 403, time.Now()); err != nil {
		t.Fatal(err)
	}
	gotB, err := s.CurrentListing(t.Context(), evEndpoint, evAdapter, keyB)
	if err != nil || gotB != nil {
		t.Fatalf("listing under B = %+v, %v; want nil", gotB, err)
	}
	accB, err := s.LatestModelAccess(t.Context(), evEndpoint, keyB, "gone")
	if err != nil || accB != nil {
		t.Fatalf("access under B = %+v, %v; want nil", accB, err)
	}
	gotA, err := s.CurrentListing(t.Context(), evEndpoint, evAdapter, keyA)
	if err != nil {
		t.Fatal(err)
	}
	wantModels(t, gotA, "only-a")
	accA, err := s.LatestModelAccess(t.Context(), evEndpoint, keyA, "gone")
	if err != nil || accA == nil || accA.State != AccessRefused || accA.HTTPStatus != 403 {
		t.Fatalf("access under A = %+v, %v", accA, err)
	}
	// B publishing its own listing leaves A's alone: two snapshots coexist.
	publish(t, s, listingOf(evEndpoint, keyB, lsDay(1), "only-b"))
	gotA, _ = s.CurrentListing(t.Context(), evEndpoint, evAdapter, keyA)
	wantModels(t, gotA, "only-a")
	if n := count(t, s, "listing_snapshots"); n != 2 {
		t.Fatalf("snapshots = %d, want 2", n)
	}
}

func TestAccount_KeyChangeStartsEmptyAnswersStay(t *testing.T) {
	s := newTestStore(t)
	appendEv(t, s, evRow(0, lsNow))
	publish(t, s, listingOf(evEndpoint, keyA, lsNow, "m"))
	if err := s.RecordModelAccess(t.Context(), evEndpoint, keyA, "m", AccessRefused, 404, time.Now()); err != nil {
		t.Fatal(err)
	}
	snap, err := s.ReadSnapshot(t.Context(), evEndpoint, evAdapter, ptr(keyB), "", []string{"m"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Rows) != 1 || snap.Rows[0].State != CapProbeYes || snap.Rows[0].Capability != "tool_call" {
		t.Fatalf("rows under the new key = %+v, want the one yes answer", snap.Rows)
	}
	if snap.Listing != nil || len(snap.Access) != 0 {
		t.Fatalf("new key sees listing %+v access %+v, want neither", snap.Listing, snap.Access)
	}
	if l, _ := s.CurrentListing(t.Context(), evEndpoint, evAdapter, keyB); l != nil {
		t.Fatalf("CurrentListing under new key = %+v", l)
	}
	if a, _ := s.LatestModelAccess(t.Context(), evEndpoint, keyB, "m"); a != nil {
		t.Fatalf("LatestModelAccess under new key = %+v", a)
	}
}

func TestAccount_NoKeyIsEmptyAccount(t *testing.T) {
	s := newTestStore(t)
	if got := s.account(evEndpoint, ""); got != "" {
		t.Fatalf("keyless account = %q, want empty", got)
	}
	publish(t, s, listingOf(evEndpoint, "", lsNow, "first"))
	publish(t, s, listingOf(evEndpoint, "", lsDay(1), "second"))
	l, err := s.CurrentListing(t.Context(), evEndpoint, evAdapter, "")
	if err != nil {
		t.Fatal(err)
	}
	wantModels(t, l, "second") // the second keyless publisher replaced the first
	var account string
	var n int
	if err := s.db.QueryRowContext(t.Context(), `SELECT account, COUNT(*) FROM listing_snapshots`).Scan(&account, &n); err != nil {
		t.Fatal(err)
	}
	if account != "" || n != 1 {
		t.Fatalf("stored account %q, snapshots %d; want empty, 1", account, n)
	}
	if err := s.RecordModelAccess(t.Context(), evEndpoint, "", "m", AccessAnswered, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.LatestModelAccess(t.Context(), evEndpoint, "", "m"); a == nil || a.State != AccessAnswered {
		t.Fatalf("keyless access = %+v", a)
	}
	if a, _ := s.LatestModelAccess(t.Context(), evEndpoint, keyA, "m"); a != nil {
		t.Fatalf("a keyed caller sees the keyless access: %+v", a)
	}
}

func ptr(s string) *string { return &s }

func TestAccount_RawKeyNeverInFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fp.db")
	s := mustEvidenceStore(t, t.Context(), path)
	publish(t, s, listingOf(evEndpoint, keyA, lsNow, "m"))
	if err := s.RecordModelAccess(t.Context(), evEndpoint, keyA, "m", AccessRefused, 403, time.Now()); err != nil {
		t.Fatal(err)
	}
	err := s.AppendCheckResult(t.Context(), evEndpoint, keyA, "m", []EvidenceRow{evRow(0, lsNow)},
		&ModelAccess{Model: "m", State: AccessAnswered, ObservedAt: lsNow})
	if err != nil {
		t.Fatal(err)
	}
	var secret []byte
	if err := s.db.QueryRowContext(t.Context(), `SELECT account_secret FROM store_meta`).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	// The account is a keyed hash of endpoint, NUL, key, computed here apart from the store.
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(evEndpoint + "\x00" + keyA))
	want := hex.EncodeToString(mac.Sum(nil))
	var gotSnap, gotAccess string
	if err := s.db.QueryRowContext(t.Context(), `SELECT account FROM listing_snapshots`).Scan(&gotSnap); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(t.Context(), `SELECT account FROM model_access ORDER BY id LIMIT 1`).Scan(&gotAccess); err != nil {
		t.Fatal(err)
	}
	if gotSnap != want || gotAccess != want {
		t.Fatalf("stored accounts %q and %q, want both %q", gotSnap, gotAccess, want)
	}
	scanned := 0
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		if bytes.Contains(b, []byte(keyA)) {
			t.Fatalf("raw key found in %s%s", filepath.Base(path), suffix)
		}
	}
	if scanned < 2 {
		t.Fatalf("scanned %d files, want the database and its WAL at least", scanned)
	}
	// The scan can see what is there: the stored account hash is in the bytes.
	b, _ := os.ReadFile(path)
	w, _ := os.ReadFile(path + "-wal")
	if !bytes.Contains(append(b, w...), []byte(want)) {
		t.Fatal("the scan cannot find the stored account hash, so it proves nothing")
	}
}

func TestNoExportedTypeCarriesAccount(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(path string, ty reflect.Type)
	walk = func(path string, ty reflect.Type) {
		switch ty.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(path, ty.Elem())
		case reflect.Map:
			walk(path, ty.Key())
			walk(path, ty.Elem())
		case reflect.Struct:
			if seen[ty] {
				return
			}
			seen[ty] = true
			for i := range ty.NumField() {
				f := ty.Field(i)
				if strings.Contains(strings.ToLower(f.Name), "account") {
					t.Errorf("%s.%s carries an account", path, f.Name)
				}
				walk(path+"."+f.Name, f.Type)
			}
		}
	}
	for _, v := range []any{
		EvidenceRow{}, ListingMember{}, ListingPublish{}, ListingSnapshot{}, NamedModel{}, ModelAccess{},
		EvidenceSnapshot{}, CapabilityReading{}, CapProbe{},
	} {
		walk(reflect.TypeOf(v).Name(), reflect.TypeOf(v))
	}
	st := reflect.TypeOf(&SQLiteStore{})
	for i := range st.NumMethod() {
		if strings.Contains(strings.ToLower(st.Method(i).Name), "account") {
			t.Errorf("exported method %s mentions an account", st.Method(i).Name)
		}
	}
}

func TestPublishListings_OlderStartedAtDoesNotReplace(t *testing.T) {
	s := newTestStore(t)
	publish(t, s, listingOf(evEndpoint, keyA, lsDay(2), "new"))
	late := listingOf(evEndpoint, keyA, lsDay(1), "old")
	late.Facts = []EvidenceRow{evRow(0, lsNow, asOllamaShow)}
	publish(t, s, late)
	l, _ := s.CurrentListing(t.Context(), evEndpoint, evAdapter, keyA)
	wantModels(t, l, "new")
	if !l.StartedAt.Equal(lsDay(2)) {
		t.Fatalf("StartedAt = %v, want %v", l.StartedAt, lsDay(2))
	}
	if n, m, e := count(t, s, "listing_snapshots"), count(t, s, "listing_members"), count(t, s, "capability_evidence"); n != 1 || m != 1 || e != 0 {
		t.Fatalf("snapshots %d members %d evidence %d; want 1, 1, 0 (a late older publish inserts nothing)", n, m, e)
	}
	// An equal start is not newer either.
	publish(t, s, listingOf(evEndpoint, keyA, lsDay(2), "tie"))
	l, _ = s.CurrentListing(t.Context(), evEndpoint, evAdapter, keyA)
	wantModels(t, l, "new")
	// A newer one replaces, and its predecessor's members go with it.
	publish(t, s, listingOf(evEndpoint, keyA, lsDay(3), "newer", "newest"))
	l, _ = s.CurrentListing(t.Context(), evEndpoint, evAdapter, keyA)
	wantModels(t, l, "newer", "newest")
	if n, m := count(t, s, "listing_snapshots"), count(t, s, "listing_members"); n != 1 || m != 2 {
		t.Fatalf("snapshots %d members %d; want 1, 2", n, m)
	}
	if l.Members[0] != (ListingMember{Model: "newer", OwnedBy: "o-newer", Digest: "d-newer"}) {
		t.Fatalf("member = %+v", l.Members[0])
	}
}

func TestPublishListings_EmptyIsRecordedEmpty(t *testing.T) {
	s := newTestStore(t)
	if l, _ := s.CurrentListing(t.Context(), evEndpoint, evAdapter, keyA); l != nil {
		t.Fatalf("listing before any publish = %+v, want nil", l)
	}
	publish(t, s, listingOf(evEndpoint, keyA, lsNow, "gone-soon"))
	publish(t, s, listingOf(evEndpoint, keyA, lsDay(1)))
	l, err := s.CurrentListing(t.Context(), evEndpoint, evAdapter, keyA)
	if err != nil {
		t.Fatal(err)
	}
	wantModels(t, l)
	if !l.StartedAt.Equal(lsDay(1)) {
		t.Fatalf("empty listing StartedAt = %v", l.StartedAt)
	}
	if n, m := count(t, s, "listing_snapshots"), count(t, s, "listing_members"); n != 1 || m != 0 {
		t.Fatalf("snapshots %d members %d; want 1, 0", n, m)
	}
}

func TestPublishListings_OneTransaction(t *testing.T) {
	s := newTestStore(t)
	publish(t, s, listingOf(evEndpoint, keyA, lsNow, "kept"))
	wantGeneration(t, s, 1)
	failInserts(t, s, "capability_evidence")
	two := []ListingPublish{
		listingOf("http://one:1", keyA, lsDay(1), "x"),
		listingOf(evEndpoint, keyA, lsDay(1), "replacement"),
	}
	two[1].Facts = []EvidenceRow{evRow(0, lsNow, asOllamaShow)}
	if err := s.PublishListings(t.Context(), two); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("PublishListings error = %v, want the injected failure", err)
	}
	if n := count(t, s, "listing_snapshots"); n != 1 {
		t.Fatalf("snapshot rows = %d, want only the first publish's", n)
	}
	l, _ := s.CurrentListing(t.Context(), evEndpoint, evAdapter, keyA)
	wantModels(t, l, "kept")
	if l, _ := s.CurrentListing(t.Context(), "http://one:1", evAdapter, keyA); l != nil {
		t.Fatalf("the first listing of the failed batch survived: %+v", l)
	}
	wantGeneration(t, s, 1)
}

func TestPublishListings_RejectsBeforeWriting(t *testing.T) {
	s := newTestStore(t)
	bad := listingOf(evEndpoint, keyA, lsNow, "m")
	bad.Facts = []EvidenceRow{evRow(0, lsNow, func(r *EvidenceRow) { r.Endpoint = "" })}
	for name, pub := range map[string]ListingPublish{
		"invalid fact":    bad,
		"no endpoint":     listingOf("", keyA, lsNow, "m"),
		"no start":        listingOf(evEndpoint, keyA, time.Time{}, "m"),
		"unresolved key":  listingOf(evEndpoint, "${OPENAI_KEY}", lsNow, "m"),
		"empty member":    listingOf(evEndpoint, keyA, lsNow, ""),
		"partial secrets": listingOf(evEndpoint, "Bearer ${X}", lsNow, "m"),
	} {
		err := s.PublishListings(t.Context(), []ListingPublish{listingOf("http://ok:1", "", lsNow, "fine"), pub})
		if err == nil {
			t.Errorf("%s: want an error", name)
		} else if strings.Contains(err.Error(), keyA) || strings.Contains(err.Error(), "OPENAI_KEY") {
			t.Errorf("%s: error quotes the credential: %v", name, err)
		}
	}
	if n := count(t, s, "listing_snapshots"); n != 0 {
		t.Fatalf("snapshots = %d, want none written", n)
	}
	wantGeneration(t, s, 0)
}

func TestPublishListings_TwoHandlesRaceNoBusySnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fp.db")
	stores := []*SQLiteStore{mustEvidenceStore(t, t.Context(), path), mustEvidenceStore(t, t.Context(), path)}
	const each = 100
	errs := make(chan error, 2*each)
	var wg sync.WaitGroup
	for h, s := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				// Distinct, increasing starts interleave across the handles.
				at := lsDay(0).Add(time.Duration(2*i+h) * time.Millisecond)
				errs <- s.PublishListings(t.Context(), []ListingPublish{listingOf(evEndpoint, keyA, at, fmt.Sprintf("m%d-%d", h, i))})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	l, err := stores[0].CurrentListing(t.Context(), evEndpoint, evAdapter, keyA)
	if err != nil {
		t.Fatal(err)
	}
	wantModels(t, l, "m1-99")
	if n := count(t, stores[1], "listing_snapshots"); n != 1 {
		t.Fatalf("snapshots = %d, want 1", n)
	}
	wantGeneration(t, stores[0], 2*each)
}

func TestNamedModels_SurvivePublish(t *testing.T) {
	s := newTestStore(t)
	for _, m := range []string{"zeta", "alpha"} {
		if err := s.AddNamedModel(t.Context(), evEndpoint, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddNamedModel(t.Context(), "http://other:1", "elsewhere"); err != nil {
		t.Fatal(err)
	}
	before, err := s.NamedModels(t.Context(), evEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 || before[0].Model != "alpha" || before[1].Model != "zeta" || before[0].AddedAt.IsZero() {
		t.Fatalf("named = %+v, want alpha then zeta, dated", before)
	}
	publish(t, s, listingOf(evEndpoint, keyA, lsNow, "alpha"))
	publish(t, s, listingOf(evEndpoint, keyA, lsDay(1))) // a listing that omits both
	if err := s.AddNamedModel(t.Context(), evEndpoint, "alpha"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.NamedModels(t.Context(), evEndpoint)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("named models after publishes = %+v, want %+v (re-adding keeps AddedAt)", after, before)
	}
	l, _ := s.CurrentListing(t.Context(), evEndpoint, evAdapter, keyA)
	wantModels(t, l)
	if err := s.RemoveNamedModel(t.Context(), evEndpoint, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveNamedModel(t.Context(), evEndpoint, "never-named"); err != nil {
		t.Fatalf("removing an unnamed model: %v", err)
	}
	after, _ = s.NamedModels(t.Context(), evEndpoint)
	if len(after) != 1 || after[0].Model != "zeta" {
		t.Fatalf("after remove = %+v, want zeta only", after)
	}
	if other, _ := s.NamedModels(t.Context(), "http://other:1"); len(other) != 1 {
		t.Fatalf("another endpoint's named models = %+v", other)
	}
}

func TestLatestModelAccess_RefusedExpiresAt7Days(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	at := lsNow
	if err := s.RecordModelAccess(ctx, evEndpoint, keyA, "m", AccessRefused, 403, at); err != nil {
		t.Fatal(err)
	}
	var expires int64
	if err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM model_access`).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if want := at.Add(7 * 24 * time.Hour).UnixMilli(); expires != want {
		t.Fatalf("stored expires_at = %d, want %d", expires, want)
	}
	read := func(now time.Time) *ModelAccess {
		snap, err := s.ReadSnapshot(ctx, evEndpoint, evAdapter, ptr(keyA), "", []string{"m"}, now)
		if err != nil {
			t.Fatal(err)
		}
		return snap.Access["m"]
	}
	if a := read(at.Add(7 * 24 * time.Hour)); a == nil || a.State != AccessRefused || a.HTTPStatus != 403 ||
		!a.ObservedAt.Equal(at) || !a.ExpiresAt.Equal(at.Add(7*24*time.Hour)) || a.Model != "m" {
		t.Fatalf("access on the last valid instant = %+v", a)
	}
	if a := read(at.Add(7*24*time.Hour + time.Millisecond)); a != nil {
		t.Fatalf("access a millisecond past expiry = %+v, want nil", a)
	}
	// Through the getter, which uses the real clock: a fresh refusal is current, an old one is not.
	if err := s.RecordModelAccess(ctx, evEndpoint, keyA, "fresh", AccessRefused, 404, time.Now()); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.LatestModelAccess(ctx, evEndpoint, keyA, "fresh"); a == nil || a.HTTPStatus != 404 {
		t.Fatalf("fresh refusal = %+v", a)
	}
	if err := s.RecordModelAccess(ctx, evEndpoint, keyA, "stale", AccessRefused, 403, time.Now().Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.LatestModelAccess(ctx, evEndpoint, keyA, "stale"); a != nil {
		t.Fatalf("expired refusal = %+v, want nil", a)
	}
	// An expired newest row is no claim, and does not revive an older answered row.
	if err := s.RecordModelAccess(ctx, evEndpoint, keyA, "rev", AccessAnswered, 0, lsDay(-30)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordModelAccess(ctx, evEndpoint, keyA, "rev", AccessRefused, 403, lsDay(-20)); err != nil {
		t.Fatal(err)
	}
	if snap, _ := s.ReadSnapshot(ctx, evEndpoint, evAdapter, ptr(keyA), "", []string{"rev"}, lsNow); snap.Access["rev"] != nil {
		t.Fatalf("expired refusal revived an older row: %+v", snap.Access["rev"])
	}
	// An answered row never expires.
	if err := s.RecordModelAccess(ctx, evEndpoint, keyA, "ok", AccessAnswered, 0, lsNow); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.ReadSnapshot(ctx, evEndpoint, evAdapter, ptr(keyA), "", []string{"ok"}, lsDay(3650))
	if a := snap.Access["ok"]; a == nil || !a.ExpiresAt.IsZero() || a.HTTPStatus != 0 || a.State != AccessAnswered {
		t.Fatalf("answered access = %+v", a)
	}
	for _, bad := range []struct {
		state AccessState
		at    time.Time
	}{{"maybe", lsNow}, {AccessRefused, time.Time{}}} {
		if err := s.RecordModelAccess(ctx, evEndpoint, keyA, "m", bad.state, 403, bad.at); err == nil {
			t.Errorf("RecordModelAccess(%q, %v) accepted", bad.state, bad.at)
		}
	}
}

func TestPrune_ModelAccessKeepsNewestPerAccountModel(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	rec := func(key, model string, st AccessState, at time.Time) {
		t.Helper()
		if err := s.RecordModelAccess(ctx, evEndpoint, key, model, st, 403, at); err != nil {
			t.Fatal(err)
		}
	}
	rec(keyA, "m", AccessAnswered, lsDay(-200)) // 1: older than the newest, outside 90 days: goes
	rec(keyA, "m", AccessRefused, lsDay(-150))  // 2: the newest of A/m, however old: stays
	rec(keyB, "m", AccessAnswered, lsDay(-300)) // 3: B's only row: stays
	rec(keyA, "n", AccessAnswered, lsDay(-120)) // 4: goes, superseded by 5
	rec(keyA, "n", AccessAnswered, lsDay(-100)) // 5: stays (newest)
	rec(keyA, "n", AccessRefused, lsDay(-10))   // 6: newest, recent
	rec(keyA, "o", AccessAnswered, lsDay(-95))  // 7: outside the window but A/o's only row: stays
	rec(keyA, "o", AccessRefused, lsDay(-89))   // 8: inside 90 days: stays, newest
	rec(keyA, "p", AccessAnswered, lsDay(-89))  // 9: inside 90 days, superseded: stays
	rec(keyA, "p", AccessRefused, lsDay(-1))    // 10: newest
	wantGeneration(t, s, 10)
	pruneEv(t, s, lsNow)
	// 5 was superseded by 6 and is older than 90 days, so it goes too; 7 is outside 90 days and superseded by 8.
	wantAccessIDs(t, s, 2, 3, 6, 8, 9, 10)
	wantGeneration(t, s, 11)
	// Idle: nothing left to delete, so no write transaction and no generation bump.
	pruneEv(t, s, lsNow)
	wantAccessIDs(t, s, 2, 3, 6, 8, 9, 10)
	wantGeneration(t, s, 11)
}

func wantAccessIDs(t *testing.T, s *SQLiteStore, want ...int64) {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), `SELECT id FROM model_access ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("model_access IDs = %v, want %v", got, want)
	}
}

func TestReadSnapshot_ConcurrentPublishIsEntirelyBeforeOrAfter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fp.db")
	writer := mustEvidenceStore(t, t.Context(), path)
	reader := mustEvidenceStore(t, t.Context(), path)
	const rounds = 300
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer close(done)
		for i := 1; i <= rounds; i++ {
			// Publish i is the only write to bump the generation to i, and it carries
			// its number in the listing digest and in the evidence row it publishes.
			pub := listingOf(evEndpoint, keyA, lsDay(0).Add(time.Duration(i)*time.Millisecond))
			pub.Members = []ListingMember{{Model: "m", Digest: strconv.Itoa(i)}}
			pub.Facts = []EvidenceRow{evRow(0, lsDay(0).Add(time.Duration(i)*time.Millisecond), asOllamaShow, withDigest(strconv.Itoa(i)))}
			if err := writer.PublishListings(ctx, []ListingPublish{pub}); err != nil {
				done <- err
				return
			}
		}
	}()
	reads := 0
	for {
		snap, err := reader.ReadSnapshot(ctx, evEndpoint, evAdapter, ptr(keyA), "", []string{"m"}, lsDay(1))
		if err != nil {
			t.Fatalf("ReadSnapshot: %v", err)
		}
		reads++
		if snap.Listing == nil {
			if snap.Generation != 0 || len(snap.Rows) != 0 {
				t.Fatalf("no listing, but generation %d and %d rows", snap.Generation, len(snap.Rows))
			}
		} else {
			want := strconv.Itoa(int(snap.Generation))
			if got := snap.Listing.Members[0].Digest; got != want {
				t.Fatalf("listing digest %q at generation %d: read straddled a publish", got, snap.Generation)
			}
			if n := len(snap.Rows); n != int(snap.Generation) || snap.Rows[n-1].Digest != want {
				t.Fatalf("evidence (%d rows, newest digest %q) does not match listing digest %q", n, snap.Rows[len(snap.Rows)-1].Digest, want)
			}
		}
		select {
		case err, open := <-done:
			if err != nil {
				t.Fatalf("publisher: %v", err)
			}
			if !open {
				if reads < 5 {
					t.Fatalf("only %d reads overlapped the writer", reads)
				}
				return
			}
		default:
		}
	}
}

func TestReadSnapshot_ClosesBeforeCallerNetworkWork(t *testing.T) {
	// One connection, as the shared opener makes it: an open read transaction would
	// starve every other statement on the handle.
	s := newTestStore(t)
	s.db.SetMaxOpenConns(1)
	appendEv(t, s, evRow(0, lsNow))
	if _, err := s.ReadSnapshot(t.Context(), evEndpoint, evAdapter, ptr(keyA), "p", []string{"m", "m"}, lsNow); err != nil {
		t.Fatal(err)
	}
	if in := s.db.Stats().InUse; in != 0 {
		t.Fatalf("connections in use after ReadSnapshot = %d, want 0", in)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := s.AddNamedModel(ctx, evEndpoint, "later"); err != nil {
		t.Fatalf("write after ReadSnapshot: %v", err)
	}
	// An error path ends the transaction too.
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	if _, err := s.ReadSnapshot(cancelled, evEndpoint, evAdapter, nil, "", nil, lsNow); err == nil {
		t.Fatal("ReadSnapshot on a cancelled context succeeded")
	}
	if in := s.db.Stats().InUse; in != 0 {
		t.Fatalf("connections in use after a failed ReadSnapshot = %d, want 0", in)
	}
}

func TestReadSnapshot_NilCredentialSkipsAccountReads(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	publish(t, s, listingOf(evEndpoint, "", lsNow, "m"))
	publish(t, s, listingOf(evEndpoint, keyA, lsNow, "keyed"))
	if err := s.RecordModelAccess(ctx, evEndpoint, "", "m", AccessAnswered, 0, lsNow); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNamedModel(ctx, evEndpoint, "named"); err != nil {
		t.Fatal(err)
	}
	appendEv(t, s, evRow(0, lsNow))
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO capability_probes (backend_id, model_name, capability, state, model_digest, probe_version, tested_at)
		VALUES ('prov', 'm', 'tool_call', 'yes', 'dg', 1, ?)`, lsNow.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	models := []string{"m", "absent"}

	nilSnap, err := s.ReadSnapshot(ctx, evEndpoint, evAdapter, nil, "prov", models, lsNow)
	if err != nil {
		t.Fatal(err)
	}
	if nilSnap.Listing != nil || len(nilSnap.Access) != 0 {
		t.Fatalf("nil credential read listing %+v access %+v", nilSnap.Listing, nilSnap.Access)
	}
	if len(nilSnap.Rows) != 1 || len(nilSnap.Named) != 1 || nilSnap.Named[0].Model != "named" {
		t.Fatalf("nil credential: rows %d named %+v; evidence and named models stay readable", len(nilSnap.Rows), nilSnap.Named)
	}
	if p := nilSnap.Legacy["m"]; p == nil || p.State != CapProbeYes || p.BackendID != "prov" || p.ModelDigest != "dg" || len(nilSnap.Legacy) != 1 {
		t.Fatalf("legacy = %+v", nilSnap.Legacy)
	}

	keyless, err := s.ReadSnapshot(ctx, evEndpoint, evAdapter, ptr(""), "", models, lsNow)
	if err != nil {
		t.Fatal(err)
	}
	wantModels(t, keyless.Listing, "m")
	if keyless.Access["m"] == nil || len(keyless.Access) != 1 || len(keyless.Legacy) != 0 {
		t.Fatalf("keyless access %+v legacy %+v", keyless.Access, keyless.Legacy)
	}

	// An unresolved credential reads no account data and the rest as usual.
	unres, err := s.ReadSnapshot(ctx, evEndpoint, evAdapter, ptr("${API_KEY}"), "", models, lsNow)
	if err != nil {
		t.Fatal(err)
	}
	if unres.Listing != nil || len(unres.Access) != 0 || len(unres.Rows) != 1 || len(unres.Named) != 1 {
		t.Fatalf("unresolved credential: %+v", unres)
	}
	if l, err := s.CurrentListing(ctx, evEndpoint, evAdapter, "${API_KEY}"); l != nil || err != nil {
		t.Fatalf("CurrentListing unresolved = %+v, %v", l, err)
	}

	// No models: metadata only.
	meta, err := s.ReadSnapshot(ctx, evEndpoint, evAdapter, ptr(keyA), "prov", nil, lsNow)
	if err != nil {
		t.Fatal(err)
	}
	wantModels(t, meta.Listing, "keyed")
	if len(meta.Rows) != 0 || len(meta.Access) != 0 || len(meta.Legacy) != 0 || len(meta.Named) != 1 {
		t.Fatalf("metadata-only read = %+v", meta)
	}
}

func TestAppendCheckResult_OneCommitOneGeneration(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Now().Truncate(time.Millisecond)
	rows := []EvidenceRow{evRow(0, now), evRow(0, now, withCap("chat"), withMethod(MethodChatStream))}
	acc := &ModelAccess{Model: evModel, State: AccessRefused, HTTPStatus: 403, ObservedAt: now}
	if err := s.AppendCheckResult(ctx, evEndpoint, keyA, evModel, rows, acc); err != nil {
		t.Fatal(err)
	}
	wantGeneration(t, s, 1)
	if n := count(t, s, "capability_evidence"); n != 2 {
		t.Fatalf("evidence rows = %d, want 2", n)
	}
	got, _ := s.LatestModelAccess(ctx, evEndpoint, keyA, evModel)
	if got == nil || got.State != AccessRefused || got.HTTPStatus != 403 || !got.ExpiresAt.Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("access = %+v, want a 403 refusal expiring in 7 days", got)
	}
	// An access-only refusal and an evidence-only check are each one commit.
	if err := s.AppendCheckResult(ctx, evEndpoint, keyA, evModel, nil,
		&ModelAccess{Model: evModel, State: AccessAnswered, ObservedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	wantGeneration(t, s, 2)
	if err := s.AppendCheckResult(ctx, evEndpoint, keyA, evModel, []EvidenceRow{evRow(0, now.Add(-time.Hour))}, nil); err != nil {
		t.Fatal(err)
	}
	wantGeneration(t, s, 3)
	if n, a := count(t, s, "capability_evidence"), count(t, s, "model_access"); n != 3 || a != 2 {
		t.Fatalf("evidence %d access %d; want 3, 2", n, a)
	}
	// Nothing to write writes nothing.
	if err := s.AppendCheckResult(ctx, evEndpoint, keyA, evModel, nil, nil); err != nil {
		t.Fatal(err)
	}
	wantGeneration(t, s, 3)
}

func TestAppendCheckResult_SecondWriteFailureRollsBackEverything(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Now().Truncate(time.Millisecond)
	appendEv(t, s, evRow(0, now))
	if err := s.RecordModelAccess(ctx, evEndpoint, keyA, evModel, AccessRefused, 403, now); err != nil {
		t.Fatal(err)
	}
	wantGeneration(t, s, 2)
	failInserts(t, s, "model_access")
	err := s.AppendCheckResult(ctx, evEndpoint, keyA, evModel,
		[]EvidenceRow{evRow(0, now.Add(-time.Hour)), evRow(0, now.Add(-time.Minute))},
		&ModelAccess{Model: evModel, State: AccessAnswered, ObservedAt: now.Add(-time.Hour)})
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("error = %v, want the injected failure", err)
	}
	wantGeneration(t, s, 2)
	wantStoredIDs(t, s, 1)
	wantAccessIDs(t, s, 1)
	if a, _ := s.LatestModelAccess(ctx, evEndpoint, keyA, evModel); a == nil || a.State != AccessRefused {
		t.Fatalf("previous refusal = %+v, want it unchanged", a)
	}
}

func TestAppendCheckResult_CancelBeforeCommitRollsBackEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fp.db")
	s := mustEvidenceStore(t, t.Context(), path)
	other := mustEvidenceStore(t, t.Context(), path)
	appendEv(t, s, evRow(0, lsNow))
	wantGeneration(t, s, 1)

	// Another handle holds the write lock for 300ms, so the transaction is open and
	// waiting when the context is cancelled at 100ms. SQLite's busy wait does not
	// watch the context, so the cancellation lands on the next statement or the
	// commit, which is before the commit takes effect either way.
	conn, err := other.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(t.Context(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
	}()
	start := time.Now()
	err = s.AppendCheckResult(ctx, evEndpoint, keyA, evModel, []EvidenceRow{evRow(0, lsDay(1))},
		&ModelAccess{Model: evModel, State: AccessRefused, HTTPStatus: 403, ObservedAt: lsDay(1)})
	if err == nil {
		t.Fatal("AppendCheckResult succeeded under a cancelled context")
	}
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("error %v after %v, want the context deadline", err, time.Since(start))
	}
	wantGeneration(t, s, 1)
	wantStoredIDs(t, s, 1)
	wantAccessIDs(t, s)

	// A context cancelled before the call changes nothing either.
	dead, stop := context.WithCancel(t.Context())
	stop()
	if err := s.AppendCheckResult(dead, evEndpoint, keyA, evModel, []EvidenceRow{evRow(0, lsDay(1))}, nil); err == nil {
		t.Fatal("AppendCheckResult succeeded under a dead context")
	}
	wantGeneration(t, s, 1)
	wantStoredIDs(t, s, 1)
}

func TestAppendCheckResult_MismatchedEndpointOrModelRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	good := evRow(0, lsNow)
	otherModel := evRow(0, lsNow, func(r *EvidenceRow) { r.Model = "other" })
	otherEndpoint := evRow(0, lsNow, func(r *EvidenceRow) { r.Endpoint = "http://elsewhere:1" })
	invalid := evRow(0, lsNow, func(r *EvidenceRow) { r.Method = "" })
	okAccess := &ModelAccess{Model: evModel, State: AccessRefused, ObservedAt: lsNow}
	for name, c := range map[string]struct {
		rows   []EvidenceRow
		access *ModelAccess
		key    string
	}{
		"row of another model":    {[]EvidenceRow{good, otherModel}, okAccess, keyA},
		"row of another endpoint": {[]EvidenceRow{good, otherEndpoint}, okAccess, keyA},
		"access of another model": {[]EvidenceRow{good}, &ModelAccess{Model: "other", State: AccessRefused, ObservedAt: lsNow}, keyA},
		"unreadable row":          {[]EvidenceRow{good, invalid}, okAccess, keyA},
		"unknown access state":    {[]EvidenceRow{good}, &ModelAccess{Model: evModel, State: "x", ObservedAt: lsNow}, keyA},
		"unresolved credential":   {[]EvidenceRow{good}, okAccess, "${KEY}"},
	} {
		err := s.AppendCheckResult(ctx, evEndpoint, c.key, evModel, c.rows, c.access)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), keyA) || strings.Contains(err.Error(), "KEY}") {
			t.Errorf("%s: error quotes the credential: %v", name, err)
		}
	}
	if n, a := count(t, s, "capability_evidence"), count(t, s, "model_access"); n != 0 || a != 0 {
		t.Fatalf("rejected results wrote %d evidence and %d access rows", n, a)
	}
	wantGeneration(t, s, 0)
}

func TestCurrentListing_NeverEmptyDuringTwoProviderPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fp.db")
	writer := mustEvidenceStore(t, t.Context(), path)
	reader := mustEvidenceStore(t, t.Context(), path)
	const rounds = 300
	done := make(chan error, 1)
	go func() {
		defer close(done)
		for i := 1; i <= rounds; i++ {
			at := lsDay(0).Add(time.Duration(i) * time.Millisecond)
			err := writer.PublishListings(t.Context(), []ListingPublish{
				listingOf(evEndpoint, keyA, at, "one"), listingOf("http://two:2", keyA, at, "two"),
			})
			if err != nil {
				done <- err
				return
			}
		}
	}()
	for reads := 0; ; reads++ {
		l, err := reader.CurrentListing(t.Context(), evEndpoint, evAdapter, keyA)
		if err != nil {
			t.Fatal(err)
		}
		if l != nil && len(l.Members) != 1 {
			t.Fatalf("read %d: current listing has %d members mid-publish, want 1", reads, len(l.Members))
		}
		select {
		case err, open := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if !open {
				return
			}
		default:
		}
	}
}

func TestAccess_FutureAndUnknownStateRowsAreNotClaims(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Now().Truncate(time.Millisecond)
	if err := s.RecordModelAccess(ctx, evEndpoint, keyA, "m", AccessAnswered, 0, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Dated three years ahead: not admitted. Unknown state, newer than the answer: not a claim.
	if err := s.RecordModelAccess(ctx, evEndpoint, keyA, "m", AccessRefused, 403, now.Add(3*365*evDayLen)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO model_access (key_version, endpoint, account, model_name, state, observed_at)
		VALUES (1, ?, ?, 'm', 'from-a-newer-binary', ?)`, evEndpoint, s.account(evEndpoint, keyA), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestModelAccess(ctx, evEndpoint, keyA, "m")
	if err != nil || got == nil || got.State != AccessAnswered {
		t.Fatalf("getter = %+v, %v; want the answered row", got, err)
	}
	snap, err := s.ReadSnapshot(ctx, evEndpoint, evAdapter, ptr(keyA), "", []string{"m"}, now)
	if err != nil || snap.Access["m"] == nil || snap.Access["m"].State != AccessAnswered {
		t.Fatalf("snapshot access = %+v, %v; want the answered row", snap.Access["m"], err)
	}
	// Later, the future row is admitted and wins.
	snap, _ = s.ReadSnapshot(ctx, evEndpoint, evAdapter, ptr(keyA), "", []string{"m"}, now.Add(3*365*evDayLen))
	if a := snap.Access["m"]; a == nil || a.State != AccessRefused {
		t.Fatalf("access once the future row's date arrives = %+v", a)
	}
}

func TestPrune_ModelAccessFutureAndUnknownRowsAuthorizeNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if err := s.RecordModelAccess(ctx, evEndpoint, keyA, "m", AccessAnswered, 0, lsDay(-200)); err != nil { // 1: the real claim
		t.Fatal(err)
	}
	if err := s.RecordModelAccess(ctx, evEndpoint, keyA, "m", AccessRefused, 403, lsDay(3*365)); err != nil { // 2: future
		t.Fatal(err)
	}
	old := func(state string, at time.Time) {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO model_access (key_version, endpoint, account, model_name, state, observed_at) VALUES (1, ?, ?, 'm', ?, ?)`,
			evEndpoint, s.account(evEndpoint, keyA), state, at.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	old("from-a-newer-binary", lsDay(-300)) // 3: another binary's row, old: never deleted
	old("from-a-newer-binary", lsDay(-1))   // 4: unknown and newer than the answer: no authority
	pruneEv(t, s, lsNow)
	wantAccessIDs(t, s, 1, 2, 3, 4)
	// An old answered row under the real one is still pruned.
	if err := s.RecordModelAccess(ctx, evEndpoint, keyA, "m", AccessAnswered, 0, lsDay(-250)); err != nil { // 5
		t.Fatal(err)
	}
	pruneEv(t, s, lsNow)
	wantAccessIDs(t, s, 1, 2, 3, 4)
}

func TestPublishListings_FutureStartedAtRefusedAndStoredOneReplaced(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Now().Truncate(time.Millisecond)
	future := now.Add(3 * 365 * evDayLen)
	if err := s.PublishListings(ctx, []ListingPublish{listingOf(evEndpoint, keyA, future, "skewed")}); err == nil {
		t.Fatal("a listing dated three years ahead was accepted")
	}
	if n := count(t, s, "listing_snapshots"); n != 0 {
		t.Fatalf("snapshots = %d, want none", n)
	}
	wantGeneration(t, s, 0)
	// A row already stored with such a date (another clock, another binary) is not current.
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO listing_snapshots (key_version, endpoint, adapter, account, started_at, committed_at) VALUES (1, ?, ?, ?, ?, ?)`,
		evEndpoint, evAdapter, s.account(evEndpoint, keyA), future.UnixMilli(), now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO listing_members (snapshot_id, model_name) VALUES (?, 'skewed')`, id); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.CurrentListing(ctx, evEndpoint, evAdapter, keyA); l != nil {
		t.Fatalf("CurrentListing = %+v, want the future-dated row ignored", l)
	}
	if snap, _ := s.ReadSnapshot(ctx, evEndpoint, evAdapter, ptr(keyA), "", nil, now); snap.Listing != nil {
		t.Fatalf("ReadSnapshot listing = %+v, want it ignored", snap.Listing)
	}
	publish(t, s, listingOf(evEndpoint, keyA, now.Add(-time.Minute), "real"))
	l, _ := s.CurrentListing(ctx, evEndpoint, evAdapter, keyA)
	wantModels(t, l, "real")
	if n, m := count(t, s, "listing_snapshots"), count(t, s, "listing_members"); n != 1 || m != 1 {
		t.Fatalf("snapshots %d members %d; want 1, 1", n, m)
	}
}

func TestModelAccess_ExpiryIsTheRuleNotTheCallers(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	at := lsNow
	long := at.Add(8759 * time.Hour)
	for _, c := range []struct {
		model string
		state AccessState
		want  any
	}{
		{"refused", AccessRefused, at.Add(7 * 24 * time.Hour).UnixMilli()},
		{"answered", AccessAnswered, nil},
	} {
		err := s.AppendCheckResult(ctx, evEndpoint, keyA, c.model, nil,
			&ModelAccess{Model: c.model, State: c.state, ObservedAt: at, ExpiresAt: long})
		if err != nil {
			t.Fatal(err)
		}
		var got sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM model_access WHERE model_name = ?`, c.model).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if c.want == nil && got.Valid || c.want != nil && (!got.Valid || got.Int64 != c.want) {
			t.Fatalf("%s: stored expires_at = %+v, want %v", c.model, got, c.want)
		}
	}
	// And the answered row is honored forever, though the caller set an expiry.
	snap, _ := s.ReadSnapshot(ctx, evEndpoint, evAdapter, ptr(keyA), "", []string{"answered"}, long.Add(time.Hour))
	if snap.Access["answered"] == nil {
		t.Fatal("answered access expired on a caller-set expiry")
	}
}

func TestListingPublish_NeverPrintsKey(t *testing.T) {
	const marker = "MARKER-key-7f3a9c"
	pub := listingOf(evEndpoint, marker, lsNow, "m")
	for name, got := range map[string]string{
		"%v":        fmt.Sprintf("%v", pub),
		"%+v":       fmt.Sprintf("%+v", pub),
		"%#v":       fmt.Sprintf("%#v", pub),
		"ptr %+v":   fmt.Sprintf("%+v", &pub),
		"slice %v":  fmt.Sprintf("%v", []ListingPublish{pub}),
		"slice %+v": fmt.Sprintf("%+v", []ListingPublish{pub}),
		"slice %#v": fmt.Sprintf("%#v", []ListingPublish{pub}),
	} {
		if strings.Contains(got, marker) {
			t.Errorf("%s prints the key: %s", name, got)
		}
		if !strings.Contains(got, evEndpoint) {
			t.Errorf("%s lost the endpoint: %s", name, got)
		}
	}
	for name, v := range map[string]any{"value": pub, "slice": []ListingPublish{pub}, "pointer": &pub} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), marker) {
			t.Errorf("json of %s prints the key: %s", name, b)
		}
	}
}

// One server reached through two adapters at one address keeps two listings:
// neither replaces the other, in either started_at order.
func TestPublishListings_AdaptersKeepSeparateListings(t *testing.T) {
	for name, order := range map[string][2]time.Time{"native newer": {lsDay(1), lsDay(2)}, "compat newer": {lsDay(2), lsDay(1)}} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			native := listingOf(evEndpoint, keyA, order[0], "n")
			native.Adapter = AdapterOllama
			compat := listingOf(evEndpoint, keyA, order[1], "c")
			compat.Members[0].Digest = ""
			publish(t, s, native)
			publish(t, s, compat)
			ln, err := s.CurrentListing(t.Context(), evEndpoint, AdapterOllama, keyA)
			if err != nil || ln == nil || len(ln.Members) != 1 || ln.Members[0] != (ListingMember{Model: "n", OwnedBy: "o-n", Digest: "d-n"}) {
				t.Fatalf("ollama listing = %+v, %v", ln, err)
			}
			lc, err := s.CurrentListing(t.Context(), evEndpoint, AdapterOpenAICompat, keyA)
			if err != nil || lc == nil || len(lc.Members) != 1 || lc.Members[0] != (ListingMember{Model: "c", OwnedBy: "o-c"}) {
				t.Fatalf("compat listing = %+v, %v", lc, err)
			}
			if n := count(t, s, "listing_snapshots"); n != 2 {
				t.Fatalf("snapshots = %d, want 2", n)
			}
		})
	}
}

func TestPublishListings_RejectsEmptyAdapter(t *testing.T) {
	s := newTestStore(t)
	p := listingOf(evEndpoint, keyA, lsNow, "m")
	p.Adapter = ""
	if err := s.PublishListings(t.Context(), []ListingPublish{p}); err == nil || !strings.Contains(err.Error(), "adapter") {
		t.Fatalf("error = %v, want an adapter refusal", err)
	}
	if n := count(t, s, "listing_snapshots"); n != 0 {
		t.Fatalf("snapshots = %d, want none", n)
	}
	wantGeneration(t, s, 0)
}
