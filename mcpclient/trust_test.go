package mcpclient

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func testPins(t *testing.T) *PinStore {
	t.Helper()
	s, err := newPinStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The fixture crosses the SDK's JSON transport. Middleware substitutes only
// remote tools/list responses; Connect, decoding and session close are real.
func catalogServer(t *testing.T, alias string, list func(context.Context, *gomcp.ListToolsParams) (*gomcp.ListToolsResult, error)) (Server, <-chan struct{}, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "catalog-test"}, nil)
	srv.AddReceivingMiddleware(func(next gomcp.MethodHandler) gomcp.MethodHandler {
		return func(ctx context.Context, method string, req gomcp.Request) (gomcp.Result, error) {
			if method == "tools/call" {
				calls.Add(1)
			}
			if method == "tools/list" {
				return list(ctx, req.GetParams().(*gomcp.ListToolsParams))
			}
			return next(ctx, method, req)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	st, ct := gomcp.NewInMemoryTransports()
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Run(ctx, st) }()
	t.Cleanup(func() { cancel(); waitOn(t, done, "server cleanup") })
	return Server{Alias: alias, tr: ct}, done, calls
}
func staticCatalogServer(t *testing.T, alias string, remote ...*gomcp.Tool) (Server, <-chan struct{}, *atomic.Int32) {
	return catalogServer(t, alias, func(context.Context, *gomcp.ListToolsParams) (*gomcp.ListToolsResult, error) {
		return &gomcp.ListToolsResult{Tools: remote}, nil
	})
}
func pinBytes(t *testing.T, s *PinStore, alias string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(s.dir, pinKey(alias)+".json"))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func connectCatalog(t *testing.T, pins *PinStore, require bool, remote ...*gomcp.Tool) (*Manager, []error) {
	t.Helper()
	s, done, _ := staticCatalogServer(t, "fs", remote...)
	m, w, e := Connect(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: pins, RequirePinned: require})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = m.Close() })
	if len(m.sessions) == 0 {
		waitOn(t, done, "rejected session close")
	}
	return m, w
}
func admission(t *testing.T, w []error) *AdmissionError {
	t.Helper()
	for _, e := range w {
		var a *AdmissionError
		if errors.As(e, &a) {
			return a
		}
	}
	t.Fatalf("missing admission error: %v", w)
	return nil
}

func TestTrustFirstMatchChangeApproveAndStale(t *testing.T) {
	pins := testPins(t)
	a := &gomcp.Tool{Name: "read", Description: "A"}
	m, w := connectCatalog(t, pins, false, a)
	if len(m.Tools()) != 1 || len(w) != 1 {
		t.Fatalf("first pin: %d, %v", len(m.Tools()), w)
	}
	before := pinBytes(t, pins, "fs")
	if len(before) == 0 || !strings.Contains(w[0].Error(), "explicit alias=") {
		t.Fatalf("missing first pin/notice: %v", w)
	}
	m, w = connectCatalog(t, pins, false, a)
	if len(m.Tools()) != 1 || len(w) != 0 {
		t.Fatalf("matching: %d, %v", len(m.Tools()), w)
	}
	b := &gomcp.Tool{Name: "read", Description: "B"}
	m, w = connectCatalog(t, pins, false, b)
	blocked := admission(t, w)
	if len(m.Tools()) != 0 || blocked.Reason != "catalog_changed" || !bytes.Equal(before, pinBytes(t, pins, "fs")) {
		t.Fatalf("mismatch admitted: %v", w)
	}
	if blocked.Diff.String() != "changed: mcp__fs__read (description)" {
		t.Fatal(blocked.Diff.String())
	}
	s, done, calls := staticCatalogServer(t, "fs", b)
	inspected, e := Inspect(context.Background(), Implementation{Name: "test"}, s, pins)
	if e != nil {
		t.Fatal(e)
	}
	waitOn(t, done, "inspect close")
	if inspected.CandidateDigest != blocked.CandidateDigest || !bytes.Equal(before, pinBytes(t, pins, "fs")) || calls.Load() != 0 {
		t.Fatal("inspection changed trust or invoked tool")
	}
	s, done, calls = staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read", Description: "C"})
	if _, e = Approve(context.Background(), Implementation{Name: "test"}, s, pins, inspected.CandidateDigest); e == nil {
		t.Fatal("stale approval accepted")
	}
	waitOn(t, done, "stale approval close")
	if !bytes.Equal(before, pinBytes(t, pins, "fs")) || calls.Load() != 0 {
		t.Fatal("stale approval changed pin/called tool")
	}
	s, done, calls = staticCatalogServer(t, "fs", b)
	approved, e := Approve(context.Background(), Implementation{Name: "test"}, s, pins, inspected.CandidateDigest)
	if e != nil {
		t.Fatal(e)
	}
	waitOn(t, done, "approve close")
	if approved.Diff.String() != "changed: mcp__fs__read (description)" || calls.Load() != 0 {
		t.Fatal("approval diff/call mismatch")
	}
	m, w = connectCatalog(t, pins, true, b)
	if len(m.Tools()) != 1 || len(w) != 0 {
		t.Fatalf("approved reconnect: %v", w)
	}
}

func TestTrustRequirePinned(t *testing.T) {
	for _, name := range []string{"absent", "empty-absent", "matching", "changed", "empty-matching", "invalid", "unavailable", "unreadable"} {
		t.Run(name, func(t *testing.T) {
			pins := testPins(t)
			remote := []*gomcp.Tool{tool("read")}
			if strings.HasPrefix(name, "empty") {
				remote = nil
			}
			if name != "absent" && name != "empty-absent" && name != "invalid" && name != "unavailable" {
				connectCatalog(t, pins, false, remote...)
			}
			if name == "unreadable" {
				if e := os.WriteFile(filepath.Join(pins.dir, pinKey("fs")+".json"), []byte("corrupt"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			before := pinBytes(t, pins, "fs")
			if name == "changed" {
				remote = []*gomcp.Tool{tool("new")}
			}
			if name == "invalid" {
				remote = append(remote, tool("bad name"))
			}
			s, done, _ := staticCatalogServer(t, "fs", remote...)
			if name == "unavailable" {
				s.tr = &failingTransport{err: errors.New("secret")}
			}
			m, w, e := Connect(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: pins, RequirePinned: true})
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = m.Close() }()
			good := name == "matching" || name == "empty-matching"
			if good {
				if len(w) != 0 || len(m.sessions) != 1 {
					t.Fatalf("matching rejected: %v", w)
				}
			} else {
				_ = admission(t, w)
				if len(m.Tools()) != 0 || len(m.sessions) != 0 {
					t.Fatal("untrusted tools admitted")
				}
				if name != "unavailable" {
					waitOn(t, done, "rejection close")
				}
			}
			if !bytes.Equal(before, pinBytes(t, pins, "fs")) {
				t.Fatal("RequirePinned wrote pin")
			}
		})
	}
}

func TestTrustInvalidCatalogNeverPins(t *testing.T) {
	many := make([]*gomcp.Tool, 129)
	for i := range many {
		many[i] = tool("t" + itoa(i))
	}
	cases := map[string][]*gomcp.Tool{"nil-entry": {tool("good"), nil}, "duplicate": {tool("good"), tool("good")}, "empty-name": {tool("")}, "bad-name": {tool("bad name")}, "schema": {{Name: "bad", InputSchema: []any{1}}}, "oversized": {{Name: "big", InputSchema: map[string]any{"x": strings.Repeat("a", 32769)}}}, "129": many}
	for name, remote := range cases {
		t.Run(name, func(t *testing.T) {
			pins := testPins(t)
			m, w := connectCatalog(t, pins, false, remote...)
			_ = admission(t, w)
			if len(m.Tools()) != 0 || pinBytes(t, pins, "fs") != nil {
				t.Fatal("invalid catalog partly pinned/published")
			}
		})
	}
	t.Run("128-complete", func(t *testing.T) {
		pins := testPins(t)
		m, w := connectCatalog(t, pins, false, many[:128]...)
		if len(m.Tools()) != 128 || len(w) != 1 || pinBytes(t, pins, "fs") == nil {
			t.Fatalf("128 rejected: %v", w)
		}
	})
}

func TestTrustIncompleteListingNeverPins(t *testing.T) {
	for _, mode := range []string{"error", "cycle", "pages", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			pins := testPins(t)
			n := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, done, _ := catalogServer(t, "fs", func(context.Context, *gomcp.ListToolsParams) (*gomcp.ListToolsResult, error) {
				n++
				if mode == "error" && n == 2 {
					return nil, errors.New("secret\\x1b")
				}
				if mode == "cancel" {
					cancel()
				}
				cursor := "c" + itoa(n)
				if mode == "cycle" && n == 3 {
					cursor = "c1"
				}
				return &gomcp.ListToolsResult{Tools: []*gomcp.Tool{tool("t" + itoa(n))}, NextCursor: cursor}, nil
			})
			m, w, e := Connect(ctx, Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: pins})
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = m.Close() }()
			_ = admission(t, w)
			waitOn(t, done, "incomplete session close")
			if len(m.Tools()) != 0 || pinBytes(t, pins, "fs") != nil {
				t.Fatal("incomplete catalog partly pinned/published")
			}
		})
	}
}

func TestTrustApprovalRevisionRace(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(itoa(map[bool]int{false: 0, true: 1}[existing]), func(t *testing.T) {
			pins := testPins(t)
			if existing {
				connectCatalog(t, pins, false, &gomcp.Tool{Name: "read", Description: "A"})
			}
			s, _, _ := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read", Description: "B"})
			inspected, e := Inspect(context.Background(), Implementation{Name: "test"}, s, pins)
			if e != nil {
				t.Fatal(e)
			}
			started, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			s, done, _ := catalogServer(t, "fs", func(ctx context.Context, _ *gomcp.ListToolsParams) (*gomcp.ListToolsResult, error) {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &gomcp.ListToolsResult{Tools: []*gomcp.Tool{{Name: "read", Description: "B"}}}, nil
			})
			result := make(chan error, 1)
			go func() {
				_, e := Approve(context.Background(), Implementation{Name: "test"}, s, pins, inspected.CandidateDigest)
				result <- e
			}()
			waitOn(t, started, "approval discovery")
			other, _, _ := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read", Description: "B"})
			if existing {
				if _, e := Approve(context.Background(), Implementation{Name: "test"}, other, pins, inspected.CandidateDigest); e != nil {
					t.Fatal(e)
				}
			} else {
				m, _, e := Connect(context.Background(), Implementation{Name: "test"}, []Server{other}, ConnectOptions{Pins: pins})
				if e != nil {
					t.Fatal(e)
				}
				_ = m.Close()
			}
			before := pinBytes(t, pins, "fs")
			release <- struct{}{}
			e = waitOn(t, result, "approval result")
			if !errors.Is(e, errPinRevisionConflict) {
				t.Fatalf("concurrent revision accepted: %v", e)
			}
			waitOn(t, done, "conflicting approval close")
			if !bytes.Equal(before, pinBytes(t, pins, "fs")) {
				t.Fatal("conflict replaced pin")
			}
		})
	}
}

func TestTrustConfigValidatedBeforeDial(t *testing.T) {
	for _, mode := range []string{"missing-store", "zero-store", "invalid-alias", "duplicate", "empty-command", "empty-endpoint", "bad-digest"} {
		t.Run(mode, func(t *testing.T) {
			pins := testPins(t)
			attempted := make(chan string, 2)
			servers, gates := makeGatedFakes(t, []string{"fs"}, attempted)
			gates[0].release()
			switch mode {
			case "missing-store":
				pins = nil
			case "zero-store":
				pins = &PinStore{}
			case "invalid-alias":
				servers = append(servers, StdioServer("bad\x1b", []string{"x"}))
			case "duplicate":
				servers = append(servers, StdioServer("fs", []string{"x"}))
			case "empty-command":
				servers = append(servers, StdioServer("other", nil))
			case "empty-endpoint":
				servers = append(servers, HTTPServer("other", ""))
			}
			var err error
			if mode == "bad-digest" {
				_, err = Approve(context.Background(), Implementation{Name: "test"}, servers[0], pins, "sha256:"+strings.Repeat("A", 64))
			} else {
				_, _, err = Connect(context.Background(), Implementation{Name: "test"}, servers, ConnectOptions{Pins: pins})
			}
			if err == nil {
				t.Fatal("invalid configuration accepted")
			}
			select {
			case <-attempted:
				t.Fatal("dialed before validating configuration")
			default:
			}
		})
	}
}

func TestTrustEmptyPinAndApproval(t *testing.T) {
	pins := testPins(t)
	s, done, calls := staticCatalogServer(t, "fs")
	inspected, e := Inspect(context.Background(), Implementation{Name: "test"}, s, pins)
	if e != nil {
		t.Fatal(e)
	}
	waitOn(t, done, "empty inspect close")
	if inspected.CandidateDigest != "sha256:4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945" || pinBytes(t, pins, "fs") != nil || calls.Load() != 0 {
		t.Fatal("empty inspect created pin")
	}
	s, done, _ = staticCatalogServer(t, "fs")
	if _, e = Approve(context.Background(), Implementation{Name: "test"}, s, pins, inspected.CandidateDigest); e != nil {
		t.Fatal(e)
	}
	waitOn(t, done, "empty approve close")
	m, w := connectCatalog(t, pins, true)
	if len(m.sessions) != 1 || len(w) != 0 {
		t.Fatalf("empty pin not admitted: %v", w)
	}
	before := pinBytes(t, pins, "fs")
	m, w = connectCatalog(t, pins, false, tool("new"))
	_ = admission(t, w)
	if len(m.Tools()) != 0 || !bytes.Equal(before, pinBytes(t, pins, "fs")) {
		t.Fatal("empty pin exempted from membership check")
	}
}

func TestTrustMismatchPreservesHealthyOrder(t *testing.T) {
	pins := testPins(t)
	connectCatalog(t, pins, false, tool("old"))
	before := pinBytes(t, pins, "fs")
	a, _, _ := staticCatalogServer(t, "a", tool("z"), tool("a"))
	bad, done, _ := staticCatalogServer(t, "fs", tool("new"))
	c, _, _ := staticCatalogServer(t, "c", tool("z"), tool("a"))
	m, w, e := Connect(context.Background(), Implementation{Name: "test"}, []Server{a, bad, c}, ConnectOptions{Pins: pins})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = m.Close() }()
	waitOn(t, done, "mismatch close")
	want := []string{"mcp__a__z", "mcp__a__a", "mcp__c__z", "mcp__c__a"}
	got := m.Tools()
	if len(got) != len(want) {
		t.Fatalf("got %d tools", len(got))
	}
	for i, n := range want {
		if got[i].Spec().Name != n {
			t.Fatalf("order %d: %s", i, got[i].Spec().Name)
		}
	}
	if len(w) != 3 {
		t.Fatal(w)
	}
	if admission(t, w[1:2]).Alias != "fs" {
		t.Fatal(w)
	}
	if !bytes.Equal(before, pinBytes(t, pins, "fs")) {
		t.Fatal("mismatch rewrote pin")
	}
}

func TestTrustDiffAndSafeInspection(t *testing.T) {
	pins := testPins(t)
	prose := "old\x00\x1b\u0085\u009f\u202e\n\xff"
	connectCatalog(t, pins, false, &gomcp.Tool{Name: "gone"}, &gomcp.Tool{Name: "rename"}, &gomcp.Tool{Name: "change", Description: prose, InputSchema: map[string]any{"const": "old\x1b\u202e"}})
	s, _, _ := staticCatalogServer(t, "fs", tool("renamed"), tool("added"), &gomcp.Tool{Name: "change", Description: "new", InputSchema: map[string]any{"const": "new"}})
	inspected, e := Inspect(context.Background(), Implementation{Name: "test"}, s, pins)
	if e != nil {
		t.Fatal(e)
	}
	want := "added: mcp__fs__added, mcp__fs__renamed; removed: mcp__fs__gone, mcp__fs__rename; changed: mcp__fs__change (description, inputSchema)"
	if inspected.Diff.String() != want {
		t.Fatalf("diff: %s", inspected.Diff.String())
	}
	text := inspected.String()
	for _, bad := range []string{"\x00", "\x1b", "\u009f", "\u202e", "\xff"} {
		if strings.Contains(text, bad) {
			t.Fatalf("raw control in inspection: %q", text)
		}
	}
	if !strings.Contains(text, `\x1b`) || !strings.Contains(text, `\u202e`) || !strings.Contains(text, `pin file: "`) {
		t.Fatalf("unquoted full inspection: %s", text)
	}
	candidate := inspected.CandidateDigest
	// Display must not mutate the hashed model-facing values.
	_ = inspected.String()
	if inspected.candidate.digest() != candidate {
		t.Fatal("display mutated digest")
	}
	s.tr = &failingTransport{err: errors.New("https://user:password@host/token?secret=foo\x1b\u202e")}
	_, e = Inspect(context.Background(), Implementation{Name: "test"}, s, pins)
	if e == nil || strings.Contains(e.Error(), "password") || strings.Contains(e.Error(), "secret") || strings.ContainsAny(e.Error(), "\x1b\u202e") {
		t.Fatalf("unsafe error: %v", e)
	}
}

func TestTrustDiffAll256Names(t *testing.T) {
	pins := testPins(t)
	old, newTools := make([]*gomcp.Tool, 128), make([]*gomcp.Tool, 128)
	for i := range old {
		old[i] = tool("a" + itoa(i))
		newTools[i] = tool("b" + itoa(i))
	}
	connectCatalog(t, pins, false, old...)
	s, _, _ := staticCatalogServer(t, "fs", newTools...)
	got, e := Inspect(context.Background(), Implementation{Name: "test"}, s, pins)
	if e != nil {
		t.Fatal(e)
	}
	if len(got.Diff.Added) != 128 || len(got.Diff.Removed) != 128 || strings.Count(got.Diff.String(), "mcp__fs__") != 256 {
		t.Fatal("diff truncated names")
	}
}

func TestTrustExactCapStillCompletesPagination(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(itoa(map[bool]int{false: 0, true: 1}[extra]), func(t *testing.T) {
			pins := testPins(t)
			n := 0
			remote := make([]*gomcp.Tool, 128)
			for i := range remote {
				remote[i] = tool("t" + itoa(i))
			}
			s, done, _ := catalogServer(t, "fs", func(context.Context, *gomcp.ListToolsParams) (*gomcp.ListToolsResult, error) {
				n++
				if n == 1 {
					return &gomcp.ListToolsResult{Tools: remote, NextCursor: "next"}, nil
				}
				r := &gomcp.ListToolsResult{}
				if extra {
					r.Tools = []*gomcp.Tool{tool("extra")}
				}
				return r, nil
			})
			m, w, e := Connect(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: pins})
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = m.Close() }()
			if n != 2 {
				t.Fatal("did not finish listing at exact cap")
			}
			if extra {
				_ = admission(t, w)
				waitOn(t, done, "overcap close")
				if len(m.Tools()) != 0 || pinBytes(t, pins, "fs") != nil {
					t.Fatal("overcap partly admitted")
				}
			} else if len(m.Tools()) != 128 {
				t.Fatal("exact cap rejected")
			}
		})
	}
}

func TestTrustCancellationBeforeRegistration(t *testing.T) {
	pins := testPins(t)
	connectCatalog(t, pins, false, tool("read"))
	before := pinBytes(t, pins, "fs")
	s, done, _ := staticCatalogServer(t, "fs", tool("read"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, w, e := connectWithHooks(ctx, Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: pins}, &connectHooks{published: func(int) { cancel() }})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = m.Close() }()
	if len(m.Tools()) != 0 || len(m.sessions) != 0 {
		t.Fatal("canceled before registration but published tools")
	}
	if !errors.Is(admission(t, w), context.Canceled) {
		t.Fatal(w)
	}
	waitOn(t, done, "cancel before registration close")
	if !bytes.Equal(before, pinBytes(t, pins, "fs")) {
		t.Fatal("cancellation altered existing pin")
	}
}

func TestTrustDurabilityErrorSurvivesCancellation(t *testing.T) {
	pins := testPins(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	renamed := false
	originalRename, originalSync := pins.ops.rename, pins.ops.syncDir
	pins.ops.rename = func(r *os.Root, a, b string) error { err := originalRename(r, a, b); renamed = err == nil; return err }
	pins.ops.syncDir = func(r *os.Root) error {
		if renamed {
			cancel()
			return errors.New("private path/secret\x1b")
		}
		return originalSync(r)
	}
	s, done, _ := staticCatalogServer(t, "fs", tool("read"))
	m, w, e := Connect(ctx, Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: pins})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = m.Close() }()
	waitOn(t, done, "durability rejection close")
	failure := admission(t, w)
	if len(m.Tools()) != 0 || failure.Reason != "pin_durability" || !strings.Contains(failure.Error(), "published bytes may already be present") || strings.Contains(failure.Error(), "secret") {
		t.Fatalf("misleading durability result: %v", w)
	}
}
