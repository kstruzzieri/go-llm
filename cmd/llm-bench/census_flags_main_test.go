//go:build !windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The subprocess half of TestStrayCensusFlags: runLLMBenchMain is !windows, so
// the end-to-end refusal lives beside it in a !windows file.
func TestStrayCensusFlagsRefusedByMain(t *testing.T) {
	for _, flag := range []string{"-census-manifest", "-census-root", "-census-out"} {
		// -fc-render alone fails fast on its own missing -fc-sidemap if the
		// census flag were accepted.
		code, out := runLLMBenchMain(t, nil, flag, t.TempDir(), "-fc-render")
		if code != 1 || !strings.Contains(out, flag+" require -interceptor-census") {
			t.Errorf("%s without -interceptor-census: exit %d output %q", flag, code, out)
		}
	}
}

// The one reproduce command (README): main passes -benign-authored through,
// so the subprocess writes the same bytes the draft mode does, and reports
// the delegated tasks apart from the extraction tasks.
func TestBenignDraftAuthoredByMain(t *testing.T) {
	manifest, root, results := benignFixtureCensus(t)
	authored, _ := fixtureDelegated(t)
	viaMain, direct := filepath.Join(t.TempDir(), "cases.json"), filepath.Join(t.TempDir(), "cases.json")
	code, out := runLLMBenchMain(t, nil, "-interceptor-benign-draft", viaMain, "-census-root", root, "-census-manifest", manifest,
		"-census-results", results, "-benign-n", "3", "-benign-nonfiring", "1", "-benign-authored", authored)
	if code != 0 || !strings.Contains(out, "extraction 3 of 4 eligible") || !strings.Contains(out, "; 2 delegated;") {
		t.Fatalf("exit %d output %q", code, out)
	}
	p := benignDraftParams{N: 3, Nonfiring: 1, Seed: benignSeed, Authored: authored}
	if err := runBenignDraftMode(t.Context(), manifest, root, results, direct, p, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(viaMain)
	b, _ := os.ReadFile(direct)
	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Error("the CLI draft differs from runBenignDraftMode's")
	}
}

// The #517 benign modes: their flags are refused without their mode, the
// census inputs serve the draft, and the two modes exclude each other.
func TestBenignFlagsRefusedByMain(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-benign-out", t.TempDir(), "-fc-render"}, "-benign-out require -build-interceptor-benign"},
		{[]string{"-census-results", "x.json", "-fc-render"}, "-census-results require -interceptor-benign-draft"},
		{[]string{"-benign-reject", "a/b", "-fc-render"}, "-benign-reject require -interceptor-benign-draft"},
		{[]string{"-benign-authored", "a.json", "-fc-render"}, "-benign-authored require -interceptor-benign-draft"},
		{[]string{"-interceptor-benign-draft", "same.json", "-benign-authored", "same.json"}, "-interceptor-benign-draft must differ from -benign-authored"},
		{[]string{"-interceptor-benign-draft", "d.json", "-census-out", t.TempDir()}, "-census-out require -interceptor-census"},
		{[]string{"-interceptor-benign-draft", "d.json", "-build-interceptor-benign", "c.json"}, "are mutually exclusive"},
		// -census-root is the draft's input: the run gets past the flag checks
		// and fails on the missing manifest instead.
		{[]string{"-interceptor-benign-draft", filepath.Join(t.TempDir(), "d.json"), "-census-root", t.TempDir(), "-census-manifest", filepath.Join(t.TempDir(), "none.json")}, "interceptor-benign-draft: census manifest"},
	} {
		code, out := runLLMBenchMain(t, nil, tc.args...)
		if code != 1 || !strings.Contains(out, tc.want) {
			t.Errorf("%v: exit %d output %q, want %q", tc.args, code, out, tc.want)
		}
	}
}

// -build-interceptor-benign names the directory it publishes to and the stale
// traces it prunes there, before publishing (-benign-out defaults to the
// committed traces directory).
func TestBenignBuildNamesOutputDir(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "ben-stale.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := filepath.Join(benignCorpusDir(t), "cases.json")
	code, log := runLLMBenchMain(t, nil, "-build-interceptor-benign", cases, "-benign-out", out)
	if want := "benign build: publishing 252 trace(s) to " + out + ", removing 1 stale ben-*.json"; code != 0 || !strings.Contains(log, want) {
		t.Fatalf("exit %d output %q, want %q", code, log, want)
	}
	if _, err := os.Stat(filepath.Join(out, "ben-stale.json")); !os.IsNotExist(err) {
		t.Errorf("the stale trace survived: %v", err)
	}
}
