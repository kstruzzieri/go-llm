package fingerprint

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/kstruzzieri/go-llm/memory"
)

func xdg(dir string) func(string) string {
	return func(k string) string {
		if k == "XDG_DATA_HOME" {
			return dir
		}
		return ""
	}
}

func openShared(t *testing.T, getenv func(string) string, root string) *SharedStore {
	t.Helper()
	s, err := OpenSharedStore(context.Background(), getenv, root)
	if err != nil {
		t.Fatalf("OpenSharedStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenSharedStore_Misuse(t *testing.T) {
	if _, err := OpenSharedStore(nil, xdg(t.TempDir()), t.TempDir()); err == nil { //nolint:staticcheck // nil ctx is the misuse under test
		t.Error("nil ctx: want error")
	}
	if _, err := OpenSharedStore(context.Background(), nil, t.TempDir()); err == nil {
		t.Error("nil getenv: want error")
	}
}

func TestOpenSharedStore_PathAndModes(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	s := openShared(t, xdg(base), root)
	if s.Status() != StoreFile || s.Warning() != "" {
		t.Fatalf("status %q warning %q, want file and none", s.Status(), s.Warning())
	}
	dbPath := filepath.Join(base, "golem", "fingerprints.db")
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("db not at %s: %v", dbPath, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode %o, want 0600", info.Mode().Perm())
	}
	dir, err := os.Stat(filepath.Dir(dbPath))
	if err != nil || dir.Mode().Perm() != 0o700 {
		t.Errorf("dir %v mode %v, want 0700", err, dir.Mode().Perm())
	}
	if s.Log() == nil || s.Probes() == nil || s.Profiles() == nil {
		t.Error("file-backed store must expose log, probes and profiles")
	}
}

func TestOpenSharedStore_HomeFallback(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	s := openShared(t, func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}, root)
	if s.Status() != StoreFile {
		t.Fatalf("status %q: %s", s.Status(), s.Warning())
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "golem", "fingerprints.db")); err != nil {
		t.Errorf("db not under ~/.local/share/golem: %v", err)
	}
}

func TestOpenSharedStore_InsideWorkspaceFallsToMemory(t *testing.T) {
	root := t.TempDir()
	s := openShared(t, xdg(filepath.Join(root, "data")), root)
	if s.Status() != StoreMemoryOnly {
		t.Fatalf("status %q, want memory-only", s.Status())
	}
	if !strings.HasPrefix(s.Warning(), "fingerprint cache degraded to in-memory: ") {
		t.Errorf("warning %q", s.Warning())
	}
	if _, err := os.Stat(filepath.Join(root, "data")); !os.IsNotExist(err) {
		t.Errorf("nothing may be created inside the workspace, stat err = %v", err)
	}
	// The fallback works for this run.
	ctx := context.Background()
	if err := s.Log().AppendEvidence(ctx, evRow(0, time.Now())); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Log().EvidenceRows(ctx, evEndpoint, evModel)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %d, err %v, want 1", len(rows), err)
	}
}

func TestOpenSharedStore_DisabledWhenMemoryAlsoFails(t *testing.T) {
	old := openMemoryStore
	openMemoryStore = func(context.Context) (*sql.DB, *SQLiteStore, *memoryPin, error) {
		return nil, nil, nil, errors.New("no memory")
	}
	t.Cleanup(func() { openMemoryStore = old })
	root := t.TempDir()
	s := openShared(t, xdg(filepath.Join(root, "data")), root)
	if s.Status() != StoreDisabled {
		t.Fatalf("status %q, want disabled", s.Status())
	}
	// Callers compare to nil: these must be nil interfaces, not typed nil pointers.
	if s.Log() != nil {
		t.Errorf("Log() = %#v, want nil interface", s.Log())
	}
	if s.Probes() != nil {
		t.Errorf("Probes() = %#v, want nil interface", s.Probes())
	}
	st := Store(s.Profiles()) // a host wires it this way: the interface itself must be nil
	if st != nil {
		t.Errorf("Profiles() = %#v, want nil interface", st)
	}
	if !strings.HasPrefix(s.Warning(), "fingerprint cache disabled: ") || !strings.HasSuffix(s.Warning(), "(memory fallback failed: no memory)") {
		t.Errorf("warning %q", s.Warning())
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close on disabled: %v", err)
	}
}

func TestSharedStore_SidecarsResecuredAfterEveryWrite(t *testing.T) {
	base := t.TempDir()
	s := openShared(t, xdg(base), t.TempDir())
	dbPath := filepath.Join(base, "golem", "fingerprints.db")
	ctx := context.Background()
	now := time.Now()
	loosen := func(t *testing.T) {
		t.Helper()
		n := 0
		for _, suffix := range []string{"-wal", "-shm"} {
			if err := os.Chmod(dbPath+suffix, 0o666); err == nil {
				n++
			}
		}
		if n == 0 {
			t.Fatal("no sidecar exists to loosen; the test could not fail")
		}
	}
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"AppendEvidence", func() error { return s.Log().AppendEvidence(ctx, evRow(0, now)) }},
		{"AppendCheckResult", func() error {
			return s.Log().AppendCheckResult(ctx, evEndpoint, "", evModel, []EvidenceRow{evRow(0, now)}, nil)
		}},
		{"PublishListings", func() error {
			return s.Log().PublishListings(ctx, []ListingPublish{{Endpoint: evEndpoint, Adapter: evAdapter, StartedAt: now.Add(-time.Second), Members: []ListingMember{{Model: evModel}}}})
		}},
		{"AddNamedModel", func() error { return s.Log().AddNamedModel(ctx, evEndpoint, "named") }},
		{"RemoveNamedModel", func() error { return s.Log().RemoveNamedModel(ctx, evEndpoint, "named") }},
		{"RecordModelAccess", func() error {
			return s.Log().RecordModelAccess(ctx, evEndpoint, "", evModel, AccessAnswered, 200, now)
		}},
		{"Profiles.Save", func() error {
			return s.Profiles().Save(ctx, Profile{BackendID: "lc", ModelName: "m", ModelDigest: "d", ProfileVersion: CurrentProfileVersion, TestedAt: now})
		}},
		{"Profiles.SaveFailure", func() error { return s.Profiles().SaveFailure(ctx, "lc", "m", "d", "boom") }},
		{"SaveCapProbe", func() error {
			return s.Probes().SaveCapProbe(ctx, CapProbe{BackendID: "lc", ModelName: "m", Capability: "tool_call", State: CapProbeYes, ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion, TestedAt: now})
		}},
		{"DeleteCapProbes", func() error { return s.Probes().DeleteCapProbes(ctx, "lc", "m") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			loosen(t)
			if err := c.call(); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
				info, err := os.Stat(p)
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Errorf("%s mode %o after %s, want 0600", filepath.Base(p), info.Mode().Perm(), c.name)
				}
			}
		})
	}
}

func TestSharedStore_ProbesAndLogShareTheDatabase(t *testing.T) {
	s := openShared(t, xdg(t.TempDir()), t.TempDir())
	ctx := context.Background()
	if err := s.Probes().SaveCapProbe(ctx, CapProbe{BackendID: "lc", ModelName: "m", Capability: "tool_call", State: CapProbeYes, ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion, TestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Log().GetCapProbe(ctx, "lc", "m", "tool_call"); err != nil || got == nil || got.State != CapProbeYes {
		t.Fatalf("legacy row not visible through Log: %v %v", got, err)
	}
}

func TestOpenSharedStore_PrunesOnlyWhenSomethingIsPrunable(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	s := openShared(t, xdg(base), root)
	ctx := context.Background()
	// A row inside the 90-day window: present, but not prunable.
	if err := s.Log().AppendEvidence(ctx, evRow(0, time.Now().AddDate(0, 0, -10), asNo)); err != nil {
		t.Fatal(err)
	}
	gen0, err := s.Log().Generation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Quiet reopen: nothing to prune, so the generation stays put.
	s = openShared(t, xdg(base), root)
	if g, err := s.Log().Generation(ctx); err != nil || g != gen0 {
		t.Fatalf("quiet open moved generation %d -> %d (%v)", gen0, g, err)
	}
	// An old no, shadowed by a newer no of the same partition, is prunable.
	now := time.Now()
	old, mid, newest := now.AddDate(0, 0, -200), now.AddDate(0, 0, -150), now.AddDate(0, 0, -1)
	if err := s.Log().AppendEvidence(ctx,
		evRow(0, old, asNo), evRow(0, mid, asNo), evRow(0, newest, asNo)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openShared(t, xdg(base), root)
	rows, err := s.Log().EvidenceRows(ctx, evEndpoint, evModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) >= 3 {
		t.Fatalf("open did not prune: %d rows remain", len(rows))
	}
}

// helperEnv names the child's inputs; the child is this test binary.
const helperEnv = "GO_LLM_SHARED_STORE_HELPER"

// TestSharedStoreHelper is the child process of TestSharedStore_TwoProcessesAppendAndRead.
// It does nothing in a normal run.
func TestSharedStoreHelper(t *testing.T) {
	dir := os.Getenv(helperEnv)
	if dir == "" {
		t.Skip("child process only")
	}
	s, err := OpenSharedStore(context.Background(), xdg(dir), t.TempDir())
	if err != nil || s.Status() != StoreFile {
		t.Fatalf("child open: %v status %q %s", err, s.Status(), s.Warning())
	}
	defer func() { _ = s.Close() }()
	n, _ := strconv.Atoi(os.Getenv(helperEnv + "_N"))
	for i := 0; i < n; i++ {
		if err := s.Log().AppendEvidence(context.Background(), evRow(0, time.Now())); err != nil {
			t.Fatalf("child append %d: %v", i, err)
		}
		// Read while the other process writes: a BUSY read fails the child.
		if _, err := s.Log().EvidenceRows(context.Background(), evEndpoint, evModel); err != nil {
			t.Fatalf("child read %d: %v", i, err)
		}
	}
}

func TestSharedStore_TwoProcessesAppendAndRead(t *testing.T) {
	base := t.TempDir()
	parent := openShared(t, xdg(base), t.TempDir()) // migrates before the children race
	const perChild = 200
	var wg sync.WaitGroup
	outs := make([]string, 2)
	errs := make([]error, 2)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestSharedStoreHelper$", "-test.v")
			cmd.Env = append(os.Environ(), helperEnv+"="+base, helperEnv+"_N="+strconv.Itoa(perChild))
			out, err := cmd.CombinedOutput()
			outs[i], errs[i] = string(out), err
		}()
	}
	wg.Wait()
	for i := range outs {
		if errs[i] != nil || strings.Contains(outs[i], "SKIP") {
			t.Fatalf("child %d: %v\n%s", i, errs[i], outs[i])
		}
	}
	rows, err := parent.Log().EvidenceRows(context.Background(), evEndpoint, evModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2*perChild {
		t.Fatalf("parent sees %d rows, want %d", len(rows), 2*perChild)
	}
}

func TestSharedStore_ContentionBeyondTimeoutReturnsLockError(t *testing.T) {
	base := t.TempDir()
	s := openShared(t, xdg(base), t.TempDir())
	dbPath := filepath.Join(base, "golem", "fingerprints.db")
	holderDB, err := memory.OpenHardenedDB(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holderDB.Close() }()
	tx, err := holderDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE store_meta SET generation = generation`); err != nil { // takes the write lock
		t.Fatal(err)
	}
	start := time.Now()
	err = s.Log().AppendEvidence(context.Background(), evRow(0, time.Now()))
	waited := time.Since(start)
	if err == nil {
		t.Fatal("append succeeded while another connection held the write lock")
	}
	if !strings.Contains(err.Error(), "locked") && !strings.Contains(err.Error(), "BUSY") {
		t.Errorf("error %q is not a lock error", err)
	}
	if waited < 4*time.Second {
		t.Errorf("gave up after %v, want the 5s busy timeout", waited)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := s.Log().AppendEvidence(context.Background(), evRow(0, time.Now())); err != nil {
		t.Errorf("append after the holder released: %v", err)
	}
}

// sharedMemoryStore opens a store whose data directory is inside the workspace,
// the documented way to reach the in-memory fallback.
func sharedMemoryStore(t *testing.T) *SharedStore {
	t.Helper()
	root := t.TempDir()
	s := openShared(t, xdg(filepath.Join(root, "data")), root)
	if s.Status() != StoreMemoryOnly {
		t.Fatalf("status %q, want memory-only", s.Status())
	}
	return s
}

// A cancel during a read outside a transaction makes database/sql discard the
// connection. The replacement must find the same database.
func TestOpenSharedStore_MemoryOnlySurvivesCancelledRead(t *testing.T) {
	s := sharedMemoryStore(t)
	ctx := context.Background()
	now := time.Now()
	const rowCount = 50
	if err := s.Log().AppendEvidence(ctx, slices.Repeat([]EvidenceRow{evRow(0, now)}, rowCount)...); err != nil {
		t.Fatal(err)
	}
	if err := s.Log().AddNamedModel(ctx, evEndpoint, "kept"); err != nil {
		t.Fatal(err)
	}

	// A scan that never finds its first row by itself, so the cancel always lands
	// inside the driver's step, which is what interrupts the connection.
	const endless = `WITH RECURSIVE c(x) AS (SELECT id FROM capability_evidence UNION ALL SELECT x FROM c) SELECT count(*) FROM c`
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := time.AfterFunc(100*time.Millisecond, cancel)
	defer stop.Stop()
	var scanned int64
	err := s.db.QueryRowContext(cctx, endless).Scan(&scanned)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan = %d, %v; want it cancelled mid-read", scanned, err)
	}

	named, err := s.Log().NamedModels(ctx, evEndpoint)
	if err != nil || len(named) != 1 || named[0].Model != "kept" {
		t.Fatalf("named after the cancel = %v, %v", named, err)
	}
	got, err := s.Log().EvidenceRows(ctx, evEndpoint, evModel)
	if err != nil || len(got) != rowCount {
		t.Fatalf("rows after the cancel = %d, %v; want %d", len(got), err, rowCount)
	}
	if err := s.Log().AppendEvidence(ctx, evRow(0, now)); err != nil {
		t.Fatalf("write after the cancel: %v", err)
	}
	if got, err := s.Log().EvidenceRows(ctx, evEndpoint, evModel); err != nil || len(got) != rowCount+1 {
		t.Fatalf("rows after the write = %d, %v; want %d", len(got), err, rowCount+1)
	}
}

func TestOpenSharedStore_MemoryOnlyStoresArePrivate(t *testing.T) {
	a, b := sharedMemoryStore(t), sharedMemoryStore(t)
	ctx := context.Background()
	if err := a.Log().AddNamedModel(ctx, evEndpoint, "only-in-a"); err != nil {
		t.Fatal(err)
	}
	named, err := b.Log().NamedModels(ctx, evEndpoint)
	if err != nil || len(named) != 0 {
		t.Fatalf("second store sees %v, %v; want nothing", named, err)
	}
}

// damageMiddlePages overwrites the middle third of a closed database, leaving the
// header, schema and store_meta at the front of the file readable.
func damageMiddlePages(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xFF}, int(info.Size()/3)), info.Size()/3); err != nil {
		t.Fatal(err)
	}
}

func TestOpenSharedStore_DamagedStoreWarns(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	s := openShared(t, xdg(base), root)
	ctx := context.Background()
	row := evRow(0, time.Now(), func(r *EvidenceRow) { r.Detail = strings.Repeat("x", 512) })
	if err := s.Log().AppendEvidence(ctx, slices.Repeat([]EvidenceRow{row}, 2000)...); err != nil {
		t.Fatal(err)
	}
	if s.Warning() != "" {
		t.Fatalf("healthy store warns: %q", s.Warning())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	damageMiddlePages(t, filepath.Join(base, "golem", "fingerprints.db"))

	s = openShared(t, xdg(base), root)
	if s.Status() != StoreFile {
		t.Fatalf("status %q (%s), want file: only the prune sees the damage", s.Status(), s.Warning())
	}
	const sentence = "fingerprint cache could not be pruned and may be damaged: "
	if !strings.HasPrefix(s.Warning(), sentence) || !strings.Contains(s.Warning(), "malformed") {
		t.Fatalf("warning %q, want %q and the error", s.Warning(), sentence)
	}
}

func TestOpenSharedStore_BusyPruneStaysSilent(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	s := openShared(t, xdg(base), root)
	ctx := context.Background()
	now := time.Now()
	if err := s.Log().AppendEvidence(ctx, evRow(0, now.AddDate(0, 0, -200), asNo), evRow(0, now.AddDate(0, 0, -1), asNo)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	holder, err := memory.OpenHardenedDB(ctx, filepath.Join(base, "golem", "fingerprints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	tx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE store_meta SET generation = generation`); err != nil {
		t.Fatal(err)
	}

	s = openShared(t, xdg(base), root)
	if s.Status() != StoreFile || s.Warning() != "" {
		t.Fatalf("status %q warning %q, want file and none", s.Status(), s.Warning())
	}
	rows, err := s.Log().EvidenceRows(ctx, evEndpoint, evModel)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %d, %v; want both: the prune must have failed busy", len(rows), err)
	}
}

// An empty workspace root is no workspace: the current directory is not one.
func TestOpenSharedStore_EmptyRootMeansNoWorkspace(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	data := filepath.Join(cwd, "data")
	s := openShared(t, xdg(data), "")
	if s.Status() != StoreFile || s.Warning() != "" {
		t.Fatalf("empty root: status %q warning %q, want file and none", s.Status(), s.Warning())
	}
	if _, err := os.Stat(filepath.Join(data, "golem", "fingerprints.db")); err != nil {
		t.Fatalf("db under the current directory: %v", err)
	}
	if s := openShared(t, xdg(data), cwd); s.Status() != StoreMemoryOnly {
		t.Fatalf("root holding the data dir: status %q, want memory-only", s.Status())
	}
}

// Close releases the pinned connection as well, and a second Close is still fine.
func TestOpenSharedStore_MemoryOnlyCloseReleasesPin(t *testing.T) {
	s := sharedMemoryStore(t)
	ctx := context.Background()
	if err := s.pin.conn.PingContext(ctx); err != nil {
		t.Fatalf("pinned connection before Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.pin.conn.PingContext(ctx); !errors.Is(err, sql.ErrConnDone) {
		t.Errorf("pinned connection after Close: %v, want %v", err, sql.ErrConnDone)
	}
	if err := s.pin.db.PingContext(ctx); err == nil {
		t.Error("pin handle still open after Close")
	}
	if err := s.db.PingContext(ctx); err == nil {
		t.Error("store handle still open after Close")
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// lockedError returns a real SQLITE_LOCKED: dropping a table the same connection
// is still reading.
func lockedError(t *testing.T) error {
	t.Helper()
	ctx := t.Context()
	conn, err := openTestDB(t).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE x (v INTEGER); INSERT INTO x VALUES (1), (2)`); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(ctx, `SELECT v FROM x`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("no first row: %v", rows.Err())
	}
	_, err = conn.ExecContext(ctx, `DROP TABLE x`)
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_LOCKED {
		t.Fatalf("err = %v, want SQLITE_LOCKED", err)
	}
	return err
}

func TestPruneWarning(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	failure := errors.New("database disk image is malformed")
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"no failure", context.Background(), nil, ""},
		{"cancelled open, whatever the driver reports", cancelled, errors.New("interrupted"), ""},
		{"locked store", context.Background(), fmt.Errorf("fingerprint: prune: %w", lockedError(t)), ""},
		{"other failure", context.Background(), failure, "fingerprint cache could not be pruned and may be damaged: database disk image is malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pruneWarning(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("pruneWarning = %q, want %q", got, tc.want)
			}
		})
	}
}
