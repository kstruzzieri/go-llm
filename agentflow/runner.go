// Package agentflow is a host-owned client for the agentflow CLI, the durable
// planning/execution/review proof layer behind Golem's #209 task mode. It is
// deliberately NOT exposed to a model as tools: proof state must be
// adapter-driven so the model cannot forge receipts.
package agentflow

import (
	"bytes"
	"context"
	"os"
	"os/exec"
)

// Runner executes one agentflow subcommand invocation and returns its result.
// Implementations must set the working directory to the workspace root so
// commands that locate .agent/ by cwd (e.g. lock-plan, which has no --root) work.
type Runner interface {
	Run(ctx context.Context, args []string, stdin []byte) (stdout, stderr []byte, exit int, err error)
}

// ExecRunner runs the real agentflow CLI. Two modes: the installed `agentflow`
// binary, or `python3 -m agentflow` with PYTHONPATH pointed at a checkout (for
// environments where the console script is not installed). argv is always built
// explicitly; no shell string is ever parsed.
type ExecRunner struct {
	bin    string
	prefix []string // e.g. {"-m","agentflow"} for src mode
	dir    string   // Cmd.Dir = workspace root
	env    []string // extra environment, e.g. PYTHONPATH=<checkout>/src
}

// NewExecRunner runs the installed `agentflow` binary with Cmd.Dir = dir.
func NewExecRunner(dir string) *ExecRunner {
	return &ExecRunner{bin: "agentflow", dir: dir}
}

// NewSrcExecRunner runs `python3 -m agentflow` with PYTHONPATH=<checkout>/src.
func NewSrcExecRunner(dir, checkout string) *ExecRunner {
	return &ExecRunner{
		bin:    "python3",
		prefix: []string{"-m", "agentflow"},
		dir:    dir,
		env:    []string{"PYTHONPATH=" + checkout + "/src"},
	}
}

// commandFor returns the concrete (bin, argv, extraEnv) for a subcommand call.
// Split out for testability.
func (r *ExecRunner) commandFor(args []string) (bin string, argv []string, env []string) {
	argv = append(append([]string(nil), r.prefix...), args...)
	return r.bin, argv, r.env
}

func (r *ExecRunner) Run(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	bin, argv, extraEnv := r.commandFor(args)
	cmd := exec.CommandContext(ctx, bin, argv...)
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
