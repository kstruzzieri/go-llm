package main

// Output/input alias audit (#331 slice 3c, external PR review P1): every
// output flag in every mode must route through refuseOutputAlias against all
// of that mode's inputs and its sibling outputs — the review reproduced
// -assembly-report -report clobbering the artifacts JSONL. Guards fire
// BEFORE any input is loaded, so each case here only needs paths, not valid
// file contents.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathsAliasResolvesNonexistentLeavesThroughSymlinkedParents(t *testing.T) {
	dir := t.TempDir()
	realParent := filepath.Join(dir, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(dir, "link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	got, err := pathsAlias(filepath.Join(realParent, "future.json"), filepath.Join(linkParent, "future.json"))
	if err != nil {
		t.Fatalf("pathsAlias: %v", err)
	}
	if !got {
		t.Fatal("pathsAlias = false, want true for future leaves below the same symlinked parent")
	}
}

func TestPathsAliasRejectsDanglingOutputSymlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.json")
	if err := os.WriteFile(src, []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "output.json")
	if err := os.Symlink(filepath.Join(dir, "missing.json"), dangling); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	if _, err := pathsAlias(src, dangling); err == nil {
		t.Fatal("pathsAlias accepted dangling output symlink")
	}
}

func TestPathsAliasTreatsCaseOnlyFutureNamesAsAliases(t *testing.T) {
	dir := t.TempDir()
	got, err := pathsAlias(filepath.Join(dir, "Future.json"), filepath.Join(dir, "future.json"))
	if err != nil {
		t.Fatalf("pathsAlias: %v", err)
	}
	if !got {
		t.Fatal("pathsAlias = false, want true for case-only future names")
	}
}

func TestPathsAliasUsesExistingFileIdentityBeforeCaseFolding(t *testing.T) {
	dir := t.TempDir()
	upper := filepath.Join(dir, "A.json")
	lower := filepath.Join(dir, "a.json")
	if err := os.WriteFile(upper, []byte("upper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lower, []byte("lower\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	upperInfo, err := os.Stat(upper)
	if err != nil {
		t.Fatal(err)
	}
	lowerInfo, err := os.Stat(lower)
	if err != nil {
		t.Fatal(err)
	}

	got, err := pathsAlias(upper, lower)
	if err != nil {
		t.Fatalf("pathsAlias: %v", err)
	}
	if os.SameFile(upperInfo, lowerInfo) {
		if !got {
			t.Fatal("pathsAlias = false, want true when this filesystem resolves case-only existing names to one file")
		}
		return
	}
	if got {
		t.Fatal("pathsAlias = true, want false for distinct existing case-only files")
	}
}

func TestPathsAliasTreatsExistingHardlinksAndSymlinksAsAliases(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte("target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(dir, "hardlink.json")
	if err := os.Link(target, hardlink); err != nil {
		t.Skipf("hardlinks unsupported here: %v", err)
	}
	symlink := filepath.Join(dir, "symlink.json")
	if err := os.Symlink(target, symlink); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	for _, path := range []string{hardlink, symlink} {
		got, err := pathsAlias(target, path)
		if err != nil {
			t.Fatalf("pathsAlias(%q): %v", path, err)
		}
		if !got {
			t.Errorf("pathsAlias(%q) = false, want true", path)
		}
	}
}

func TestMainOutputAliasGuardsAllModes(t *testing.T) {
	dir := t.TempDir()
	mkFile := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	arts := mkFile("artifacts.jsonl")
	labels := mkFile("labels.jsonl")
	corpus := mkFile("corpus-manifest.jsonl")
	captureMan := mkFile("artifacts.jsonl.manifest.json")
	trace := mkFile("trace.json")
	fimCase := mkFile("fim-case.json")
	xlamSrc := mkFile("xlam.json")
	sidemap := filepath.Join(dir, "sidemap.json") // need not exist: guards fire before loads
	discOut := filepath.Join(dir, "discriminators.jsonl")

	cases := []struct {
		name, wantErr string
		args          []string
	}{
		{"assembly-report report aliasing artifacts", "-report must differ from -artifacts",
			[]string{"-assembly-report", "-labels", labels, "-artifacts", arts, "-report", arts}},
		{"assembly-report report aliasing capture manifest", "-report must differ from -capture-manifest",
			[]string{"-assembly-report", "-labels", labels, "-artifacts", arts, "-capture-manifest", captureMan, "-report", captureMan}},
		{"manual-report report aliasing labels", "-report must differ from -labels",
			[]string{"-manual-report", "-labels", labels, "-artifacts", arts, "-report", labels}},
		{"paired-report report aliasing artifacts", "-report must differ from -artifacts",
			[]string{"-paired-report", "-labels", labels, "-artifacts", arts, "-report", arts}},
		{"paired-report report aliasing corpus manifest", "-report must differ from -corpus-manifest",
			[]string{"-paired-report", "-labels", labels, "-artifacts", arts, "-corpus-manifest", corpus, "-report", corpus}},
		{"discrimination manifest-out aliasing corpus manifest", "-discriminator-manifest-out must differ from -corpus-manifest",
			[]string{"-discrimination-report", "-labels", labels, "-artifacts", arts, "-corpus-manifest", corpus, "-discriminator-manifest-out", corpus}},
		{"discrimination report aliasing manifest-out", "-report must differ from -discriminator-manifest-out",
			[]string{"-discrimination-report", "-labels", labels, "-artifacts", arts, "-corpus-manifest", corpus, "-discriminator-manifest-out", discOut, "-report", discOut}},
		{"blind-render report aliasing artifacts", "-report must differ from -artifacts",
			[]string{"-blind-render", "-artifacts", arts, "-report", arts}},
		{"fc-render report aliasing sidemap", "-report must differ from -fc-sidemap",
			[]string{"-fc-render", "-artifacts", arts, "-fc-sidemap", sidemap, "-report", sidemap}},
		{"fc-render report aliasing artifacts", "-report must differ from -artifacts",
			[]string{"-fc-render", "-artifacts", arts, "-fc-sidemap", sidemap, "-report", arts}},
		{"adjudicate-render report aliasing labels", "-report must differ from -labels",
			[]string{"-adjudicate-render", "-artifacts", arts, "-labels", labels, "-report", labels}},
		{"calibrate-capture labels-out aliasing a trace", "-labels-out must differ from -traces",
			[]string{"-calibrate-capture", "-traces", trace, "-models", "m", "-labels-out", trace}},
		{"run report aliasing a trace", "-report must differ from -traces",
			[]string{"-traces", trace, "-models", "m", "-report", trace}},
		{"run report aliasing manual labels", "-report must differ from -labels",
			[]string{"-traces", trace, "-models", "m", "-scorer", "manual", "-labels", labels, "-report", labels}},
		{"fim-latency report aliasing a case", "-report must differ from -fim-cases",
			[]string{"-fim-latency", "-fim-cases", fimCase, "-models", "m", "-report", fimCase}},
		{"import-xlam manifest aliasing the source", "-import-xlam-manifest must differ from -import-xlam",
			[]string{"-import-xlam", xlamSrc, "-import-xlam-manifest", xlamSrc, "-import-xlam-out", filepath.Join(dir, "xlam-out")}},
		// Round-2 review P2 (a): the -blind-render two-output collision was
		// caught by cleaned-path equality only; it now routes through
		// refuseOutputAlias and fires before any input loads.
		{"blind-render blockmap-out aliasing report", "-blind-blockmap-out must differ from -report",
			[]string{"-blind-render", "-artifacts", arts, "-blind-blockmap-out", filepath.Join(dir, "map.json"), "-report", filepath.Join(dir, "map.json")}},
		{"blind-render blockmap-out aliasing artifacts", "-blind-blockmap-out must differ from -artifacts",
			[]string{"-blind-render", "-artifacts", arts, "-blind-blockmap-out", arts, "-report", filepath.Join(dir, "ws.txt")}},
		// Round-2 review P2 (b): a normal run's -report could truncate the
		// OPEN -judge-cache SQLite file; the cache belongs in the guard set.
		{"run report aliasing the judge cache", "-report must differ from -judge-cache",
			[]string{"-traces", trace, "-models", "m", "-judge-cache", labels, "-report", labels}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], tc.args...)
			cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("output aliasing an input was accepted:\n%s", out)
			}
			if !strings.Contains(string(out), tc.wantErr) {
				t.Fatalf("output missing %q:\n%s", tc.wantErr, out)
			}
		})
	}

	t.Run("hardlinked report caught by os.SameFile", func(t *testing.T) {
		hard := filepath.Join(dir, "report-hardlink.json")
		if err := os.Link(arts, hard); err != nil {
			t.Skipf("hardlink unsupported here: %v", err)
		}
		cmd := exec.Command(os.Args[0],
			"-assembly-report", "-labels", labels, "-artifacts", arts, "-report", hard)
		cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("hardlinked report path accepted:\n%s", out)
		}
		if !strings.Contains(string(out), "resolves to the same file as -artifacts") {
			t.Fatalf("output missing the SameFile refusal:\n%s", out)
		}
	})

	// Round-2 review P1: the calibrate-capture artifacts output and its
	// manifest sibling were each guarded against the trace inputs but not
	// against EACH OTHER — a pre-existing hardlink between <labels-out> and
	// <labels-out>.manifest.json ended with the artifacts path holding the
	// manifest bytes.
	t.Run("calibrate-capture hardlinked manifest sibling caught by os.SameFile", func(t *testing.T) {
		lout := filepath.Join(dir, "cap-artifacts.jsonl")
		if err := os.WriteFile(lout, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(lout, lout+".manifest.json"); err != nil {
			t.Skipf("hardlink unsupported here: %v", err)
		}
		cmd := exec.Command(os.Args[0],
			"-calibrate-capture", "-traces", trace, "-models", "m", "-labels-out", lout)
		cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("hardlinked labels-out/manifest pair accepted:\n%s", out)
		}
		if !strings.Contains(string(out), "resolves to the same file as -labels-out") {
			t.Fatalf("output missing the sibling SameFile refusal:\n%s", out)
		}
	})

	// Round-2 review P2 (a): a hardlink between -blind-blockmap-out and
	// -report lost the only block map when the worksheet write landed second.
	t.Run("blind-render hardlinked blockmap/report pair caught by os.SameFile", func(t *testing.T) {
		mapOut := filepath.Join(dir, "blockmap.json")
		if err := os.WriteFile(mapOut, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		report := filepath.Join(dir, "worksheet-hardlink.txt")
		if err := os.Link(mapOut, report); err != nil {
			t.Skipf("hardlink unsupported here: %v", err)
		}
		cmd := exec.Command(os.Args[0],
			"-blind-render", "-artifacts", arts, "-blind-blockmap-out", mapOut, "-report", report)
		cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("hardlinked blockmap/report pair accepted:\n%s", out)
		}
		if !strings.Contains(string(out), "resolves to the same file as -report") {
			t.Fatalf("output missing the SameFile refusal:\n%s", out)
		}
	})
}
