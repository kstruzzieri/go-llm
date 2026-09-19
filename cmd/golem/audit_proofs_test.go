package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

type auditProofRunner struct {
	stdout []byte
	stderr []byte
	exit   int
	err    error
	calls  [][]string
	stdin  [][]byte
}

func (r *auditProofRunner) Run(_ context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	r.stdin = append(r.stdin, append([]byte(nil), stdin...))
	return r.stdout, r.stderr, r.exit, r.err
}

func TestAuditProofsAcceptsValidProposedV1Response(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &auditProofRunner{stdout: []byte(`{"schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":3,"checked_sources":2,"violations":[],"incomplete_reasons":[]}`)}

	got := auditProofs(context.Background(), root, runner)

	if got.scope != "proofs" || got.assurance != "structural/checksum; unsigned" || got.outcome != "valid" || got.checked != 3 || got.sources != 2 || len(got.diagnostics) != 0 {
		t.Fatalf("result = %+v", got)
	}
	wantArgs := [][]string{{"verify-proof", "--integrity-only", "--json"}}
	if !reflect.DeepEqual(runner.calls, wantArgs) {
		t.Fatalf("runner calls = %#v, want %#v", runner.calls, wantArgs)
	}
	if len(runner.stdin) != 1 || runner.stdin[0] != nil {
		t.Fatalf("runner stdin = %#v, want one nil input", runner.stdin)
	}
}

func TestAuditProofsMapsBoundedViolationAndIncompleteReasons(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &auditProofRunner{
		exit: 1,
		stdout: []byte(`{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":4,"checked_sources":3,"violations":[` +
			`{"code":"artifact_malformed","target":".agent/proof-pack.json","message":"secret one"},` +
			`{"code":"reference_missing","target":"steps/P1","message":"secret two"},` +
			`{"code":"reference_invalid","target":"steps/P2","message":"secret three"},` +
			`{"code":"checksum_mismatch","target":"src/main.go","message":"secret four"},` +
			`{"code":"derived_state_mismatch","target":"reviews/R1","message":"secret five"}],` +
			`"incomplete_reasons":[{"code":"source_unreadable","path":"receipts/R2","message":"private provider output"}]}`),
	}

	got := auditProofs(context.Background(), root, runner)

	want := auditResult{
		scope: "proofs", assurance: "structural/checksum; unsigned", outcome: "violation", checked: 4, sources: 3,
		diagnostics: []auditDiagnostic{
			{code: "artifact_malformed", target: ".agent/proof-pack.json", message: "agentflow proof artifact is malformed"},
			{code: "reference_missing", target: "steps/P1", message: "agentflow proof reference is missing"},
			{code: "reference_invalid", target: "steps/P2", message: "agentflow proof reference is invalid"},
			{code: "checksum_mismatch", target: "src/main.go", message: "agentflow proof checksum does not match"},
			{code: "derived_state_mismatch", target: "reviews/R1", message: "agentflow proof derived state is inconsistent"},
			{code: "source_unreadable", target: "receipts/R2", message: "agentflow proof source is unreadable"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %#v, want %#v", got, want)
	}
}

func TestAuditProofsMapsEveryBoundedIncompleteReason(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &auditProofRunner{
		exit: 1,
		stdout: []byte(`{"schema_version":"v1","outcome":"incomplete","assurance":"structural/checksum; unsigned","checked_steps":2,"checked_sources":1,"violations":[],"incomplete_reasons":[` +
			`{"code":"proof_missing","path":".agent/proof-pack.json","message":"provider detail"},` +
			`{"code":"source_unreadable","path":"receipts/R1","message":"provider detail"},` +
			`{"code":"schema_unsupported","path":".agent/proof-pack.json","message":"provider detail"},` +
			`{"code":"source_changed","path":"src/main.go","message":"provider detail"},` +
			`{"code":"canceled","path":"","message":"provider detail"}]}`),
	}

	got := auditProofs(context.Background(), root, runner)

	want := []auditDiagnostic{
		{code: "proof_missing", target: ".agent/proof-pack.json", message: "agentflow proof is missing"},
		{code: "source_unreadable", target: "receipts/R1", message: "agentflow proof source is unreadable"},
		{code: "schema_unsupported", target: ".agent/proof-pack.json", message: "agentflow proof schema is unsupported"},
		{code: "source_changed", target: "src/main.go", message: "agentflow proof source changed during verification"},
		{code: "canceled", target: "", message: "agentflow proof verification was canceled"},
	}
	if got.outcome != "incomplete" || got.checked != 2 || got.sources != 1 || !reflect.DeepEqual(got.diagnostics, want) {
		t.Fatalf("result = %#v, want diagnostics %#v", got, want)
	}
}

func TestAuditProofsSourceChangedSuppressesTransientViolations(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &auditProofRunner{
		exit: 1,
		stdout: []byte(`{"schema_version":"v1","outcome":"incomplete","assurance":"structural/checksum; unsigned","checked_steps":2,"checked_sources":1,` +
			`"violations":[{"code":"checksum_mismatch","target":"src/main.go","message":"transient"}],` +
			`"incomplete_reasons":[{"code":"source_changed","path":"src/main.go","message":"changed"}]}`),
	}

	got := auditProofs(context.Background(), root, runner)

	want := []auditDiagnostic{{code: "source_changed", target: "src/main.go", message: "agentflow proof source changed during verification"}}
	if got.outcome != "incomplete" || !reflect.DeepEqual(got.diagnostics, want) {
		t.Fatalf("result = %#v, want only source-changed diagnostic %#v", got, want)
	}
}

func TestAuditProofsRejectsIncompatibleResponseVersion(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &auditProofRunner{stdout: []byte(`{"schema_version":"v2","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`)}

	got := auditProofs(context.Background(), root, runner)

	want := []auditDiagnostic{{code: "agentflow_response_unusable", message: "agentflow proof verification returned an incompatible response"}}
	if got.outcome != "incomplete" || !reflect.DeepEqual(got.diagnostics, want) {
		t.Fatalf("result = %#v, want incomplete with %#v", got, want)
	}
}

func TestAuditProofsRejectsUnusableProviderResponses(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		body string
		exit int
	}{
		{name: "missing required field", body: `{"outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`},
		{name: "null scalar", body: `{"schema_version":"v1","outcome":null,"assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`},
		{name: "mistyped scalar", body: `{"schema_version":"v1","outcome":"valid","assurance":7,"checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`},
		{name: "null violations", body: `{"schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":null,"incomplete_reasons":[]}`},
		{name: "null incomplete reasons", body: `{"schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":null}`},
		{name: "mistyped finding member", body: `{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"checksum_mismatch","target":8,"message":"detail"}],"incomplete_reasons":[]}`, exit: 1},
		{name: "negative checked steps", body: `{"schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":-1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`},
		{name: "negative checked sources", body: `{"schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":-1,"violations":[],"incomplete_reasons":[]}`},
		{name: "fractional count", body: `{"schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1.5,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`},
		{name: "duplicate top-level member", body: `{"schema_version":"v1","schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`},
		{name: "duplicate finding member", body: `{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"checksum_mismatch","code":"checksum_mismatch","target":"x","message":"detail"}],"incomplete_reasons":[]}`, exit: 1},
		{name: "case-variant required member", body: `{"SCHEMA_VERSION":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`},
		{name: "case-variant finding member", body: `{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"checksum_mismatch","TARGET":"x","message":"detail"}],"incomplete_reasons":[]}`, exit: 1},
		{name: "truncated JSON", body: `{"schema_version":"v1"`},
		{name: "multiple JSON values", body: `{"schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]} {}`},
		{name: "wrong assurance", body: `{"schema_version":"v1","outcome":"valid","assurance":"signed","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`},
		{name: "unknown outcome", body: `{"schema_version":"v1","outcome":"maybe","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`, exit: 1},
		{name: "valid with findings", body: `{"schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"checksum_mismatch","target":"x","message":"detail"}],"incomplete_reasons":[]}`},
		{name: "violation without violations", body: `{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`, exit: 1},
		{name: "incomplete without reasons", body: `{"schema_version":"v1","outcome":"incomplete","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`, exit: 1},
		{name: "incomplete with stable violation", body: `{"schema_version":"v1","outcome":"incomplete","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"checksum_mismatch","target":"x","message":"detail"}],"incomplete_reasons":[{"code":"proof_missing","path":"x","message":"detail"}]}`, exit: 1},
		{name: "violation contradicts source changed", body: `{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"checksum_mismatch","target":"x","message":"detail"}],"incomplete_reasons":[{"code":"source_changed","path":"x","message":"detail"}]}`, exit: 1},
		{name: "unknown violation code", body: `{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"new_code","target":"x","message":"detail"}],"incomplete_reasons":[]}`, exit: 1},
		{name: "unknown incomplete code", body: `{"schema_version":"v1","outcome":"incomplete","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[{"code":"new_code","path":"x","message":"detail"}]}`, exit: 1},
		{name: "valid uses wrong process exit", body: `{"schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`, exit: 1},
		{name: "violation uses wrong process exit", body: `{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"checksum_mismatch","target":"x","message":"detail"}],"incomplete_reasons":[]}`},
		{name: "nonzero process status alone", body: `provider failed without a response`, exit: 3},
	}
	want := []auditDiagnostic{{code: "agentflow_response_unusable", message: "agentflow proof verification returned an incompatible response"}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := auditProofs(context.Background(), root, &auditProofRunner{stdout: []byte(test.body), exit: test.exit})
			if got.outcome != "incomplete" || got.checked != 0 || got.sources != 0 || !reflect.DeepEqual(got.diagnostics, want) {
				t.Fatalf("result = %#v, want incomplete with %#v", got, want)
			}
		})
	}
}

func TestAuditProofsRejectsNonlocalResponseIdentifiers(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		body string
	}{
		{name: "empty violation target", body: `{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"checksum_mismatch","target":"","message":"detail"}],"incomplete_reasons":[]}`},
		{name: "absolute violation target", body: `{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"checksum_mismatch","target":"/outside","message":"detail"}],"incomplete_reasons":[]}`},
		{name: "escaping violation target", body: `{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"checksum_mismatch","target":"safe/../../outside","message":"detail"}],"incomplete_reasons":[]}`},
		{name: "windows drive violation target", body: `{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"checksum_mismatch","target":"C:\\outside","message":"detail"}],"incomplete_reasons":[]}`},
		{name: "UNC violation target", body: `{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[{"code":"checksum_mismatch","target":"\\\\server\\share","message":"detail"}],"incomplete_reasons":[]}`},
		{name: "absolute incomplete path", body: `{"schema_version":"v1","outcome":"incomplete","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[{"code":"source_unreadable","path":"/outside","message":"detail"}]}`},
		{name: "escaping incomplete path", body: `{"schema_version":"v1","outcome":"incomplete","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[{"code":"source_unreadable","path":"..\\outside","message":"detail"}]}`},
	}
	want := []auditDiagnostic{{code: "agentflow_response_unusable", message: "agentflow proof verification returned an incompatible response"}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := auditProofs(context.Background(), root, &auditProofRunner{stdout: []byte(test.body), exit: 1})
			if got.outcome != "incomplete" || !reflect.DeepEqual(got.diagnostics, want) {
				t.Fatalf("result = %#v, want incomplete with %#v", got, want)
			}
		})
	}
}

func TestAuditProofsAcceptsSafeRelativeAndProviderWideIdentifiers(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &auditProofRunner{
		exit: 1,
		stdout: []byte(`{"schema_version":"v1","outcome":"violation","assurance":"structural/checksum; unsigned","checked_steps":2,"checked_sources":1,` +
			`"violations":[{"code":"reference_invalid","target":"steps\\P1\u001b","message":"detail"}],` +
			`"incomplete_reasons":[{"code":"canceled","path":"","message":"detail"}]}`),
	}

	got := auditProofs(context.Background(), root, runner)

	want := []auditDiagnostic{
		{code: "reference_invalid", target: "steps\\P1\x1b", message: "agentflow proof reference is invalid"},
		{code: "canceled", target: "", message: "agentflow proof verification was canceled"},
	}
	if got.outcome != "violation" || !reflect.DeepEqual(got.diagnostics, want) {
		t.Fatalf("result = %#v, want violation with %#v", got, want)
	}
}

func TestAuditProofsAcceptsLargeNumberInAdditiveField(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[],"future_number":1e1000}`

	got := auditProofs(context.Background(), root, &auditProofRunner{stdout: []byte(body)})

	if got.outcome != "valid" || got.checked != 1 || got.sources != 1 || len(got.diagnostics) != 0 {
		t.Fatalf("result = %#v", got)
	}
}

func TestAuditProofsAcceptsStructurallyValidUnsatisfiedWorkflowPolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	additiveFields := []string{
		`"gate":{"status":"failed"}`,
		`"step":{"status":"pending"}`,
		`"command":{"exit":17}`,
		`"review":{"blocking":true}`,
		`"future":{"nested":[{"accepted":true}]}`,
	}
	for _, additive := range additiveFields {
		t.Run(additive, func(t *testing.T) {
			body := `{"schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[],` + additive + `}`
			got := auditProofs(context.Background(), root, &auditProofRunner{stdout: []byte(body)})
			if got.outcome != "valid" || len(got.diagnostics) != 0 {
				t.Fatalf("result = %#v", got)
			}
		})
	}
}

func TestAuditProofsReportsMissingAgentStateAsNotPresent(t *testing.T) {
	runner := &auditProofRunner{stdout: []byte(`{"schema_version":"v1","outcome":"valid","assurance":"structural/checksum; unsigned","checked_steps":1,"checked_sources":1,"violations":[],"incomplete_reasons":[]}`)}

	got := auditProofs(context.Background(), t.TempDir(), runner)

	if got.scope != "proofs" || got.assurance != "structural/checksum; unsigned" || got.outcome != "not-present" || len(got.diagnostics) != 0 {
		t.Fatalf("result = %+v", got)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("missing .agent invoked runner: %#v", runner.calls)
	}
}

func TestAuditProofsReportsOlderProviderWithoutParsingUsageText(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &auditProofRunner{
		exit:   2,
		stdout: []byte("usage may contain private workspace output"),
		stderr: []byte("arbitrary provider text must not escape"),
	}

	got := auditProofs(context.Background(), root, runner)

	want := []auditDiagnostic{{
		code:    "agentflow_unavailable",
		message: "agentflow proof verification unavailable: requires agentflow with --integrity-only support",
	}}
	if got.outcome != "incomplete" || !reflect.DeepEqual(got.diagnostics, want) {
		t.Fatalf("result = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(runner.calls, [][]string{{"verify-proof", "--integrity-only", "--json"}}) {
		t.Fatalf("runner calls = %#v", runner.calls)
	}
}

func TestAuditProofsMapsRunnerFailuresToSafeIncompleteReasons(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want auditDiagnostic
	}{
		{
			name: "missing executable", ctx: context.Background(), err: &exec.Error{Name: "agentflow", Err: exec.ErrNotFound},
			want: auditDiagnostic{code: "agentflow_unavailable", message: "agentflow proof verification unavailable: agentflow executable not found"},
		},
		{
			name: "transport failure", ctx: context.Background(), err: errors.New("private transport detail"),
			want: auditDiagnostic{code: "agentflow_unavailable", message: "agentflow proof verification unavailable"},
		},
		{
			name: "canceled", ctx: canceledContext(), err: context.Canceled,
			want: auditDiagnostic{code: "canceled", message: "agentflow proof verification was canceled"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := auditProofs(test.ctx, root, &auditProofRunner{err: test.err})
			if got.outcome != "incomplete" || !reflect.DeepEqual(got.diagnostics, []auditDiagnostic{test.want}) {
				t.Fatalf("result = %#v, want %#v", got, test.want)
			}
		})
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
