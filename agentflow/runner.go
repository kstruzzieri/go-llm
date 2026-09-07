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
	"os"
	"os/exec"
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
// explicitly; no shell string is ever parsed.
type ExecRunner struct {
	bin     string
	prefix  []string // e.g. {"-P","-m","agentflow"} for src mode
	dir     string   // Cmd.Dir = workspace root
	env     []string // extra environment, e.g. PYTHONPATH=<checkout>/src
	initErr error
}

// NewExecRunner runs the installed `agentflow` binary with Cmd.Dir = dir.
func NewExecRunner(dir string) *ExecRunner {
	return &ExecRunner{bin: "agentflow", dir: dir}
}

// NewSrcExecRunner runs `python3 -P -m agentflow` with PYTHONPATH=<checkout>/src.
// It requires Python 3.11+ and excludes the workspace from implicit module search.
// Run rejects checkout paths containing a path-list separator before launching.
func NewSrcExecRunner(dir, checkout string) *ExecRunner {
	r := &ExecRunner{
		bin:    "python3",
		prefix: []string{"-P", "-m", "agentflow"},
		dir:    dir,
		env:    []string{"PYTHONPATH=" + checkout + "/src"},
	}
	if strings.ContainsRune(checkout, os.PathListSeparator) {
		r.initErr = errors.New("agentflow source checkout contains a path-list separator")
	}
	return r
}

// DisablePythonBytecodeWrites keeps Python-backed verification from writing import
// caches into a read-only caller's workspace. It changes only this runner's
// child environment.
func (r *ExecRunner) DisablePythonBytecodeWrites() {
	r.env = append(r.env, "PYTHONDONTWRITEBYTECODE=1")
}

// commandFor returns the concrete (bin, argv, extraEnv) for a subcommand call.
// Split out for testability.
func (r *ExecRunner) commandFor(args []string) (bin string, argv []string, env []string) {
	argv = append(append([]string(nil), r.prefix...), args...)
	return r.bin, argv, r.env
}

func (r *ExecRunner) Run(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	if r.initErr != nil {
		return nil, nil, 0, r.initErr
	}
	bin, argv, extraEnv := r.commandFor(args)
	cmd := exec.CommandContext(ctx, bin, argv...)
	configureProcessCancellation(cmd)
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = r.dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
		err = nil // nonzero exit is not a Go error; the caller maps exit+stderr
	}
	return outBuf.Bytes(), errBuf.Bytes(), exit, err
}
