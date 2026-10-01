package mcpclient

import (
	"context"
	"errors"
	"strings"
	"testing"
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
