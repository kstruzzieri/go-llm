package sqlitetest

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// AssertWAL fails t unless the database at path is in WAL journal mode. The
// mode is stored in the database header, so a fresh connection reports what
// any opener left behind, whatever type that opener returns. The connection
// waits up to 5s for locks: another process may be recovering the WAL.
func AssertWAL(t testing.TB, path string) {
	t.Helper()
	// A plain path with a query: modernc applies and strips the query.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %q to check journal mode: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal mode of %q: %v", path, err)
	}
	if mode != "wal" {
		t.Fatalf("journal mode of %q = %q, want wal", path, mode)
	}
}

const (
	concurrentOpenPathEnv   = "GO_LLM_SQLITETEST_OPEN_PATH"
	concurrentOpenTrialsEnv = "GO_LLM_SQLITETEST_OPEN_TRIALS"
	concurrentOpenReady     = "GO_LLM_SQLITETEST_READY"
)

// RunConcurrentFirstOpens checks that procs processes opening one new
// database path at the same moment all succeed and leave it in WAL mode, once
// per trial on a fresh path. Call it from a top-level test: the parent re-runs
// the test binary with -test.run=^<t.Name()>$, and in those children it calls
// open, checks the result, closes it, and returns, so the test and any
// TestMain finish normally. Every child must report ready before any is
// released, so a child that never reaches the opener (for example, one whose
// -test.run pattern matches nothing) fails the trial instead of passing.
// GO_LLM_SQLITETEST_OPEN_TRIALS overrides trials, for mutation runs.
func RunConcurrentFirstOpens(t *testing.T, procs, trials int, open func(ctx context.Context, path string) (io.Closer, error)) {
	t.Helper()
	if path := os.Getenv(concurrentOpenPathEnv); path != "" {
		concurrentOpenChild(t, path, open)
		return
	}
	if strings.Contains(t.Name(), "/") {
		t.Fatalf("RunConcurrentFirstOpens needs a top-level test, not %q", t.Name())
	}
	if v := os.Getenv(concurrentOpenTrialsEnv); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("%s=%q: want a positive integer", concurrentOpenTrialsEnv, v)
		}
		trials = n
	}
	for trial := range trials {
		concurrentOpenTrial(t, trial, procs)
	}
}

type concurrentChild struct {
	cmd     *exec.Cmd
	release io.WriteCloser // closing it releases the child
	stderr  bytes.Buffer
	scanned chan struct{} // closed once stdout is fully read
	stdout  strings.Builder
}

func concurrentOpenTrial(t *testing.T, trial, procs int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "concurrent.db")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ready := make(chan bool, procs) // one event per child: ready, or exited first
	children := make([]*concurrentChild, 0, procs)
	reap := func() {
		for _, c := range children {
			_ = c.release.Close()
			<-c.scanned
			_ = c.cmd.Wait()
		}
	}
	for i := range procs {
		c, err := startConcurrentChild(ctx, t.Name(), path, ready)
		if err != nil {
			cancel()
			reap()
			t.Fatalf("trial %d: start process %d: %v", trial, i, err)
		}
		children = append(children, c)
	}
	allReady := true
	for range children {
		if !<-ready {
			allReady = false
			cancel() // kill the rest; their failures are reported below
			break
		}
	}
	// Release every child before waiting on any, so the opens overlap.
	for _, c := range children {
		_ = c.release.Close()
	}
	var failed []string
	for i, c := range children {
		<-c.scanned
		err := c.cmd.Wait()
		if err != nil {
			failed = append(failed, fmt.Sprintf("process %d (%v):\n%s%s", i, err, c.stdout.String(), c.stderr.String()))
		}
	}
	if !allReady || len(failed) > 0 {
		t.Fatalf("trial %d: %d of %d concurrent first opens of %s failed (all ready: %v):\n%s",
			trial, len(failed), procs, path, allReady, strings.Join(failed, "\n"))
	}
}

func startConcurrentChild(ctx context.Context, name, path string, ready chan<- bool) (*concurrentChild, error) {
	c := &concurrentChild{scanned: make(chan struct{})}
	c.cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^"+regexp.QuoteMeta(name)+"$", "-test.count=1")
	c.cmd.Env = append(os.Environ(), concurrentOpenPathEnv+"="+path)
	c.cmd.Stderr = &c.stderr
	var err error
	if c.release, err = c.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		defer close(c.scanned)
		sentReady := false
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			c.stdout.WriteString(line + "\n")
			if line == concurrentOpenReady && !sentReady {
				sentReady = true
				ready <- true
			}
		}
		if !sentReady {
			ready <- false
		}
	}()
	return c, nil
}

func concurrentOpenChild(t *testing.T, path string, open func(ctx context.Context, path string) (io.Closer, error)) {
	t.Helper()
	// Nothing this test starts should inherit the child role.
	_ = os.Unsetenv(concurrentOpenPathEnv)
	fmt.Println(concurrentOpenReady)
	// The parent closes stdin once every child is ready.
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatalf("wait for release: %v", err)
	}
	c, err := open(context.Background(), path)
	if err != nil {
		t.Fatalf("concurrent first open: %v", err)
	}
	closeOnce := sync.OnceValue(c.Close)
	t.Cleanup(func() { _ = closeOnce() })
	AssertWAL(t, path)
	if err := closeOnce(); err != nil {
		t.Fatalf("close after concurrent first open: %v", err)
	}
}
