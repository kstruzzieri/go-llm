package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/memory"
)

func TestParseAuditFlags(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		want       auditFlags
		wantErr    string
		wantHelp   bool
		wantOutput string
	}{
		{name: "defaults", want: auditFlags{root: ".", scope: "all"}},
		{name: "workspace", args: []string{"-scope", "workspace", "-root", "work"}, want: auditFlags{root: "work", scope: "workspace"}},
		{name: "memory", args: []string{"-scope=memory"}, want: auditFlags{root: ".", scope: "memory"}},
		{name: "proofs", args: []string{"-scope", "proofs", "-agentflow-src", "../agentflow"}, want: auditFlags{root: ".", scope: "proofs", agentflowSrc: "../agentflow"}},
		{name: "all explicit", args: []string{"-scope", "all"}, want: auditFlags{root: ".", scope: "all"}},
		{name: "unknown scope", args: []string{"-scope", "files"}, wantErr: `invalid -scope "files"`},
		{name: "empty scope", args: []string{"-scope="}, wantErr: "-scope requires a non-empty value"},
		{name: "empty root", args: []string{"-root="}, wantErr: "-root requires a non-empty value"},
		{name: "empty agentflow source", args: []string{"-agentflow-src="}, wantErr: "-agentflow-src requires a non-empty value"},
		{name: "extra argument", args: []string{"workspace"}, wantErr: "unexpected positional arguments"},
		{name: "unknown flag", args: []string{"-wat"}, wantErr: "flag provided but not defined"},
		{name: "json unsupported", args: []string{"-json"}, wantErr: "flag provided but not defined"},
		{name: "output format unsupported", args: []string{"-output-format", "json"}, wantErr: "flag provided but not defined"},
		{name: "help", args: []string{"-help"}, wantHelp: true, wantOutput: "Usage of golem audit:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			got, err := parseAuditFlags(tc.args, &output)
			if tc.wantHelp {
				if !errors.Is(err, flag.ErrHelp) {
					t.Fatalf("parseAuditFlags error = %v, want flag.ErrHelp", err)
				}
			} else if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseAuditFlags error = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("parseAuditFlags = %#v, %v; want %#v, nil", got, err, tc.want)
			}
			if !strings.Contains(output.String(), tc.wantOutput) {
				t.Fatalf("flag output = %q, want substring %q", output.String(), tc.wantOutput)
			}
		})
	}
}

func TestRunAuditReportAndExitPrecedence(t *testing.T) {
	total := int64(50)
	var calls []string
	scanners := auditScanners{
		workspace: func(context.Context, string) auditResult {
			calls = append(calls, "workspace")
			return auditResult{scope: "workspace", assurance: "signed", outcome: "valid", checked: 2, paths: 1}
		},
		memory: func(context.Context, string) auditResult {
			calls = append(calls, "memory")
			return auditResult{scope: "memory", assurance: "signed", outcome: "incomplete", checked: 42, total: &total, earlyStop: true, diagnostics: []auditDiagnostic{{code: "memory-record-incomplete", target: "memory\nidentifier", message: "Agent-memory record verification could not be completed."}}}
		},
		proofs: func(context.Context, string, agentflow.Runner) auditResult {
			calls = append(calls, "proofs")
			return auditResult{scope: "proofs", assurance: auditProofAssurance, outcome: "violation", checked: 4, sources: 3, diagnostics: []auditDiagnostic{{code: "checksum_mismatch", target: "proof\tidentifier", message: "agentflow proof checksum does not match"}}}
		},
	}
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := runAuditWith(context.Background(), []string{"-root", root}, &stdout, &stderr, scanners)
	var exit *auditExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("runAuditWith error = %v, want audit exit 1", err)
	}
	if got := strings.Join(calls, ","); got != "workspace,memory,proofs" {
		t.Fatalf("scanner order = %q", got)
	}
	want := "workspace: outcome=valid assurance=\"signed\" checked=2 paths=1\n" +
		"memory: outcome=incomplete assurance=\"signed\" checked=42 total=50 coverage=early-stop\n" +
		"  diagnostic code=memory-record-incomplete target=\"memory\\nidentifier\" message=\"Agent-memory record verification could not be completed.\"\n" +
		"proofs: outcome=violation assurance=\"structural/checksum; unsigned\" checked=4 sources=3\n" +
		"  diagnostic code=checksum_mismatch target=\"proof\\tidentifier\" message=\"agentflow proof checksum does not match\"\n" +
		"overall: outcome=violation\n"
	if stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("stdout/stderr = %q / %q, want %q / empty", stdout.String(), stderr.String(), want)
	}
}

func TestAuditExitErrorOnlyExposesDocumentedCodes(t *testing.T) {
	for _, tc := range []struct{ input, want int }{{1, 1}, {2, 2}, {0, 2}, {3, 2}} {
		err := newAuditExitError(tc.input)
		if err.ExitCode() != tc.want {
			t.Errorf("newAuditExitError(%d).ExitCode() = %d, want %d", tc.input, err.ExitCode(), tc.want)
		}
		var got *auditExitError
		if !errors.As(fmt.Errorf("wrapped: %w", err), &got) || got.ExitCode() != tc.want {
			t.Errorf("wrapped audit exit %d was not discoverable", tc.want)
		}
	}
	if code, ok := auditExitCode(fmt.Errorf("outer: %w", newAuditExitError(1))); !ok || code != 1 {
		t.Fatalf("auditExitCode(wrapped exit 1) = %d, %v", code, ok)
	}
}

func TestRunAuditMissingScopeAndNothingToAudit(t *testing.T) {
	absent := func(scope, assurance string) auditResult {
		return auditResult{scope: scope, assurance: assurance, outcome: "not-present"}
	}
	scanners := auditScanners{
		workspace: func(context.Context, string) auditResult { return absent("workspace", "signed") },
		memory:    func(context.Context, string) auditResult { return absent("memory", "signed") },
		proofs: func(context.Context, string, agentflow.Runner) auditResult {
			return absent("proofs", auditProofAssurance)
		},
	}
	root := t.TempDir()
	for _, tc := range []struct {
		name, scope, want string
	}{
		{
			name:  "explicit missing",
			scope: "workspace",
			want: "workspace: outcome=incomplete assurance=\"signed\" checked=0 paths=0\n" +
				"  diagnostic code=scope-not-present target=\"\" message=\"Selected audit scope is not present.\"\n" +
				"overall: outcome=incomplete\n",
		},
		{
			name:  "all absent",
			scope: "all",
			want: "workspace: outcome=not-present assurance=\"signed\" checked=0 paths=0\n" +
				"memory: outcome=not-present assurance=\"signed\" checked=0 total=unavailable\n" +
				"proofs: outcome=not-present assurance=\"structural/checksum; unsigned\" checked=0 sources=0\n" +
				"overall: outcome=incomplete reason=\"nothing to audit\"\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := runAuditWith(context.Background(), []string{"-root", root, "-scope", tc.scope}, &out, &errOut, scanners)
			var exit *auditExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 || out.String() != tc.want || errOut.Len() != 0 {
				t.Fatalf("runAuditWith = %v, stdout=%q stderr=%q", err, out.String(), errOut.String())
			}
		})
	}
}

type auditFailWriter struct{}

func (auditFailWriter) Write([]byte) (int, error) { return 0, errors.New("private output failure") }

func TestRunAuditOutputFailureExitsTwo(t *testing.T) {
	scanners := auditScanners{
		workspace: func(context.Context, string) auditResult {
			return auditResult{scope: "workspace", assurance: "signed", outcome: "violation"}
		},
		memory: func(context.Context, string) auditResult { panic("not selected") },
		proofs: func(context.Context, string, agentflow.Runner) auditResult { panic("not selected") },
	}
	var errOut bytes.Buffer
	err := runAuditWith(context.Background(), []string{"-root", t.TempDir(), "-scope", "workspace"}, auditFailWriter{}, &errOut, scanners)
	var exit *auditExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || errOut.Len() != 0 || strings.Contains(fmt.Sprint(err), "private") {
		t.Fatalf("output failure = %v, stderr=%q", err, errOut.String())
	}
}

func TestRunAuditRejectsUnavailableRootsBeforeScanning(t *testing.T) {
	scan := func(context.Context, string) auditResult { panic("scanner ran") }
	scanners := auditScanners{
		workspace: scan,
		memory:    scan,
		proofs:    func(context.Context, string, agentflow.Runner) auditResult { panic("scanner ran") },
	}
	missing := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{missing, file} {
		var out, errOut bytes.Buffer
		err := runAuditWith(context.Background(), []string{"-root", root}, &out, &errOut, scanners)
		var exit *auditExitError
		wantErr := fmt.Sprintf("audit: outcome=incomplete code=root-unavailable target=%q message=\"Workspace root is unavailable or unsafe.\"\noverall: outcome=incomplete\n", root)
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || out.Len() != 0 || errOut.String() != wantErr {
			t.Fatalf("root %q: error/stdout/stderr = %v / %q / %q", root, err, out.String(), errOut.String())
		}
	}
}

func TestRunAuditBoundsDiagnostics(t *testing.T) {
	diagnostics := make([]auditDiagnostic, 12)
	for i := range diagnostics {
		diagnostics[i] = auditDiagnostic{code: "workspace-schema", target: fmt.Sprintf("id-%d", i), message: "Required workspace evidence structure is invalid."}
	}
	scanners := auditScanners{
		workspace: func(context.Context, string) auditResult {
			return auditResult{scope: "workspace", assurance: "signed", outcome: "violation", diagnostics: diagnostics}
		},
		memory: func(context.Context, string) auditResult { panic("not selected") },
		proofs: func(context.Context, string, agentflow.Runner) auditResult { panic("not selected") },
	}
	var out, errOut bytes.Buffer
	_ = runAuditWith(context.Background(), []string{"-root", t.TempDir(), "-scope", "workspace"}, &out, &errOut, scanners)
	if got := strings.Count(out.String(), "  diagnostic code="); got != 10 || !strings.Contains(out.String(), "  (+2 more diagnostics)\n") {
		t.Fatalf("bounded report has %d diagnostics:\n%s", got, out.String())
	}
}

func TestAuditExitContractHelper(t *testing.T) {
	if os.Getenv("GOLEM_AUDIT_EXIT_HELPER") != "1" {
		return
	}
	root := os.Getenv("GOLEM_AUDIT_ROOT")
	args := []string{"golem", "audit", "-root", root}
	switch os.Getenv("GOLEM_AUDIT_CASE") {
	case "help":
		args = []string{"golem", "audit", "-help"}
	case "invalid":
		args = []string{"golem", "audit", "-json"}
	case "explicit-missing":
		args = append(args, "-scope", "workspace")
	case "memory":
		args = append(args, "-scope", "memory")
	case "proofs":
		args = append(args, "-scope", "proofs")
	case "output-failure":
		args = append(args, "-scope", "workspace")
		f, err := os.CreateTemp(os.Getenv("GOLEM_AUDIT_DATA"), "closed-stdout")
		if err != nil {
			os.Exit(99)
		}
		_ = f.Close()
		os.Stdout = f
	}
	os.Args = args
	main()
	os.Exit(0)
}

func TestAuditExitContract(t *testing.T) {
	secretMemory := "MEMORY-SECRET-447"
	secretProof := "PROOF-SECRET-447"

	type processCase struct {
		name, scenario string
		prepare        func(*testing.T, string, string) map[string]string
		wantExit       int
		wantStdout     func(string, string) string
		wantStderr     string
		absent         []string
	}
	cases := []processCase{
		{
			name: "empty initialized workspace", scenario: "explicit-missing", wantExit: 0,
			prepare: func(t *testing.T, root, data string) map[string]string {
				createEmptyAuditWorkspace(t, root, data)
				return nil
			},
			wantStdout: func(_, _ string) string {
				return "workspace: outcome=valid assurance=\"signed\" checked=0 paths=0\noverall: outcome=valid\n"
			},
		},
		{
			name: "absent default scopes", wantExit: 0,
			prepare: func(t *testing.T, root, data string) map[string]string {
				createEmptyAuditWorkspace(t, root, data)
				return nil
			},
			wantStdout: func(root, data string) string {
				memoryPath := filepath.Join(data, "golem", "memories.db")
				return "workspace: outcome=valid assurance=\"signed\" checked=0 paths=0\n" +
					fmt.Sprintf("memory: outcome=not-present assurance=\"signed\" checked=0 total=unavailable\n  diagnostic code=store-not-present target=%q message=\"Store is not present.\"\n", memoryPath) +
					"proofs: outcome=not-present assurance=\"structural/checksum; unsigned\" checked=0 sources=0\n" +
					"overall: outcome=valid\n"
			},
		},
		{
			name: "explicit missing scope", scenario: "explicit-missing", wantExit: 2,
			wantStdout: func(_, _ string) string {
				return "workspace: outcome=incomplete assurance=\"signed\" checked=0 paths=0\n  diagnostic code=scope-not-present target=\"\" message=\"Selected audit scope is not present.\"\noverall: outcome=incomplete\n"
			},
		},
		{
			name: "all absent", wantExit: 2,
			wantStdout: func(root, data string) string {
				workspacePath, _ := checkpointDBPath(func(string) string { return data }, root)
				memoryPath := filepath.Join(data, "golem", "memories.db")
				return fmt.Sprintf("workspace: outcome=not-present assurance=\"signed\" checked=0 paths=0\n  diagnostic code=store-not-present target=%q message=\"Store is not present.\"\n", workspacePath) +
					fmt.Sprintf("memory: outcome=not-present assurance=\"signed\" checked=0 total=unavailable\n  diagnostic code=store-not-present target=%q message=\"Store is not present.\"\n", memoryPath) +
					"proofs: outcome=not-present assurance=\"structural/checksum; unsigned\" checked=0 sources=0\noverall: outcome=incomplete reason=\"nothing to audit\"\n"
			},
		},
		{
			name: "empty initialized memory", scenario: "memory", wantExit: 0,
			prepare: func(t *testing.T, _, data string) map[string]string {
				createEmptyAuditMemory(t, data)
				return nil
			},
			wantStdout: func(_, _ string) string {
				return "memory: outcome=valid assurance=\"signed\" checked=0 total=0\noverall: outcome=valid\n"
			},
		},
		{
			name: "old AgentFlow usage", scenario: "proofs", wantExit: 2,
			prepare: func(t *testing.T, root, _ string) map[string]string {
				if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
					t.Fatal(err)
				}
				return map[string]string{"GOLEM_AUDIT_AF_EXIT": "2", "GOLEM_AUDIT_AF_PAYLOAD": secretProof}
			},
			wantStdout: func(_, _ string) string {
				return "proofs: outcome=incomplete assurance=\"structural/checksum; unsigned\" checked=0 sources=0\n  diagnostic code=agentflow_unavailable target=\"\" message=\"agentflow proof verification unavailable: requires agentflow with --integrity-only support\"\noverall: outcome=incomplete\n"
			},
			absent: []string{secretProof},
		},
		{
			name: "mixed violation and incomplete", wantExit: 1,
			prepare: func(t *testing.T, root, data string) map[string]string {
				path, err := checkpointDBPath(func(string) string { return data }, root)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
					t.Fatal(err)
				}
				return map[string]string{"GOLEM_AUDIT_AF_EXIT": "2", "GOLEM_AUDIT_AF_PAYLOAD": secretProof}
			},
			wantStdout: func(root, data string) string {
				workspacePath, _ := checkpointDBPath(func(string) string { return data }, root)
				memoryPath := filepath.Join(data, "golem", "memories.db")
				return fmt.Sprintf("workspace: outcome=violation assurance=\"signed\" checked=0 paths=0\n  diagnostic code=store-corrupt target=%q message=\"SQLite integrity verification failed.\"\n", workspacePath) +
					fmt.Sprintf("memory: outcome=not-present assurance=\"signed\" checked=0 total=unavailable\n  diagnostic code=store-not-present target=%q message=\"Store is not present.\"\n", memoryPath) +
					"proofs: outcome=incomplete assurance=\"structural/checksum; unsigned\" checked=0 sources=0\n  diagnostic code=agentflow_unavailable target=\"\" message=\"agentflow proof verification unavailable: requires agentflow with --integrity-only support\"\noverall: outcome=violation\n"
			},
			absent: []string{secretProof},
		},
		{
			name: "memory violation hides record body", scenario: "memory", wantExit: 1,
			prepare: func(t *testing.T, _, data string) map[string]string {
				createInvalidAuditMemory(t, data, secretMemory)
				return nil
			},
			wantStdout: func(_, data string) string {
				path := filepath.Join(data, "golem", "memories.db")
				return fmt.Sprintf("memory: outcome=violation assurance=\"signed\" checked=0 total=1 coverage=early-stop\n  diagnostic code=memory-record-invalid target=%q message=\"Stored agent-memory record integrity verification failed.\"\noverall: outcome=violation\n", path)
			},
			absent: []string{secretMemory},
		},
		{
			name: "output failure", scenario: "output-failure", wantExit: 2,
			prepare: func(t *testing.T, root, data string) map[string]string {
				createEmptyAuditWorkspace(t, root, data)
				return nil
			},
			wantStdout: func(_, _ string) string { return "" },
		},
		{
			name: "help", scenario: "help", wantExit: 0,
			wantStdout: func(_, _ string) string { return "" },
			wantStderr: "Usage of golem audit:\n  -agentflow-src string\n    \tAgentFlow source checkout\n  -root string\n    \tworkspace root (default \".\")\n  -scope string\n    \taudit scope: all, workspace, memory, or proofs (default \"all\")\n",
		},
		{
			name: "unsupported machine output", scenario: "invalid", wantExit: 2,
			wantStdout: func(_, _ string) string { return "" },
			wantStderr: "flag provided but not defined: -json\nUsage of golem audit:\n  -agentflow-src string\n    \tAgentFlow source checkout\n  -root string\n    \tworkspace root (default \".\")\n  -scope string\n    \taudit scope: all, workspace, memory, or proofs (default \"all\")\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, data := t.TempDir(), t.TempDir()
			canonicalRoot, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			root = canonicalRoot
			extra := map[string]string{}
			if tc.prepare != nil {
				extra = tc.prepare(t, root, data)
			}
			bin := writeAuditAgentflow(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestAuditExitContractHelper$")
			env := map[string]string{
				"GOLEM_AUDIT_EXIT_HELPER": "1", "GOLEM_AUDIT_CASE": tc.scenario,
				"GOLEM_AUDIT_ROOT": root, "GOLEM_AUDIT_DATA": data,
				"XDG_DATA_HOME": data, "PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			}
			for key, value := range extra {
				env[key] = value
			}
			cmd.Env = auditProcessEnv(env)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err = cmd.Run()
			gotExit := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatal(err)
				}
				gotExit = exitErr.ExitCode()
			}
			if gotExit != tc.wantExit || stdout.String() != tc.wantStdout(root, data) || stderr.String() != tc.wantStderr {
				t.Fatalf("exit/stdout/stderr = %d / %q / %q", gotExit, stdout.String(), stderr.String())
			}
			for _, secret := range tc.absent {
				if strings.Contains(stdout.String()+stderr.String(), secret) {
					t.Fatalf("audit output disclosed %q", secret)
				}
			}
		})
	}
}

func createEmptyAuditWorkspace(t *testing.T, root, data string) {
	t.Helper()
	store, err := openCheckpointStore(t.Context(), func(key string) string {
		if key == "XDG_DATA_HOME" {
			return data
		}
		return os.Getenv(key)
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func createEmptyAuditMemory(t *testing.T, data string) {
	t.Helper()
	path := filepath.Join(data, "golem", "memories.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.NewMemoryRecordStore(t.Context(), db, memory.RecordStoreConfig{KeyDir: path + ".keys"}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func createInvalidAuditMemory(t *testing.T, data, secret string) {
	t.Helper()
	createEmptyAuditMemory(t, data)
	path := filepath.Join(data, "golem", "memories.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.NewMemoryRecordStore(t.Context(), db, memory.RecordStoreConfig{KeyDir: path + ".keys"})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	record, err := store.Create(t.Context(), memory.CreateRecordParams{Kind: memory.KindSemantic, Content: secret})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE memory_records SET content='corrupt' WHERE id=?`, record.ID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeAuditAgentflow(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$PWD\" = \"$GOLEM_AUDIT_ROOT\" ] || exit 98\n[ \"$*\" = \"verify-proof --integrity-only --json\" ] || exit 98\nprintf '%s' \"$GOLEM_AUDIT_AF_PAYLOAD\"\nexit \"${GOLEM_AUDIT_AF_EXIT:-0}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "agentflow"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func auditProcessEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			env = append(env, entry)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}
