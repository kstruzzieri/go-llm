package mcpclient

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// countingTransport records dials, so tests can prove nothing was launched.
type countingTransport struct {
	gomcp.Transport
	dials atomic.Int32
}

func (c *countingTransport) Connect(ctx context.Context) (gomcp.Connection, error) {
	c.dials.Add(1)
	return c.Transport.Connect(ctx)
}

func countedServer(t *testing.T, command []string, remote ...*gomcp.Tool) (Server, *countingTransport) {
	t.Helper()
	s, _, _ := staticCatalogServer(t, "fs", remote...)
	counter := &countingTransport{Transport: s.tr}
	s.tr, s.command = counter, command
	return s, counter
}

func connectOnce(t *testing.T, pins *PinStore, require bool, s Server) (*Manager, []error) {
	t.Helper()
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: pins, RequirePinned: require})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, w
}

func TestConnectionChangedBlocksBeforeLaunch(t *testing.T) {
	pins := testPins(t)
	first, _ := countedServer(t, []string{"a"}, tool("read"))
	if m, w := connectOnce(t, pins, false, first); len(m.Tools()) != 1 {
		t.Fatalf("first contact: %v", w)
	}
	before := pinBytes(t, pins, "fs")
	changed, counter := countedServer(t, []string{"b"}, tool("read"))
	m, w := connectOnce(t, pins, false, changed)
	failure := admission(t, w)
	if failure.Reason != "connection_changed" || !slices.Equal(failure.ConnectionChanges, []string{"launcher", "target", "argv"}) {
		t.Fatalf("failure = (%q, %q), want (connection_changed, [launcher target argv])", failure.Reason, failure.ConnectionChanges)
	}
	if counter.dials.Load() != 0 || len(m.Tools()) != 0 || !bytes.Equal(before, pinBytes(t, pins, "fs")) {
		t.Fatalf("changed connection dialed %d times or altered state", counter.dials.Load())
	}
	text := failure.Error()
	if !strings.Contains(text, "connection fields: launcher, target, argv") || strings.Contains(text, "hmac-sha256") ||
		!strings.Contains(text, "review with golem mcp inspect using the same -root, server and -mcp-env arguments and explicit alias=fs, then golem mcp approve with the -digest and -connection it prints") {
		t.Fatalf("Error() = %q", text)
	}
}

func TestV1PinBlocksInEveryModeBeforeLaunch(t *testing.T) {
	for _, require := range []bool{false, true} {
		pins := testPins(t)
		raw := writeV1Pin(t, pins, "fs", pinCatalog(t, "fs", "").toolCatalog)
		s, counter := countedServer(t, nil, tool("read"))
		m, w := connectOnce(t, pins, require, s)
		if failure := admission(t, w); failure.Reason != "connection_missing" || counter.dials.Load() != 0 || len(m.Tools()) != 0 {
			t.Fatalf("require=%t: (%q, %d dials), want connection_missing with no dial", require, failure.Reason, counter.dials.Load())
		}
		if !bytes.Equal(raw, pinBytes(t, pins, "fs")) {
			t.Fatal("v1 record rewritten")
		}
	}
}

func TestHeadlessPinMissingBeforeLaunch(t *testing.T) {
	pins := testPins(t)
	s, counter := countedServer(t, nil, tool("read"))
	_, w := connectOnce(t, pins, true, s)
	if failure := admission(t, w); failure.Reason != "pin_missing" || counter.dials.Load() != 0 || pinBytes(t, pins, "fs") != nil {
		t.Fatalf("(%q, %d dials), want pin_missing with no dial and no pin", failure.Reason, counter.dials.Load())
	}
}

func TestPinChangedBetweenPreflightAndAdmitIsConflict(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, pins *PinStore){
		"replaced": func(t *testing.T, pins *PinStore) {
			entry, rev, err := pins.capturePin(context.Background(), "fs")
			if err == nil {
				err = pins.replacePin(context.Background(), "fs", rev, pinEntry{toolCatalog: entry.toolCatalog, conn: testConnPin()})
			}
			if err != nil {
				t.Error(err)
			}
		},
		"deleted": func(t *testing.T, pins *PinStore) {
			if err := os.Remove(filepath.Join(pins.dir, pinKey("fs")+".json")); err != nil {
				t.Error(err)
			}
		},
		"undecodable": func(t *testing.T, pins *PinStore) {
			if err := os.WriteFile(filepath.Join(pins.dir, pinKey("fs")+".json"), []byte("corrupt"), 0o600); err != nil {
				t.Error(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			pins := testPins(t)
			first, _ := countedServer(t, nil, tool("read"))
			connectOnce(t, pins, false, first)
			s, done, _ := staticCatalogServer(t, "fs", tool("read"))
			hooks := &connectHooks{preflighted: func(string) { change(t, pins) }}
			m, w, err := connectWithHooks(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: pins}, hooks)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.Close() })
			waitOn(t, done, "raced session close")
			if failure := admission(t, w); failure.Reason != "pin_conflict" || len(m.Tools()) != 0 {
				t.Fatalf("%s race = (%q, %d tools), want pin_conflict and nothing published", name, failure.Reason, len(m.Tools()))
			}
			if name == "deleted" && pinBytes(t, pins, "fs") != nil {
				t.Fatal("a pin deleted after preflight was re-created")
			}
		})
	}
}

// Retargeting a launcher symlink or a symlinked working directory changes
// exactly one identity field. Preflight refuses before launch (the ordering
// tests above prove nothing is dialed on connection_changed), so the
// retargeted /bin/sh never runs.
func TestRetargetedLauncherOrDirIsConnectionChanged(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	link := filepath.Join(root, "launcher")
	work := filepath.Join(root, "work")
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	retarget := func(path, to string) {
		_ = os.Remove(path)
		if err := os.Symlink(to, path); err != nil {
			t.Fatal(err)
		}
	}
	retarget(link, exe)
	retarget(work, dirA)
	server := StdioServer("probe", []string{link, "-test.run=^TestMCPClientEnvProbeHelper$", "--", envProbeMarker}).WithDir(work)
	le := testLaunchEnv(nil)
	pins := testPins(t)
	connect := func() (*Manager, []error) {
		m, w, err := connectWithHooks(context.Background(), Implementation{Name: "test"}, []Server{server}, ConnectOptions{Pins: pins}, &connectHooks{launch: &le})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close() })
		return m, w
	}
	if m, w := connect(); len(m.Tools()) != 1 {
		t.Fatalf("first contact: %v", w)
	}
	before := pinBytes(t, pins, "probe")
	for _, tt := range []struct {
		path, to string
		want     []string
	}{
		{link, "/bin/sh", []string{"target"}},
		{work, dirB, []string{"dir"}},
	} {
		retarget(link, exe)
		retarget(work, dirA)
		retarget(tt.path, tt.to)
		_, w := connect()
		if failure := admission(t, w); failure.Reason != "connection_changed" || !slices.Equal(failure.ConnectionChanges, tt.want) {
			t.Fatalf("retarget %s = (%q, %q), want (connection_changed, %q)", tt.path, failure.Reason, failure.ConnectionChanges, tt.want)
		}
	}
	if !bytes.Equal(before, pinBytes(t, pins, "probe")) {
		t.Fatal("a refused connection changed the pin")
	}
}

func TestPersistedPinAfterKeyLossIsConnectionChanged(t *testing.T) {
	ws, base := t.TempDir(), t.TempDir()
	pins, err := newPinStore(ws, base)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := countedServer(t, nil, tool("read"))
	connectOnce(t, pins, false, first)
	before := pinBytes(t, pins, "fs")
	if err := os.Remove(filepath.Join(base, "golem", "mcp-pins", connectionKeyFile)); err != nil {
		t.Fatal(err)
	}
	rekeyed, err := newPinStore(ws, base)
	if err != nil {
		t.Fatal(err)
	}
	again, counter := countedServer(t, nil, tool("read"))
	_, w := connectOnce(t, rekeyed, false, again)
	if failure := admission(t, w); failure.Reason != "connection_changed" || !slices.Equal(failure.ConnectionChanges, []string{"key"}) || counter.dials.Load() != 0 {
		t.Fatalf("after key loss = (%q, %q, %d dials), want (connection_changed, [key], 0)", failure.Reason, failure.ConnectionChanges, counter.dials.Load())
	}
	if !bytes.Equal(before, pinBytes(t, rekeyed, "fs")) {
		t.Fatal("key loss silently re-pinned")
	}
}

func TestConcurrentIdenticalFirstContactsOneConflict(t *testing.T) {
	pins := testPins(t)
	var arrived sync.WaitGroup
	arrived.Add(2)
	release := make(chan struct{})
	hooks := &connectHooks{preflighted: func(string) { arrived.Done(); <-release }}
	type result struct {
		tools int
		warns []error
	}
	results := make(chan result, 2)
	for range 2 {
		s, _, _ := staticCatalogServer(t, "fs", tool("read"))
		go func() {
			m, w, err := connectWithHooks(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: pins}, hooks)
			if err != nil {
				// A fatal error returns no Manager; report it and count neither.
				t.Error(err)
				results <- result{}
				return
			}
			t.Cleanup(func() { _ = m.Close() })
			results <- result{len(m.Tools()), w}
		}()
	}
	arrived.Wait() // both passed preflight against an absent record
	close(release)
	admitted, conflicts := 0, 0
	for range 2 {
		r := waitOn(t, results, "concurrent first contact")
		if r.tools == 1 {
			admitted++
			continue
		}
		var failure *AdmissionError
		for _, w := range r.warns {
			if errors.As(w, &failure) && failure.Reason == "pin_conflict" {
				conflicts++
			}
		}
	}
	if admitted != 1 || conflicts != 1 {
		t.Fatalf("concurrent first contacts = (%d admitted, %d pin_conflict), want exactly one of each (spec §6)", admitted, conflicts)
	}
}
