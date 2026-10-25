package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/contextdepth"
	"github.com/kstruzzieri/go-llm/internal/secretscan"
	"github.com/kstruzzieri/go-llm/mcpclient"
	"github.com/kstruzzieri/go-llm/provider"
)

// The interceptor census (#517 spec §6.2) renders every manifest file through
// Golem's production tools, driven by the real Orchestrator with a scripted
// caller under interceptor.Defaults(), and records each interceptor's
// findings per observation. Arms are derived later from those findings.

const (
	censusManifestSchema = "llm-bench.census-manifest/v1"
	censusAdversarial    = "adversarial-fixture"
	// The verifier carrier writes this scratch file (and removes it) to
	// trigger post-write verification, and passes the missing operand to cat
	// so the check fails: a passing verifier renders no output (verify_slot.go
	// verifyRenderResult), a failing one renders the bounded streams.
	censusVerifyScratch = ".census-verify-scratch"
	censusVerifyMissing = ".census-verify-missing"
	censusMCPAlias      = "census"
)

// The carriers in observation order: each file carrier runs once per file,
// then search runs once per registered query per stratum.
var (
	censusFileCarriers   = []string{"read_file", "run_command", "command_tail", "verifier", "mcp", "mixed"}
	censusSearchQueries  = []string{"func ", "TODO", "error", "return", "import", "config", "test", "http"}
	censusStratumPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	censusSHA256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type censusManifest struct {
	Schema string          `json:"schema"`
	Strata []censusStratum `json:"strata"`
}

type censusStratum struct {
	Name    string       `json:"name"`
	Source  string       `json:"source"`
	Commit  string       `json:"commit"`
	License string       `json:"license"`
	Files   []censusFile `json:"files"`
}

type censusFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Class  string `json:"class,omitempty"` // "" or "adversarial-fixture" (BC5)
}

// censusObservation is one rendered observation. For the search carrier Path
// holds the registered query. Refused is set when the tool refused the
// request (read_file on a binary file): a non-observation, with no bytes and
// no findings. Bytes is the length of the fallback content the chain
// inspected.
type censusObservation struct {
	Stratum  string          `json:"stratum"`
	Path     string          `json:"path"`
	Carrier  string          `json:"carrier"`
	Origin   string          `json:"origin"`
	Bytes    int             `json:"bytes"`
	Refused  string          `json:"refused,omitempty"`
	Findings []censusFinding `json:"findings"`
}

// censusFinding is one normalized pipeline finding. Target is the
// agent.TargetKind name; Group and Alt are -1 unless Target is "alternative".
type censusFinding struct {
	Interceptor string `json:"interceptor"`
	Rule        string `json:"rule"`
	Verdict     string `json:"verdict"`
	Target      string `json:"target"`
	Group       int    `json:"group"`
	Alt         int    `json:"alt"`
}

// loadCensusManifest strictly decodes and validates a census manifest and
// returns it with the sha256 of its bytes.
func loadCensusManifest(path string) (censusManifest, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return censusManifest{}, "", fmt.Errorf("census manifest: %w", err)
	}
	var m censusManifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return censusManifest{}, "", fmt.Errorf("census manifest %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return censusManifest{}, "", fmt.Errorf("census manifest %s: trailing data after the manifest", path)
	}
	if err := m.validate(); err != nil {
		return censusManifest{}, "", fmt.Errorf("census manifest %s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	return m, hex.EncodeToString(sum[:]), nil
}

func (m censusManifest) validate() error {
	if m.Schema != censusManifestSchema {
		return fmt.Errorf("schema %q, want %q", m.Schema, censusManifestSchema)
	}
	if len(m.Strata) == 0 {
		return errors.New("no strata")
	}
	names := map[string]bool{}
	for _, s := range m.Strata {
		switch {
		case !censusStratumPattern.MatchString(s.Name) || names[s.Name]:
			return fmt.Errorf("invalid or duplicate stratum name %q", s.Name)
		case s.Source == "" || s.License == "":
			return fmt.Errorf("stratum %s: blank source or license", s.Name)
		case len(s.Files) == 0:
			return fmt.Errorf("stratum %s: no files", s.Name)
		}
		names[s.Name] = true
		for i, f := range s.Files {
			switch {
			case !fs.ValidPath(f.Path) || f.Path == ".":
				return fmt.Errorf("stratum %s: invalid path %q", s.Name, f.Path)
			case i > 0 && f.Path <= s.Files[i-1].Path:
				return fmt.Errorf("stratum %s: %s: paths are not strictly ascending", s.Name, f.Path)
			case f.Path == censusVerifyScratch || f.Path == censusVerifyMissing:
				return fmt.Errorf("stratum %s: %s: path is reserved by the census", s.Name, f.Path)
			case !censusSHA256Pattern.MatchString(f.SHA256):
				return fmt.Errorf("stratum %s: %s: sha256 is not 64 lowercase hex digits", s.Name, f.Path)
			case f.Class != "" && f.Class != censusAdversarial:
				return fmt.Errorf("stratum %s: %s: unknown class %q", s.Name, f.Path, f.Class)
			}
		}
	}
	return nil
}

// verifyCensusStratum refuses a tree that drifted from the manifest: every
// listed file must hash to its sha256, and every regular file under dir must
// be listed (search walks the whole tree). The error names the first path.
func verifyCensusStratum(dir string, s censusStratum) error {
	listed := make(map[string]bool, len(s.Files))
	for _, f := range s.Files {
		listed[f.Path] = true
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return fmt.Errorf("census: %s: %s: %w", s.Name, f.Path, err)
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != f.SHA256 {
			return fmt.Errorf("census: %s: %s: sha256 differs from the manifest", s.Name, f.Path)
		}
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); !listed[rel] {
			return fmt.Errorf("census: %s: %s: file is not in the manifest", s.Name, rel)
		}
		return nil
	})
}

// runCensus verifies every stratum under root (one subdirectory per stratum
// name) against m, then observes each file through every file carrier and
// runs the registered search queries, in manifest order. The MCP carrier pins
// its loopback server in the user data dir (XDG_DATA_HOME), which callers
// isolate: the CLI points it at a temporary directory.
func runCensus(ctx context.Context, m censusManifest, root string) ([]censusObservation, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("census: unsupported on windows: the run_command, command_tail and verifier carriers need the POSIX exec runner")
	}
	for _, s := range m.Strata {
		if err := verifyCensusStratum(filepath.Join(root, s.Name), s); err != nil {
			return nil, err
		}
	}
	var out []censusObservation
	for _, s := range m.Strata {
		obs, err := censusStratumRun(ctx, s, filepath.Join(root, s.Name))
		if err != nil {
			return nil, err
		}
		out = append(out, obs...)
	}
	return out, nil
}

// runCensusMode is the -interceptor-census CLI entry. It writes
// <out>/full/observations.jsonl (one observation per line, gitignored) and
// then <out>/census-results.json, which binds the manifest's and the JSONL's
// sha256. The MCP trust pins go to a throwaway data dir, never the user's.
func runCensusMode(ctx context.Context, manifestPath, root, out string, stderr io.Writer) error {
	if root == "" || out == "" {
		return errors.New("-census-root and -census-out are required")
	}
	m, sum, err := loadCensusManifest(manifestPath)
	if err != nil {
		return err
	}
	pins, err := os.MkdirTemp("", "llm-bench-census-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(pins) }()
	if err := os.Setenv("XDG_DATA_HOME", pins); err != nil {
		return err
	}
	obs, err := runCensus(ctx, m, root)
	if err != nil {
		return err
	}
	var full bytes.Buffer
	refused := 0
	for _, o := range obs {
		line, err := json.Marshal(o)
		if err != nil {
			return err
		}
		full.Write(append(line, '\n'))
		if o.Refused != "" {
			refused++
		}
	}
	r := aggregateCensus(obs, m)
	fullSum := sha256.Sum256(full.Bytes())
	r.ManifestSHA256, r.FullSHA256 = sum, hex.EncodeToString(fullSum[:])
	results, err := marshalCensusResults(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(out, "full"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "full", "observations.jsonl"), full.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "census-results.json"), results, 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "llm-bench: interceptor census: manifest sha256 %s: %d observation(s), %d with findings, %d refused; full sha256 %s\n", sum, len(obs), len(r.Fired), refused, r.FullSHA256)
	return nil
}

// censusEnv holds one stratum's production tools.
type censusEnv struct {
	stratum, root string
	ws            *agenttools.Workspace
	read          *agenttools.ReadFile
	search, write agent.Tool
	runCmd, mcp   agent.Tool
	slot          *agenttools.VerifierSlot
}

func censusStratumRun(ctx context.Context, s censusStratum, dir string) ([]censusObservation, error) {
	ws, err := agenttools.NewWorkspace(dir)
	if err != nil {
		return nil, fmt.Errorf("census: %s: %w", s.Name, err)
	}
	e := &censusEnv{stratum: s.Name, root: dir, ws: ws, read: agenttools.NewReadFile(ws), search: agenttools.NewSearch(ws), write: agenttools.NewWriteFile(ws, nil)}
	mgr, exec, err := censusExecTools(dir)
	if err != nil {
		return nil, err
	}
	defer mgr.Shutdown()
	e.runCmd = exec[0]
	if e.slot, err = agenttools.NewVerifierSlot(dir, agenttools.SandboxConfig{}); err != nil {
		return nil, fmt.Errorf("census: %s: %w", s.Name, err)
	}
	mcpTool, closeMCP, err := censusMCP(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("census: %s: %w", s.Name, err)
	}
	defer closeMCP()
	e.mcp = mcpTool

	var out []censusObservation
	for _, f := range s.Files {
		for _, carrier := range censusFileCarriers {
			o, err := e.observe(ctx, carrier, f.Path)
			if err != nil {
				return nil, fmt.Errorf("census: %s: %s %s: %w", s.Name, carrier, f.Path, err)
			}
			out = append(out, o)
		}
	}
	for _, q := range censusSearchQueries {
		o, err := e.run(ctx, "search", q, []agent.Tool{e.search}, nil, censusScript(censusCall(0, "search", map[string]any{"pattern": q})))
		if err != nil {
			return nil, fmt.Errorf("census: %s: search %q: %w", s.Name, q, err)
		}
		out = append(out, o)
	}
	return out, nil
}

// censusExecTools mounts the exec tool set as Golem's -allow-exec does with
// zero options (cmd/golem buildExecMount): the host runtime, SandboxConfig{}.
// The returned tools are run_command, start_command, command_status,
// command_tail and stop_command, in that order.
func censusExecTools(dir string) (*agenttools.BackgroundManager, []agent.Tool, error) {
	mgr, err := agenttools.NewSandboxedBackgroundManager(agenttools.SandboxConfig{})
	if err != nil {
		return nil, nil, fmt.Errorf("census: exec backend: %w", err)
	}
	tools, err := agenttools.NewExecToolsWithBackground(dir, mgr)
	if err != nil {
		mgr.Shutdown()
		return nil, nil, fmt.Errorf("census: exec tools: %w", err)
	}
	return mgr, tools, nil
}

// censusMCP serves one tool, fetch(path), from a loopback go-sdk
// streamable-HTTP server and adapts it through mcpclient.Connect, the
// production MCP client path.
func censusMCP(ctx context.Context, dir string) (agent.Tool, func(), error) {
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "census", Version: "1"}, nil)
	schema := map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}
	srv.AddTool(&gomcp.Tool{Name: "fetch", Description: "Fetch a file by its path.", InputSchema: schema}, func(_ context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		var args struct {
			Path string `json:"path"`
		}
		var data []byte
		err := json.Unmarshal(req.Params.Arguments, &args)
		if err == nil && !fs.ValidPath(args.Path) {
			err = errors.New("invalid path")
		}
		if err == nil {
			data, err = os.ReadFile(filepath.Join(dir, filepath.FromSlash(args.Path)))
		}
		if err != nil {
			return &gomcp.CallToolResult{IsError: true, Content: []gomcp.Content{&gomcp.TextContent{Text: "fetch failed"}}}, nil
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: string(data)}}}, nil
	})
	hs := httptest.NewServer(gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return srv }, nil))
	pins, err := mcpclient.NewPinStore(dir)
	if err != nil {
		hs.Close()
		return nil, nil, err
	}
	mgr, warns, err := mcpclient.Connect(ctx, mcpclient.Implementation{Name: "llm-bench-census", Version: "1"}, []mcpclient.Server{mcpclient.HTTPServer(censusMCPAlias, hs.URL)}, mcpclient.ConnectOptions{Pins: pins})
	if err != nil {
		hs.Close()
		return nil, nil, err
	}
	closeAll := func() { _ = mgr.Close(); hs.Close() }
	if tools := mgr.Tools(); len(tools) == 1 {
		return tools[0], closeAll, nil
	}
	closeAll()
	return nil, nil, fmt.Errorf("loopback MCP server admitted no fetch tool: %v", warns)
}

// observe drives one file through one carrier.
func (e *censusEnv) observe(ctx context.Context, carrier, path string) (censusObservation, error) {
	pathArgs := map[string]any{"path": path}
	switch carrier {
	case "read_file":
		return e.run(ctx, carrier, path, []agent.Tool{e.read}, nil, censusScript(censusCall(0, "read_file", pathArgs)))
	case "mixed":
		return e.run(ctx, carrier, path, []agent.Tool{censusMixedRead{e.read}}, nil, censusScript(censusCall(0, "read_file", pathArgs)))
	case "mcp":
		return e.run(ctx, carrier, path, []agent.Tool{e.mcp}, nil, censusScript(censusCall(0, e.mcp.Spec().Name, pathArgs)))
	case "run_command":
		return e.run(ctx, carrier, path, []agent.Tool{e.runCmd}, nil, censusScript(censusCall(0, "run_command", map[string]any{"argv": []string{"cat", path}})))
	case "command_tail":
		return e.commandTail(ctx, path)
	case "verifier":
		return e.verifier(ctx, path)
	}
	return censusObservation{}, fmt.Errorf("unknown carrier %q", carrier)
}

// commandTail starts `cat <path>` in the background, waits for it to exit and
// reads stdout with command_tail's default arguments, on a fresh manager so
// its one job is unambiguous. The handle is random hex that command_tail
// renders: a draw that Secrets would flag on its own (a Luhn-valid digit run)
// is redrawn so findings stay a function of the file.
// ponytail: only Scan is checked; a handle whose hex decodes to a phrase or a
// zero-width rune (about 1e-5 per draw) still lands in the observation.
func (e *censusEnv) commandTail(ctx context.Context, path string) (censusObservation, error) {
	for range 8 {
		mgr, tools, err := censusExecTools(e.root)
		if err != nil {
			return censusObservation{}, err
		}
		var handle string
		start := censusCall(0, "start_command", map[string]any{"argv": []string{"cat", path}})
		tail := func() (*provider.ToolCall, error) {
			var err error
			handle, err = censusAwaitJob(ctx, mgr)
			return censusCall(1, "command_tail", map[string]any{"handle": handle}), err
		}
		o, err := e.run(ctx, "command_tail", path, []agent.Tool{tools[1], tools[3]}, nil,
			[]func() (*provider.ToolCall, error){func() (*provider.ToolCall, error) { return start, nil }, tail})
		mgr.Shutdown()
		if err != nil || len(secretscan.Scan(handle)) == 0 {
			return o, err
		}
	}
	return censusObservation{}, errors.New("every background handle drawn was secret-shaped")
}

// censusAwaitJob waits until the manager's one job has exited.
func censusAwaitJob(ctx context.Context, mgr *agenttools.BackgroundManager) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		jobs := mgr.List()
		if len(jobs) != 1 {
			return "", fmt.Errorf("background jobs = %d, want 1", len(jobs))
		}
		if jobs[0].State != "running" {
			return jobs[0].Handle, nil
		}
		if time.Now().After(deadline) {
			return "", errors.New("background job still running after 30s")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// verifier arms the session verifier with a failing `cat <path>` and triggers
// it with a write_file of an unrelated scratch file, removed afterwards.
func (e *censusEnv) verifier(ctx context.Context, path string) (censusObservation, error) {
	cmd, err := agenttools.NewVerifyCommand(e.ws, []string{"cat", path, censusVerifyMissing}, "", 30*time.Second)
	if err != nil {
		return censusObservation{}, err
	}
	if err := e.slot.Set(cmd); err != nil {
		return censusObservation{}, err
	}
	defer func() { _ = os.Remove(filepath.Join(e.root, censusVerifyScratch)) }()
	write := censusCall(0, "write_file", map[string]any{"path": censusVerifyScratch, "content": "census scratch\n"})
	return e.run(ctx, "verifier", path, []agent.Tool{e.write}, e.slot, censusScript(write))
}

// censusMixedRead is read_file under mixed assembly: the production read_file
// rendering, returned with a registered ContextSet of one group whose three
// verbatim alternatives are the rendering's line thirds (empty thirds are
// dropped). The fallback Content is the first alternative.
type censusMixedRead struct{ *agenttools.ReadFile }

func (t censusMixedRead) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	res, err := t.ReadFile.Invoke(ctx, raw)
	if err != nil || res.IsError {
		return res, err
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolResult{}, err
	}
	lines := strings.SplitAfter(res.Content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	verbatim := contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{{Depth: contextdepth.DepthL2, Kind: contextdepth.RepresentationVerbatim}}}
	group := agent.ContextGroup{Desc: contextdepth.GroupDesc{Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: args.Path}}}
	n := len(lines)
	for k := range 3 {
		if third := strings.Join(lines[k*n/3:(k+1)*n/3], ""); third != "" {
			group.Alternatives = append(group.Alternatives, agent.ContextAlternative{Desc: verbatim, Content: third})
		}
	}
	res.Content = ""
	if len(group.Alternatives) > 0 {
		res.Content = group.Alternatives[0].Content
		res.Context = &agent.ContextSet{Groups: []agent.ContextGroup{group}}
	}
	return res, nil
}

func censusCall(i int, name string, args map[string]any) *provider.ToolCall {
	raw, _ := json.Marshal(args) // maps of strings and string slices always marshal
	return &provider.ToolCall{ID: fmt.Sprintf("census-%d", i+1), Type: "function", Function: provider.ToolCallFunction{Name: name, Arguments: raw}}
}

func censusScript(calls ...*provider.ToolCall) []func() (*provider.ToolCall, error) {
	out := make([]func() (*provider.ToolCall, error), len(calls))
	for i, c := range calls {
		out[i] = func() (*provider.ToolCall, error) { return c, nil }
	}
	return out
}

// censusCaller emits one scripted tool call per model turn, then ends the run.
type censusCaller struct {
	script []func() (*provider.ToolCall, error)
	turns  int
	last   *provider.ToolCall
}

func (c *censusCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if c.turns == len(c.script) {
		return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
	}
	call, err := c.script[c.turns]()
	c.turns++
	if err != nil {
		return agent.ModelResult{}, err
	}
	c.last = call
	return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{*call}}}, nil
}

// censusProbe is installed after interceptor.Defaults() and returns no
// finding: as the last interceptor it sees exactly the content the chain
// inspected at each tool or verifier ingress, and as the run's observer it
// receives each hook's normalized findings right after that hook ran. It
// never fails the run: a pairing error is kept in err and checked after Run,
// because a hook error joined to a block would otherwise pass as a block.
type censusProbe struct {
	inspections []censusInspection
	callFinds   []agent.Finding // tool-call and model-output hooks
	all         []agent.Finding // every hook's findings, in pipeline order
	err         error
}

type censusInspection struct {
	callID   string
	origin   agent.Origin
	content  string
	findings []agent.Finding
}

func (*censusProbe) Name() string { return "census_probe" }

func (p *censusProbe) InspectInput(_ context.Context, in agent.InputInspection) ([]agent.Finding, error) {
	if len(in.Messages) == 1 && in.Messages[0].Role == "tool" {
		m := in.Messages[0]
		p.inspections = append(p.inspections, censusInspection{callID: m.ToolCallID, origin: m.Origin, content: m.Content})
	}
	return nil, nil
}

func (*censusProbe) InspectOutput(context.Context, agent.OutputInspection) ([]agent.Finding, error) {
	return nil, nil
}

func (*censusProbe) InspectToolCall(context.Context, agent.ToolCallInspection) ([]agent.Finding, error) {
	return nil, nil
}

func (p *censusProbe) OnInterception(_ context.Context, ev agent.InterceptionEvent) error {
	p.all = append(p.all, ev.Findings...)
	last := len(p.inspections) - 1
	switch {
	case ev.Hook != agent.HookInput:
		p.callFinds = append(p.callFinds, ev.Findings...)
	case ev.ToolCallID == "":
		p.fail(errors.New("census: the scripted goal produced findings"))
	case last < 0 || p.inspections[last].callID != ev.ToolCallID:
		p.fail(fmt.Errorf("census: ingress findings for %q follow no matching inspection", ev.ToolCallID))
	default:
		p.inspections[last].findings = append(p.inspections[last].findings, ev.Findings...)
	}
	return nil
}

func (p *censusProbe) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}

func (*censusProbe) OnStep(context.Context, agent.StepEvent) error         { return nil }
func (*censusProbe) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }
func (*censusProbe) OnToken(context.Context, agent.TokenEvent) error       { return nil }

// run executes one scripted Orchestrator run and builds its observation: the
// findings on every scripted call, plus those on the last ingress inspection
// of the final call (for the verifier carrier, the verifier output that
// follows the write's own observation).
func (e *censusEnv) run(ctx context.Context, carrier, path string, tools []agent.Tool, verifier agent.Verifier, script []func() (*provider.ToolCall, error)) (censusObservation, error) {
	probe := &censusProbe{}
	opts := []agent.Option{agent.WithInterceptors(append(interceptor.Defaults(), probe)...)}
	if verifier != nil {
		opts = append(opts, agent.WithVerifier(verifier))
	}
	caller := &censusCaller{script: script}
	res, err := agent.New(caller, agent.ContextManager{Mixed: carrier == "mixed"}, opts...).Run(ctx, agent.Request{
		Goal: "census", Tools: tools, MaxSteps: len(script) + 1, Approver: approveAll{},
	}, probe)
	if probe.err != nil {
		return censusObservation{}, probe.err
	}
	// Only a bare BlockedError is an observation (a terminal block of the
	// scripted call); one joined with a hook error is a failure.
	if blocked := (*agent.BlockedError)(nil); err != nil && (!errors.As(err, &blocked) || err != error(blocked)) {
		return censusObservation{}, err
	}
	var risk []agent.Finding
	if res.Risk != nil {
		risk = res.Risk.Findings
	}
	if !slices.Equal(probe.all, risk) {
		return censusObservation{}, fmt.Errorf("census: the probe saw %d finding(s), Result.Risk holds %d, or they differ", len(probe.all), len(risk))
	}
	if caller.last == nil {
		return censusObservation{}, errors.New("the scripted call was never emitted")
	}
	var final *censusInspection
	seen := 0
	for i := range probe.inspections {
		if probe.inspections[i].callID == caller.last.ID {
			final, seen = &probe.inspections[i], seen+1
		}
	}
	if carrier == "verifier" && seen != 2 {
		return censusObservation{}, fmt.Errorf("verifier output inspections = %d, want the write and the verifier", seen)
	}
	o := censusObservation{Stratum: e.stratum, Path: path, Carrier: carrier, Findings: []censusFinding{}}
	for _, t := range tools {
		if ot, ok := t.(agent.OriginTool); ok && t.Spec().Name == caller.last.Function.Name {
			o.Origin = ot.Origin().String()
		}
	}
	findings := probe.callFinds
	if final != nil {
		o.Origin, o.Bytes = final.origin.String(), len(final.content)
		findings = append(findings, final.findings...)
	}
	if n := len(res.ToolCalls); carrier != "verifier" && n > 0 && res.ToolCalls[n-1].IsError && !res.ToolCalls[n-1].Blocked {
		o.Bytes, findings = 0, nil
		if final != nil {
			o.Refused, _, _ = strings.Cut(final.content, "\n")
		}
		if o.Refused == "" {
			o.Refused = "tool error"
		}
	}
	for _, f := range findings {
		o.Findings = append(o.Findings, censusFinding{Interceptor: f.Interceptor, Rule: f.Rule, Verdict: f.Verdict.String(), Target: f.Target.String(), Group: f.Group, Alt: f.Alternative})
	}
	return o, nil
}
