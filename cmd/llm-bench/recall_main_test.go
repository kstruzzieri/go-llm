//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The -build-recall-corpus flags: -recall-split and -recall-out are required
// with the mode and refused without it, the mode excludes every other, and a
// refused or failing invocation creates nothing.
func TestRecallCLIFlagsRefusedByMain(t *testing.T) {
	cases := filepath.FromSlash(recallFixturePath)
	out := filepath.Join(t.TempDir(), "out")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-build-recall-corpus", cases}, "-build-recall-corpus requires -recall-split"},
		{[]string{"-build-recall-corpus", cases, "-recall-split", "pilot"}, "-build-recall-corpus requires -recall-out"},
		{[]string{"-recall-out", out, "-fc-render"}, "-recall-out require -build-recall-corpus"},
		{[]string{"-recall-split", "pilot", "-fc-render"}, "-recall-split require -build-recall-corpus"},
		{[]string{"-build-recall-corpus", cases, "-recall-split", "pilot", "-recall-out", out, "-fc-render"}, "are mutually exclusive"},
		// The flags reach the builder: the two-family fixture cannot satisfy the
		// confirmatory template counts, so the build refuses before writing.
		{[]string{"-build-recall-corpus", cases, "-recall-split", "confirmatory", "-recall-out", out}, "build-recall-corpus: recall build: template"},
	} {
		code, log := runLLMBenchMain(t, nil, tc.args...)
		if code != 1 || !strings.Contains(log, tc.want) {
			t.Errorf("%v: exit %d output %q, want %q", tc.args, code, log, tc.want)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a refused invocation created the output dir")
	}
}

// The -recall-report flags: the mode excludes every other, needs -captures and
// -registration, and -recall-run and -recall-headroom are refused without it.
// -recall-run is required for a confirmatory registration and refused for a
// pilot one, so it is checked once the registration is read.
func TestRecallReportFlagsRefusedByMain(t *testing.T) {
	conf := recallExactRegistrationFile(t, recallTestTraces(recallFamilyCount, "confirmatory"), "confirmatory", nil)
	pilot := recallExactRegistrationFile(t, recallTestTraces(recallSplitFamilyCount("pilot"), "pilot"), "pilot", nil)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-recall-run", "2", "-fc-render"}, "-recall-run require -recall-report"},
		{[]string{"-recall-headroom", "-fc-render"}, "-recall-headroom require -recall-report"},
		{[]string{"-recall-report", "-fc-render"}, "are mutually exclusive"},
		{[]string{"-recall-report"}, "-recall-report requires -captures and -registration"},
		{[]string{"-recall-report", "-captures", "x.jsonl", "-registration", "missing.json"}, "recall report: recall registration: read"},
		{[]string{"-recall-report", "-recall-run", "3", "-captures", "x.jsonl", "-registration", "missing.json"}, "-recall-run must be 1 or 2"},
		{[]string{"-recall-report", "-captures", "x.jsonl", "-registration", conf}, "a confirmatory registration requires -recall-run 1 or 2"},
		{[]string{"-recall-report", "-recall-run", "1", "-captures", "x.jsonl", "-registration", pilot}, "-recall-run does not apply to a pilot registration"},
		{[]string{"-recall-report", "-recall-headroom", "-recall-run", "1", "-captures", "x.jsonl", "-registration", "missing.json"}, "-recall-run does not apply to -recall-headroom"},
		{[]string{"-recall-report", "-recall-run", "1", "-captures", "x.jsonl", "-registration", "missing.json"}, "recall report: recall registration: read"},
		{[]string{"-recall-report", "-recall-headroom", "-captures", "x.jsonl", "-registration", "missing.json"}, "recall report: recall registration: read"},
	} {
		code, log := runLLMBenchMain(t, nil, tc.args...)
		if code != 1 || !strings.Contains(log, tc.want) {
			t.Errorf("%v: exit %d output %q, want %q", tc.args, code, log, tc.want)
		}
	}
}

// Runbook step 1: -recall-report on a pilot registration, without -recall-run,
// renders the pilot view and states that there is no verdict.
func TestRecallReportPilotViewByMain(t *testing.T) {
	traces := recallTestTraces(recallSplitFamilyCount("pilot"), "pilot")
	models := recallRegModels
	reg, digest := recallTestRegistration(t, traces, "pilot")
	capture := recallTestCapture(t, traces, models, digest, recallTestDefault(1, 1), nil)
	code, out := runLLMBenchMain(t, nil, "-recall-report", "-captures", capture, "-registration", reg)
	if code != 0 {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
	for _, want := range []string{"## Pilot view", "| " + recallPrimaryModel + " | recall-f00-A-1 | relied | none | The retry limit is 5. |", "recall pilot view: no verdict"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
