package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/contextdepth"
	"github.com/kstruzzieri/go-llm/internal/secretscan/secrettest"
	"github.com/kstruzzieri/go-llm/mcpclient"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// The #517 Secrets workflow table (spec §5.5): a real agent.Orchestrator built
// by Golem's own factory (newOrchestratorFactoryChain, through the containment
// fixture), real tools in temp workspaces and a scripted model, run once under
// the guards-only chain G and once under guards plus Secrets G+S.
//
// No live credentials; generated fixtures minimize committed secret-shaped
// material; GitHub enforcement is verified separately. Every secret-shaped
// value below is expanded at test time by secrettest.Generate, written only
// into temp workspaces, and never printed: results carry rule names, not
// values.

// secretsRowResult is what one row records under one arm.
//   - FirstBoundary: what stopped the value first: "guard" (an always-on
//     guard refused the call), "secrets", "indexer" (rag scrubbed it at index
//     time, outside the hooks), or "" when nothing did.
//   - Hook: the agent hook where that boundary acted ("input", "output",
//     "tool_call"); "" when nothing acted or the boundary is outside the hooks.
//   - RunOutcome: "terminal" for a run a block ended, otherwise the run's
//     StopReason ("completed", "tool_error_cap_reached"). The advisory preview
//     row (13a) records "staged" or "refused"; the dispatch row (10) records
//     the child's outcome.
//   - ParentOutcome: the dispatch row's parent run, "continued" or "ended";
//     "" elsewhere.
//   - Message: the user- or model-visible text of the block: one of the four
//     pinned texts (see pinnedMessage), "" when nothing was blocked. Row 12
//     appends whether the blocked text had already streamed to the user; row
//     9a appends whether replacing a mixed result was protection or an
//     overblock.
//   - ContinuationSucceeds: spec §5.5 "prescribed continuation succeeds", on
//     every row: the row's completion probe (file bytes, a second run, the
//     delivered answer).
//   - Delivered: see the field.
type secretsRowResult struct {
	FirstBoundary, Hook, RunOutcome, ParentOutcome, Message string
	ContinuationSucceeds                                    bool
	// Delivered is meaningful only on the protection rows 14a-c (BC4): the
	// surrogate reached its sink, read from the serialized next wire request
	// (14a), the file bytes (14b) or the server-recorded MCP argument (14c).
	// Every other row leaves it false.
	Delivered bool
}

// secretsRow is one workflow row. ID is the spec §5.5 row number, lettered
// where the row's variants end differently; Class is the spec §5.2 class of
// the value the row carries, assigned by role.
type secretsRow struct {
	ID, Class, Surface string
	Run                func(t *testing.T, chain []agent.Interceptor) secretsRowResult
}

// secretsArms filters interceptor.Defaults() in library order (spec F4),
// never reordered and never with the canary: G = Invariants, Egress,
// ChildScopeDenials; G+S adds Secrets in its library position.
func secretsArms() (g, gs []agent.Interceptor) {
	for _, ic := range interceptor.Defaults() {
		switch ic.(type) {
		case interceptor.Invariants, interceptor.Egress, interceptor.ChildScopeDenials:
			g = append(g, ic)
			gs = append(gs, ic)
		case interceptor.Secrets:
			gs = append(gs, ic)
		}
	}
	return g, gs
}

// surrogateParams are secrettest generator params; provider prefixes are split
// so no whole prefix is committed (spec §5.1).
var surrogateParams = map[string]struct {
	gen    string
	params map[string]string
}{
	"openai":        {"provider", map[string]string{"prefix_a": "s", "prefix_b": "k-", "len": "40", "charset": "alnum"}},
	"npm":           {"provider", map[string]string{"prefix_a": "n", "prefix_b": "pm_", "len": "36", "charset": "alnum"}},
	"pem":           {"pem", map[string]string{"label": "PRIVATE KEY", "complete": "true"}},
	"pem-truncated": {"pem", map[string]string{"label": "PRIVATE KEY", "complete": "false"}},
	"bearer":        {"bearer", map[string]string{"len": "32", "entropy": "high"}},
}

// surrogate expands one named surrogate at test time. Never print the value.
func surrogate(t *testing.T, name string) string {
	t.Helper()
	s, ok := surrogateParams[name]
	if !ok {
		t.Fatalf("unknown surrogate %q", name)
	}
	p := maps.Clone(s.params)
	p["seed"] = "517-workflow-" + name
	v, err := secrettest.Generate(s.gen, p)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// rowFixture is one row's workspace: the containment fixture (Golem's write
// and exec tools, its factory) with the arm's chain, the read-only file tools,
// and the /auto-edits grant so writes never prompt; approval is not what these
// rows measure. root "" puts the workspace under a fresh temp dir.
func rowFixture(t *testing.T, root string, opts fixtureOpts, files map[string]string) *fixture {
	t.Helper()
	if root == "" {
		root = filepath.Join(t.TempDir(), "ws")
	}
	for rel, content := range files {
		writeWorkspaceFile(t, root, rel, content)
	}
	opts.autoEdits = true
	f := newFixtureAt(t, root, opts)
	readers, err := buildTools(f.Root, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tools = append(readers, f.tools...)
	return f
}

func answer(text string) agent.ModelResult {
	return agent.ModelResult{Response: provider.ChatResponse{Content: text, Done: true}}
}

// done is the prescribed continuation most rows script after their event.
func done() agent.ModelResult {
	return call("write_file", map[string]string{"path": "DONE.txt", "content": "done\n"})
}

func (f *fixture) continued(t *testing.T) bool {
	t.Helper()
	return readFile(t, filepath.Join(f.Root, "DONE.txt")) == "done\n"
}

// wireRecorder keeps every request the scripted model receives, as sent.
type wireRecorder struct {
	agent.ModelCaller
	reqs []provider.ChatRequest
}

func (w *wireRecorder) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	w.reqs = append(w.reqs, req)
	return w.ModelCaller.Chat(ctx, req, onToken)
}

// request returns the i-th wire request (0 is the first model call).
func (w *wireRecorder) request(t *testing.T, i int) provider.ChatRequest {
	t.Helper()
	if i >= len(w.reqs) {
		t.Fatalf("wire request %d of %d", i, len(w.reqs))
	}
	return w.reqs[i]
}

// script runs calls, then a final answer, through the arm's orchestrator.
// scriptCaller streams each response through onToken before returning it.
func (f *fixture) script(t *testing.T, goal, answers string, calls ...agent.ModelResult) (agent.Result, *wireRecorder, error) {
	t.Helper()
	w := &wireRecorder{ModelCaller: &scriptCaller{responses: append(calls, answer("done"))}}
	res, err := f.runRequest(t, w, agent.Request{Goal: goal, Tools: f.tools, Approver: f.approver(answers), MaxSteps: len(calls) + 1}, nil)
	return res, w, err
}

// observeRun reads a row result off one Run: the first block-or-higher
// finding on Result.Risk names the boundary and hook; a run error must be an
// interceptor block (anything else fails the row); Message is Golem's own
// failure text for a terminal block (runFailureMessage), else the first
// replaced observation.
func observeRun(t *testing.T, res agent.Result, err error) secretsRowResult {
	t.Helper()
	// Validity: a call that failed inside the tool (a missing directory, a
	// bad path) would otherwise read as a clean "completed" row.
	for _, rec := range res.ToolCalls {
		if rec.IsError && !rec.Blocked {
			t.Fatalf("%s failed in the tool, not at an interceptor: the row's workflow is broken", rec.Name)
		}
	}
	var r secretsRowResult
	var blocked *agent.BlockedError
	switch {
	case err == nil:
		r.RunOutcome = res.StopReason.String()
	case errors.As(err, &blocked):
		r.RunOutcome, r.Message = "terminal", runFailureMessage("", err)
	default:
		t.Fatalf("run failed outside the interceptor chain: %v", err)
	}
	if res.Risk != nil {
		r.FirstBoundary, r.Hook = firstBlock(res.Risk.Findings)
	}
	if r.Message == "" {
		r.Message = blockedLine(res.Messages)
	}
	return r
}

func firstBlock(findings []agent.Finding) (boundary, hook string) {
	for _, fd := range findings {
		if fd.Verdict >= agent.VerdictBlock {
			return boundaryOf(fd.Interceptor), fd.Hook.String()
		}
	}
	return "", ""
}

// boundaryOf names who acted: Secrets, or one of G's guards.
func boundaryOf(interceptorName string) string {
	if interceptorName == "secrets" {
		return "secrets"
	}
	return "guard"
}

// blockedLine returns the first model-visible block line in msgs' tool
// observations: a replaced result (agent/interceptor.go:803, also appended
// to a verifier anchor) or a blocked call (:767).
func blockedLine(msgs []provider.ChatMessage) string {
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		for line := range strings.SplitSeq(m.Content, "\n") {
			if strings.HasPrefix(line, "tool result blocked by interceptor ") || strings.HasPrefix(line, "tool call blocked by interceptor ") {
				return line
			}
		}
	}
	return ""
}

// wireCarries reports whether the serialized request holds s (ruling R23):
// the probe reads the bytes a provider would receive, with s JSON-escaped the
// same way, never the agent transcript.
func wireCarries(t *testing.T, req provider.ChatRequest, s string) bool {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(b, enc[1:len(enc)-1])
}

// readThenContinue reads paths with read_file, then writes DONE.txt.
func readThenContinue(t *testing.T, chain []agent.Interceptor, files map[string]string, paths ...string) secretsRowResult {
	t.Helper()
	f := rowFixture(t, "", fixtureOpts{chain: chain}, files)
	var calls []agent.ModelResult
	for _, p := range paths {
		calls = append(calls, call("read_file", map[string]string{"path": p}))
	}
	res, _, err := f.script(t, "inspect the files", "", append(calls, done())...)
	r := observeRun(t, res, err)
	r.ContinuationSucceeds = f.continued(t)
	return r
}

// mkdirs creates workspace directories a real repository would already hold;
// write_file refuses a missing parent (agent/tools/paths.go errParentMissing).
func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// writeThenProbe has the model write content to path; the probe is the file
// holding exactly those bytes.
func writeThenProbe(t *testing.T, chain []agent.Interceptor, path, content string) secretsRowResult {
	t.Helper()
	f := rowFixture(t, "", fixtureOpts{chain: chain}, nil)
	mkdirs(t, f.Root, filepath.Dir(path))
	res, _, err := f.script(t, "write the file", "", call("write_file", map[string]string{"path": path, "content": content}))
	r := observeRun(t, res, err)
	r.ContinuationSucceeds = readFile(t, filepath.Join(f.Root, path)) == content
	return r
}

// answerRun returns resp as the model's only turn; the probe is the answer
// reaching the caller.
func answerRun(t *testing.T, chain []agent.Interceptor, resp provider.ChatResponse, obs agent.Observer) secretsRowResult {
	t.Helper()
	f := rowFixture(t, "", fixtureOpts{chain: chain}, nil)
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: resp}}}
	res, err := f.runRequest(t, caller, agent.Request{Goal: "what key does the deploy config use?", Tools: f.tools, Approver: f.approver(""), MaxSteps: 1}, obs)
	r := observeRun(t, res, err)
	r.ContinuationSucceeds = err == nil && res.Answer == resp.Content
	return r
}

func unitTestWithKey(key string) string {
	return "package client\n\nimport \"testing\"\n\n// fakeKey is a test value for the auth header.\nconst fakeKey = \"" + key + "\"\n\n" +
		"func TestAuthHeader(t *testing.T) {\n\tif got := authHeader(fakeKey); got != \"Bearer \"+fakeKey {\n\t\tt.Fatalf(\"authHeader = %q\", got)\n\t}\n}\n"
}

func npmrcWith(token string) string {
	return "registry=https://registry.npmjs.org/\n//registry.npmjs.org/:_authToken=" + token + "\n"
}

const sshConfig = "Host staging\n    HostName staging.example.com\n    User deploy\n    IdentityFile ~/.ssh/deploy_ed25519\n"

// tokenRecorder is the display: it keeps every streamed content delta.
type tokenRecorder struct{ shown strings.Builder }

func (*tokenRecorder) OnStep(context.Context, agent.StepEvent) error         { return nil }
func (*tokenRecorder) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }
func (r *tokenRecorder) OnToken(_ context.Context, e agent.TokenEvent) error {
	r.shown.WriteString(e.Content)
	return nil
}

// mixedRetrieve stands in for a retrieval backend whose index was never
// scrubbed (row 9b runs the real index): one registered group whose verbatim
// alternatives are alts, the first doubling as the fallback content, the
// shape cmd/llm-bench/census.go's mixed carrier uses.
type mixedRetrieve struct{ alts []string }

func (mixedRetrieve) Spec() agent.ToolSpec { return agenttools.Retrieve{}.Spec() }
func (mixedRetrieve) Effect() agent.Effect { return agenttools.Retrieve{}.Effect() }
func (mixedRetrieve) Origin() agent.Origin { return agenttools.Retrieve{}.Origin() }
func (m mixedRetrieve) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	verbatim := contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{{Depth: contextdepth.DepthL2, Kind: contextdepth.RepresentationVerbatim}}}
	group := agent.ContextGroup{Desc: contextdepth.GroupDesc{Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "docs/billing.md"}}}
	for _, a := range m.alts {
		group.Alternatives = append(group.Alternatives, agent.ContextAlternative{Desc: verbatim, Content: a})
	}
	return agent.ToolResult{Content: m.alts[0], Context: &agent.ContextSet{Groups: []agent.ContextGroup{group}}}, nil
}

// mixedRetrieval runs retrieve (mixedRetrieve over alts, mixed assembly on)
// then the DONE continuation, and returns the row result with a probe over the
// serialized request that follows the retrieval.
func mixedRetrieval(t *testing.T, chain []agent.Interceptor, alts []string) (secretsRowResult, func(string) bool) {
	t.Helper()
	f := rowFixture(t, "", fixtureOpts{chain: chain, progressive: true}, nil)
	f.tools = append(f.tools, mixedRetrieve{alts: alts})
	res, w, err := f.script(t, "how does the billing client deploy?", "", call("retrieve", map[string]any{"query": "billing client deploy"}), done())
	r := observeRun(t, res, err)
	r.ContinuationSucceeds = f.continued(t)
	next := w.request(t, 1)
	return r, func(s string) bool { return wireCarries(t, next, s) }
}

// withoutSecrets is chain minus Secrets, order kept: the counterfactual a
// G+S row compares against.
func withoutSecrets(chain []agent.Interceptor) []agent.Interceptor {
	var out []agent.Interceptor
	for _, ic := range chain {
		if _, ok := ic.(interceptor.Secrets); !ok {
			out = append(out, ic)
		}
	}
	return out
}

// rowEmbedder is index_test.go's fake embedder: one fixed vector in one
// vector space, so the indexer and the retriever agree.
func rowEmbedder(vsid string) rag.Embedder {
	return rag.EmbedderFunc(func(_ context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
		vecs := make([][]float64, len(inputs))
		for i := range vecs {
			vecs[i] = realisticTestVector()
		}
		return rag.EmbedResult{Embeddings: vecs, Model: "nomic", Provider: "ollama", VectorSpaceID: vsid}, nil
	})
}

// git runs one setup command in dir with no user or system config.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=w", "-c", "user.email=w@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func testAdvisory(content string) agent.Advisory {
	sum := sha256.Sum256([]byte(content))
	return agent.Advisory{Source: "codex", Tool: "codex 0.0.0", Model: "gpt-test", Digest: hex.EncodeToString(sum[:]), Content: content, Origin: agent.OriginModel}
}

// secretsWorkflowRows are the 14 rows of spec §5.5, one orchestrator row per
// distinct hook surface (carrier variety lives in the §5.1 case table).
func secretsWorkflowRows() []secretsRow {
	return []secretsRow{
		{ID: "1", Class: string(secrettest.BenignLookalike), Surface: "tool observation: read_file of .env.example, .env.sample, .env.template, .env.dist (placeholders)",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				files := map[string]string{
					".env.example":  "# Copy to .env and fill in real values.\nOPENAI_API_KEY=changeme\nDATABASE_URL=postgres://app@localhost:5432/app\n",
					".env.sample":   "API_TOKEN=<API_TOKEN>\nLOG_LEVEL=info\n",
					".env.template": "SECRET=${SECRET}\nPORT=8080\n",
					".env.dist":     "NPM_TOKEN=YOUR_NPM_TOKEN\nREGION=us-east-1\n",
				}
				return readThenContinue(t, chain, files, ".env.example", ".env.sample", ".env.template", ".env.dist")
			}},
		{ID: "2a", Class: string(secrettest.SyntheticFixture), Surface: "tool observation: read_file of a complete testdata PEM",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				return readThenContinue(t, chain, map[string]string{"testdata/server_key.pem": surrogate(t, "pem") + "\n"}, "testdata/server_key.pem")
			}},
		{ID: "2b", Class: string(secrettest.SyntheticFixture), Surface: "tool observation: read_file of a truncated testdata PEM (no END line)",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				return readThenContinue(t, chain, map[string]string{"testdata/server_key.pem": surrogate(t, "pem-truncated") + "\n"}, "testdata/server_key.pem")
			}},
		{ID: "2c", Class: string(secrettest.SyntheticFixture), Surface: "model tool-call arguments: write_file of a complete testdata PEM",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				return writeThenProbe(t, chain, "testdata/client_key.pem", surrogate(t, "pem")+"\n")
			}},
		{ID: "2d", Class: string(secrettest.SyntheticFixture), Surface: "model tool-call arguments: write_file of a truncated testdata PEM",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				return writeThenProbe(t, chain, "testdata/client_key.pem", surrogate(t, "pem-truncated")+"\n")
			}},
		{ID: "3a", Class: string(secrettest.SyntheticFixture), Surface: "model tool-call arguments: write_file of a unit test holding a fake provider token; probe: the test file",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				return writeThenProbe(t, chain, "client/auth_test.go", unitTestWithKey(surrogate(t, "openai")))
			}},
		{ID: "3b", Class: string(secrettest.SyntheticFixture), Surface: "model tool-call arguments: one response writing a clean sibling and the token unit test; probe: the sibling",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				f := rowFixture(t, "", fixtureOpts{chain: chain}, nil)
				mkdirs(t, f.Root, "client")
				const sibling = "// Package client talks to the billing API.\npackage client\n"
				calls := append(call("write_file", map[string]string{"path": "client/doc.go", "content": sibling}).Response.ToolCalls,
					call("write_file", map[string]string{"path": "client/auth_test.go", "content": unitTestWithKey(surrogate(t, "openai"))}).Response.ToolCalls...)
				res, _, err := f.script(t, "add the client package and its test", "", agent.ModelResult{Response: provider.ChatResponse{ToolCalls: calls}})
				r := observeRun(t, res, err)
				r.ContinuationSucceeds = readFile(t, filepath.Join(f.Root, "client/doc.go")) == sibling
				return r
			}},
		{ID: "4", Class: string(secrettest.ExpectedSensitive), Surface: "initial input: the goal pastes a curl command with a bearer token",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				f := rowFixture(t, "", fixtureOpts{chain: chain}, nil)
				goal := "this request fails with 401, why?\ncurl -H 'Authorization: " + surrogate(t, "bearer") + "' https://api.example.com/v1/reports"
				res, _, err := f.script(t, goal, "", done())
				r := observeRun(t, res, err)
				r.ContinuationSucceeds = f.continued(t)
				return r
			}},
		{ID: "5a", Class: string(secrettest.ExpectedSensitive), Surface: "model content: the answer quotes a deploy key",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				return answerRun(t, chain, provider.ChatResponse{Content: "The deploy config uses " + surrogate(t, "openai") + "; rotate it.", Done: true}, nil)
			}},
		{ID: "5b", Class: string(secrettest.ExpectedSensitive), Surface: "model thinking: the reasoning quotes a deploy key, the answer is clean",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				return answerRun(t, chain, provider.ChatResponse{Thinking: "the config line reads " + surrogate(t, "openai"), Content: "Rotate the deploy key.", Done: true}, nil)
			}},
		{ID: "6", Class: string(secrettest.SyntheticFixture), Surface: "exec observation: git diff, git log -p, git show over a committed test fixture",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				if !hostHasCapability(capExec) {
					t.Skip("exec runner unsupported on this host")
				}
				if _, err := exec.LookPath("git"); err != nil {
					t.Skip("git not on PATH")
				}
				f := rowFixture(t, "", fixtureOpts{chain: chain}, map[string]string{"README.md": "billing client\n"})
				git(t, f.Root, "init", "-q")
				git(t, f.Root, "add", "README.md")
				git(t, f.Root, "commit", "-qm", "readme")
				writeWorkspaceFile(t, f.Root, "client/auth_test.go", unitTestWithKey(surrogate(t, "openai")))
				git(t, f.Root, "add", "client/auth_test.go")
				git(t, f.Root, "commit", "-qm", "auth test")
				var calls []agent.ModelResult
				for _, argv := range [][]string{{"git", "diff", "HEAD~1", "HEAD"}, {"git", "log", "-p", "-1"}, {"git", "show", "HEAD"}} {
					calls = append(calls, call("run_command", map[string]any{"argv": argv}))
				}
				res, _, err := f.script(t, "review the last commit", "y\ny\ny\n", append(calls, done())...)
				r := observeRun(t, res, err)
				r.ContinuationSucceeds = f.continued(t)
				return r
			}},
		// Row 7 reads one path per variant: observeRun records the first block
		// only, so a second path in the same run could lose its refusal unseen.
		{ID: "7a", Class: string(secrettest.ExpectedSensitive), Surface: "tool-call guard: read_file of .npmrc (credential basename)",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				return readThenContinue(t, chain, map[string]string{".npmrc": npmrcWith(surrogate(t, "npm"))}, ".npmrc")
			}},
		{ID: "7b", Class: string(secrettest.Benign), Surface: "tool-call guard: read_file of .ssh/config (credential directory; IdentityFile path only)",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				return readThenContinue(t, chain, map[string]string{".ssh/config": sshConfig}, ".ssh/config")
			}},
		{ID: "7c", Class: string(secrettest.SyntheticFixture), Surface: "tool observation: read_file of npmrc.fixture",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				return readThenContinue(t, chain, map[string]string{"npmrc.fixture": npmrcWith(surrogate(t, "npm"))}, "npmrc.fixture")
			}},
		{ID: "7d", Class: string(secrettest.Benign), Surface: "tool observation: read_file of testdata/ssh_config (IdentityFile path only)",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				return readThenContinue(t, chain, map[string]string{"testdata/ssh_config": sshConfig}, "testdata/ssh_config")
			}},
		{ID: "8", Class: string(secrettest.SyntheticFixture), Surface: "verifier observation: a failing test prints a snapshot holding a fake token after a write",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				if !hostHasCapability(capExec) || !hostHasCapability(capBash) {
					t.Skip("verifier needs the exec runner and bash")
				}
				root := filepath.Join(t.TempDir(), "ws")
				writeWorkspaceFile(t, root, "tests.sh", "#!/usr/bin/env bash\necho '--- FAIL: TestRender (0.00s)'\necho '    render_test.go:12: snapshot mismatch, want:'\ncat testdata/render.snap\nexit 1\n")
				writeWorkspaceFile(t, root, "testdata/render.snap", "endpoint: https://api.example.com\nkey: "+surrogate(t, "openai")+"\n")
				ws, err := agenttools.NewWorkspace(root)
				if err != nil {
					t.Fatal(err)
				}
				cmd, err := agenttools.NewVerifyCommand(ws, []string{"bash", "tests.sh"}, "", 30*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				slot := newTestVerifierSlot(t, root)
				if err := slot.Set(cmd); err != nil {
					t.Fatal(err)
				}
				f := rowFixture(t, root, fixtureOpts{chain: chain, verifier: slot}, nil)
				const render = "package render\n\nfunc Render() string { return \"ok\" }\n"
				res, _, err := f.script(t, "fix the renderer", "a\n", call("write_file", map[string]string{"path": "render.go", "content": render}), done())
				r := observeRun(t, res, err)
				r.ContinuationSucceeds = readFile(t, filepath.Join(f.Root, "render.go")) == render && f.continued(t)
				return r
			}},
		{ID: "9a", Class: string(secrettest.ExpectedSensitive), Surface: "mixed-context alternatives: retrieval with one sensitive alternative (not the one assembly selects) among clean ones",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				alts := []string{
					"The billing client retries 429 responses with backoff.",
					"Deploy with the service key " + surrogate(t, "openai") + ".",
					"Invoices are generated nightly.",
				}
				r, wire := mixedRetrieval(t, chain, alts)
				carried := func(w func(string) bool) [3]bool { return [3]bool{w(alts[0]), w(alts[1]), w(alts[2])} }
				if !strings.HasPrefix(r.Message, "tool result blocked by interceptor ") {
					// Accepted: the wire carries exactly the alternative assembly
					// selects. mixed_alloc.go admits alternative 0, then upgrades to
					// the cheapest affordable later-declared one (step 5): the third,
					// shorter than the second, with nothing declared after it. So the
					// sensitive second alternative never reaches the model.
					if got := carried(wire); got != [3]bool{false, false, true} {
						t.Fatalf("accepted mixed result: wire carries alternatives %v, want the third only", got)
					}
					return r
				}
				// Replaced: the blocked line is all that reaches the wire.
				if got := carried(wire); !wire(r.Message) || got != [3]bool{} {
					t.Fatalf("replaced mixed result: wire carries the blocked line %v and alternatives %v, want the line only", wire(r.Message), got)
				}
				// Protection or overblock: would the same result, assembled
				// without Secrets, have put the sensitive alternative on the wire?
				if _, shadow := mixedRetrieval(t, withoutSecrets(chain), alts); shadow(alts[1]) {
					r.Message += " (protection: assembly delivers the sensitive alternative)"
				} else {
					r.Message += " (overblock: assembly never delivers the sensitive alternative)"
				}
				return r
			}},
		{ID: "9b", Class: string(secrettest.ExpectedSensitive), Surface: "index time (outside the hooks): real rag indexing of a doc holding a key, then progressive retrieval",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				key := surrogate(t, "openai")
				f := rowFixture(t, "", fixtureOpts{chain: chain, progressive: true}, map[string]string{
					"docs/billing.md": "The billing client retries 429 responses with backoff.\n",
					"docs/deploy.md":  "Deploy with the service key " + key + ".\n",
				})
				const vsid = "ollama/nomic"
				store, idx := buildTestIndexer(t, filepath.Join(t.TempDir(), "index.db"), vsid)
				t.Cleanup(func() { _ = store.Close() })
				scrubbed := false
				for _, rel := range []string{"docs/billing.md", "docs/deploy.md"} {
					st, err := idx.IndexFileWithStatus(context.Background(), filepath.Join(f.Root, rel))
					if err != nil && !rag.IsSafeIndexSkip(err) {
						t.Fatalf("index %s: %v", rel, err)
					}
					scrubbed = scrubbed || len(st.PolicyOutcomes) > 0
				}
				retr, err := rag.NewRetrieverWithEmbedder(rowEmbedder(vsid), store, rag.WithRetrieverModel(vsid))
				if err != nil {
					t.Fatal(err)
				}
				// Golem's retrieve tool shape (cmd/golem/tools.go), progressive.
				f.tools = append(f.tools, &agenttools.Retrieve{R: retr, K: 5, MaxTokens: 2048, Progressive: true})
				res, w, err := f.script(t, "how does the billing client deploy?", "", call("retrieve", map[string]any{"query": "billing client deploy"}), done())
				r := observeRun(t, res, err)
				// The indexer is the first effective boundary only if its scrub
				// held: it reported a policy outcome and the value never reached
				// the model (the request that follows the retrieval).
				if r.FirstBoundary == "" && scrubbed && !wireCarries(t, w.request(t, 1), key) {
					r.FirstBoundary = "indexer"
				}
				r.ContinuationSucceeds = f.continued(t)
				return r
			}},
		{ID: "10", Class: string(secrettest.SyntheticFixture), Surface: "dispatch child: a read-only child reads a fixture holding a fake token",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				f := rowFixture(t, "", fixtureOpts{chain: chain}, map[string]string{"testdata/client_fixture.txt": "key: " + surrogate(t, "openai") + "\n"})
				readers, err := buildTools(f.Root, nil)
				if err != nil {
					t.Fatal(err)
				}
				child := &scriptCaller{responses: []agent.ModelResult{
					routed(call("read_file", map[string]string{"path": "testdata/client_fixture.txt"})),
					routed(answer("the fixture holds one client key line")),
				}}
				d, err := agenttools.NewDispatch(child, agent.ContextManager{}, readers, agenttools.DispatchLimits{}, chain...)
				if err != nil {
					t.Fatal(err)
				}
				f.tools = append(f.tools, d)
				dispatchCall, dispatchID := callID(agenttools.DispatchToolName, map[string]any{"tasks": []string{"read testdata/client_fixture.txt"}})
				res, _, err := f.script(t, "inspect the client fixture", "", dispatchCall, done())
				parent := observeRun(t, res, err)
				var r secretsRowResult
				r.ParentOutcome = "ended"
				if parent.RunOutcome == "completed" && f.continued(t) {
					r.ParentOutcome = "continued"
				}
				r.ContinuationSucceeds = f.continued(t)
				var env dispatchTestEnvelope
				if err := json.Unmarshal([]byte(toolObservation(t, res.Messages, dispatchID).Content), &env); err != nil || len(env.Results) != 1 {
					t.Fatalf("dispatch envelope: %v, %d results", err, len(env.Results))
				}
				r.RunOutcome = env.Results[0].StopReason
				// The child's findings stay in the child; its next wire request
				// is where a replaced observation shows.
				if line := blockedLine(child.lastRequest.Messages); line != "" {
					r.Message = line
					name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(line, "tool result blocked by interceptor "), "tool call blocked by interceptor "), " ")
					r.FirstBoundary, r.Hook = boundaryOf(name), "input"
					if strings.HasPrefix(line, "tool call blocked") {
						r.Hook = "tool_call"
					}
				}
				return r
			}},
		{ID: "11", Class: string(secrettest.SyntheticFixture), Surface: "history (resume): a persisted answer quoting a fixture token, resumed twice",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				f := rowFixture(t, "", fixtureOpts{chain: chain}, nil)
				history := []provider.ChatMessage{
					{Role: "user", Content: "what is in testdata/client_fixture.txt?"},
					{Role: "assistant", Content: "It holds one line: key: " + surrogate(t, "openai")},
				}
				resume := func(marker string) (agent.Result, error) {
					caller := &scriptCaller{responses: []agent.ModelResult{call("write_file", map[string]string{"path": marker, "content": "done\n"}), answer("done")}}
					return f.runRequest(t, caller, agent.Request{Goal: "continue where we left off", History: history, Tools: f.tools, Approver: f.approver(""), MaxSteps: 2}, nil)
				}
				res, err := resume("DONE.txt")
				r := observeRun(t, res, err)
				res2, err2 := resume("DONE-2.txt")
				second := observeRun(t, res2, err2)
				r.ContinuationSucceeds = second.RunOutcome == "completed" && readFile(t, filepath.Join(f.Root, "DONE-2.txt")) == "done\n"
				return r
			}},
		{ID: "12", Class: string(secrettest.ExpectedSensitive), Surface: "streamed model content: the answer quoting a key streams through onToken before the response",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				key := surrogate(t, "openai")
				display := &tokenRecorder{}
				r := answerRun(t, chain, provider.ChatResponse{Content: "The deploy config uses " + key + ".", Done: true}, display)
				if r.RunOutcome == "terminal" {
					if strings.Contains(display.shown.String(), key) {
						r.Message += " (blocked after display)"
					} else {
						r.Message += " (never displayed)"
					}
				}
				return r
			}},
		{ID: "13a", Class: string(secrettest.ExpectedSensitive), Surface: "advisory preview: InspectAdvisory on a consult answer quoting a key, then the goal runs",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				f := rowFixture(t, "", fixtureOpts{chain: chain}, nil)
				var r secretsRowResult
				var staged *agent.Advisory
				adv, err := f.orchestrator(t, &scriptCaller{}).InspectAdvisory(context.Background(), testAdvisory("Rotate the leaked key "+surrogate(t, "openai")+" before deploying."))
				var blocked *agent.BlockedError
				switch {
				case err == nil:
					r.RunOutcome, staged = "staged", &adv
				case errors.As(err, &blocked):
					r.FirstBoundary, r.Hook = firstBlock(blocked.Findings)
					r.RunOutcome, r.Message = "refused", consultFailureLine(err)
				default:
					t.Fatalf("advisory preview failed outside the chain: %v", err)
				}
				caller := &scriptCaller{responses: []agent.ModelResult{done(), answer("done")}}
				res, err := f.runRequest(t, caller, agent.Request{Goal: "apply the advice", Advisory: staged, Tools: f.tools, Approver: f.approver(""), MaxSteps: 2}, nil)
				r.ContinuationSucceeds = err == nil && res.StopReason == agent.Completed && f.continued(t)
				return r
			}},
		{ID: "13b", Class: string(secrettest.ExpectedSensitive), Surface: "staged advisory: Run re-inspects an advisory quoting a key",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				f := rowFixture(t, "", fixtureOpts{chain: chain}, nil)
				adv := testAdvisory("Rotate the leaked key " + surrogate(t, "openai") + " before deploying.")
				caller := &scriptCaller{responses: []agent.ModelResult{done(), answer("done")}}
				res, err := f.runRequest(t, caller, agent.Request{Goal: "apply the advice", Advisory: &adv, Tools: f.tools, Approver: f.approver(""), MaxSteps: 2}, nil)
				r := observeRun(t, res, err)
				r.ContinuationSucceeds = f.continued(t)
				return r
			}},
		{ID: "14a", Class: string(secrettest.ExpectedSensitive), Surface: "protection, tool observation: does the key reach the next wire request?",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				key := surrogate(t, "openai")
				f := rowFixture(t, "", fixtureOpts{chain: chain}, map[string]string{"deploy/service.yaml": "endpoint: https://api.example.com\nkey: " + key + "\n"})
				res, w, err := f.script(t, "check the deploy config", "", call("read_file", map[string]string{"path": "deploy/service.yaml"}), done())
				r := observeRun(t, res, err)
				r.Delivered = wireCarries(t, w.request(t, 1), key) // the request that follows the read
				r.ContinuationSucceeds = f.continued(t)
				return r
			}},
		{ID: "14b", Class: string(secrettest.ExpectedSensitive), Surface: "protection, write arguments: do the key's bytes reach the file?",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				key := surrogate(t, "openai")
				f := rowFixture(t, "", fixtureOpts{chain: chain}, nil)
				mkdirs(t, f.Root, "exports")
				res, _, err := f.script(t, "export the deploy key", "", call("write_file", map[string]string{"path": "exports/creds.txt", "content": "deploy key " + key + "\n"}), done())
				r := observeRun(t, res, err)
				r.Delivered = strings.Contains(readFile(t, filepath.Join(f.Root, "exports/creds.txt")), key)
				r.ContinuationSucceeds = f.continued(t)
				return r
			}},
		{ID: "14c", Class: string(secrettest.ExpectedSensitive), Surface: "protection, MCP arguments: does the key reach the server-recorded argument?",
			Run: func(t *testing.T, chain []agent.Interceptor) secretsRowResult {
				key := surrogate(t, "openai")
				// no t.Parallel anywhere: t.Setenv (as containment M1)
				t.Setenv("XDG_DATA_HOME", t.TempDir())
				pins, err := mcpclient.NewPinStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				srv := gomcp.NewServer(&gomcp.Implementation{Name: "workflow", Version: "1"}, nil)
				var mu sync.Mutex
				var recorded []string
				schema := map[string]any{"type": "object", "properties": map[string]any{"content": map[string]any{"type": "string"}}}
				srv.AddTool(&gomcp.Tool{Name: "record", Description: "Record a note", InputSchema: schema}, func(_ context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
					mu.Lock()
					defer mu.Unlock()
					recorded = append(recorded, string(req.Params.Arguments))
					return &gomcp.CallToolResult{}, nil
				})
				httpServer := httptest.NewServer(gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return srv }, nil))
				t.Cleanup(httpServer.Close)
				mgr, warns, err := mcpclient.Connect(t.Context(), mcpclient.Implementation{Name: "workflow", Version: "1"}, []mcpclient.Server{mcpclient.HTTPServer("notes", httpServer.URL)}, mcpclient.ConnectOptions{Pins: pins})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = mgr.Close() })
				if len(mgr.Tools()) != 1 || mgr.Tools()[0].Spec().Name != "mcp__notes__record" {
					t.Fatalf("want the single adapted tool mcp__notes__record; got %d tools, warnings %v", len(mgr.Tools()), warns)
				}
				f := rowFixture(t, "", fixtureOpts{chain: chain}, nil)
				f.tools = append(f.tools, mgr.Tools()...)
				res, _, err := f.script(t, "save the deploy key to my notes", "y\n", call("mcp__notes__record", map[string]string{"content": "deploy key " + key}), done())
				r := observeRun(t, res, err)
				mu.Lock()
				defer mu.Unlock()
				r.Delivered = slices.ContainsFunc(recorded, func(a string) bool { return strings.Contains(a, key) })
				r.ContinuationSucceeds = f.continued(t)
				return r
			}},
	}
}

// pinnedMessage reports whether m is one of the four pinned user-visible texts
// (spec §5.5, F10), each a prefix because the block texts name the rule.
func pinnedMessage(m string) bool {
	for _, p := range []string{
		"run blocked: sensitive content detected",         // cmd/golem/secrets.go:5
		"tool result blocked by interceptor ",             // agent/interceptor.go:803
		"tool call blocked by interceptor ",               // agent/interceptor.go:767
		"consult failed: blocked by interceptor policy (", // cmd/golem/consult.go:43-50
	} {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// TestSecretsArms pins both arms' members and order against the library
// order (spec F4), not against defaultGuards.
func TestSecretsArms(t *testing.T) {
	names := func(ics []agent.Interceptor) []string {
		var out []string
		for _, ic := range ics {
			out = append(out, ic.Name())
		}
		return out
	}
	g, gs := secretsArms()
	if got, want := names(g), []string{"invariants", "egress", "child_scope_denials"}; !slices.Equal(got, want) {
		t.Errorf("G = %v, want %v", got, want)
	}
	if got, want := names(gs), []string{"invariants", "egress", "secrets", "child_scope_denials"}; !slices.Equal(got, want) {
		t.Errorf("G+S = %v, want %v", got, want)
	}
}

// TestSecretsWorkflowTable runs every row under G and G+S (once per test
// binary, shared with the evidence report: secretsRowRuns) and compares with
// literal expectations taken from spec §3 F9 (where Secrets acts and what
// follows) and F10 (the user-visible texts), never from a run of the code:
//
//	System, goal, history, durable summary   terminal, before assembly
//	Model content or thinking                terminal
//	Model tool-call arguments                terminal at output; the whole
//	                                         response is rejected and clean
//	                                         sibling calls are dropped
//	Tool observation                         recoverable: replaced after the
//	                                         tool's effect
//	Mixed-context alternatives               a finding in any alternative
//	                                         replaces the whole result
//	Verifier observation                     recoverable; the mutation has
//	                                         already been applied
//	Advisory                                 the preview refuses to stage it;
//	                                         re-inspection of a staged advisory
//	                                         ends that run
//	Dispatch child                           handled in the child; the parent
//	                                         continues
//
// plus: three consecutive tool errors stop a run (agent/types.go:16), streamed
// tokens are emitted before output rejection, the RAG indexer scrubs at index
// time independently of the chain, and a credential-path refusal that already
// happens under G is the guard's, not Secrets' (§5.5).
func TestSecretsWorkflowTable(t *testing.T) {
	const (
		blockedRun = "run blocked: sensitive content detected" // F10, secrets.go:5
		resultOpen = "tool result blocked by interceptor secrets (sensitive_openai_token)"
	)
	clean := secretsRowResult{RunOutcome: "completed", ContinuationSucceeds: true}
	terminal := func(hook string) secretsRowResult {
		return secretsRowResult{FirstBoundary: "secrets", Hook: hook, RunOutcome: "terminal", Message: blockedRun}
	}
	replaced := func(msg string) secretsRowResult {
		return secretsRowResult{FirstBoundary: "secrets", Hook: "input", RunOutcome: "completed", Message: msg, ContinuationSucceeds: true}
	}
	guarded := secretsRowResult{FirstBoundary: "guard", Hook: "tool_call", RunOutcome: "completed", Message: "tool call blocked by interceptor invariants (credential_path)", ContinuationSucceeds: true}
	delivered := clean
	delivered.Delivered = true
	want := map[string][2]secretsRowResult{ // [G, G+S]
		// Templates carry no supported shape: nothing acts under either arm.
		"1": {clean, clean},
		// Tool observation: recoverable; the truncated PEM has no END line
		// (F11 PEM rule) so nothing acts.
		"2a": {clean, replaced("tool result blocked by interceptor secrets (sensitive_private_key)")},
		"2b": {clean, clean},
		// Tool-call arguments: terminal at output, the file never written.
		"2c": {clean, terminal("output")},
		"2d": {clean, clean},
		// Row 3 decides K4: terminal at output; the test file is absent (3a)
		// and the clean sibling in the same response is dropped (3b).
		"3a": {clean, terminal("output")},
		"3b": {clean, terminal("output")},
		// Goal: terminal before assembly.
		"4": {clean, terminal("input")},
		// Model content or thinking: terminal.
		"5a": {clean, terminal("output")},
		"5b": {clean, terminal("output")},
		// Exec observations have no credential-path filter: each of the three
		// observations is replaced, and three consecutive tool errors stop
		// the run before the continuation.
		"6": {clean, {FirstBoundary: "secrets", Hook: "input", RunOutcome: "tool_error_cap_reached", Message: resultOpen}},
		// Guard first: each credential-path refusal (basename .npmrc, directory
		// .ssh) happens under G, so it is the first boundary under both arms;
		// the fixture copies reach Secrets, which blocks only the npm token.
		"7a": {guarded, guarded},
		"7b": {guarded, guarded},
		"7c": {clean, replaced("tool result blocked by interceptor secrets (sensitive_npm_token)")},
		"7d": {clean, clean},
		// Verifier observation: recoverable after the mutation (the probe
		// requires render.go written).
		"8": {clean, replaced(resultOpen)},
		// Mixed alternatives: one finding replaces the whole result. Here that
		// is an overblock: assembly selects only the third alternative (the row
		// checks the G wire), so the sensitive second one would never have
		// reached the model.
		"9a": {clean, replaced(resultOpen + " (overblock: assembly never delivers the sensitive alternative)")},
		// The indexer scrubs at index time under both arms.
		"9b": {
			{FirstBoundary: "indexer", RunOutcome: "completed", ContinuationSucceeds: true},
			{FirstBoundary: "indexer", RunOutcome: "completed", ContinuationSucceeds: true},
		},
		// Dispatch child: handled in the child, the parent continues.
		"10": {
			{RunOutcome: "completed", ParentOutcome: "continued", ContinuationSucceeds: true},
			{FirstBoundary: "secrets", Hook: "input", RunOutcome: "completed", ParentOutcome: "continued", Message: resultOpen, ContinuationSucceeds: true},
		},
		// History: terminal before assembly, again on the second resume.
		"11": {clean, terminal("input")},
		// Streaming: the token is emitted before output rejection.
		"12": {clean, {FirstBoundary: "secrets", Hook: "output", RunOutcome: "terminal", Message: blockedRun + " (blocked after display)"}},
		// Advisory: the preview refuses to stage it (the goal still runs,
		// without advice); a staged advisory ends the run.
		"13a": {
			{RunOutcome: "staged", ContinuationSucceeds: true},
			{FirstBoundary: "secrets", Hook: "input", RunOutcome: "refused", Message: "consult failed: blocked by interceptor policy (sensitive_openai_token)", ContinuationSucceeds: true},
		},
		"13b": {clean, terminal("input")},
		// Protection (BC4): under G the surrogate reaches the wire, the file
		// and the MCP server (Delivered); under G+S it reaches none of them.
		// The continuation survives the recoverable observation block (14a)
		// and not the terminal output blocks (14b, 14c).
		"14a": {delivered, replaced(resultOpen)},
		"14b": {delivered, terminal("output")},
		"14c": {delivered, terminal("output")},
	}

	rows := secretsWorkflowRows()
	numbers := map[string]bool{}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.ID] {
			t.Fatalf("duplicate row %s", row.ID)
		}
		seen[row.ID] = true
		numbers[strings.TrimRight(row.ID, "abcd")] = true
		if _, ok := want[row.ID]; !ok {
			t.Errorf("row %s has no expectation", row.ID)
		}
		switch secrettest.Class(row.Class) {
		case secrettest.ExpectedSensitive, secrettest.SyntheticFixture, secrettest.BenignLookalike, secrettest.Benign:
		default:
			t.Errorf("row %s class %q is not a spec §5.2 class", row.ID, row.Class)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("expectation %s has no row", id)
		}
	}
	for n := 1; n <= 14; n++ {
		if !numbers[strconv.Itoa(n)] {
			t.Errorf("spec §5.5 row %d has no workflow row", n)
		}
	}
	if len(numbers) != 14 {
		t.Errorf("rows cover %d spec numbers, want exactly 14", len(numbers))
	}

	compared := map[string]bool{}
	runs := secretsRowRuns(t, func(t *testing.T, row secretsRow, arm int, got secretsRowResult) {
		compared[row.ID+"/"+secretsArmNames[arm]] = true
		if w := want[row.ID][arm]; got != w {
			t.Errorf("row %s under %s:\n got %+v\nwant %+v", row.ID, secretsArmNames[arm], got, w)
		}
		if got.Message != "" && !pinnedMessage(got.Message) {
			t.Errorf("row %s under %s: message %q is not a pinned text", row.ID, secretsArmNames[arm], got.Message)
		}
	})
	// Whichever test ran the rows, every result that ran was compared here
	// (unless a -run subtest filter or a -skip filter left some out).
	filter := func(name string) string {
		if f := flag.Lookup(name); f != nil {
			return f.Value.String()
		}
		return ""
	}
	if !strings.Contains(filter("test.run"), "/") && filter("test.skip") == "" {
		for _, run := range runs {
			for arm, name := range secretsArmNames {
				if run.ran[arm] && !compared[run.row.ID+"/"+name] {
					t.Errorf("row %s under %s ran but was never compared with its expectation", run.row.ID, name)
				}
			}
		}
	}
}

// secretsArmNames are the two arms every workflow row runs under, in the
// order of secretsRowRun's arrays.
var secretsArmNames = [2]string{"G", "G+S"}

// secretsRowRun is one workflow row run under G and G+S. ran is set when the
// arm's run returned without failing (its result is valid); skipped when the
// host skipped it (rows 6 and 8 need the exec runner) or a -run filter left
// it out.
type secretsRowRun struct {
	row          secretsRow
	res          [2]secretsRowResult
	ran, skipped [2]bool
}

var secretsRowsMemo struct {
	sync.Mutex
	runs []secretsRowRun
}

// secretsRowRuns runs every workflow row under G and G+S once per test binary
// and memoizes the results, so TestSecretsWorkflowTable and the evidence
// report share one run (the rows dominate this package's -race time). The
// first caller runs them as its subtests <row>/<arm>. check, if non-nil, runs
// inside a subtest <row>/<arm> on each result that ran: the running subtest,
// or once the memo is filled a fresh one, which fails if the row failed when
// it ran and skips if it was skipped.
func secretsRowRuns(t *testing.T, check func(t *testing.T, row secretsRow, arm int, got secretsRowResult)) []secretsRowRun {
	t.Helper()
	secretsRowsMemo.Lock()
	defer secretsRowsMemo.Unlock()
	fresh := secretsRowsMemo.runs == nil
	if !fresh && check == nil {
		return secretsRowsMemo.runs
	}
	if fresh {
		for _, row := range secretsWorkflowRows() {
			secretsRowsMemo.runs = append(secretsRowsMemo.runs, secretsRowRun{row: row})
		}
	}
	g, gs := secretsArms()
	for i := range secretsRowsMemo.runs {
		run := &secretsRowsMemo.runs[i]
		for arm, name := range secretsArmNames {
			passed := t.Run(run.row.ID+"/"+name, func(t *testing.T) {
				switch {
				case fresh:
					got := run.row.Run(t, [2][]agent.Interceptor{g, gs}[arm])
					if t.Failed() {
						return
					}
					run.res[arm], run.ran[arm] = got, true
				case run.skipped[arm]:
					t.Skipf("row %s under %s did not run when the workflow table ran", run.row.ID, name)
				case !run.ran[arm]:
					t.Fatalf("row %s under %s failed when the workflow table ran in an earlier test; see that test's output", run.row.ID, name)
				}
				if check != nil {
					check(t, run.row, arm, run.res[arm])
				}
			})
			if fresh {
				run.skipped[arm] = passed && !run.ran[arm]
			}
		}
	}
	return secretsRowsMemo.runs
}
