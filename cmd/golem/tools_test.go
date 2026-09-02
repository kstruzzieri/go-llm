package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// stubTool is a minimal agent.Tool to stand in for retrieve.
type stubTool struct{ name string }

func (s stubTool) Spec() agent.ToolSpec { return agent.ToolSpec{Name: s.name} }
func (s stubTool) Effect() agent.Effect { return agent.Effect{Class: agent.Read} }
func (s stubTool) Invoke(ctx context.Context, _ json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

func names(tools []agent.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Spec().Name
	}
	return out
}

func TestBuildTools_FileOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, err := buildTools(root, nil)
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	got := names(tools)
	want := []string{"read_file", "search", "glob", "list"}
	if len(got) != len(want) {
		t.Fatalf("tool names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tool[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildTools_WithRetrieve(t *testing.T) {
	root := t.TempDir()
	tools, err := buildTools(root, stubTool{name: "retrieve"})
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	got := names(tools)
	if len(got) != 5 || got[4] != "retrieve" {
		t.Errorf("tool names = %v, want file tools + retrieve last", got)
	}
}

func TestBuildTools_BadRoot(t *testing.T) {
	if _, err := buildTools(filepath.Join(t.TempDir(), "does-not-exist"), nil); err == nil {
		t.Fatal("buildTools on missing root err = nil, want error")
	}
}

func TestBuildTools_NoExecTool(t *testing.T) {
	tools, err := buildTools(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	for _, tool := range tools {
		if tool.Spec().Name == "run_command" {
			t.Error("run_command must not be present in the read-only tool set (requires -allow-exec)")
		}
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	ro := buildSystemPrompt(false, false)
	if !strings.Contains(ro, "Do not") || strings.Contains(ro, "run_command to") {
		t.Errorf("read-only prompt wrong:\n%s", ro)
	}
	wo := buildSystemPrompt(true, false)
	if !strings.Contains(wo, "write_file") {
		t.Error("write-only prompt missing write capability")
	}
	if !strings.Contains(wo, "Do not") {
		t.Error("write-only must still forbid running commands (!allowExec)")
	}
	eo := buildSystemPrompt(false, true)
	if !strings.Contains(eo, "run_command") {
		t.Error("exec prompt missing exec capability")
	}
	if strings.Contains(eo, "Do not claim to run") {
		t.Error("exec-enabled prompt must NOT forbid commands")
	}
	if !strings.Contains(eo, "authoritative") {
		t.Error("priority note dropped")
	}
}

func TestBuildExecTools(t *testing.T) {
	// #346: -allow-exec registers the foreground run_command plus the four
	// background tools, built over one manager/workspace pair.
	mgr := agenttools.NewBackgroundManager()
	t.Cleanup(mgr.Shutdown)
	tools, err := buildExecTools(t.TempDir(), mgr, agenttools.ExecToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := names(tools)
	want := []string{"run_command", "start_command", "command_status", "command_tail", "stop_command"}
	if len(got) != len(want) {
		t.Fatalf("tool names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tool[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildExecTools_NilManagerFailsLoudly(t *testing.T) {
	if _, err := buildExecTools(t.TempDir(), nil, agenttools.ExecToolsOptions{}); err == nil {
		t.Fatal("nil manager must fail loudly, not register a broken tool set")
	}
}

func TestBuildDelegateTool_ResolvesCodingRole(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"coding": {Name: "coder", Provider: "local"},
		},
	}
	resolved, err := resolveDelegateChain(cfg, "coding")
	if err != nil {
		t.Fatalf("resolveDelegateChain: %v", err)
	}
	tool, chain, err := buildDelegateTool(nil, "coding", resolved, nil)
	if err != nil {
		t.Fatalf("buildDelegateTool: %v", err)
	}
	if tool == nil || tool.Spec().Name != "delegate_code" {
		t.Fatalf("expected delegate_code tool, got %+v", tool)
	}
	if len(chain) != 1 || chain[0] != "local/coder" {
		t.Fatalf("chain = %v", chain)
	}
}

func TestResolveDelegateChain_NilConfig(t *testing.T) {
	if _, err := resolveDelegateChain(nil, "coding"); err == nil {
		t.Fatal("nil config should fail loudly, not no-op")
	}
}

func TestResolveDelegateChain_UnknownRole(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.ModelConfig{}}
	if _, err := resolveDelegateChain(cfg, "coding"); err == nil {
		t.Fatal("unknown role should error")
	}
}

func TestBuildDelegateTool_EmptyChain(t *testing.T) {
	if _, _, err := buildDelegateTool(nil, "coding", nil, nil); err == nil {
		t.Fatal("empty pre-resolved chain should error, not build a broken tool")
	}
}

func TestBuildDelegateTool_WithStreamSink(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"coding": {Name: "coder", Provider: "local"},
		},
	}
	var sink = func(string) {} // non-nil sink
	resolved, rerr := resolveDelegateChain(cfg, "coding")
	if rerr != nil {
		t.Fatalf("resolveDelegateChain: %v", rerr)
	}
	tool, _, err := buildDelegateTool(nil, "coding", resolved, sink)
	if err != nil {
		t.Fatalf("buildDelegateTool with sink: %v", err)
	}
	if tool == nil || tool.Spec().Name != "delegate_code" {
		t.Fatalf("expected delegate_code tool, got %+v", tool)
	}
}

func TestDelegateSystemFragment(t *testing.T) {
	if delegateSystemFragment(false, false) != "" || delegateSystemFragment(false, true) != "" {
		t.Fatal("fragment must be empty when delegation disabled, regardless of allowWrite")
	}
	withWrite := delegateSystemFragment(true, true)
	if !strings.Contains(withWrite, "delegate_code") || !strings.Contains(withWrite, "write_file") {
		t.Fatalf("write-enabled fragment should mention delegate_code and write_file: %q", withWrite)
	}
	noWrite := delegateSystemFragment(true, false)
	if !strings.Contains(noWrite, "delegate_code") {
		t.Fatalf("fragment should mention delegate_code: %q", noWrite)
	}
	if strings.Contains(noWrite, "write_file") || strings.Contains(noWrite, "edit_file") {
		t.Fatalf("write-disabled fragment must not instruct writing to disk: %q", noWrite)
	}
}

// validDispatchAvailable returns a real read-only file tool set rooted in a
// temp dir — valid surrounding input for the builder tests, so each negative
// case can only fail for the reason it names.
func validDispatchAvailable(t *testing.T) []agent.Tool {
	t.Helper()
	fileTools, err := agenttools.NewFileTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return fileTools
}

func TestResolveDispatchChain_ExplicitRole(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"lightweight": {Name: "speedy", Provider: "local"},
		},
	}
	chain, err := resolveDispatchChain(cfg, "lightweight", []string{"local/parent"})
	if err != nil {
		t.Fatalf("resolveDispatchChain: %v", err)
	}
	if len(chain) != 1 || chain[0] != "local/speedy" {
		t.Fatalf("explicit role must resolve its own chain, not the parent's: %v", chain)
	}
}

func TestResolveDispatchChain_DefaultFollowsParentChain(t *testing.T) {
	// No role: the child chain IS the parent chain (no-swap by construction).
	// Config deliberately lacks any role so a regression to role resolution errors.
	cfg := &config.Config{Models: map[string]config.ModelConfig{}}
	chain, err := resolveDispatchChain(cfg, "", []string{"local/parent-a", "local/parent-b"})
	if err != nil {
		t.Fatalf("resolveDispatchChain: %v", err)
	}
	if len(chain) != 2 || chain[0] != "local/parent-a" || chain[1] != "local/parent-b" {
		t.Fatalf("default must follow parent chain verbatim: %v", chain)
	}
}

func TestResolveDispatchChain_DefaultWithEmptyParentChainIsRecommendMode(t *testing.T) {
	// Recommend-mode parents have no chain; dispatch mirrors that instead of erroring.
	chain, err := resolveDispatchChain(&config.Config{}, "", nil)
	if err != nil {
		t.Fatalf("resolveDispatchChain: %v", err)
	}
	if len(chain) != 0 {
		t.Fatalf("empty parent chain must resolve to recommend mode: chain=%v", chain)
	}
}

func TestResolveDispatchChain_ExplicitRoleNilConfig(t *testing.T) {
	_, err := resolveDispatchChain(nil, "lightweight", nil)
	if err == nil {
		t.Fatal("explicit role with nil config should fail loudly, not no-op")
	}
	if !strings.Contains(err.Error(), "-dispatch-role requires a models.json") {
		t.Fatalf("wrong failure category: %v", err)
	}
}

func TestResolveDispatchChain_ExplicitUnknownRole(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.ModelConfig{}}
	_, err := resolveDispatchChain(cfg, "lightweight", nil)
	if err == nil {
		t.Fatal("unknown role should error")
	}
	if !strings.Contains(err.Error(), `-dispatch-role "lightweight"`) {
		t.Fatalf("error must name the failing role flag: %v", err)
	}
}

func TestDispatchSystemFragment(t *testing.T) {
	if dispatchSystemFragment(false) != "" {
		t.Fatal("fragment must be empty when dispatch disabled")
	}
	on := dispatchSystemFragment(true)
	for _, want := range []string{"dispatch", "bounded concurrency", "ungoverned", "serial", "read"} {
		if !strings.Contains(on, want) {
			t.Fatalf("fragment should mention %q: %q", want, on)
		}
	}
	if strings.Contains(on, "sequential") {
		t.Fatalf("fragment must not claim sequential children under governed fan-out: %q", on)
	}
	for _, banned := range []string{"write_file", "edit_file", "run_command"} {
		if strings.Contains(on, banned) {
			t.Fatalf("read-only fragment must not mention %s: %q", banned, on)
		}
	}
}

func TestNewDispatchTool_MissingFileToolsError(t *testing.T) {
	// Valid caller and budget: the ONLY invalid input is the empty available set.
	_, err := newDispatchTool(&specRecordingCaller{}, false, agent.Budget{}, dispatchFanout{maxConcurrent: 1}, nil, nil)
	if err == nil {
		t.Fatal("empty available set must error: children need the file readers")
	}
	if !strings.Contains(err.Error(), "required child tool") {
		t.Fatalf("wrong failure category (want the library's missing-child-tool error): %v", err)
	}
}

func TestEffectClassName(t *testing.T) {
	cases := []struct {
		in   agent.EffectClass
		want string
	}{
		{agent.Read, "read"},
		{agent.Read | agent.Write, "read|write"},
		{agent.Exec, "exec"},
		{agent.Network, "network"},
		{agent.Read | agent.Write | agent.Exec | agent.Network, "read|write|exec|network"},
		{agent.EffectClass(0), "none"},
	}
	for _, c := range cases {
		if got := effectClassName(c.in); got != c.want {
			t.Errorf("effectClassName(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveDispatchFanout(t *testing.T) {
	head := provider.ModelKey{Provider: "local", Model: "primary"}
	fallback := provider.ModelKey{Provider: "local", Model: "fallback"}
	type state struct {
		n  int
		ok bool
	}
	states := map[provider.ModelKey]state{
		head:     {n: 3, ok: true},
		fallback: {n: 1, ok: true},
	}
	capacity := func(key provider.ModelKey) (int, bool) {
		s := states[key]
		return s.n, s.ok
	}
	chain := []string{"local/primary", "local/fallback"}

	fan := resolveDispatchFanout(capacity, chain)
	if fan.maxConcurrent != agenttools.MaxDispatchTasks || fan.governor == nil {
		t.Fatalf("governed fanout = %+v", fan)
	}
	if got := fan.governor(); got != 3 {
		t.Fatalf("governor() = %d, want head capacity 3", got)
	}
	states[head] = state{n: 4, ok: true}
	if got := fan.governor(); got != 4 {
		t.Fatalf("governor() after head refresh = %d, want 4", got)
	}
	states[head] = state{n: 0, ok: true}
	if got := fan.governor(); got != 1 {
		t.Fatalf("governor() for invalid governed capacity = %d, want serial", got)
	}
	states[head] = state{n: 4, ok: true}
	states[fallback] = state{n: 0, ok: false}
	if got := fan.governor(); got != 1 {
		t.Fatalf("governor() after governance loss = %d, want serial", got)
	}

	states[fallback] = state{n: 0, ok: false}
	if got := resolveDispatchFanout(capacity, chain); got.maxConcurrent != 1 || got.governor != nil {
		t.Fatalf("ungoverned fallback = %+v, want static serial", got)
	}
	for _, bad := range [][]string{nil, {"bare"}, {"/model"}, {"provider/"}} {
		if got := resolveDispatchFanout(capacity, bad); got.maxConcurrent != 1 || got.governor != nil {
			t.Fatalf("chain %v = %+v, want static serial", bad, got)
		}
	}
	if got := resolveDispatchFanout(nil, chain); got.maxConcurrent != 1 || got.governor != nil {
		t.Fatalf("nil capacity = %+v, want static serial", got)
	}
}

// --- #443 Task 10: -scratch wiring ---

func execToolNames(tools []agent.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Spec().Name
	}
	return names
}

func TestBuildExecToolsScratchMatrix(t *testing.T) {
	root := t.TempDir()
	base := []string{"run_command", "start_command", "command_status", "command_tail", "stop_command"}

	legacyMgr := agenttools.NewBackgroundManager()
	defer legacyMgr.Shutdown()
	tools, err := buildExecTools(root, legacyMgr, agenttools.ExecToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := execToolNames(tools); !reflect.DeepEqual(got, base) {
		t.Fatalf("zero options must reproduce the legacy tool set: %v", got)
	}

	scratchMgr := agenttools.NewBackgroundManager()
	defer scratchMgr.Shutdown()
	tools, err = buildExecTools(root, scratchMgr, agenttools.ExecToolsOptions{
		Scratch: agenttools.ScratchConfig{Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	names := execToolNames(tools)
	if !slices.Contains(names, "scratch_changes") || slices.Contains(names, "promote_artifact") {
		t.Fatalf("scratch without write must register the query tool and no promotion: %v", names)
	}

	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, root)
	journal := newCheckpointJournal(ws, store)
	promoMgr := agenttools.NewBackgroundManager()
	defer promoMgr.Shutdown()
	tools, err = buildExecTools(root, promoMgr, agenttools.ExecToolsOptions{
		Scratch:          agenttools.ScratchConfig{Enabled: true},
		PromotionJournal: journal,
	})
	if err != nil {
		t.Fatal(err)
	}
	names = execToolNames(tools)
	if !slices.Contains(names, "promote_artifact") {
		t.Fatalf("scratch with the checkpoint journal must register promotion: %v", names)
	}
}

func TestScratchExecOptionsJournalGate(t *testing.T) {
	opts, line := scratchExecOptions(false, nil)
	if opts.Scratch.Enabled || opts.PromotionJournal != nil || line != "" {
		t.Fatalf("scratch off must be the zero policy: %+v %q", opts, line)
	}
	opts, line = scratchExecOptions(true, nil)
	if !opts.Scratch.Enabled || line == "" {
		t.Fatalf("scratch on must enable and announce: %+v %q", opts, line)
	}
	// The concrete-nil check must keep a typed nil out of the interface:
	// -allow-exec alone never registers promotion.
	if opts.PromotionJournal != nil {
		t.Fatal("nil journal must never become a non-nil PromotionJournal interface")
	}
	if !strings.Contains(line, "disabled") {
		t.Fatalf("notice must say promotion is disabled without -allow-write: %q", line)
	}
	ws, err := agenttools.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	j := newCheckpointJournal(ws, openTestStore(t, t.TempDir()))
	opts, line = scratchExecOptions(true, j)
	if opts.PromotionJournal == nil || !strings.Contains(line, "prompts per artifact") {
		t.Fatalf("a real journal must enable promotion: %+v %q", opts, line)
	}
}
