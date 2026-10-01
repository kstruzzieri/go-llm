// Package agentflow is a host-owned client for the agentflow CLI, the durable
// planning/execution/review proof layer behind Golem's #209 task mode. It is
// deliberately NOT exposed to a model as tools: proof state must be
// adapter-driven so the model cannot forge receipts.
//
// The package also owns the #210 planner surface: PlanIR/StepIR/GateIR (the
// ergonomic representation a model authors), Compile (a total, deterministic
// projection of that IR into the rigid lockable Plan), and CheckPlan (a narrow
// local semantic pre-check). The model authors the IR; the host compiles,
// pre-checks, and locks it via the CLI on the model's behalf — the model still
// never touches proof state directly.
package agentflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Runner executes one agentflow subcommand invocation and returns its result.
// Implementations must set the working directory to the workspace root so
// commands that locate .agent/ by cwd (e.g. lock-plan, which has no --root) work.
type Runner interface {
	Run(ctx context.Context, args []string, stdin []byte) (stdout, stderr []byte, exit int, err error)
}

// ExecRunner runs the real agentflow CLI. Two modes: the installed `agentflow`
// binary, or `python3 -P -m agentflow` with PYTHONPATH pointed at a checkout (for
// environments where the console script is not installed). argv is always built
// explicitly; no shell string is ever parsed. The child environment is built
// from scratch for every launch (see buildChildEnv).
type ExecRunner struct {
	bin     string
	prefix  []string // e.g. {"-P","-m","agentflow"} for src mode
	dir     string   // Cmd.Dir = workspace root
	env     []string // runner-owned NAME=VALUE entries, e.g. PYTHONPATH=<checkout>/src
	allowed []string // host-approved parent variable names (AllowEnv)
	initErr error
}

// NewExecRunner runs the installed `agentflow` binary with Cmd.Dir = dir.
func NewExecRunner(dir string) *ExecRunner {
	return &ExecRunner{bin: "agentflow", dir: dir}
}

// NewSrcExecRunner runs `python3 -P -m agentflow` with PYTHONPATH set to the
// canonical checkout's src directory. It requires Python 3.11+ and excludes
// the workspace from implicit module search. The checkout must be an absolute
// path to a directory holding src/agentflow/__init__.py; symlinks are resolved
// first. An invalid checkout makes Run fail before launching anything.
func NewSrcExecRunner(dir, checkout string) *ExecRunner {
	r := &ExecRunner{bin: "python3", prefix: []string{"-P", "-m", "agentflow"}, dir: dir}
	canonical, err := canonicalSourceCheckout(checkout)
	if err != nil {
		r.initErr = err
		return r
	}
	r.env = []string{"PYTHONPATH=" + filepath.Join(canonical, "src")}
	return r
}

var errSourcePathList = errors.New("agentflow source checkout contains a path-list separator")

// canonicalSourceCheckout validates an operator-supplied checkout once, at
// construction. The checkout is trusted operator input: a swap between this
// check and a later launch is outside the threat model.
func canonicalSourceCheckout(checkout string) (string, error) {
	switch {
	case checkout == "":
		return "", errors.New("agentflow source checkout is empty")
	case !filepath.IsAbs(checkout):
		return "", errors.New("agentflow source checkout must be an absolute path")
	case strings.ContainsRune(checkout, os.PathListSeparator):
		return "", errSourcePathList
	}
	canonical, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return "", fmt.Errorf("agentflow source checkout: %w", err)
	}
	if strings.ContainsRune(canonical, os.PathListSeparator) {
		return "", errSourcePathList
	}
	fi, err := os.Stat(filepath.Join(canonical, "src", "agentflow", "__init__.py"))
	switch {
	case err == nil && fi.Mode().IsRegular():
		return canonical, nil
	case err == nil || errors.Is(err, os.ErrNotExist):
		return "", errors.New("agentflow source checkout has no src/agentflow package")
	default: // e.g. permission denied: the package may exist, so keep the cause
		return "", fmt.Errorf("agentflow source checkout: %w", err)
	}
}

// DisablePythonBytecodeWrites keeps Python-backed verification from writing import
// caches into a read-only caller's workspace. It changes only this runner's
// child environment.
func (r *ExecRunner) DisablePythonBytecodeWrites() {
	r.env = append(r.env, "PYTHONDONTWRITEBYTECODE=1")
}

// AllowEnv approves parent variables, by name, for this runner's children:
// Agentflow and every gate it runs. Values are read at each launch, and an
// approved name that is unset then fails the launch. Validation is atomic: on
// error nothing is added. A repeated name is harmless: each variable appears
// once in the child environment. Configure before the runner is used
// concurrently.
func (r *ExecRunner) AllowEnv(names ...string) error {
	if err := ValidateEnvNames(names); err != nil {
		return err
	}
	r.allowed = append(r.allowed, names...)
	return nil
}

// commandFor returns the concrete (bin, argv, owned) for a subcommand call,
// where owned is the runner-owned NAME=VALUE entries. Split out for testability.
func (r *ExecRunner) commandFor(args []string) (bin string, argv []string, owned []string) {
	argv = append(append([]string(nil), r.prefix...), args...)
	return r.bin, argv, r.env
}

func (r *ExecRunner) Run(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	if r.initErr != nil {
		return nil, nil, 0, r.initErr
	}
	bin, argv, owned := r.commandFor(args)
	env, err := buildChildEnv(childEnvPolicyFor(runtime.GOOS, r.allowed, owned), os.LookupEnv)
	if err != nil {
		return nil, nil, 0, err
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	configureProcessCancellation(cmd)
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = r.dir
	cmd.Env = env // complete and never nil: nothing is inherited implicitly
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
		err = nil // nonzero exit is not a Go error; the caller maps exit+stderr
	}
	return outBuf.Bytes(), errBuf.Bytes(), exit, err
}
