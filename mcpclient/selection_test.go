package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
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
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s.WithTools("zeta", "read", "alpha", "mu", "beta", "omega", "kappa", "delta", "sigma", "gamma")}, ConnectOptions{Pins: testPins(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	failure := admission(t, w)
	// Nine missing names: more than one map group, so any map-order iteration
	// reorders them with near certainty.
	wantNames := []string{"zeta", "alpha", "mu", "beta", "omega", "kappa", "delta", "sigma", "gamma"}
	if got, want := failure.Error(), `server "fs": selection_missing: zeta, alpha, mu, beta, omega, kappa, delta, sigma, gamma`; got != want || !slices.Equal(failure.Names, wantNames) {
		t.Fatalf("failure = (%q, %q), want (%q, %q): selection order, not sorted or map order", got, failure.Names, want, wantNames)
	}
}

func toolNames(tools []agent.Tool) []string {
	out := make([]string, len(tools))
	for i, tool := range tools {
		out[i] = tool.Spec().Name
	}
	return out
}

func TestSelectionIsPerAliasWithIdenticalRemoteNames(t *testing.T) {
	pins := testPins(t)
	a, _, _ := staticCatalogServer(t, "a", &gomcp.Tool{Name: "read"}, &gomcp.Tool{Name: "write"})
	b, _, _ := staticCatalogServer(t, "b", &gomcp.Tool{Name: "read"}, &gomcp.Tool{Name: "write"})
	m, _, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{a.WithTools("read"), b.WithTools("write")}, ConnectOptions{Pins: pins})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if got, want := toolNames(m.Tools()), []string{"mcp__a__read", "mcp__b__write"}; !slices.Equal(got, want) {
		t.Fatalf("tools = %q, want %q", got, want)
	}
	for alias, want := range map[string][]string{
		"a": {`"mcp__a__read"`, `"mcp__a__write"`},
		"b": {`"mcp__b__read"`, `"mcp__b__write"`},
	} {
		pin := pinBytes(t, pins, alias)
		for _, name := range want {
			if !bytes.Contains(pin, []byte(name)) {
				t.Fatalf("pin %s lacks %s; selection narrowed the pinned catalog", alias, name)
			}
		}
	}
}

func TestSelectionKeepsFullCatalogChangeDetection(t *testing.T) {
	pins := testPins(t)
	first, _, _ := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read", Description: "R"}, &gomcp.Tool{Name: "write", Description: "W"})
	m, _, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{first.WithTools("read")}, ConnectOptions{Pins: pins})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(m.Tools()); !slices.Equal(got, []string{"mcp__fs__read"}) {
		t.Fatalf("first connect tools = %q, want [mcp__fs__read]", got)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	changed, done, _ := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read", Description: "R"}, &gomcp.Tool{Name: "write", Description: "W2"})
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{changed.WithTools("read")}, ConnectOptions{Pins: pins})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	waitOn(t, done, "changed-hidden-tool session close")
	failure := admission(t, w)
	if failure.Reason != "catalog_changed" || failure.Diff.String() != "changed: mcp__fs__write (description)" || len(m.Tools()) != 0 {
		t.Fatalf("hidden change = (%q, %q, %d tools), want (catalog_changed, changed: mcp__fs__write (description), 0)", failure.Reason, failure.Diff.String(), len(m.Tools()))
	}
}

func TestSelectionKeepsServerListingOrder(t *testing.T) {
	s, _, _ := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read"}, &gomcp.Tool{Name: "write"}, &gomcp.Tool{Name: "delete"})
	m, _, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s.WithTools("write", "read")}, ConnectOptions{Pins: testPins(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if got, want := toolNames(m.Tools()), []string{"mcp__fs__read", "mcp__fs__write"}; !slices.Equal(got, want) {
		t.Fatalf("tools = %q, want server listing order %q (not selection order)", got, want)
	}
}

func TestOmittedSelectionExposesWholeCatalog(t *testing.T) {
	s, _, _ := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "write"}, &gomcp.Tool{Name: "read"})
	m, _, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: testPins(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if got, want := toolNames(m.Tools()), []string{"mcp__fs__write", "mcp__fs__read"}; !slices.Equal(got, want) {
		t.Fatalf("omitted selection tools = %q, want server order %q", got, want)
	}
}

func TestSelectedToolsKeepPerCallApproval(t *testing.T) {
	s, _, _ := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read", Annotations: &gomcp.ToolAnnotations{ReadOnlyHint: true}})
	m, _, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s.WithTools("read")}, ConnectOptions{Pins: testPins(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	tools := m.Tools()
	if len(tools) != 1 || tools[0].Effect().Approval != agent.ApprovalAlways {
		t.Fatalf("selected read-only-hinted tool effect = %+v, want ApprovalAlways", tools)
	}
	plan, err := tools[0].(agent.PlanningTool).Plan(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Effect.Approval != agent.ApprovalAlways || plan.ApprovalKey != "" {
		t.Fatalf("plan = (%v, %q), want (ApprovalAlways, no session-grant key)", plan.Effect.Approval, plan.ApprovalKey)
	}
}

// selectionModel replays a fixed sequence of model turns.
type selectionModel struct {
	steps []agent.ModelResult
	n     int
}

func (m *selectionModel) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if m.n >= len(m.steps) {
		return agent.ModelResult{}, errors.New("selection model exhausted")
	}
	m.n++
	return m.steps[m.n-1], nil
}

type approveAll struct{}

func (approveAll) Approve(context.Context, provider.ToolCall, string) (bool, error) { return true, nil }

func selectionCall(id, name string) agent.ModelResult {
	return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: id, Type: "function", Function: provider.ToolCallFunction{Name: name, Arguments: json.RawMessage(`{}`)},
	}}}}
}

func TestUnselectedToolCallNeverReachesSession(t *testing.T) {
	s, _, calls := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read"}, &gomcp.Tool{Name: "write"})
	m, _, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s.WithTools("read")}, ConnectOptions{Pins: testPins(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	model := &selectionModel{steps: []agent.ModelResult{
		selectionCall("1", "mcp__fs__read"),
		selectionCall("2", "mcp__fs__write"),
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	res, err := agent.New(model, agent.ContextManager{}).Run(context.Background(), agent.Request{Goal: "q", Tools: m.Tools(), Approver: approveAll{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The selected call is the positive control: it proves the counter sees
	// real calls, so a zero for the unselected one is meaningful.
	if got := calls.Load(); got != 1 {
		t.Fatalf("tools/call requests reaching the server = %d, want 1 (selected read only)", got)
	}
	found := false
	for _, msg := range res.Messages {
		if strings.Contains(msg.Content, "unknown tool: mcp__fs__write") {
			found = true
		}
	}
	if !found {
		t.Fatal("unselected call did not produce the dispatcher's unknown-tool result")
	}
}
