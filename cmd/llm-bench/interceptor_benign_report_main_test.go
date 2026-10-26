//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The -interceptor-benign-report mode end to end: it writes the committed
// report to -report, needs -registration, refuses to overwrite an input, and
// is exclusive with the other modes.
func TestMainInterceptorBenignReport(t *testing.T) {
	root := corpusRepoRoot(t)
	reg := filepath.Join(benignCorpusDir(t), "registration.json")
	out := filepath.Join(t.TempDir(), "report.md")
	if code, log := runLLMBenchMain(t, nil, "-interceptor-benign-report", "-registration", reg, "-report", out); code != 0 {
		t.Fatalf("exit %d output %q", code, log)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(root, interceptorBenignReportPath))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Error("the mode's report differs from the committed report")
	}
	// The alias refusals run before anything is loaded, so they are probed on
	// scratch copies: a mutated refusal must not clobber the committed corpus.
	scratch := t.TempDir()
	sReg, sCases := filepath.Join(scratch, "registration.json"), filepath.Join(scratch, "cases.json")
	for _, f := range []string{sReg, sCases} {
		if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-interceptor-benign-report"}, "-interceptor-benign-report requires -registration"},
		{[]string{"-interceptor-benign-report", "-registration", sReg, "-report", sReg}, "-report must differ from -registration"},
		{[]string{"-interceptor-benign-report", "-registration", sReg, "-report", sCases}, "-report must differ from -registration (cases.json)"},
		{[]string{"-interceptor-benign-report", "-registration", reg, "-interceptor-v1-report", "c.json"}, "mutually exclusive"},
	} {
		code, log := runLLMBenchMain(t, nil, tc.args...)
		if code != 1 || !strings.Contains(log, tc.want) {
			t.Errorf("%v: exit %d output %q, want %q", tc.args, code, log, tc.want)
		}
	}
	for _, f := range []string{sReg, sCases} {
		if after, _ := os.ReadFile(f); string(after) != "{}\n" {
			t.Errorf("a refused run overwrote %s", filepath.Base(f))
		}
	}
}
