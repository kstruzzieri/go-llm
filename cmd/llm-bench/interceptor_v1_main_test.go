//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The -interceptor-v1-report mode end to end: it writes the committed report
// to -report, refuses to overwrite its cases input, and is exclusive with the
// other modes.
func TestMainInterceptorV1Report(t *testing.T) {
	root := corpusRepoRoot(t)
	cases := filepath.Join(root, injectionCorpusDir, "cases.json")
	out := filepath.Join(t.TempDir(), "report.md")
	if code, log := runLLMBenchMain(t, nil, "-interceptor-v1-report", cases, "-report", out); code != 0 {
		t.Fatalf("exit %d output %q", code, log)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(root, interceptorV1ReportPath))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Error("the mode's report differs from the committed report")
	}

	scratch := filepath.Join(t.TempDir(), "cases.json")
	raw, err := os.ReadFile(cases)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scratch, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	code, log := runLLMBenchMain(t, nil, "-interceptor-v1-report", scratch, "-report", scratch)
	if code != 1 || !strings.Contains(log, "-report must differ from -interceptor-v1-report") {
		t.Errorf("report over the cases file: exit %d output %q", code, log)
	}
	if after, _ := os.ReadFile(scratch); string(after) != string(raw) {
		t.Error("the refused run overwrote the cases file")
	}

	code, log = runLLMBenchMain(t, nil, "-interceptor-v1-report", cases, "-injection-report")
	if code != 1 || !strings.Contains(log, "-interceptor-v1-report") || !strings.Contains(log, "mutually exclusive") {
		t.Errorf("with -injection-report: exit %d output %q", code, log)
	}
}
