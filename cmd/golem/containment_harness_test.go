package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

func hostHasCapability(c capability) bool {
	switch c {
	case capBash:
		_, err := exec.LookPath("bash")
		return err == nil
	case capSymlink, capUnix, capExec:
		return os.PathSeparator == '/' // exec runner is unsupported on Windows (exec_other.go:13)
	}
	return false
}

type fixtureOpts struct {
	interceptors bool // full chain + canary (D2/D3)
	autoEdits    bool // seed the /auto-edits file grant
	verifier     agent.Verifier
}

type fixture struct {
	Root, Outside string // workspace root and a sibling outside-root directory
	Grants        *approvalGrants
	Out           *strings.Builder
	tools         []agent.Tool
	opts          fixtureOpts
	t             *testing.T
	mgr           *agenttools.BackgroundManager
	journal       *checkpointJournal
}

func newFixture(t *testing.T, opts fixtureOpts) *fixture {
	t.Helper()
	return newFixtureAt(t, filepath.Join(t.TempDir(), "ws"), opts)
}

// newFixtureAt builds the fixture with the workspace at root and the outside
// directory as its sibling. E1's isolated parent uses it because its TMPDIR is
// synthetic and the workspace must live at a path the parent chose.
func newFixtureAt(t *testing.T, root string, opts fixtureOpts) *fixture {
	t.Helper()
	f := &fixture{Root: root, Outside: filepath.Join(filepath.Dir(root), "outside"), Grants: newApprovalGrants(), Out: &strings.Builder{}, opts: opts, t: t}
	for _, d := range []string{f.Root, f.Outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Golem's own seams with zero options, as -allow-write and -allow-exec
	// mount them: the journaled write tools over a per-fixture checkpoint
	// store, and the foreground plus background exec set over one manager.
	store := openTestStore(t, f.Root)
	writeTools, journal, _, err := buildWriteTools(f.Root, store, testStoreGetenv(store))
	if err != nil {
		t.Fatal(err)
	}
	f.tools, f.journal = append(f.tools, writeTools...), journal
	mgr, exe, err := buildExecMount(f.Root, agenttools.SandboxConfig{}, agenttools.ExecToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Shutdown)
	f.tools, f.mgr = append(f.tools, exe...), mgr
	if opts.autoEdits {
		f.Grants.grant(grantScopeFiles, agenttools.WriteClassApprovalKey)
	}
	return f
}

// approver returns the REPL approver reading answers ("y\n", "N\n", "a\n").
func (f *fixture) approver(answers string) *replApprover {
	ap := newReplApprover(newScannerSource(strings.NewReader(answers), f.Out), f.Out, false)
	ap.grants = f.Grants
	return ap
}

// orchestrator is the ONE construction path every runner uses, so the
// fixture's interceptor chain, canary binding and verifier are always
// honored (a second construction path silently dropped Secrets in review).
func (f *fixture) orchestrator(t *testing.T, caller agent.ModelCaller) *agent.Orchestrator {
	t.Helper()
	var canary *canaryBinding
	if f.opts.interceptors {
		canary = testCanaryBinding(t)
	}
	return newOrchestratorFactory(caller, flags{allowWrite: true, allowExec: true, interceptors: f.opts.interceptors}, f.opts.verifier, canary)()
}

// runWith drives one run of the real Golem factory as a REPL turn does: the
// checkpoint journal is armed before Run and sealed after it. approver may be
// a nil interface (H1).
func (f *fixture) runWith(t *testing.T, caller agent.ModelCaller, approver agent.Approver, maxSteps int) (agent.Result, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.journal.beginTurn(ctx, "containment", cancel); err != nil {
		t.Fatalf("checkpoint journal: %v", err)
	}
	res, err := f.orchestrator(t, caller).Run(ctx, agent.Request{Goal: "containment", Tools: f.tools, Approver: approver, MaxSteps: maxSteps}, nil)
	if serr := f.journal.sealTurn(context.Background()); serr != nil {
		t.Fatalf("checkpoint journal seal: %v", serr)
	}
	return res, err
}

// runErr drives the real Golem factory with a scripted compliant caller and
// returns the run error (D3 expects an *agent.BlockedError).
func (f *fixture) runErr(t *testing.T, caller agent.ModelCaller, maxSteps int, answers string) (agent.Result, error) {
	t.Helper()
	return f.runWith(t, caller, f.approver(answers), maxSteps)
}

// run is runErr with a scripted list of calls; any run error fails the test.
func (f *fixture) run(t *testing.T, calls []agent.ModelResult, answers string) agent.Result {
	t.Helper()
	caller := &scriptCaller{responses: append(calls, agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}})}
	res, err := f.runErr(t, caller, len(calls)+1, answers)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

var callSeq atomic.Int64

// call scripts one compliant tool call. callID returns the same call with its
// ID so validity checks can read that call's observation via toolObservation.
func call(name string, args any) agent.ModelResult {
	r, _ := callID(name, args)
	return r
}

func callID(name string, args any) (agent.ModelResult, string) {
	id := "c" + strconv.FormatInt(callSeq.Add(1), 10)
	raw, _ := json.Marshal(args)
	return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: id, Type: "function",
		Function: provider.ToolCallFunction{Name: name, Arguments: raw},
	}}}}, id
}

// prompts counts approval questions rendered so far.
func (f *fixture) prompts() int { return strings.Count(f.Out.String(), "? [y/N") }

// writeScript writes an executable bash script into the workspace.
func (f *fixture) writeScript(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(f.Root, rel)
	if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\nset -eu\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// exists reports whether p exists. Only fs.ErrNotExist means absent: any other
// stat failure (EACCES after an attack-side chmod, ELOOP) is a test failure,
// never a "forbidden effect absent" reading (spec 5.2.4).
func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	if err == nil {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	t.Fatalf("stat %s: %v", p, err)
	return false
}

// readFile returns p's contents, or "" only when p does not exist; any other
// read failure is a test failure for the same reason as exists.
func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ""
		}
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// syntheticProviderKey is byte-identical to the openai_token fixture in
// agent/hardening_contract_test.go syntheticSecrets (the shape
// agent/interceptor/secrets.go detects). It is synthetic: no real credential.
const syntheticProviderKey = "sk-" + "aB3_dE7-fG9_hJ2-kL4"

// compareEnv compares raw KEY=VALUE entries with the expected environment map.
// Duplicates are counted from the raw entries before any map is built, so a
// repeated name cannot collapse into one. A name with any occurrence that
// differs from the expected value is a mismatch. Only names are returned, sorted;
// values never leave this function.
func compareEnv(entries []string, want map[string]string) (unexpected, missing, mismatch, duplicate []string) {
	vals := map[string][]string{}
	for _, e := range entries {
		name, val, _ := strings.Cut(e, "=")
		vals[name] = append(vals[name], val)
	}
	for name, vs := range vals {
		if len(vs) > 1 {
			duplicate = append(duplicate, name)
		}
		w, ok := want[name]
		if !ok {
			unexpected = append(unexpected, name)
			continue
		}
		if slices.ContainsFunc(vs, func(v string) bool { return v != w }) {
			mismatch = append(mismatch, name)
		}
	}
	for name := range want {
		if _, ok := vals[name]; !ok {
			missing = append(missing, name)
		}
	}
	for _, l := range [][]string{unexpected, missing, mismatch, duplicate} {
		sort.Strings(l)
	}
	return unexpected, missing, mismatch, duplicate
}

// e1Verdict is what the isolated E1 parent writes to <work>/verdict.json:
// names only, never environment values.
type e1Verdict struct {
	Ran        bool     `json:"ran"`
	Names      []string `json:"names"`
	Unexpected []string `json:"unexpected"`
	Missing    []string `json:"missing"`
	Mismatch   []string `json:"mismatch"`
	Duplicate  []string `json:"duplicate"`
}

// TestContainmentHelper is argv-gated (the exec allowlist strips env vars):
// <bin> -test.run=^TestContainmentHelper$ -- __containment_helper__ dumpenv <out>
// In an ordinary go test run flag.Args() is empty and it returns at once.
func TestContainmentHelper(t *testing.T) {
	args := flag.Args()
	if len(args) < 3 || args[0] != "__containment_helper__" || args[1] != "dumpenv" {
		return
	}
	// Raw entries, not a map, so the payload is the untruncated environment as
	// this process sees it. Duplicate names are NOT observable end to end here:
	// os/exec's dedupEnv collapses them in the parent, and a Go child's
	// os.Environ keeps only the first occurrence of a repeated name. So the
	// verdict's duplicate list is pinned by TestCompareEnv alone; a conflicting
	// duplicate would surface as mismatch or unexpected instead.
	raw, _ := json.Marshal(os.Environ())
	if err := os.WriteFile(args[2], raw, 0o600); err != nil {
		os.Exit(3)
	}
}

// TestContainmentE1Parent runs the orchestrator inside a process whose whole
// environment is synthetic, so a mutation run can never dump a developer's
// real credentials. Gated by GO_LLM_CONTAINMENT_E1_PARENT=<workdir>; inert
// otherwise. It prints no environment value anywhere.
func TestContainmentE1Parent(t *testing.T) {
	work, ok := os.LookupEnv("GO_LLM_CONTAINMENT_E1_PARENT")
	if !ok || work == "" {
		return // an exported empty value is not a work dir: stay inert
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(work, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	// A regular file inside the workspace: resolveExecutable Lstat-rejects symlinks.
	bin, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "helper"), bin, 0o755); err != nil {
		t.Fatal(err)
	}
	childJSON := filepath.Join(work, "child.json")
	f := newFixtureAt(t, ws, fixtureOpts{})
	res := f.run(t, []agent.ModelResult{call("run_command", map[string]any{"argv": []string{"./helper", "-test.run=^TestContainmentHelper$", "--", "__containment_helper__", "dumpenv", childJSON}})}, "y\n")

	v := e1Verdict{Ran: len(res.ToolCalls) == 1 && res.ToolCalls[0].Invoked && exists(t, childJSON)}
	want := map[string]string{
		"HOME":   filepath.Join(work, "home"),
		"LANG":   "C",
		"USER":   "containment",
		"PATH":   filepath.Join(work, "bin") + ":/usr/bin:/bin",
		"TMPDIR": filepath.Join(work, "tmp"),
	}
	if v.Ran {
		var entries []string
		if err := json.Unmarshal([]byte(readFile(t, childJSON)), &entries); err != nil {
			t.Fatalf("child.json: %v", err) // json errors never echo the input values
		}
		for _, e := range entries {
			name, _, _ := strings.Cut(e, "=")
			v.Names = append(v.Names, name)
		}
		sort.Strings(v.Names)
		v.Unexpected, v.Missing, v.Mismatch, v.Duplicate = compareEnv(entries, want)
	}
	for _, l := range []*[]string{&v.Names, &v.Unexpected, &v.Missing, &v.Mismatch, &v.Duplicate} {
		if *l == nil {
			*l = []string{} // render [] rather than null
		}
	}
	raw, _ := json.Marshal(v)
	if err := os.WriteFile(filepath.Join(work, "verdict.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Return, not os.Exit: -test.run already scopes this binary to the parent,
	// and returning lets t.Cleanup shut the background manager and store down.
}

// TestCompareEnv pins compareEnv with independently written inputs: each case
// breaks exactly one rule, so a comparison that skips duplicates, extras,
// absences or value changes fails a named case.
func TestCompareEnv(t *testing.T) {
	want := map[string]string{"HOME": "/h", "LANG": "C", "USER": "u", "PATH": "/b:/usr/bin"}
	cases := []struct {
		name                                    string
		entries                                 []string
		unexpected, missing, mismatch, dupnames []string
	}{
		{"exact", []string{"HOME=/h", "LANG=C", "USER=u", "PATH=/b:/usr/bin"}, nil, nil, nil, nil},
		{"duplicate PATH", []string{"HOME=/h", "LANG=C", "USER=u", "PATH=/b:/usr/bin", "PATH=/b:/usr/bin"}, nil, nil, nil, []string{"PATH"}},
		{"extra SECRET", []string{"HOME=/h", "LANG=C", "USER=u", "PATH=/b:/usr/bin", "SECRET=x"}, []string{"SECRET"}, nil, nil, nil},
		{"missing LANG", []string{"HOME=/h", "USER=u", "PATH=/b:/usr/bin"}, nil, []string{"LANG"}, nil, nil},
		{"changed USER", []string{"HOME=/h", "LANG=C", "USER=other", "PATH=/b:/usr/bin"}, nil, nil, []string{"USER"}, nil},
		{"value containing =", []string{"HOME=/h", "LANG=C", "USER=u", "PATH=/b:/usr/bin", "X=a=b"}, []string{"X"}, nil, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			un, mi, mm, du := compareEnv(c.entries, want)
			for _, g := range []struct {
				label     string
				got, want []string
			}{{"unexpected", un, c.unexpected}, {"missing", mi, c.missing}, {"mismatch", mm, c.mismatch}, {"duplicate", du, c.dupnames}} {
				if !slices.Equal(g.got, g.want) {
					t.Errorf("%s = %v, want %v", g.label, g.got, g.want)
				}
			}
		})
	}
}
