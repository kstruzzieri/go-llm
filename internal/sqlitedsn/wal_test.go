package sqlitedsn

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// openWithTimeout opens path through WithBusyTimeout on one connection that
// database/sql keeps idle, so a test sees the same physical connection again.
func openWithTimeout(t *testing.T, path string, timeout time.Duration) *sql.DB {
	t.Helper()
	dsn, err := WithBusyTimeout(path, timeout)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// pinnedHolder returns a pinned connection on path with a 5s busy_timeout,
// for lock-holding fixtures. Its DSN is the plain path plus a query, which
// modernc strips and applies, so it does not depend on FileURL.
func pinnedHolder(t *testing.T, path string) *sql.Conn {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func execConn(t *testing.T, conn *sql.Conn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := conn.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("holder %q: %v", s, err)
		}
	}
}

// holdReserved holds RESERVED on path, a fresh rollback-mode database, until
// release is called (or the test ends). It first checks that the lock makes a
// single raw WAL switch fail with SQLITE_BUSY, the race EnableWAL exists for,
// so a fixture that stops producing the race fails instead of passing.
func holdReserved(t *testing.T, path string) (release func()) {
	t.Helper()
	conn := pinnedHolder(t, path)
	execConn(t, conn, "BEGIN IMMEDIATE")
	probe, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	err = probe.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode)
	_ = probe.Close()
	assertBusy(t, err)
	var once sync.Once
	release = func() {
		once.Do(func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") })
	}
	t.Cleanup(release)
	return release
}

func assertBusy(t *testing.T, err error) {
	t.Helper()
	var se *sqlite.Error
	if !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Fatalf("err = %v, want SQLITE_BUSY", err)
	}
}

func journalMode(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return mode
}

func TestEnableWALRetriesRacingSwitch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race.db")
	release := holdReserved(t, path)
	db := openWithTimeout(t, path, 5*time.Second)
	busy := 0
	if err := enableWAL(context.Background(), db, func() { busy++; release() }); err != nil {
		t.Fatalf("enableWAL: %v", err)
	}
	if busy == 0 {
		t.Fatal("enableWAL never saw SQLITE_BUSY; the fixture no longer races")
	}
	if got := journalMode(t, path); got != "wal" {
		t.Fatalf("journal_mode = %q, want wal", got)
	}
}

func TestEnableWALStopsAtBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")
	holdReserved(t, path)
	db := openWithTimeout(t, path, 200*time.Millisecond)
	// The context outlives the budget: a loop that restarts its deadline
	// returns the context's error instead of SQLITE_BUSY.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := EnableWAL(ctx, db)
	elapsed := time.Since(start)
	assertBusy(t, err)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("gave up after %v, want retries for the 200ms budget", elapsed)
	}
}

// Integration check for the per-attempt cap; TestSetBusyTimeoutCap is the
// deterministic proof. A RESERVED holder makes every attempt fail at once, so
// SQLite's own busy handler never runs. Here the holder commits at ~500ms in
// EXCLUSIVE locking mode and keeps the lock, so the attempt that starts then
// waits inside SQLite. Capped, the call ends near the 1s budget; uncapped,
// that attempt waits a full second from ~500ms.
func TestEnableWALCapsLockWaitsToBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cap.db")
	holder := pinnedHolder(t, path)
	execConn(t, holder, "PRAGMA locking_mode=EXCLUSIVE", "BEGIN IMMEDIATE", "CREATE TABLE t (x)")
	committed := make(chan error, 1)
	time.AfterFunc(500*time.Millisecond, func() {
		_, err := holder.ExecContext(context.Background(), "COMMIT")
		committed <- err
	})
	db := openWithTimeout(t, path, time.Second)
	start := time.Now()
	err := EnableWAL(context.Background(), db)
	elapsed := time.Since(start)
	if cerr := <-committed; cerr != nil {
		t.Fatalf("holder COMMIT: %v", cerr)
	}
	assertBusy(t, err)
	t.Logf("elapsed %v", elapsed)
	if elapsed > budgetSlack(time.Second) {
		t.Fatalf("returned after %v; lock waits overran the 1s budget", elapsed)
	}
}

// Integration check for refreshing the cap between prepare and execute;
// TestWALAttemptRefreshesCapBeforeExecute is the deterministic proof.
// Preparing journal_mode loads the schema, which waits while the holder is
// EXCLUSIVE; executing then waits for the commit's EXCLUSIVE lock while the
// holder keeps SHARED through an open read cursor. Without a fresh cap before
// executing, one attempt spends the remaining budget twice.
func TestEnableWALRefreshesCapBeforeExecute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refresh.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("CREATE TABLE t (x); INSERT INTO t VALUES (1), (2)"); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()
	holder := pinnedHolder(t, path)
	execConn(t, holder, "BEGIN EXCLUSIVE")
	// A read cursor left open keeps SHARED after the COMMIT ends the write
	// transaction, so the lock never drops between the two phases.
	rows, err := holder.QueryContext(context.Background(), "SELECT x FROM t")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rows.Close() })
	if !rows.Next() {
		t.Fatalf("holder cursor: %v", rows.Err())
	}
	committed := make(chan error, 1)
	time.AfterFunc(500*time.Millisecond, func() {
		_, err := holder.ExecContext(context.Background(), "COMMIT")
		committed <- err
	})
	db := openWithTimeout(t, path, time.Second)
	start := time.Now()
	err = EnableWAL(context.Background(), db)
	elapsed := time.Since(start)
	if cerr := <-committed; cerr != nil {
		t.Fatalf("holder COMMIT: %v", cerr)
	}
	assertBusy(t, err)
	t.Logf("elapsed %v", elapsed)
	if elapsed > budgetSlack(time.Second) {
		t.Fatalf("returned after %v; prepare and execute overran the 1s budget", elapsed)
	}
}

// TestSetBusyTimeoutCap pins the cap: the budget remaining in whole
// milliseconds, clamped to [0, saved], read back from the connection.
func TestSetBusyTimeoutCap(t *testing.T) {
	db := openWithTimeout(t, filepath.Join(t.TempDir(), "cap.db"), 300*time.Millisecond)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, tc := range []struct {
		remaining time.Duration
		want      int64
	}{
		{120 * time.Millisecond, 120},
		{1500 * time.Microsecond, 1},
		{500 * time.Microsecond, 0},
		{-5 * time.Millisecond, 0},
		{10 * time.Second, 300},
	} {
		if err := setBusyTimeoutCap(conn, 300, tc.remaining); err != nil {
			t.Fatal(err)
		}
		var got int64
		if err := conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("setBusyTimeoutCap(saved 300, remaining %v) set %dms, want %dms", tc.remaining, got, tc.want)
		}
	}
}

// walAttempt must cap before preparing and again before executing. A capBusy
// that fails on its second call proves the second cap runs after the prepare
// and before the switch executes: the database is still in rollback mode.
func TestWALAttemptRefreshesCapBeforeExecute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempt.db")
	db := openWithTimeout(t, path, time.Second)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	second := errors.New("second cap")
	calls := 0
	_, err = walAttempt(context.Background(), conn, func() error {
		calls++
		if calls == 2 {
			return second
		}
		return nil
	})
	if !errors.Is(err, second) || calls != 2 {
		t.Fatalf("walAttempt = %v after %d capBusy calls, want the second call's error", err, calls)
	}
	if got := journalMode(t, path); got != "delete" {
		t.Fatalf("journal_mode = %q after a failed second cap, want delete (switch must not run)", got)
	}
}

// budgetSlack is the latest a capped call may return in the integration
// checks: the budget plus a margin for scheduling, SQLite's backoff
// granularity and -race overhead. Uncapped variants overrun by the 500ms
// holder phase.
func budgetSlack(budget time.Duration) time.Duration { return budget + 250*time.Millisecond }

func TestEnableWALReturnsWhenCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancel.db")
	holdReserved(t, path)
	db := openWithTimeout(t, path, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	err := EnableWAL(ctx, db)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("returned %v after cancellation, want promptly", elapsed)
	}
}

func TestEnableWALZeroTimeoutTriesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.db")
	holdReserved(t, path)
	db := openWithTimeout(t, path, 0)
	busy := 0
	err := enableWAL(context.Background(), db, func() { busy++ })
	assertBusy(t, err)
	if busy != 0 {
		t.Fatalf("retried %d times with busy_timeout 0, want one attempt", busy)
	}
}

func TestEnableWALReturnsOtherErrorsAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ro.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("CREATE TABLE t (x)"); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()
	u, err := FileURL(path)
	if err != nil {
		t.Fatal(err)
	}
	u.RawQuery = "mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	busy := 0
	start := time.Now()
	err = enableWAL(context.Background(), db, func() { busy++ })
	var se *sqlite.Error
	if !errors.As(err, &se) || se.Code()&0xff == sqlite3.SQLITE_BUSY {
		t.Fatalf("err = %v, want a non-BUSY SQLite error", err)
	}
	t.Logf("non-BUSY code %d: %v", se.Code(), err)
	if busy != 0 || time.Since(start) > time.Second {
		t.Fatalf("retried a non-BUSY error (%d retries, %v)", busy, time.Since(start))
	}
}

func TestEnableWALModes(t *testing.T) {
	ctx := context.Background()
	t.Run("already wal", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "wal.db")
		db := openWithTimeout(t, path, time.Second)
		for i := range 2 {
			if err := EnableWAL(ctx, db); err != nil {
				t.Fatalf("EnableWAL call %d: %v", i+1, err)
			}
		}
	})
	t.Run("memory", func(t *testing.T) {
		db := openWithTimeout(t, ":memory:", time.Second)
		if err := EnableWAL(ctx, db); err != nil {
			t.Fatalf("EnableWAL(:memory:) = %v, want nil", err)
		}
	})
	t.Run("temporary database reports delete", func(t *testing.T) {
		db := openWithTimeout(t, "", time.Second)
		err := EnableWAL(ctx, db)
		if err == nil || !strings.Contains(err.Error(), `"delete"`) {
			t.Fatalf(`EnableWAL("") = %v, want a journal_mode "delete" error`, err)
		}
	})
}

func TestEnableWALRestoresBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restore.db")
	release := holdReserved(t, path)
	db := openWithTimeout(t, path, 300*time.Millisecond)
	// Let budget pass before the retry, so the cap drops below the saved value.
	err := enableWAL(context.Background(), db, func() { time.Sleep(20 * time.Millisecond); release() })
	if err != nil {
		t.Fatalf("enableWAL: %v", err)
	}
	// One kept-idle connection: this reads the connection enableWAL used.
	var got int64
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 300 {
		t.Fatalf("busy_timeout after enableWAL = %dms, want the saved 300ms", got)
	}
}
