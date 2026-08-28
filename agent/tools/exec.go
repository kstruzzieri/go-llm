package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/kstruzzieri/go-llm/agent"
)

const (
	execStdoutCap      = 32 * 1024
	execStderrCap      = 32 * 1024
	execRuntimeCap     = 80 * 1024
	execDefaultTimeout = 60 * time.Second
	execMaxTimeout     = 600 * time.Second
	execWaitDelay      = 2 * time.Second
	fingerprintLen     = 12
)

// execApprovalKeyPrefix namespaces exec grant keys (#341) so a command
// fingerprint can never collide with another tool class's key. The v3 tag
// names the fingerprint recipe (argv, cwd, canonical workspace root, env
// NAME=value pairs, effective and requested timeouts, resolved exe path), so
// any future recipe change is an explicit migration. v2 -> v3 (#442): the
// workspace root bounds a sandbox backend's write allowance, so it is part of
// the approved identity.
// Unexported: the key is opaque to consumers.
const execApprovalKeyPrefix = "exec:v3:"

var (
	defaultExecEnvAllowlist = []string{"PATH", "HOME", "LANG", "USER", "TMPDIR"}
	errExecUnsupported      = errors.New("exec unsupported on this platform")
)

type runCommandArgs struct {
	Argv           []string `json:"argv"`
	Dir            string   `json:"dir"`
	TimeoutSeconds *int     `json:"timeout_seconds"`
}

type execSpec struct {
	Path string
	Argv []string
	Dir  string
	Env  []string
	// WorkspaceRoot is the canonical root of the Workspace the plan was
	// resolved and re-checked against. Sandboxing backends scope their
	// allowances to it; the host runner ignores it.
	WorkspaceRoot string
}

type execResult struct {
	ExitCode        int
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	TimedOut        bool
}

// commandRunner is the OS-process seam. Run returns a non-nil error ONLY for infra
// failures (spawn error, timeout-kill, unsupported platform). A normal exit —
// including non-zero — returns nil error with ExitCode set. Stdout/Stderr are
// already bounded to the tool's self-caps by the runner.
type commandRunner interface {
	Run(ctx context.Context, spec execSpec) (execResult, error)
}

// execPending is the state Plan computes and Invoke consumes, keyed by raw-args hash.
type execPending struct {
	path              string
	identity          os.FileInfo
	argv              []string
	dir               string
	dirLabel          string
	dirIdentity       os.FileInfo
	workspaceRoot     string
	workspaceIdentity os.FileInfo
	env               []string
	envNames          []string
	timeout           time.Duration
	requestedTO       int
	clamped           bool
	fingerprint       string
	sandbox           sandboxApproval // stamped by the owning tool, NOT by prepareExecPlan
}

type execPlanCache struct {
	mu       sync.Mutex
	argsHash string
	plan     execPending
	hasPlan  bool
}

func (c *execPlanCache) store(argsHash string, p execPending) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.argsHash, c.plan, c.hasPlan = argsHash, p, true
}

func (c *execPlanCache) consume(argsHash string) (execPending, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hasPlan || c.argsHash != argsHash {
		return execPending{}, false
	}
	p := c.plan
	c.hasPlan, c.plan, c.argsHash = false, execPending{}, ""
	return p, true
}

// cappedBuffer stores at most cap bytes but ALWAYS consumes every Write, so a child
// writing to a full pipe never blocks (no pipe-deadlock); excess is discarded and
// flagged.
type cappedBuffer struct {
	cap       int
	buf       []byte
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.cap - len(c.buf); room > 0 {
		if len(p) <= room {
			c.buf = append(c.buf, p...)
		} else {
			c.buf = append(c.buf, p[:room]...)
			c.truncated = true
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

// RunCommand is the argv-first, approval-gated exec tool. PlanningTool: Plan validates
// and previews; Invoke re-checks and spawns through the runner seam.
type RunCommand struct {
	ws      *Workspace
	runner  commandRunner
	sandbox sandboxApproval // zero value = host; set by the sandboxed constructors
	execPlanCache
}

func NewRunCommand(ws *Workspace, runner commandRunner) *RunCommand {
	return &RunCommand{ws: ws, runner: runner}
}

// NewExecTools builds the foreground-only exec tool set bound to one Workspace
// over root, using the platform's real runner. Retained for existing callers;
// cmd/golem registers NewExecToolsWithBackground under -allow-exec.
func NewExecTools(root string) ([]agent.Tool, error) {
	ws, err := NewWorkspace(root)
	if err != nil {
		return nil, fmt.Errorf("tools: build exec tools: %w", err)
	}
	return []agent.Tool{NewRunCommand(ws, newPlatformRunner())}, nil
}

// NewSandboxedExecTools builds the foreground-only exec tool set with the
// isolation runtime selected by cfg (#440). The zero config is exactly
// NewExecTools. Unimplemented or invalid configs fail closed. For the
// combined foreground+background set, build a NewSandboxedBackgroundManager
// and pass it to NewExecToolsWithBackground instead.
func NewSandboxedExecTools(root string, cfg SandboxConfig) ([]agent.Tool, error) {
	backend, err := newExecBackend(cfg)
	if err != nil {
		return nil, fmt.Errorf("tools: build exec tools: %w", err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		return nil, fmt.Errorf("tools: build exec tools: %w", err)
	}
	rc := NewRunCommand(ws, backend)
	rc.sandbox = backend.approval
	return []agent.Tool{rc}, nil
}

func (t *RunCommand) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "run_command",
		Description: "Run a command directly (argv-first, NO shell: no metacharacters, globbing, or pipes). " +
			"Returns the exit code plus bounded stdout/stderr. The command is shown to the user and runs only after they approve it. " +
			"A non-zero exit code is a normal result to read and react to, not an error.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "argv":{"type":"array","items":{"type":"string"},"description":"command and arguments; argv[0] is the executable, run without a shell"},
    "dir":{"type":"string","description":"working directory relative to the workspace root (default: root)"},
    "timeout_seconds":{"type":"integer","description":"optional time limit; default 60, max 600"}
  },
  "required":["argv"]
}`),
	}
}

func (t *RunCommand) baseEffect() agent.Effect {
	return agent.Effect{
		Class:     agent.Read | agent.Write | agent.Exec | agent.Network,
		Approval:  agent.ApprovalAlways,
		Timeout:   execDefaultTimeout,
		OutputCap: execRuntimeCap,
	}
}

func (t *RunCommand) Effect() agent.Effect { return t.baseEffect() }

// Plan validates args, resolves cwd + executable + env + timeout, renders the preview,
// and stashes a pending plan keyed by the raw-args hash. It never spawns. Any failure
// returns an error so dispatch reports it without prompting for approval.
func (t *RunCommand) Plan(_ context.Context, raw json.RawMessage) (agent.ToolPlan, error) {
	eff := t.baseEffect()
	if err := t.sandbox.validateWorkspace(t.ws.root); err != nil {
		return agent.ToolPlan{Effect: eff}, err
	}
	var args runCommandArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("invalid arguments: %w", err)
	}
	timeout, requested, clamped, err := resolveExecTimeout(args.TimeoutSeconds)
	if err != nil {
		return agent.ToolPlan{Effect: eff}, err
	}
	p, err := prepareExecPlan(t.ws, args.Argv, args.Dir, timeout, requested, clamped)
	if err != nil {
		return agent.ToolPlan{Effect: eff}, err
	}
	p.sandbox = t.sandbox
	t.store(ContentHash(raw), p)
	eff.Timeout = timeout
	return agent.ToolPlan{
		Effect:      eff,
		Preview:     renderExecPreview(p, args.Argv[0]),
		ApprovalKey: execApprovalKeyPrefix + p.sandbox.keyComponent + p.fingerprint,
	}, nil
}

// prepareExecPlan is the ONE place command preparation happens, shared by the
// foreground run_command Plan and the background start_command tool (#346): argv
// validation (empty/blank/NUL), dir resolution through the Workspace chokepoint,
// sanitized-env construction, executable resolution, owned slice copies, and the
// canonical approval fingerprint. Timeout parsing stays with the callers.
func prepareExecPlan(ws *Workspace, argv []string, dir string, timeout time.Duration, requestedTimeout int, clamped bool) (execPending, error) {
	if len(argv) == 0 {
		return execPending{}, fmt.Errorf("argv is required and must be non-empty")
	}
	if strings.TrimSpace(argv[0]) == "" {
		return execPending{}, fmt.Errorf("argv[0] must not be blank")
	}
	for _, a := range argv {
		if strings.IndexByte(a, 0) >= 0 {
			return execPending{}, fmt.Errorf("argv must not contain NUL bytes")
		}
	}
	cwd, dirLabel, dirIdentity, err := resolveExecDir(ws, dir)
	if err != nil {
		return execPending{}, err
	}
	rootIdentity, err := os.Lstat(ws.root)
	if err != nil {
		return execPending{}, fmt.Errorf("workspace root: %w", err)
	}
	env, envNames := buildExecEnv(os.LookupEnv)
	path, fi, err := resolveExecutable(ws, cwd, argv[0], pathFromEnv(env))
	if err != nil {
		return execPending{}, fmt.Errorf("resolve %q: %w", argv[0], err)
	}
	owned := append([]string(nil), argv...) // caller may mutate its slice after Plan
	return execPending{
		path: path, identity: fi, argv: owned, dir: cwd, dirLabel: dirLabel,
		dirIdentity: dirIdentity, workspaceRoot: ws.root, workspaceIdentity: rootIdentity,
		env: env, envNames: envNames, timeout: timeout,
		requestedTO: requestedTimeout, clamped: clamped,
		fingerprint: commandFingerprint(owned, cwd, ws.root, env, timeout, requestedTimeout, path),
	}, nil
}

// recheckExecPlan re-resolves and re-checks the approved cwd, workspace root,
// and executable (path equality + os.SameFile identity) at spawn time, so an
// escape/symlink, root substitution, or binary swap introduced after approval
// fails closed. On success it returns an owned execSpec snapshot; on mismatch
// a descriptive error the caller renders model-visible. Shared by foreground
// Invoke and the background tool (#346).
func recheckExecPlan(ws *Workspace, pp execPending) (execSpec, error) {
	dir, _, dirIdentity, err := resolveExecDir(ws, pp.dirLabel)
	if err != nil || dir != pp.dir || pp.dirIdentity == nil || !os.SameFile(dirIdentity, pp.dirIdentity) {
		return execSpec{}, errors.New("working directory changed since approval; retry")
	}
	rootIdentity, rootErr := os.Lstat(ws.root)
	if rootErr != nil || ws.root != pp.workspaceRoot || pp.workspaceIdentity == nil ||
		!os.SameFile(rootIdentity, pp.workspaceIdentity) {
		return execSpec{}, errors.New("workspace root changed since approval; retry")
	}
	path, fi, rerr := resolveExecutable(ws, pp.dir, pp.argv[0], pathFromEnv(pp.env))
	if rerr != nil || path != pp.path || !os.SameFile(fi, pp.identity) {
		return execSpec{}, errors.New("executable changed since approval; retry")
	}
	return execSpec{
		Path:          pp.path,
		Argv:          append([]string(nil), pp.argv...),
		Dir:           pp.dir,
		Env:           append([]string(nil), pp.env...),
		WorkspaceRoot: ws.root,
	}, nil
}

// resolveExecDir resolves the optional dir argument through the Workspace chokepoint,
// returning the absolute cwd and a human label. Empty -> workspace root (label "").
// The label is the workspace-relative path passed in, or "" for root; callers that need
// a display string should substitute "(workspace root)" when the label is empty.
func resolveExecDir(ws *Workspace, dir string) (abs, label string, identity os.FileInfo, err error) {
	if strings.TrimSpace(dir) == "" || filepath.Clean(dir) == "." {
		fi, err := os.Lstat(ws.root)
		if err != nil {
			return "", "", nil, err
		}
		if !fi.IsDir() {
			return "", "", nil, errNotDir
		}
		return ws.root, "", fi, nil
	}
	abs, identity, err = ws.resolveDir(dir)
	if err != nil {
		return "", "", nil, fmt.Errorf("dir %q: %w", dir, err)
	}
	return abs, dir, identity, nil
}

// Invoke consumes the pending plan (fail-closed on hash mismatch), re-checks it via
// recheckExecPlan, then spawns through the runner. Non-zero exit is a normal
// observation; only infra failures are tool errors.
func (t *RunCommand) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	pp, ok := t.consume(ContentHash(raw))
	if !ok {
		return errResult("exec preview missing; retry"), nil
	}
	spec, err := recheckExecPlan(t.ws, pp)
	if err != nil {
		return errResult(err.Error()), nil
	}
	res, runErr := t.runner.Run(ctx, spec)
	if runErr != nil {
		if res.TimedOut {
			return errResult(fmt.Sprintf("command timed out after %s", pp.timeout)), nil
		}
		if errors.Is(runErr, context.Canceled) {
			return errResult("command canceled"), nil
		}
		return errResult("command failed to run: " + runErr.Error()), nil
	}
	return agent.ToolResult{Content: formatExecResult(res)}, nil
}

// resolveExecTimeout maps timeout_seconds to an effective duration. nil -> default;
// <= 0 -> error; > max -> clamp (requested retained for the preview).
func resolveExecTimeout(sec *int) (eff time.Duration, requested int, clamped bool, err error) {
	if sec == nil {
		return execDefaultTimeout, 0, false, nil
	}
	if *sec <= 0 {
		return 0, 0, false, fmt.Errorf("timeout_seconds must be positive")
	}
	requested = *sec
	maxSeconds := int(execMaxTimeout / time.Second)
	if *sec > maxSeconds {
		return execMaxTimeout, requested, true, nil
	}
	d := time.Duration(*sec) * time.Second
	return d, requested, false, nil
}

// buildExecEnv returns the sanitized child env (only the allowlist names present in
// the parent) as "K=V" entries plus the sorted list of names actually exposed. The
// parent's full env (including secrets) is otherwise dropped. lookup is injected for
// testability (production passes os.LookupEnv).
// NOTE: HOME is passed through by design, so approved commands may read files under
// the user's home directory. This is an approval gate, not a filesystem sandbox.
func buildExecEnv(lookup func(string) (string, bool)) (env []string, names []string) {
	for _, k := range defaultExecEnvAllowlist {
		if v, ok := lookup(k); ok {
			env = append(env, k+"="+v)
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return env, names
}

// pathFromEnv extracts the PATH value from a built env slice ("" if absent).
func pathFromEnv(env []string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			return v
		}
	}
	return ""
}

var errNotExecutable = errors.New("not an executable file")

// isExecutableFile reports whether fi is a regular file with any execute bit set.
func isExecutableFile(fi os.FileInfo) bool {
	return fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// resolveExecutable resolves argv0 to the executable path that will run, by case:
//   - bare name (no separator): searched on the sanitized PATH (symlinks followed —
//     system binaries are commonly symlinked);
//   - absolute path: stat'd as given (symlinks followed);
//   - relative with a separator: resolved under the planned cwd through the Workspace
//     chokepoint (containment + never-follow-symlink), the only case where in-workspace
//     symlink/containment rules apply.
//
// The returned os.FileInfo is used by Invoke for an os.SameFile identity re-check.
func resolveExecutable(ws *Workspace, dir, argv0, pathValue string) (string, os.FileInfo, error) {
	if argv0 == "" {
		return "", nil, fmt.Errorf("empty executable")
	}
	if !strings.ContainsRune(argv0, os.PathSeparator) && !strings.ContainsRune(argv0, '/') {
		return customLookPath(pathValue, argv0)
	}
	if filepath.IsAbs(argv0) {
		clean := filepath.Clean(argv0)
		fi, err := os.Stat(clean) // follow symlinks: allow Homebrew-style links
		if err != nil {
			return "", nil, err
		}
		if !isExecutableFile(fi) {
			return "", nil, errNotExecutable
		}
		return clean, fi, nil
	}
	// relative with separator: contain under the planned cwd via the Workspace.
	abs := filepath.Join(dir, argv0)
	rel, err := filepath.Rel(ws.root, abs)
	if err != nil {
		return "", nil, err
	}
	resolved, fi, err := ws.resolveFile(rel) // Lstat: rejects symlink + non-regular + escape
	if err != nil {
		return "", nil, err
	}
	if !isExecutableFile(fi) {
		return "", nil, errNotExecutable
	}
	return resolved, fi, nil
}

// customLookPath searches pathValue (os.PathListSeparator-delimited) for name,
// mirroring exec.LookPath semantics over an explicit PATH: the first directory holding
// an executable regular file wins. Empty PATH entries are skipped (no implicit cwd).
// Returned paths are absolute so cmd.Dir cannot change what the approved path means.
func customLookPath(pathValue, name string) (string, os.FileInfo, error) {
	for _, dir := range strings.Split(pathValue, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		candidate, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		fi, err := os.Stat(candidate) // follow symlinks
		if err != nil {
			continue
		}
		if isExecutableFile(fi) {
			return candidate, fi, nil
		}
	}
	return "", nil, fmt.Errorf("executable %q not found in PATH", name)
}

// commandFingerprint is a stable hash over the approval-relevant inputs of the
// exact command (#341 v3 recipe): argv, resolved cwd, the canonical workspace
// root (#442 — it bounds a sandbox backend's allowances, so nested workspaces
// sharing a cwd must not share grants), the sanitized env as NAME=value pairs
// (values are keyed — a changed PATH/HOME/LANG/TMPDIR/USER value changes
// behavior even when the shape is identical), effective and requested
// timeouts, and the resolved executable path (resolution can change under an
// identical env when a new binary shadows an earlier PATH directory).
// Returns the FULL digest — the approval key uses all of it; display call sites
// truncate to fingerprintLen. Uses NUL separators so field boundaries cannot
// be forged by embedded content.
func commandFingerprint(argv []string, cwd, workspaceRoot string, env []string, timeout time.Duration, requestedTimeout int, exePath string) string {
	h := sha256.New()
	write := func(s string) { _, _ = h.Write([]byte(s)); _, _ = h.Write([]byte{0}) }
	write("argv")
	for _, a := range argv {
		write(a)
	}
	write("cwd")
	write(cwd)
	write("workspace_root")
	write(workspaceRoot)
	write("env")
	for _, kv := range env { // sanitized NAME=value pairs, deterministic allowlist order
		write(kv)
	}
	write("timeout")
	write(timeout.String())
	write("requested_timeout")
	write(strconv.Itoa(requestedTimeout))
	write("exe")
	write(exePath)
	return hex.EncodeToString(h.Sum(nil)) // full digest; callers truncate for display only
}

// fmtTimeout formats a duration as seconds (e.g. "60s") for the approval preview.
func fmtTimeout(d time.Duration) string {
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// quoteArgForPreview returns a quoted form of arg when it is empty or contains
// whitespace or a non-graphic rune, so every dynamic approval value stays on
// one visible line with exact argument boundaries. Simple arguments stay bare.
func quoteArgForPreview(arg string) string {
	if arg == "" {
		return `""`
	}
	for _, r := range arg {
		if unicode.IsSpace(r) || !unicode.IsGraphic(r) {
			return strconv.QuoteToGraphic(arg)
		}
	}
	return arg
}

// renderArgvForPreview renders an argv slice so that each argument containing
// whitespace or that is empty is shown quoted, and simple arguments are bare.
func renderArgvForPreview(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = quoteArgForPreview(a)
	}
	return strings.Join(parts, " ")
}

// renderExecPreview builds the human approval preview (never parsed, never fed to the
// model). It lists names + source class for env, never values.
func renderExecPreview(p execPending, originalArgv0 string) string {
	var b strings.Builder
	dirDisplay := p.dirLabel
	if dirDisplay == "" {
		dirDisplay = "(workspace root)"
	}
	b.WriteString("run command:\n")
	fmt.Fprintf(&b, "  argv:    %s\n", renderArgvForPreview(p.argv))
	fmt.Fprintf(&b, "  exe:     %s -> %s\n", quoteArgForPreview(originalArgv0), quoteArgForPreview(p.path))
	if p.dirLabel != "" {
		dirDisplay = quoteArgForPreview(dirDisplay)
	}
	fmt.Fprintf(&b, "  cwd:     %s\n", dirDisplay)
	if p.clamped {
		fmt.Fprintf(&b, "  timeout: %s (requested %ds, clamped)\n", fmtTimeout(p.timeout), p.requestedTO)
	} else {
		fmt.Fprintf(&b, "  timeout: %s\n", fmtTimeout(p.timeout))
	}
	parts := make([]string, len(p.envNames))
	for i, n := range p.envNames {
		parts[i] = n + "(parent)"
	}
	fmt.Fprintf(&b, "  env:     %s\n", strings.Join(parts, ", "))
	if p.sandbox.preview != "" {
		fmt.Fprintf(&b, "  sandbox: %s\n", p.sandbox.preview)
	}
	// The stored fingerprint is the full digest (the approval key uses all of
	// it); the id line stays the short display form.
	id := p.fingerprint
	if len(id) > fingerprintLen {
		id = id[:fingerprintLen]
	}
	fmt.Fprintf(&b, "  id:      %s\n", id)
	return b.String()
}

// formatExecResult renders the model-facing observation (spec §10.3). Truncation
// markers are appended in-band when a stream was capped.
func formatExecResult(r execResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit code: %d\n", r.ExitCode)
	b.WriteString("--- stdout ---\n")
	b.Write(r.Stdout)
	if r.StdoutTruncated {
		b.WriteString("\n… [truncated]")
	}
	b.WriteString("\n--- stderr ---\n")
	b.Write(r.Stderr)
	if r.StderrTruncated {
		b.WriteString("\n… [truncated]")
	}
	return b.String()
}
