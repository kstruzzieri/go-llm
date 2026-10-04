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

// reviewHintFS is the review hint for alias fs. pin_missing and
// connection_missing carry no candidate digest, so only the reason selects it.
const reviewHintFS = "review with golem mcp inspect using the same -root, server and -mcp-env arguments and explicit alias=fs, then golem mcp approve with the -digest and -connection it prints"

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
	m, w := connectOnce(t, pins, false, first)
	if len(m.Tools()) != 1 {
		t.Fatalf("first contact: %v", w)
	}
	pinned, _, err := pins.capturePin(context.Background(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	if want := "server \"fs\": first pin " + pinned.digest() + "; connection pinned; tools: mcp__fs__read; use explicit alias= values for stable pins"; len(w) != 1 || w[0].Error() != want {
		t.Fatalf("first contact notices = %v, want exactly %q", w, want)
	}
	before := pinBytes(t, pins, "fs")
	changed, counter := countedServer(t, []string{"b"}, tool("read"))
	m, w = connectOnce(t, pins, false, changed)
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
		failure := admission(t, w)
		if failure.Reason != "connection_missing" || counter.dials.Load() != 0 || len(m.Tools()) != 0 {
			t.Fatalf("require=%t: (%q, %d dials), want connection_missing with no dial", require, failure.Reason, counter.dials.Load())
		}
		if text := failure.Error(); !strings.HasSuffix(text, "; "+reviewHintFS) {
			t.Fatalf("require=%t: Error() = %q, want the review hint (spec §5.9)", require, text)
		}
		if !bytes.Equal(raw, pinBytes(t, pins, "fs")) {
			t.Fatal("v1 record rewritten")
		}
	}
}

// A record that cannot be read is no evidence of a launch: preflight refuses it
// in every mode instead of treating it as absent.
func TestUnreadablePinBlocksBeforeLaunch(t *testing.T) {
	for _, require := range []bool{false, true} {
		pins := testPins(t)
		corrupt := []byte("corrupt")
		if err := os.WriteFile(filepath.Join(pins.dir, pinKey("fs")+".json"), corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		s, counter := countedServer(t, nil, tool("read"))
		m, w := connectOnce(t, pins, require, s)
		if failure := admission(t, w); failure.Reason != "pin_unavailable" || counter.dials.Load() != 0 || len(m.Tools()) != 0 {
			t.Fatalf("require=%t: (%q, %d dials), want pin_unavailable with no dial", require, failure.Reason, counter.dials.Load())
		}
		if !bytes.Equal(corrupt, pinBytes(t, pins, "fs")) {
			t.Fatalf("require=%t: unreadable record rewritten", require)
		}
	}
}

func TestHeadlessPinMissingBeforeLaunch(t *testing.T) {
	pins := testPins(t)
	s, counter := countedServer(t, nil, tool("read"))
	_, w := connectOnce(t, pins, true, s)
	failure := admission(t, w)
	if failure.Reason != "pin_missing" || counter.dials.Load() != 0 || pinBytes(t, pins, "fs") != nil {
		t.Fatalf("(%q, %d dials), want pin_missing with no dial and no pin", failure.Reason, counter.dials.Load())
	}
	if text := failure.Error(); !strings.HasSuffix(text, "; "+reviewHintFS) {
		t.Fatalf("Error() = %q, want the review hint (spec §5.9)", text)
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
			failure := admission(t, w)
			if failure.Reason != "pin_conflict" || len(m.Tools()) != 0 {
				t.Fatalf("%s race = (%q, %d tools), want pin_conflict and nothing published", name, failure.Reason, len(m.Tools()))
			}
			// A conflict loads no record to compare, so it reports no digests,
			// no diff and no review hint (a false "added: <every tool>" otherwise).
			if text := failure.Error(); failure.PinnedDigest != "" || failure.CandidateDigest != "" || failure.Diff.String() != "" ||
				strings.Contains(text, "added:") || strings.Contains(text, "review with") {
				t.Fatalf("%s race Error() = %q, want no digests, diff or review hint", name, text)
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
		failure := admission(t, w)
		if failure.Reason != "connection_changed" || !slices.Equal(failure.ConnectionChanges, tt.want) {
			t.Fatalf("retarget %s = (%q, %q), want (connection_changed, %q)", tt.path, failure.Reason, failure.ConnectionChanges, tt.want)
		}
		if text := failure.Error(); !strings.HasSuffix(text, "; review with golem mcp inspect using the same -root, server and -mcp-env arguments and explicit alias=probe, then golem mcp approve with the -digest and -connection it prints") {
			t.Fatalf("retarget %s Error() = %q, want the hint for alias probe", tt.path, text)
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

// The change labels name fields, never their values: a changed identity full
// of canaries renders none of them and no fingerprint.
func TestConnectionChangedNeverEchoesIdentity(t *testing.T) {
	pins := testPins(t)
	first, _ := countedServer(t, []string{"a"}, tool("read"))
	if m, w := connectOnce(t, pins, false, first); len(m.Tools()) != 1 {
		t.Fatalf("first contact: %v", w)
	}
	changed, counter := countedServer(t, []string{"/canary-launcher", "--token=canary-argv"}, tool("read"))
	changed = changed.WithDir("/canary-dir").WithEnv(SetEnv("X", "canary-env"))
	_, w := connectOnce(t, pins, false, changed)
	failure := admission(t, w)
	if failure.Reason != "connection_changed" || counter.dials.Load() != 0 {
		t.Fatalf("(%q, %d dials), want connection_changed with no dial", failure.Reason, counter.dials.Load())
	}
	if text := failure.Error(); !strings.Contains(text, "connection fields: ") || strings.Contains(text, "canary") || strings.Contains(text, "hmac-sha256") {
		t.Fatalf("Error() = %q, want change labels without identity values or fingerprints", text)
	}
}

// Change labels belong to connection_changed alone: a cause that also carries
// a higher-priority condition reports that reason without them.
func TestAdmissionFailureLabelsOnlyConnectionChanged(t *testing.T) {
	changed := &connectionChangedError{labels: []string{"argv"}}
	if failure := admissionFailure("fs", "pin_unavailable", errors.Join(changed, context.Canceled)); failure.Reason != "canceled" || failure.ConnectionChanges != nil {
		t.Fatalf("canceled = (%q, %q), want (canceled, nil)", failure.Reason, failure.ConnectionChanges)
	}
	if failure := admissionFailure("fs", "pin_unavailable", changed); failure.Reason != "connection_changed" || !slices.Equal(failure.ConnectionChanges, []string{"argv"}) {
		t.Fatalf("changed = (%q, %q), want (connection_changed, [argv])", failure.Reason, failure.ConnectionChanges)
	}
}
