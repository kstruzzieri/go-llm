package provider

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// openFeedbackFileDB opens a caller-owned file database for the deadline tests.
func openFeedbackFileDB(t *testing.T, path, query string, maxConns int) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: path, RawQuery: query}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(maxConns)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func execFeedbackSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), query); err != nil {
		t.Fatalf("Exec(%q): %v", query, err)
	}
}

func newFileFeedbackStore(t *testing.T, db *sql.DB) *SQLiteFeedbackStore {
	t.Helper()
	store, err := NewSQLiteFeedbackStore(t.Context(), db, SQLiteFeedbackStoreConfig{})
	if err != nil {
		t.Fatalf("NewSQLiteFeedbackStore: %v", err)
	}
	return store
}

// holdFeedbackLock runs stmts on a pinned connection of a separate handle and
// keeps the resulting lock until release (or test cleanup). A deferred BEGIN
// takes no lock by itself; follow it with an executed SELECT for SHARED, or
// use BEGIN IMMEDIATE for the write lock.
func holdFeedbackLock(t *testing.T, path string, stmts ...string) (release func()) {
	t.Helper()
	holder := openFeedbackFileDB(t, path, "", 1)
	conn, err := holder.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if strings.HasPrefix(s, "SELECT") {
			var n int
			if err := conn.QueryRowContext(t.Context(), s).Scan(&n); err != nil {
				t.Fatalf("holder %q: %v", s, err)
			}
			continue
		}
		if _, err := conn.ExecContext(t.Context(), s); err != nil {
			t.Fatalf("holder %q: %v", s, err)
		}
	}
	release = sync.OnceFunc(func() {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
	})
	t.Cleanup(release)
	return release
}

func feedbackRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM routing_feedback_signals`).Scan(&n); err != nil {
		t.Fatalf("count signals: %v", err)
	}
	return n
}

// assertBusyTimeouts pins n connections at once and checks each one's
// busy_timeout, so every pooled connection is inspected.
func assertBusyTimeouts(t *testing.T, db *sql.DB, n int, want int64) {
	t.Helper()
	conns := make([]*sql.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := range n {
		c, err := db.Conn(t.Context())
		if err != nil {
			t.Fatalf("pin connection %d: %v", i+1, err)
		}
		conns = append(conns, c)
		var got int64
		if err := c.QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&got); err != nil {
			t.Fatalf("read busy_timeout on connection %d: %v", i+1, err)
		}
		if got != want {
			t.Fatalf("connection %d busy_timeout = %d, want %d", i+1, got, want)
		}
	}
}

func testFeedbackItem(model string) FeedbackItem {
	return FeedbackItem{
		Key:    FeedbackKey{Provider: "p", Model: model, UseCase: "chat"},
		Signal: FeedbackSignal{Kind: RoutingSignalSuccess, At: time.Now()},
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

// assertContendedFailure fails unless err comes from waiting on the held lock
// (SQLITE_BUSY, an interrupt, or the deadline) after at least minWait, and
// arrived within maxWait. An early unrelated error is an invalid fixture.
func assertContendedFailure(t *testing.T, err error, elapsed, minWait, maxWait time.Duration) {
	t.Helper()
	var se *sqlite.Error
	lockErr := errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &se) && (se.Code()&255 == 5 || se.Code()&255 == 9))
	if !lockErr {
		t.Fatalf("fixture invalid: err = %v, want a lock-wait failure", err)
	}
	if elapsed < minWait {
		t.Fatalf("fixture invalid: failed after %v, before the %v it should have waited", elapsed, minWait)
	}
	if elapsed > maxWait {
		t.Fatalf("returned after %v, want under %v", elapsed, maxWait)
	}
}

// Test 1: a short deadline bounds the lock wait on a caller-owned pool whose
// busy_timeout is 5s.
func TestSQLiteFeedbackStoreDeadlineBoundsLockWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.db")
	db := openFeedbackFileDB(t, path, "_pragma=busy_timeout(5000)", 2)
	execFeedbackSQL(t, db, "PRAGMA journal_mode=WAL")
	store := newFileFeedbackStore(t, db)
	release := holdFeedbackLock(t, path, "BEGIN IMMEDIATE")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := store.RecordBatch(ctx, []FeedbackItem{testFeedbackItem("m")})
	elapsed := time.Since(start)
	release()
	t.Logf("ELAPSED deadline-bounds %v", elapsed)
	assertContendedFailure(t, err, elapsed, 150*time.Millisecond, 600*time.Millisecond)
	if n := feedbackRows(t, db); n != 0 {
		t.Fatalf("signals after a failed write = %d, want 0", n)
	}
}

// Test 4: time spent waiting for a pool connection comes out of the budget.
func TestSQLiteFeedbackStoreDeductsPoolWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.db")
	db := openFeedbackFileDB(t, path, "_pragma=busy_timeout(5000)", 1)
	execFeedbackSQL(t, db, "PRAGMA journal_mode=WAL")
	store := newFileFeedbackStore(t, db)
	_ = holdFeedbackLock(t, path, "BEGIN IMMEDIATE")
	pinned, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	done := make(chan error, 1)
	acquired := make(chan struct{})
	item := testFeedbackItem("m")
	go func() {
		done <- store.runInTx(ctx, func(tx *sql.Tx) error {
			if ctx.Err() != nil {
				return errors.New("fixture invalid: context expired before the SQLite write")
			}
			close(acquired)
			return insertSignalTx(ctx, tx, item.Key, item.Signal)
		})
	}()
	// Hold the only connection for 1.2s of the 2s budget, then hand it over.
	// WaitCount counts queued requests, not acquisitions; acquired proves the
	// write really started after the handover.
	waitUntil(t, func() bool { return db.Stats().WaitCount > 0 })
	time.Sleep(1200*time.Millisecond - time.Since(start))
	handedOver := time.Since(start)
	_ = pinned.Close()
	var recErr error
	select {
	case recErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runInTx did not return")
	}
	elapsed := time.Since(start)
	t.Logf("ELAPSED pool-wait %v", elapsed)
	select {
	case <-acquired:
	default:
		t.Fatalf("fixture invalid: returned before acquiring the pool connection: %v", recErr)
	}
	assertContendedFailure(t, recErr, elapsed, handedOver, 2600*time.Millisecond)
}

// Test 5: the cap is refreshed before COMMIT, which in rollback-journal mode
// needs EXCLUSIVE and waits on readers.
func TestSQLiteFeedbackStoreRefreshesCapBeforeCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.db")
	db := openFeedbackFileDB(t, path, "_pragma=busy_timeout(5000)", 1)
	execFeedbackSQL(t, db, "PRAGMA journal_mode=DELETE")
	store := newFileFeedbackStore(t, db)
	release := holdFeedbackLock(t, path, "BEGIN", "SELECT COUNT(*) FROM routing_feedback_signals")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	item := testFeedbackItem("m")
	insertedLive := false
	start := time.Now()
	err := store.runInTx(ctx, func(tx *sql.Tx) error {
		if err := insertSignalTx(ctx, tx, item.Key, item.Signal); err != nil {
			return err
		}
		insertedLive = ctx.Err() == nil
		time.Sleep(1200 * time.Millisecond)
		return nil
	})
	elapsed := time.Since(start)
	release()
	t.Logf("ELAPSED refresh-before-commit %v", elapsed)
	if !insertedLive {
		t.Fatal("fixture invalid: the INSERT did not finish while the deadline was live")
	}
	var se *sqlite.Error
	if !errors.As(err, &se) || se.Code()&255 != 5 {
		t.Fatalf("fixture invalid: err = %v, want SQLITE_BUSY from the contended COMMIT", err)
	}
	if elapsed > 2600*time.Millisecond {
		t.Fatalf("returned after %v, want under 2.6s", elapsed)
	}
	if n := feedbackRows(t, db); n != 0 {
		t.Fatalf("signals after a failed COMMIT = %d, want 0", n)
	}
	if err := store.RecordBatch(t.Context(), []FeedbackItem{testFeedbackItem("after")}); err != nil {
		t.Fatalf("store unusable after a failed COMMIT: %v", err)
	}
}

// Test 3: the cap applies during the write, and the saved busy_timeout comes
// back afterward.
func TestSQLiteFeedbackStoreRestoresBusyTimeout(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		saved, duringMin, duringMax int64
	}{
		{"default", 5000, 1, 1000},
		{"zero", 0, 0, 0},
		{"below deadline", 300, 300, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feedback.db")
			db := openFeedbackFileDB(t, path, fmt.Sprintf("_pragma=busy_timeout(%d)", tc.saved), 2)
			execFeedbackSQL(t, db, "PRAGMA journal_mode=WAL")
			store := newFileFeedbackStore(t, db)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			var during int64
			if err := store.runInTx(ctx, func(tx *sql.Tx) error {
				return tx.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&during)
			}); err != nil {
				t.Fatalf("runInTx: %v", err)
			}
			if during < tc.duringMin || during > tc.duringMax {
				t.Fatalf("busy_timeout during the write = %d, want %d..%d", during, tc.duringMin, tc.duringMax)
			}
			assertBusyTimeouts(t, db, 2, tc.saved)
		})
	}
}

// Test 6: a deadline-bearing write that fails partway leaves no rows and a
// clean, restored connection.
func TestSQLiteFeedbackStoreDeadlineWriteRollsBackPartialBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.db")
	db := openFeedbackFileDB(t, path, "_pragma=busy_timeout(5000)", 2)
	execFeedbackSQL(t, db, "PRAGMA journal_mode=WAL")
	store := newFileFeedbackStore(t, db)
	execFeedbackSQL(t, db, `CREATE TRIGGER reject_boom BEFORE INSERT ON routing_feedback_signals
		WHEN NEW.model = 'boom' BEGIN SELECT RAISE(ABORT, 'boom'); END`)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	// Watchdog: a transaction left open on the pinned Conn would block
	// conn.Close forever instead of failing an assertion.
	batchDone := make(chan error, 1)
	go func() {
		batchDone <- store.RecordBatch(ctx, []FeedbackItem{testFeedbackItem("ok"), testFeedbackItem("boom")})
	}()
	var err error
	select {
	case err = <-batchDone:
	case <-time.After(5 * time.Second):
		t.Fatal("RecordBatch did not return after the trigger abort")
	}
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("RecordBatch err = %v, want the trigger abort", err)
	}

	cctx, ccancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer ccancel()
	first := testFeedbackItem("ok")
	err = store.runInTx(cctx, func(tx *sql.Tx) error {
		if err := insertSignalTx(cctx, tx, first.Key, first.Signal); err != nil {
			return err
		}
		ccancel()
		return insertSignalTx(cctx, tx, first.Key, first.Signal)
	})
	if err == nil {
		t.Fatal("runInTx after cancellation succeeded")
	}

	if n := feedbackRows(t, db); n != 0 {
		t.Fatalf("signals after failed writes = %d, want 0", n)
	}
	assertBusyTimeouts(t, db, 2, 5000)
	if err := store.RecordBatch(t.Context(), []FeedbackItem{testFeedbackItem("ok")}); err != nil {
		t.Fatalf("write after failures: %v", err)
	}
	if n := feedbackRows(t, db); n != 1 {
		t.Fatalf("signals after a good write = %d, want 1", n)
	}
}

// Test 7: an already-cancelled or already-expired context writes nothing.
func TestSQLiteFeedbackStoreRejectsDoneContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.db")
	db := openFeedbackFileDB(t, path, "_pragma=busy_timeout(5000)", 2)
	execFeedbackSQL(t, db, "PRAGMA journal_mode=WAL")
	store := newFileFeedbackStore(t, db)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	for name, ctx := range map[string]context.Context{"cancelled": cancelled, "expired": expired} {
		if err := store.RecordBatch(ctx, []FeedbackItem{testFeedbackItem("m")}); err == nil {
			t.Errorf("%s context: RecordBatch succeeded", name)
		}
	}
	if n := feedbackRows(t, db); n != 0 {
		t.Fatalf("signals after done-context writes = %d, want 0", n)
	}
}
