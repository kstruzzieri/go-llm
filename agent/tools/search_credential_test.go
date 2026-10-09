package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
	"github.com/kstruzzieri/go-llm/provider"
)

// searchedFiles runs search and returns the paths of its match lines, sorted.
// Every fixture holds one line, so each match is "path:1: text".
func searchedFiles(t *testing.T, s *Search, args map[string]any) []string {
	t.Helper()
	res := invoke(t, s, args)
	if res.IsError || res.Truncated {
		t.Fatalf("search %v = %+v", args, res)
	}
	var got []string
	for _, line := range strings.Split(res.Content, "\n") {
		if rel, _, ok := strings.Cut(line, ":1: "); ok {
			got = append(got, rel)
		}
	}
	slices.Sort(got)
	return got
}

// TestSearchSkipsCredentialFiles (#627 T7) pins the skip set with a literal
// list of the files search may show. A wrong credential set fails here even
// though the parity test would pass, because both readers share one predicate.
func TestSearchSkipsCredentialFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"notes.txt":                 "SECRET allowed\n",
		".env":                      "SECRET env\n",
		".env.local":                "SECRET env-local\n",
		"sub/.env.production.local": "SECRET env-prod\n",
		".env.example":              "SECRET example\n",
		".env.sample":               "SECRET sample\n",
		".env.template":             "SECRET template\n",
		".env.dist":                 "SECRET dist\n",
		".netrc":                    "SECRET netrc\n",
		"_netrc":                    "SECRET win-netrc\n",
		"web/.npmrc":                "SECRET npmrc\n",
		".pypirc":                   "SECRET pypirc\n",
		".git-credentials":          "SECRET git-credentials\n",
		".ssh/id_rsa":               "SECRET key\n",
		"a/.aws/credentials":        "SECRET aws\n",
		".kube/config":              "SECRET kube\n",
		".gnupg/secring.gpg":        "SECRET gnupg\n",
		"py/.env/bin/activate":      "SECRET venv\n",
		"repo/.Git/config":          "SECRET git-config\n",
		"repo/.Git/HEAD":            "SECRET git-head\n",
		"upper/.ENV":                "SECRET upper-env\n",
	})
	want := []string{".env.dist", ".env.example", ".env.sample", ".env.template", "notes.txt", "py/.env/bin/activate", "repo/.Git/HEAD"}
	s := NewSearch(mustWorkspace(t, root))
	for _, args := range []map[string]any{{"pattern": "SECRET"}, {"pattern": ".", "regex": true}} {
		if got := searchedFiles(t, s, args); !slices.Equal(got, want) {
			t.Errorf("search %v shows %v, want exactly %v", args, got, want)
		}
	}
}

// TestSearchSkipsExactlyWhatReadFileRefuses (#627 T8, the issue's acceptance
// test): search shows a file exactly when the default read_file invariant
// allows it and the walk does not exclude it. It catches drift between the
// two readers, such as a table that stops using the shared predicate or a
// search that stops skipping. The set itself is pinned by the literal tests.
func TestSearchSkipsExactlyWhatReadFileRefuses(t *testing.T) {
	corpus := []string{
		"notes.txt", "sub/readme.md", ".envrc", "prod.env", "env", ".environment", ".gitconfig",
		".env", "sub/.env", ".env.local", "deep/a/.env.production.local", ".env.",
		".env.example", "sub/.env.sample", ".env.template", "cfg/.env.dist",
		".netrc", "_netrc", "sub/.npmrc", ".pypirc", ".git-credentials",
		".ssh/id_rsa", "x/.aws/credentials", ".kube/config", ".gnupg/pubring.kbx",
		"py/.env/bin/activate", "upper/.ENV", "mixed/.Env.Local", "upper2/.ENV.EXAMPLE",
		"repo/.Git/config", "repo/.Git/HEAD", ".git/config", ".git/HEAD", "vendor/.env.example",
		"l/.\u017Fsh/id_rsa", "z/.s\u200Csh/id_rsa", "g/.git-credential\u017F",
	}
	files := make(map[string]string, len(corpus))
	for _, p := range corpus {
		files[p] = "PARITY-627 " + p + "\n"
	}
	root := t.TempDir()
	writeTree(t, root, files)
	shown := map[string]bool{}
	for _, p := range searchedFiles(t, NewSearch(mustWorkspace(t, root)), map[string]any{"pattern": "PARITY-627"}) {
		shown[p] = true
	}
	for _, p := range corpus {
		args, err := json.Marshal(map[string]string{"path": p})
		if err != nil {
			t.Fatal(err)
		}
		blocked := len(inspectDefault(t, "read_file", string(args))) > 0
		walkExcluded := slices.ContainsFunc(strings.Split(p, "/"), func(c string) bool { return ignoreDirs[c] })
		if want := !blocked && !walkExcluded; shown[p] != want {
			t.Errorf("%s: search shows=%v, want %v (read_file blocked=%v, walk-excluded=%v)", p, shown[p], want, blocked, walkExcluded)
		}
	}
}

// TestCredentialAliasSearchSkipsStoredSpellings (#627, spec F13): a credential
// path stored on disk under an alias spelling is skipped like the canonical
// name. Each spelling has its own parent, so every filesystem stores the alias
// itself. The darwin-smoke step runs this natively on APFS.
func TestCredentialAliasSearchSkipsStoredSpellings(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"notes.txt":               "ALIAS-627 allowed\n",
		"l/.\u017Fsh/id_rsa":      "ALIAS-627 long-s\n",
		"b/.\u00DFh/id_rsa":       "ALIAS-627 sharp-s\n",
		"z/.s\u200Csh/id_rsa":     "ALIAS-627 zwnj\n",
		"g/.git-credential\u017F": "ALIAS-627 git-credentials\n",
		"k/.\u212Aube/config":     "ALIAS-627 kelvin\n",
	})
	if got := searchedFiles(t, NewSearch(mustWorkspace(t, root)), map[string]any{"pattern": "ALIAS-627"}); !slices.Equal(got, []string{"notes.txt"}) {
		t.Fatalf("search shows %v, want only notes.txt", got)
	}
}

// TestCredentialAliasReadFileBlocked (#627, spec F8): on a filesystem that
// opens .ssh for .ſsh (APFS), a dispatch child's guarded read of the alias
// spelling is blocked. The unguarded control read proves the alias really
// reaches the key on this volume, so the block is not vacuous. Filesystems
// without the alias skip. The darwin-smoke step runs this natively.
func TestCredentialAliasReadFileBlocked(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{".ssh/id_rsa": "ALIAS-READ-627 key-secret\n"})
	ws := mustWorkspace(t, root)
	if res := invoke(t, NewReadFile(ws), map[string]any{"path": ".ſsh/id_rsa"}); res.IsError || !strings.Contains(res.Content, "key-secret") {
		t.Skipf("this filesystem does not open .ssh for .ſsh: %q", res.Content)
	}
	child := &credentialChild{calls: []provider.ToolCall{childCall("r1", "read_file", `{"path":".ſsh/id_rsa"}`)}}
	d, err := NewDispatch(child, agent.ContextManager{}, NewFileToolsForWorkspace(ws), DispatchLimits{}, defaultInvariants(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Invoke(context.Background(), json.RawMessage(`{"tasks":["inspect"]}`))
	if err != nil || out.IsError {
		t.Fatalf("dispatch = %+v, %v", out, err)
	}
	if obs := child.observations()["r1"]; !strings.Contains(obs, "tool call blocked by interceptor invariants (credential_path)") || strings.Contains(obs, "key-secret") {
		t.Fatalf("child alias read = %q, want the credential_path block", obs)
	}
}

// credentialChild scripts a dispatch child: its first turn issues calls, the
// next answers. It records every request so a test reads the observations the
// child actually received.
type credentialChild struct {
	calls []provider.ToolCall
	mu    sync.Mutex
	reqs  []provider.ChatRequest
}

func (c *credentialChild) Chat(_ context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	outcome := &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}, RouteOutcome: outcome}, nil
		}
	}
	return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: c.calls}, RouteOutcome: outcome}, nil
}

// observations maps each answered call id to the tool message the child saw.
func (c *credentialChild) observations() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for _, req := range c.reqs {
		for _, m := range req.Messages {
			if m.Role == "tool" {
				out[m.ToolCallID] = m.Content
			}
		}
	}
	return out
}

func childCall(id, name, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Type: "function", Function: provider.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

func defaultInvariants(t *testing.T) agent.Interceptor {
	t.Helper()
	inv, err := interceptor.NewInvariants(interceptor.DefaultInvariants())
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// TestDispatchChildSearchSkipsCredentials (#627 T9): an unscoped dispatch
// child shares the parent's search, so it skips the same files. The allowed
// line proves the search ran, so the missing secrets are not vacuous. The
// default invariants still block the child's widened-set read.
func TestDispatchChildSearchSkipsCredentials(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"notes.txt":   "CHILD-627 allowed\n",
		".env":        "CHILD-627 env-secret\n",
		".netrc":      "CHILD-627 netrc-secret\n",
		".ssh/id_rsa": "CHILD-627 key-secret\n",
	})
	child := &credentialChild{calls: []provider.ToolCall{
		childCall("s1", "search", `{"pattern":"CHILD-627"}`),
		childCall("r1", "read_file", `{"path":".netrc"}`),
	}}
	d, err := NewDispatch(child, agent.ContextManager{}, NewFileToolsForWorkspace(mustWorkspace(t, root)), DispatchLimits{}, defaultInvariants(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Invoke(context.Background(), json.RawMessage(`{"tasks":["inspect"]}`))
	if err != nil || out.IsError {
		t.Fatalf("dispatch = %+v, %v", out, err)
	}
	obs := child.observations()
	if !strings.Contains(obs["s1"], "notes.txt:1: CHILD-627 allowed") {
		t.Fatalf("child search observation = %q, want the allowed line", obs["s1"])
	}
	if !strings.Contains(obs["r1"], "tool call blocked by interceptor invariants (credential_path)") {
		t.Fatalf("child .netrc read = %q, want the credential_path block", obs["r1"])
	}
	for id, o := range obs {
		if strings.Contains(o, "-secret") {
			t.Fatalf("observation %s carried credential content: %q", id, o)
		}
	}
}
