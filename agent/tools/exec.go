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
	"strings"
	"sync"
	"time"

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

var defaultExecEnvAllowlist = []string{"PATH", "HOME", "LANG", "USER", "TMPDIR"}

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
}

type execResult struct {
	ExitCode        int
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	Started         bool
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
	path        string
	identity    os.FileInfo
	argv        []string
	dir         string
	dirLabel    string
	env         []string
	envNames    []string
	timeout     time.Duration
	requestedTO int
	clamped     bool
	fingerprint string
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
	ws     *Workspace
	runner commandRunner
	execPlanCache
}

func NewRunCommand(ws *Workspace, runner commandRunner) *RunCommand {
	return &RunCommand{ws: ws, runner: runner}
}

// NewExecTools builds the exec tool set bound to one Workspace over root, using the
// platform's real runner. Registered by cmd/golem only under -allow-exec.
func NewExecTools(root string) ([]agent.Tool, error) {
	ws, err := NewWorkspace(root)
	if err != nil {
		return nil, fmt.Errorf("tools: build exec tools: %w", err)
	}
	return []agent.Tool{NewRunCommand(ws, newPlatformRunner())}, nil
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

// Plan is implemented in Task 8.
func (t *RunCommand) Plan(_ context.Context, _ json.RawMessage) (agent.ToolPlan, error) {
	return agent.ToolPlan{Effect: t.baseEffect()}, fmt.Errorf("run_command: Plan not yet implemented")
}

// Invoke is implemented in Task 9.
func (t *RunCommand) Invoke(_ context.Context, _ json.RawMessage) (agent.ToolResult, error) {
	return errResult("run_command: Invoke not yet implemented"), nil
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
	d := time.Duration(*sec) * time.Second
	if d > execMaxTimeout {
		return execMaxTimeout, requested, true, nil
	}
	return d, requested, false, nil
}

// buildExecEnv returns the sanitized child env (only the allowlist names present in
// the parent) as "K=V" entries plus the sorted list of names actually exposed. The
// parent's full env (including secrets) is otherwise dropped. lookup is injected for
// testability (production passes os.LookupEnv).
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
func customLookPath(pathValue, name string) (string, os.FileInfo, error) {
	for _, dir := range strings.Split(pathValue, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
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

// commandFingerprint is a short, stable hash over the approval-relevant inputs
// (argv, resolved cwd, exposed env-var NAMES, effective timeout). Surfaced in the
// preview; reused by logs/tests and later 2.1c policy. Uses NUL separators so field
// boundaries cannot be forged by embedded content.
func commandFingerprint(argv []string, cwd string, envNames []string, timeout time.Duration) string {
	h := sha256.New()
	write := func(s string) { _, _ = h.Write([]byte(s)); _, _ = h.Write([]byte{0}) }
	write("argv")
	for _, a := range argv {
		write(a)
	}
	write("cwd")
	write(cwd)
	write("env")
	for _, n := range envNames {
		write(n)
	}
	write("timeout")
	write(timeout.String())
	return hex.EncodeToString(h.Sum(nil))[:fingerprintLen]
}

// renderExecPreview builds the human approval preview (never parsed, never fed to the
// model). It lists names + source class for env, never values.
func renderExecPreview(p execPending, originalArgv0 string) string {
	var b strings.Builder
	b.WriteString("run command:\n")
	fmt.Fprintf(&b, "  argv:    %s\n", strings.Join(p.argv, " "))
	fmt.Fprintf(&b, "  exe:     %s -> %s\n", originalArgv0, p.path)
	fmt.Fprintf(&b, "  cwd:     %s\n", p.dirLabel)
	if p.clamped {
		fmt.Fprintf(&b, "  timeout: %s (requested %ds, clamped)\n", p.timeout, p.requestedTO)
	} else {
		fmt.Fprintf(&b, "  timeout: %s\n", p.timeout)
	}
	parts := make([]string, len(p.envNames))
	for i, n := range p.envNames {
		parts[i] = n + "(parent)"
	}
	fmt.Fprintf(&b, "  env:     %s\n", strings.Join(parts, ", "))
	fmt.Fprintf(&b, "  id:      %s\n", p.fingerprint)
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

// TEMP (removed in Task 10 when exec_unix.go/exec_other.go provide it):
func newPlatformRunner() commandRunner { return nil }
