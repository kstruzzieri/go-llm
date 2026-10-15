package provider

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/kstruzzieri/go-llm/internal/sqlitedsn"
)

// openFeedbackFileDB opens a caller-owned file database for the deadline tests.
func openFeedbackFileDB(t *testing.T, path, query string, maxConns int) *sql.DB {
	t.Helper()
	u, err := sqlitedsn.FileURL(path) // builds a URI SQLite accepts on Windows too
	if err != nil {
		t.Fatal(err)
	}
	u.RawQuery = query
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

// markConn creates a TEMP table on the (single) pooled connection. It survives
// only while that physical connection is reused rather than discarded.
func markConn(t *testing.T, db *sql.DB) {
	t.Helper()
	execFeedbackSQL(t, db, "CREATE TEMP TABLE conn_marker(x)")
}

func connMarked(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM temp.conn_marker").Scan(&n)
	if err != nil && !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("probe marker: %v", err)
	}
	return err == nil
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

// recordedCaps installs store.capHook and returns, per phase and in order, the
// busy_timeout SQLite reports on the write's own connection right after the
// store capped it: ground truth for the cap in effect at BEGIN and at COMMIT,
// with no wall clock involved (#693). The map is written from the write's
// goroutine, so read it only after the write has returned, and record one
// deadline-bearing write per store.
func recordedCaps(t *testing.T, store *SQLiteFeedbackStore) map[capPhase][]int64 {
	t.Helper()
	caps := map[capPhase][]int64{}
	store.capHook = func(phase capPhase, e sqlExecer) {
		var got int64
		// Background, not the write's ctx: an interrupt raised here by an
		// expiring ctx would stay on the pinned connection (see runInTxBefore).
		if err := e.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&got); err != nil {
			t.Errorf("read busy_timeout %s: %v", phase, err)
			return
		}
		caps[phase] = append(caps[phase], got)
	}
	return caps
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
// (SQLITE_BUSY, an interrupt, or the deadline) and elapsed lies within
// [minWait, maxWait]. An unrelated error is an invalid fixture; an early return
// means the write gave up before its budget ran out.
func assertContendedFailure(t *testing.T, err error, elapsed, minWait, maxWait time.Duration) {
	t.Helper()
	var se *sqlite.Error
	lockErr := errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &se) && (se.Code()&255 == 5 || se.Code()&255 == 9))
	if !lockErr {
		t.Fatalf("fixture invalid: err = %v, want a lock-wait failure", err)
	}
	if elapsed < minWait {
		t.Fatalf("returned after %v, before %v: the write did not wait out its budget", elapsed, minWait)
	}
	if elapsed > maxWait {
		t.Fatalf("returned after %v, want under %v", elapsed, maxWait)
	}
}

// Test 1: a short deadline bounds the lock wait on a caller-owned pool whose
// busy_timeout is 5s. Under _txlock=immediate, BEGIN itself takes the write
// lock, so the cap must be in place before BEGIN, not just before the first
// write. The cap read back before BEGIN is at most the deadline; the wall
// clock keeps a 2s ceiling for a cap that lands on another connection.
func TestSQLiteFeedbackStoreDeadlineBoundsLockWait(t *testing.T) {
	for _, tc := range []struct{ name, query, phase string }{
		{"deferred", "_pragma=busy_timeout(5000)", "insert signal"},
		{"txlock-immediate", "_pragma=busy_timeout(5000)&_txlock=immediate", "begin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feedback.db")
			db := openFeedbackFileDB(t, path, tc.query, 2)
			execFeedbackSQL(t, db, "PRAGMA journal_mode=WAL")
			store := newFileFeedbackStore(t, db)
			caps := recordedCaps(t, store)
			release := holdFeedbackLock(t, path, "BEGIN IMMEDIATE")
			start := time.Now()
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			err := store.RecordBatch(ctx, []FeedbackItem{testFeedbackItem("m")})
			elapsed := time.Since(start)
			release()
			t.Logf("ELAPSED deadline-bounds/%s %v caps %v", tc.name, elapsed, caps)
			if err != nil && strings.Contains(err.Error(), "cap busy_timeout") {
				t.Skipf("fixture invalid: stalled past the deadline before the first cap: %v", err)
			}
			if err == nil || !strings.Contains(err.Error(), tc.phase) {
				t.Fatalf("fixture invalid: err = %v, want the lock wait in %q", err, tc.phase)
			}
			if got := caps[capBeforeBegin]; len(got) != 1 || got[0] > 200 {
				t.Fatalf("busy_timeout %s = %v ms, want one cap of at most the 200ms deadline", capBeforeBegin, got)
			}
			// 100ms: the write waited out its budget. 2s is a gross ceiling
			// (#693) that still catches a cap applied through another pooled
			// connection: it reads back correctly through the hook while this
			// connection waits the DSN's 5s. A cap moved after BEGIN never
			// reaches the hook under _txlock=immediate (BEGIN fails first);
			// the deferred subtest cannot see ordering at all.
			assertContendedFailure(t, err, elapsed, 100*time.Millisecond, 2*time.Second)
			if n := feedbackRows(t, db); n != 0 {
				t.Fatalf("signals after a failed write = %d, want 0", n)
			}
		})
	}
}

// Test 4: time spent waiting for a pool connection comes out of the budget:
// the cap read back after the handover is at most the time that was left at
// the handover.
func TestSQLiteFeedbackStoreDeductsPoolWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.db")
	db := openFeedbackFileDB(t, path, "_pragma=busy_timeout(5000)", 1)
	execFeedbackSQL(t, db, "PRAGMA journal_mode=WAL")
	store := newFileFeedbackStore(t, db)
	caps := recordedCaps(t, store)
	_ = holdFeedbackLock(t, path, "BEGIN IMMEDIATE")
	pinned, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
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
	// The cap goes on after the pool hands this connection over, so the
	// budget left now bounds it from above.
	releaseLeft := time.Until(deadline)
	_ = pinned.Close()
	var recErr error
	select {
	case recErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runInTx did not return")
	}
	elapsed := time.Since(start)
	t.Logf("ELAPSED pool-wait %v caps %v releaseLeft %v", elapsed, caps, releaseLeft)
	select {
	case <-acquired:
	default:
		t.Fatalf("fixture invalid: returned before acquiring the pool connection: %v", recErr)
	}
	// A budget measured before the pool wait would read back near the full
	// 2s; one that deducts it reads back at most what was left at handover.
	if got := caps[capBeforeBegin]; len(got) != 1 || got[0] > releaseLeft.Milliseconds() {
		t.Fatalf("busy_timeout %s = %v ms, want one cap of at most %dms (the budget left when the pool connection was released)", capBeforeBegin, got, releaseLeft.Milliseconds())
	}
	// The write must spend the rest of the budget waiting, not less. 5s is a
	// gross ceiling only (#693): slow lock I/O stretches the wait past the
	// cap, and no cap at all would wait the DSN's 5s on top of the 1.2s hold.
	assertContendedFailure(t, recErr, elapsed, 1500*time.Millisecond, 5*time.Second)
}

// Test 5: the cap is refreshed before COMMIT, which in rollback-journal mode
// needs EXCLUSIVE and waits on readers. The proof is the busy_timeout SQLite
// reports right after each cap: the one before COMMIT must sit at least the
// 1200ms fn slept below the one before BEGIN. Elapsed time is not the proof:
// SQLite charges only its requested sleeps against busy_timeout, so slow lock
// I/O stretches the wait past the cap (3.43s on a hosted Windows runner, #693)
// while the stale-cap defect lands near 3.2s; no ceiling separates them. What
// remains of the wall clock is a lower bound (slow I/O only lengthens a wait)
// and a gross 5s ceiling under the 6.2s a write with no cap at all takes with
// the DSN's 5000ms.
func TestSQLiteFeedbackStoreRefreshesCapBeforeCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.db")
	db := openFeedbackFileDB(t, path, "_pragma=busy_timeout(5000)", 1)
	execFeedbackSQL(t, db, "PRAGMA journal_mode=DELETE")
	store := newFileFeedbackStore(t, db)
	markConn(t, db)
	caps := recordedCaps(t, store)
	release := holdFeedbackLock(t, path, "BEGIN", "SELECT COUNT(*) FROM routing_feedback_signals")
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	item := testFeedbackItem("m")
	insertedLive := false
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
	t.Logf("ELAPSED refresh-before-commit %v caps %v", elapsed, caps)
	if !insertedLive {
		t.Fatal("fixture invalid: the INSERT did not finish while the deadline was live")
	}
	// A deadline that expired inside fn returns the ctx error from the
	// "before commit" check, not a *sqlite.Error, and stops here.
	var se *sqlite.Error
	if !errors.As(err, &se) || se.Code()&255 != 5 {
		t.Fatalf("fixture invalid: err = %v, want SQLITE_BUSY from the contended COMMIT", err)
	}
	if len(caps[capBeforeBegin]) != 1 || len(caps[capBeforeCommit]) != 1 {
		t.Fatalf("caps = %v, want exactly one %s and one %s", caps, capBeforeBegin, capBeforeCommit)
	}
	beginCap, commitCap := caps[capBeforeBegin][0], caps[capBeforeCommit][0]
	if beginCap <= 1200 {
		t.Fatalf("fixture invalid: busy_timeout %s = %dms, want more than the 1200ms fn sleeps", capBeforeBegin, beginCap)
	}
	// fn slept at least 1200ms between the two caps, so the time left, and
	// with it the cap, dropped by at least 1200 whole milliseconds
	// (Milliseconds truncates). A stale cap reads back equal to beginCap.
	if commitCap > beginCap-1200 {
		t.Fatalf("busy_timeout %s = %dms, want at most %dms (%s cap %dms minus the 1200ms spent in fn): the cap was not refreshed", capBeforeCommit, commitCap, beginCap-1200, capBeforeBegin, beginCap)
	}
	if elapsed < 1500*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("returned after %v, want 1.5s..5s (COMMIT waits out the rest of the budget; no cap at all would wait the DSN's 5s)", elapsed)
	}
	// modernc rolled the failed COMMIT back, so the store keeps the
	// connection, and that connection is outside any transaction.
	if !connMarked(t, db) {
		t.Fatal("connection discarded after a failed COMMIT that left it outside a transaction")
	}
	c, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE on the kept connection: %v", err)
	}
	if _, err := c.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatalf("ROLLBACK on the kept connection: %v", err)
	}
	_ = c.Close()
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
// clean, restored connection. The single-connection pool and its TEMP marker
// prove the store kept that connection rather than replacing it.
func TestSQLiteFeedbackStoreDeadlineWriteRollsBackPartialBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.db")
	db := openFeedbackFileDB(t, path, "_pragma=busy_timeout(5000)", 1)
	execFeedbackSQL(t, db, "PRAGMA journal_mode=WAL")
	store := newFileFeedbackStore(t, db)
	execFeedbackSQL(t, db, `CREATE TRIGGER reject_boom BEFORE INSERT ON routing_feedback_signals
		WHEN NEW.model = 'boom' BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	markConn(t, db)

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
	if !connMarked(t, db) {
		t.Fatal("connection discarded after failed writes, want it kept")
	}
	assertBusyTimeouts(t, db, 1, 5000)
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
	// A cancelled ctx that still carries a deadline takes the deadline path.
	cancelledWithDeadline, cancelWithDeadline := context.WithTimeout(t.Context(), time.Hour)
	cancelWithDeadline()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"cancelled", cancelled, context.Canceled},
		{"cancelled with deadline", cancelledWithDeadline, context.Canceled},
		{"expired", expired, context.DeadlineExceeded},
	} {
		if err := store.RecordBatch(tc.ctx, []FeedbackItem{testFeedbackItem("m")}); !errors.Is(err, tc.want) {
			t.Errorf("%s context: RecordBatch err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if n := feedbackRows(t, db); n != 0 {
		t.Fatalf("signals after done-context writes = %d, want 0", n)
	}
}

// SQLite rolls back a transaction itself when a write is interrupted; the
// store must not report that as a failed rollback or discard the connection,
// which for ":memory:" is the whole database (#592).
func TestSQLiteFeedbackStoreInterruptedWriteKeepsDatabase(t *testing.T) {
	store, err := OpenSQLiteFeedbackStore(t.Context(), ":memory:", SQLiteFeedbackStoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// Finite bound: if the interrupt is lost, the write ends in minutes, not a package timeout.
	execFeedbackSQL(t, store.db, `CREATE TRIGGER slow AFTER INSERT ON routing_feedback_signals
		WHEN NEW.model = 'slow' BEGIN
		SELECT COUNT(*) FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 10000000) SELECT x FROM c);
	END`)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err = store.RecordBatch(ctx, []FeedbackItem{testFeedbackItem("slow")})
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "rollback") {
		t.Fatalf("interrupted write err = %v, want only the deadline", err)
	}
	if err := store.RecordBatch(t.Context(), []FeedbackItem{testFeedbackItem("after")}); err != nil {
		t.Fatalf("write after an interrupted write: %v", err)
	}
}

// Cancelling ctx mid-fn must not end the transaction under the store, and the
// store must return the connection (restored) instead of discarding it.
func TestSQLiteFeedbackStoreCancelledWriteKeepsTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.db")
	db := openFeedbackFileDB(t, path, "_pragma=busy_timeout(5000)", 1)
	execFeedbackSQL(t, db, "PRAGMA journal_mode=WAL")
	store := newFileFeedbackStore(t, db)
	markConn(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	item := testFeedbackItem("m")
	err := store.runInTx(ctx, func(tx *sql.Tx) error {
		if err := insertSignalTx(ctx, tx, item.Key, item.Signal); err != nil {
			return err
		}
		cancel()
		// A ctx-bound transaction is rolled back by database/sql shortly
		// after cancel; give that the chance to happen.
		stop := time.Now().Add(100 * time.Millisecond)
		for time.Now().Before(stop) {
			if _, err := tx.ExecContext(context.Background(), "SELECT 1"); err != nil {
				return fmt.Errorf("transaction ended under the store: %w", err)
			}
			time.Sleep(time.Millisecond)
		}
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "busy_timeout") || strings.Contains(err.Error(), "rollback") {
		t.Fatalf("err = %v, want only the cancellation", err)
	}
	if n := feedbackRows(t, db); n != 0 {
		t.Fatalf("signals after a cancelled write = %d, want 0", n)
	}
	if !connMarked(t, db) {
		t.Fatal("connection discarded after a cancelled write")
	}
	assertBusyTimeouts(t, db, 1, 5000)
}

// outsideTx alone tells a connection SQLite already rolled back from one still
// inside a transaction. modernc never leaves the latter after a failed
// rollback or COMMIT, so the store tests cannot reach it; pin the probe here.
func TestOutsideTxReportsTransactionState(t *testing.T) {
	db := openFeedbackFileDB(t, filepath.Join(t.TempDir(), "feedback.db"), "", 1)
	c, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if !outsideTx(c) {
		t.Fatal("autocommit connection reported inside a transaction")
	}
	// The probe must leave no transaction behind.
	if _, err := c.ExecContext(t.Context(), "BEGIN"); err != nil {
		t.Fatalf("BEGIN after the probe: %v", err)
	}
	if outsideTx(c) {
		t.Fatal("open transaction reported as autocommit")
	}
	if _, err := c.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}

// isNoTransactionError drops only SQLite's "no transaction is active" reply
// to ROLLBACK; any other rollback failure must stay in the returned error.
// The cases are real driver errors, not hand-built ones.
func TestIsNoTransactionErrorMatchesOnlyNoTransactionReply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.db")
	db := openFeedbackFileDB(t, path, "_pragma=busy_timeout(0)", 1)
	execFeedbackSQL(t, db, "PRAGMA journal_mode=WAL")

	_, noTx := db.ExecContext(t.Context(), "ROLLBACK")
	if noTx == nil {
		t.Fatal("fixture invalid: ROLLBACK outside a transaction succeeded")
	}
	_ = holdFeedbackLock(t, path, "BEGIN IMMEDIATE")
	_, busy := db.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	var se *sqlite.Error
	if !errors.As(busy, &se) || se.Code()&0xff != 5 {
		t.Fatalf("fixture invalid: contended BEGIN IMMEDIATE err = %v, want SQLITE_BUSY", busy)
	}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no transaction is active", noTx, true},
		{"wrapped no transaction", fmt.Errorf("provider: rollback: %w", noTx), true},
		{"SQLITE_BUSY", busy, false},
		{"non-SQLite error", errors.New("disk on fire"), false},
	} {
		if got := isNoTransactionError(tc.err); got != tc.want {
			t.Errorf("%s: isNoTransactionError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
