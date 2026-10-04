package sqlitetest_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/sqlitedsn"
	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
)

// openWAL opens path the way a converted store opener does.
func openWAL(ctx context.Context, path string) (io.Closer, error) {
	dsn, err := sqlitedsn.WithBusyTimeout(path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := sqlitedsn.EnableWAL(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func TestRunConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 3, openWAL)
}

// openWithoutWAL creates the database file and never switches it to WAL.
func openWithoutWAL(ctx context.Context, path string) (io.Closer, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// harnessModeEnv selects which failing harness run a child performs.
const harnessModeEnv = "GO_LLM_SQLITETEST_HARNESS_MODE"

// TestRunConcurrentFirstOpensFails runs the harness in a child process over
// inputs it must reject and requires that child test to fail for the stated
// reason, so the harness's own checks cannot be hollowed out unnoticed.
func TestRunConcurrentFirstOpensFails(t *testing.T) {
	if mode := os.Getenv(harnessModeEnv); mode != "" {
		runFailingHarness(t, mode)
		return
	}
	for _, tc := range []struct{ mode, want string }{
		{"no-wal", `= "delete", want wal`},
		{"skip", "done: false"},
		{"one-proc", "procs=1"},
		{"zero-trials", "trials=0"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunConcurrentFirstOpensFails$", "-test.count=1")
			// No race-detector exit sleep: see startConcurrentChild.
			cmd.Env = append(os.Environ(), harnessModeEnv+"="+tc.mode,
				"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
			out, err := cmd.CombinedOutput()
			if err == nil || !bytes.Contains(out, []byte(tc.want)) {
				t.Fatalf("harness in mode %s: err = %v, want a failure containing %q; output:\n%s", tc.mode, err, tc.want, out)
			}
		})
	}
}

func runFailingHarness(t *testing.T, mode string) {
	switch mode {
	case "no-wal":
		sqlitetest.RunConcurrentFirstOpens(t, 2, 1, openWithoutWAL)
	case "skip":
		sqlitetest.RunConcurrentFirstOpens(t, 2, 1, func(context.Context, string) (io.Closer, error) {
			t.Skip("an opener that skips never finishes an open")
			return nil, nil
		})
	case "one-proc":
		sqlitetest.RunConcurrentFirstOpens(t, 1, 1, openWAL)
	case "zero-trials":
		sqlitetest.RunConcurrentFirstOpens(t, 2, 0, openWAL)
	default:
		t.Fatalf("unknown %s=%q", harnessModeEnv, mode)
	}
}

// A child's stdout line longer than bufio.Scanner's 64 KiB token limit must
// not stop the harness from reading the markers after it.
func TestRunConcurrentFirstOpensLongOutput(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 2, 1, func(ctx context.Context, path string) (io.Closer, error) {
		fmt.Println(strings.Repeat("x", 128<<10))
		return openWAL(ctx, path)
	})
}
