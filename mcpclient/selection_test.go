package mcpclient

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioServerCopiesCommand(t *testing.T) {
	argv := []string{"server", "--flag"}
	s := StdioServer("fs", argv)
	argv[1] = "--changed"
	if got := s.command[1]; got != "--flag" {
		t.Fatalf("StdioServer command[1] after caller mutation = %q, want %q", got, "--flag")
	}
}

func TestWithToolsSemantics(t *testing.T) {
	base := StdioServer("fs", []string{"server"})
	if base.toolsSet {
		t.Fatal("omitted selection reports toolsSet")
	}
	names := []string{"read"}
	picked := base.WithTools(names...)
	names[0] = "write"
	if !picked.toolsSet || len(picked.tools) != 1 || picked.tools[0] != "read" {
		t.Fatalf("WithTools(read) = (%v, %q), want (true, [read])", picked.toolsSet, picked.tools)
	}
	if base.toolsSet {
		t.Fatal("WithTools mutated its receiver")
	}
	empty := base.WithTools()
	if !empty.toolsSet || len(empty.tools) != 0 {
		t.Fatalf("WithTools() = (%v, %q), want explicit-empty (true, [])", empty.toolsSet, empty.tools)
	}
	replaced := picked.WithTools("write")
	if len(replaced.tools) != 1 || replaced.tools[0] != "write" || picked.tools[0] != "read" {
		t.Fatalf("replacement = %q, original = %q, want [write] and [read]", replaced.tools, picked.tools)
	}
}

func TestConnectRejectsInvalidSelection(t *testing.T) {
	over := make([]string, maxToolsPerServer+1)
	for i := range over {
		over[i] = "t" + itoa(i)
	}
	for name, names := range map[string][]string{
		"space":     {"bad name"},
		"wildcard":  {"*"},
		"glob":      {"read?"},
		"empty":     {""},
		"duplicate": {"read", "read"},
		"too-long":  {strings.Repeat("a", 64)},
		"over-cap":  over,
	} {
		t.Run(name, func(t *testing.T) {
			// failingTransport keeps a mutation that drops validation from
			// launching whatever "server" program happens to be on PATH.
			s := Server{Alias: "fs", tr: &failingTransport{err: errors.New("unreachable")}}.WithTools(names...)
			_, _, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: testPins(t)})
			var failure *AdmissionError
			if !errors.As(err, &failure) || failure.Reason != "invalid_config" {
				t.Fatalf("Connect(selection %s) error = %v, want fatal invalid_config", name, err)
			}
		})
	}
}

// TestValidateSelection pins what Connect accepts, the exact boundaries, and
// that errors name entries by 1-based position without echoing them.
func TestValidateSelection(t *testing.T) {
	full := make([]string, maxToolsPerServer)
	for i := range full {
		full[i] = "t" + itoa(i)
	}
	for _, tt := range []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"read", "write_file", "a-b"}, ""},
		{[]string{strings.Repeat("a", 55)}, ""},                                                           // "mcp__fs__" + 55 bytes = 64, the composed-name limit
		{[]string{strings.Repeat("a", 56)}, "mcpclient: tool selection entry 1 is not a valid tool name"}, // one byte over
		{full, ""},
		{[]string{"read", "bad name", "read"}, "mcpclient: tool selection entry 2 is not a valid tool name"},
		{[]string{"read", "write", "read"}, "mcpclient: tool selection entry 3 repeats a name"},
		{append(full, "extra"), "mcpclient: tool selection exceeds 128 names"},
	} {
		err := validateSelection(Server{Alias: "fs"}.WithTools(tt.names...))
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tt.want {
			t.Errorf("validateSelection(%d names) = %q, want %q", len(tt.names), got, tt.want)
		}
	}
}

func TestSelectionMissingBlocksAlias(t *testing.T) {
	pins := testPins(t)
	s, done, _ := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read"})
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s.WithTools("read", "delete")}, ConnectOptions{Pins: pins})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	failure := admission(t, w)
	if failure.Reason != "selection_missing" || len(failure.Names) != 1 || failure.Names[0] != "delete" {
		t.Fatalf("failure = (%q, %q), want (selection_missing, [delete])", failure.Reason, failure.Names)
	}
	if got, want := failure.Error(), `server "fs": selection_missing: delete`; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if len(m.Tools()) != 0 || len(m.sessions) != 0 {
		t.Fatalf("blocked alias published %d tools and %d sessions, want 0 and 0", len(m.Tools()), len(m.sessions))
	}
	if len(w) != 2 || !strings.Contains(w[0].Error(), "first pin") {
		t.Fatalf("warnings = %v, want the first-pin notice then the failure", w)
	}
	if !bytes.Contains(pinBytes(t, pins, "fs"), []byte(`"mcp__fs__read"`)) {
		t.Fatal("full-catalog pin was not created before selection applied")
	}
	waitOn(t, done, "selection_missing session close")
}

// closeErrTransport makes the client connection's Close report err after
// really closing, to prove cleanup errors cannot rename a refusal.
type closeErrTransport struct {
	gomcp.Transport
	err error
}

func (t closeErrTransport) Connect(ctx context.Context) (gomcp.Connection, error) {
	c, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return closeErrConn{Connection: c, err: t.err}, nil
}

type closeErrConn struct {
	gomcp.Connection
	err error
}

func (c closeErrConn) Close() error {
	_ = c.Connection.Close()
	return c.err
}

func TestSelectionMissingSurvivesCloseError(t *testing.T) {
	s, _, _ := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read"})
	s.tr = closeErrTransport{Transport: s.tr, err: context.Canceled}
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s.WithTools("delete")}, ConnectOptions{Pins: testPins(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	failure := admission(t, w)
	if !errors.Is(failure, context.Canceled) {
		t.Fatalf("selection_missing failure lacks the close error as its cause (fixture, or the session was not closed): %v", failure)
	}
	if failure.Reason != "selection_missing" {
		t.Fatalf("reason = %q, want selection_missing despite a canceled close", failure.Reason)
	}
}

func TestExplicitEmptySelectionExposesNothing(t *testing.T) {
	pins := testPins(t)
	s, _, _ := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read"})
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s.WithTools()}, ConnectOptions{Pins: pins})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if len(m.Tools()) != 0 || len(m.sessions) != 1 || len(w) != 1 {
		t.Fatalf("explicit-empty = (%d tools, %d sessions, %v), want (0, 1, [first-pin notice])", len(m.Tools()), len(m.sessions), w)
	}
	if !bytes.Contains(pinBytes(t, pins, "fs"), []byte(`"mcp__fs__read"`)) {
		t.Fatal("explicit-empty selection skipped the full-catalog pin")
	}
}

func TestSelectionMissingNamesKeepSelectionOrder(t *testing.T) {
	s, _, _ := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read"})
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s.WithTools("zeta", "read", "alpha")}, ConnectOptions{Pins: testPins(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	failure := admission(t, w)
	if got, want := failure.Error(), `server "fs": selection_missing: zeta, alpha`; got != want || !slices.Equal(failure.Names, []string{"zeta", "alpha"}) {
		t.Fatalf("failure = (%q, %q), want (%q, [zeta alpha]): selection order, not sorted or catalog order", got, failure.Names, want)
	}
}
