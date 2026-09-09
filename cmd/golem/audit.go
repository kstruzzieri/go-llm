package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/agentflow"
)

type auditFlags struct {
	root, scope, agentflowSrc string
}

type auditExitError struct{ code int }

func newAuditExitError(code int) *auditExitError {
	if code != 1 {
		code = 2
	}
	return &auditExitError{code: code}
}

func (e *auditExitError) Error() string { return fmt.Sprintf("audit exit %d", e.ExitCode()) }
func (e *auditExitError) ExitCode() int {
	if e != nil && e.code == 1 {
		return 1
	}
	return 2
}

func auditExitCode(err error) (int, bool) {
	var exit *auditExitError
	if !errors.As(err, &exit) {
		return 0, false
	}
	return exit.ExitCode(), true
}

type auditScanners struct {
	workspace func(context.Context, string) auditResult
	memory    func(context.Context, string) auditResult
	proofs    func(context.Context, string, agentflow.Runner) auditResult
}

func parseAuditFlags(args []string, output io.Writer) (auditFlags, error) {
	f := auditFlags{root: ".", scope: "all"}
	fs := flag.NewFlagSet("golem audit", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&f.root, "root", f.root, "workspace root")
	fs.StringVar(&f.scope, "scope", f.scope, "audit scope: all, workspace, memory, or proofs")
	fs.StringVar(&f.agentflowSrc, "agentflow-src", "", "AgentFlow source checkout")
	if err := fs.Parse(args); err != nil {
		return auditFlags{}, err
	}
	if fs.NArg() != 0 {
		return auditFlags{}, fmt.Errorf("golem audit: unexpected positional arguments %q", fs.Args())
	}
	var rootSet, scopeSet, sourceSet bool
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "root":
			rootSet = true
		case "scope":
			scopeSet = true
		case "agentflow-src":
			sourceSet = true
		}
	})
	if rootSet && f.root == "" {
		return auditFlags{}, fmt.Errorf("golem audit: -root requires a non-empty value")
	}
	if scopeSet && f.scope == "" {
		return auditFlags{}, fmt.Errorf("golem audit: -scope requires a non-empty value")
	}
	if sourceSet && f.agentflowSrc == "" {
		return auditFlags{}, fmt.Errorf("golem audit: -agentflow-src requires a non-empty value")
	}
	switch f.scope {
	case "all", "workspace", "memory", "proofs":
	default:
		return auditFlags{}, fmt.Errorf("golem audit: invalid -scope %q (want all, workspace, memory, or proofs)", f.scope)
	}
	return f, nil
}

func runAudit(ctx context.Context, args []string, out, errOut io.Writer) error {
	return runAuditWith(ctx, args, out, errOut, auditScanners{workspace: auditWorkspace, memory: auditMemory, proofs: auditProofs})
}

func runAuditWith(ctx context.Context, args []string, out, errOut io.Writer, scanners auditScanners) error {
	var flagOutput strings.Builder
	f, err := parseAuditFlags(args, &flagOutput)
	if err != nil {
		if flagOutput.Len() == 0 {
			_, _ = fmt.Fprintln(&flagOutput, err)
		}
		if _, writeErr := io.WriteString(errOut, flagOutput.String()); writeErr != nil {
			return newAuditExitError(2)
		}
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return newAuditExitError(2)
	}

	root, err := agenttools.CanonicalWorkspaceRoot(f.root)
	if err == nil {
		var info os.FileInfo
		info, err = os.Stat(root)
		if err == nil && !info.IsDir() {
			err = fmt.Errorf("root is not a directory")
		}
	}
	if err != nil {
		line := fmt.Sprintf("audit: outcome=incomplete code=root-unavailable target=%s message=%q\noverall: outcome=incomplete\n", strconv.Quote(f.root), "Workspace root is unavailable or unsafe.")
		_, _ = io.WriteString(errOut, line)
		return newAuditExitError(2)
	}

	source, err := resolveTaskAgentflowSource(root, f.agentflowSrc)
	if err != nil {
		line := fmt.Sprintf("audit: outcome=incomplete code=agentflow-source target=%s message=%q\noverall: outcome=incomplete\n", strconv.Quote(f.agentflowSrc), "AgentFlow source is unavailable or unsafe.")
		_, _ = io.WriteString(errOut, line)
		return newAuditExitError(2)
	}
	runner := agentflow.NewExecRunner(root)
	if source != "" {
		runner = agentflow.NewSrcExecRunner(root, source)
	}
	runner.DisablePythonBytecodeWrites()

	results := make([]auditResult, 0, 3)
	if f.scope == "all" || f.scope == "workspace" {
		results = append(results, scanners.workspace(ctx, root))
	}
	if f.scope == "all" || f.scope == "memory" {
		results = append(results, scanners.memory(ctx, root))
	}
	if f.scope == "all" || f.scope == "proofs" {
		results = append(results, scanners.proofs(ctx, root, runner))
	}
	if f.scope != "all" && len(results) == 1 && results[0].outcome == "not-present" {
		results[0].outcome = "incomplete"
		results[0].diagnostics = []auditDiagnostic{{code: "scope-not-present", message: "Selected audit scope is not present."}}
	}

	exitCode, overall, reason := auditOutcome(results)
	var report strings.Builder
	for _, result := range results {
		renderAuditResult(&report, result)
	}
	fmt.Fprintf(&report, "overall: outcome=%s", overall)
	if reason != "" {
		fmt.Fprintf(&report, " reason=%s", strconv.Quote(reason))
	}
	report.WriteByte('\n')
	if _, err := io.WriteString(out, report.String()); err != nil {
		return newAuditExitError(2)
	}
	if exitCode != 0 {
		return newAuditExitError(exitCode)
	}
	return nil
}

func auditOutcome(results []auditResult) (int, string, string) {
	violation, incomplete, auditable := false, false, false
	for _, result := range results {
		switch result.outcome {
		case "violation":
			violation, auditable = true, true
		case "incomplete":
			incomplete, auditable = true, true
		case "valid":
			auditable = true
		}
	}
	if violation {
		return 1, "violation", ""
	}
	if incomplete {
		return 2, "incomplete", ""
	}
	if !auditable {
		return 2, "incomplete", "nothing to audit"
	}
	return 0, "valid", ""
}

func renderAuditResult(out *strings.Builder, result auditResult) {
	fmt.Fprintf(out, "%s: outcome=%s assurance=%s checked=%d", result.scope, result.outcome, strconv.Quote(result.assurance), result.checked)
	switch result.scope {
	case "workspace":
		fmt.Fprintf(out, " paths=%d", result.paths)
	case "memory":
		if result.total == nil {
			out.WriteString(" total=unavailable")
		} else {
			fmt.Fprintf(out, " total=%d", *result.total)
		}
	case "proofs":
		fmt.Fprintf(out, " sources=%d", result.sources)
	}
	if result.earlyStop {
		out.WriteString(" coverage=early-stop")
	}
	out.WriteByte('\n')
	const diagnosticLimit = 10
	for i, diagnostic := range result.diagnostics {
		if i == diagnosticLimit {
			fmt.Fprintf(out, "  (+%d more diagnostics)\n", len(result.diagnostics)-diagnosticLimit)
			break
		}
		fmt.Fprintf(out, "  diagnostic code=%s target=%s message=%s\n", diagnostic.code, strconv.Quote(diagnostic.target), strconv.Quote(diagnostic.message))
	}
}
