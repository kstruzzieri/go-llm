package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func validAssemblyTrace(mode AssemblyMode) Trace {
	return Trace{
		ID:     "case-1-" + string(mode),
		Source: "assembly-corpus",
		System: "answer from context",
		Turns:  []Turn{{Role: "user", Content: "ctx + question"}},
		Golden: Golden{FinalAnswerCriteria: "names the port"},
		AssemblyEval: &AssemblyEval{
			PairID:                "case-1",
			Mode:                  mode,
			CandidateIDs:          []string{"c1", "c2"},
			EstimatedPromptTokens: 100,
		},
	}
}

func TestValidateTraceAssemblyEval(t *testing.T) {
	if err := validateTrace(validAssemblyTrace(AssemblyFlat)); err != nil {
		t.Fatalf("valid flat trace rejected: %v", err)
	}
	if err := validateTrace(validAssemblyTrace(AssemblyProgressive)); err != nil {
		t.Fatalf("valid progressive trace rejected: %v", err)
	}

	bad := validAssemblyTrace(AssemblyFlat)
	bad.AssemblyEval.Mode = "outline"
	if err := validateTrace(bad); err == nil || !strings.Contains(err.Error(), "assembly_eval") {
		t.Fatalf("unknown mode accepted or wrong error: %v", err)
	}

	bad = validAssemblyTrace(AssemblyFlat)
	bad.AssemblyEval.PairID = ""
	if err := validateTrace(bad); err == nil || !strings.Contains(err.Error(), "assembly_eval") {
		t.Fatalf("blank PairID accepted or wrong error: %v", err)
	}

	bad = validAssemblyTrace(AssemblyFlat)
	bad.AssemblyEval.CandidateIDs = nil
	if err := validateTrace(bad); err == nil || !strings.Contains(err.Error(), "assembly_eval") {
		t.Fatalf("empty CandidateIDs accepted or wrong error: %v", err)
	}

	bad = validAssemblyTrace(AssemblyFlat)
	bad.AssemblyEval.EstimatedPromptTokens = 0
	if err := validateTrace(bad); err == nil || !strings.Contains(err.Error(), "assembly_eval") {
		t.Fatalf("non-positive EstimatedPromptTokens accepted or wrong error: %v", err)
	}

	// nil AssemblyEval is a normal non-assembly trace — untouched path.
	plain := validAssemblyTrace(AssemblyFlat)
	plain.AssemblyEval = nil
	if err := validateTrace(plain); err != nil {
		t.Fatalf("plain trace rejected: %v", err)
	}
}

// assemblyFixtureJSON is the builder-test corpus: two structurally DIFFERENT
// cases so paired assertions cannot pass by accident of symmetric layout.
// Case 1 carries its single summary pair on the MIDDLE source; case 2 carries
// two summary pairs on its first and last sources.
const assemblyFixtureJSON = `[
  {
    "id": "case-metadata",
    "category": "metadata",
    "question": "Which port does the gateway server listen on?",
    "golden": {"final_answer_criteria": "States the gateway listens on port 9443."},
    "max_tokens": 512,
    "sources": [
      {
        "path": "pkg/api/handlers.go",
        "content": "package api\n\n// RegisterHandlers wires the HTTP routes.\nfunc RegisterHandlers(mux *http.ServeMux) {\n\tmux.HandleFunc(\"/healthz\", healthz)\n}\n",
        "language": "go"
      },
      {
        "path": "pkg/api/server.go",
        "content": "package api\n\nconst gatewayPort = 9443\n\n// Serve starts the gateway listener.\nfunc Serve() error {\n\treturn http.ListenAndServe(fmt.Sprintf(\":%d\", gatewayPort), nil)\n}\n",
        "language": "go",
        "abstract": "Gateway HTTP server entry point; owns the listen port constant.",
        "overview": "Defines gatewayPort (9443) and Serve, which binds the gateway HTTP listener to that port."
      },
      {
        "path": "pkg/api/middleware.go",
        "content": "package api\n\n// withLogging wraps a handler with request logging.\nfunc withLogging(next http.Handler) http.Handler {\n\treturn loggingHandler{next}\n}\n",
        "language": "go"
      }
    ]
  },
  {
    "id": "case-content",
    "category": "content_only",
    "question": "What does the retry helper do when the budget is exhausted?",
    "golden": {"final_answer_criteria": "Says retryWithBudget returns ErrBudgetExhausted once attempts are used up."},
    "max_tokens": 512,
    "sources": [
      {
        "path": "internal/retry/retry.go",
        "content": "package retry\n\n// retryWithBudget retries fn until the attempt budget is spent.\nfunc retryWithBudget(fn func() error, budget int) error {\n\tfor i := 0; i < budget; i++ {\n\t\tif err := fn(); err == nil {\n\t\t\treturn nil\n\t\t}\n\t}\n\treturn ErrBudgetExhausted\n}\n",
        "language": "go",
        "abstract": "Bounded retry loop returning ErrBudgetExhausted when attempts run out.",
        "overview": "retryWithBudget invokes fn up to budget times, returning nil on first success and ErrBudgetExhausted after the final failure."
      },
      {
        "path": "internal/retry/backoff.go",
        "content": "package retry\n\n// nextDelay doubles the delay up to the cap.\nfunc nextDelay(d time.Duration) time.Duration {\n\tif d*2 > maxDelay {\n\t\treturn maxDelay\n\t}\n\treturn d * 2\n}\n",
        "language": "go"
      },
      {
        "path": "internal/retry/errors.go",
        "content": "package retry\n\nimport \"errors\"\n\n// ErrBudgetExhausted reports a spent retry budget.\nvar ErrBudgetExhausted = errors.New(\"retry: budget exhausted\")\n",
        "language": "go",
        "abstract": "Error values for the retry package.",
        "overview": "Declares ErrBudgetExhausted, the sentinel returned when the retry budget is spent."
      }
    ]
  }
]`

func mustReadAssemblyFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readAssemblyJSON(t *testing.T, path string, dst any) {
	t.Helper()
	if err := json.Unmarshal(mustReadAssemblyFile(t, path), dst); err != nil {
		t.Fatal(err)
	}
}

func TestAssemblyBuildProducesValidPairs(t *testing.T) {
	dir := t.TempDir()
	fixture := writeFile(t, dir, "cases.json", assemblyFixtureJSON)
	outDir := filepath.Join(dir, "traces")

	if err := assemblyBuild(context.Background(), fixture, outDir); err != nil {
		t.Fatalf("assemblyBuild: %v", err)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	var traceEntries []os.DirEntry
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			traceEntries = append(traceEntries, entry)
		}
	}
	if len(traceEntries) != 4 { // 2 cases x 2 arms
		t.Fatalf("wrote %d traces, want 4", len(traceEntries))
	}
	byPair := map[string][]Trace{}
	for _, e := range traceEntries {
		var tr Trace
		readAssemblyJSON(t, filepath.Join(outDir, e.Name()), &tr)
		if err := validateTrace(tr); err != nil {
			t.Fatalf("built trace %s invalid: %v", e.Name(), err)
		}
		if tr.AssemblyEval == nil {
			t.Fatalf("trace %s missing assembly_eval", e.Name())
		}
		byPair[tr.AssemblyEval.PairID] = append(byPair[tr.AssemblyEval.PairID], tr)
	}
	for pair, arms := range byPair {
		if len(arms) != 2 {
			t.Fatalf("pair %s has %d arms, want 2", pair, len(arms))
		}
		a, b := arms[0].AssemblyEval, arms[1].AssemblyEval
		if a.Mode == b.Mode {
			t.Fatalf("pair %s has duplicate mode %s", pair, a.Mode)
		}
		// Candidate-set equality is the #246 guard: representation may not
		// prune selection, so both arms carry identical candidate IDs.
		if !reflect.DeepEqual(a.CandidateIDs, b.CandidateIDs) {
			t.Fatalf("pair %s candidate sets differ: %v vs %v", pair, a.CandidateIDs, b.CandidateIDs)
		}
		// Both arms embed rendered context + the same question in one user turn.
		for _, tr := range arms {
			if len(tr.Turns) != 1 || tr.Turns[0].Role != "user" || tr.Turns[0].Content == "" {
				t.Fatalf("pair %s arm %s: want exactly one non-empty user turn", pair, tr.AssemblyEval.Mode)
			}
			if tr.AssemblyEval.Mode == AssemblyProgressive &&
				!strings.Contains(tr.Turns[0].Content, "purpose:") {
				t.Fatalf("pair %s: progressive arm never rendered a fresh summary", pair)
			}
			// Pin guard: byte-identity cannot falsify the indexed_at pin (both
			// builds run in the same wall-clock second), so assert the fixed
			// epoch's rendering directly. Must match the "indexed:" line, not
			// the bare timestamp — the summary line shares the same epoch and
			// would keep a bare-timestamp check green with the pin deleted.
			if tr.AssemblyEval.Mode == AssemblyProgressive &&
				!strings.Contains(tr.Turns[0].Content, "indexed: 2025-07-27T00:00:00Z") {
				t.Fatalf("pair %s: progressive arm missing pinned indexed_at rendering", pair)
			}
		}
		// The progressive arm must actually differ from flat (v2 headers).
		if arms[0].Turns[0].Content == arms[1].Turns[0].Content {
			t.Fatalf("pair %s arms rendered identical context", pair)
		}
	}
	// Determinism: a second build into a fresh dir is byte-identical.
	outDir2 := filepath.Join(dir, "traces2")
	if err := assemblyBuild(context.Background(), fixture, outDir2); err != nil {
		t.Fatal(err)
	}
	for _, e := range traceEntries {
		b1 := mustReadAssemblyFile(t, filepath.Join(outDir, e.Name()))
		b2 := mustReadAssemblyFile(t, filepath.Join(outDir2, e.Name()))
		if !bytes.Equal(b1, b2) {
			t.Fatalf("non-deterministic build for %s", e.Name())
		}
	}
}

func TestAssemblyCommittedCorpusUpToDate(t *testing.T) {
	corpusDir := filepath.Join("..", "..", "docs", "llm", "assembly-corpus")
	wantDir := filepath.Join(corpusDir, "traces")
	gotDir := t.TempDir()
	if err := assemblyBuild(context.Background(), filepath.Join(corpusDir, "cases.json"), gotDir); err != nil {
		t.Fatal(err)
	}

	wantEntries, err := os.ReadDir(wantDir)
	if err != nil {
		t.Fatal(err)
	}
	gotEntries, err := os.ReadDir(gotDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotEntries) != len(wantEntries) {
		t.Fatalf("generated %d traces, committed corpus has %d", len(gotEntries), len(wantEntries))
	}
	for i, wantEntry := range wantEntries {
		if gotEntries[i].Name() != wantEntry.Name() {
			t.Fatalf("trace %d = %q, want %q", i, gotEntries[i].Name(), wantEntry.Name())
		}
		got := mustReadAssemblyFile(t, filepath.Join(gotDir, gotEntries[i].Name()))
		want := mustReadAssemblyFile(t, filepath.Join(wantDir, wantEntry.Name()))
		if !bytes.Equal(got, want) {
			t.Fatalf("committed trace %s is stale; rebuild the assembly corpus", wantEntry.Name())
		}
	}
}

func TestAssemblyBuildRemovesStaleGeneratedTraces(t *testing.T) {
	dir := t.TempDir()
	fixture := writeFile(t, dir, "cases.json", assemblyFixtureJSON)
	outDir := filepath.Join(dir, "traces")
	if err := assemblyBuild(context.Background(), fixture, outDir); err != nil {
		t.Fatal(err)
	}
	keepPath := filepath.Join(outDir, "keep.txt")
	if err := os.WriteFile(keepPath, []byte("not an assembly trace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var cases []assemblyCase
	if err := json.Unmarshal([]byte(assemblyFixtureJSON), &cases); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cases[:1])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := assemblyBuild(context.Background(), fixture, outDir); err != nil {
		t.Fatal(err)
	}

	for _, stale := range []string{"case-content-flat.json", "case-content-progressive.json"} {
		if _, err := os.Stat(filepath.Join(outDir, stale)); !os.IsNotExist(err) {
			t.Fatalf("stale generated trace %s remains: %v", stale, err)
		}
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("non-assembly file was removed: %v", err)
	}
}

func TestAssemblyBuildRefusesUnownedAssemblyJSON(t *testing.T) {
	dir := t.TempDir()
	fixture := writeFile(t, dir, "cases.json", assemblyFixtureJSON)
	outDir := filepath.Join(dir, "traces")
	if err := assemblyBuild(context.Background(), fixture, outDir); err != nil {
		t.Fatal(err)
	}
	owned := mustReadAssemblyFile(t, filepath.Join(outDir, "case-content-flat.json"))
	unowned := filepath.Join(outDir, "valuable-run.json")
	if err := os.WriteFile(unowned, owned, 0o644); err != nil {
		t.Fatal(err)
	}

	var cases []assemblyCase
	if err := json.Unmarshal([]byte(assemblyFixtureJSON), &cases); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cases[:1])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	err = assemblyBuild(context.Background(), fixture, outDir)
	if err == nil || !strings.Contains(err.Error(), "unowned JSON") {
		t.Fatalf("assemblyBuild error = %v, want unowned JSON refusal", err)
	}
	if got := mustReadAssemblyFile(t, unowned); !bytes.Equal(got, owned) {
		t.Fatal("builder changed an assembly-shaped JSON file it did not own")
	}
	if _, err := os.Stat(filepath.Join(outDir, "case-content-progressive.json")); err != nil {
		t.Fatalf("builder removed an owned stale trace before preflight failed: %v", err)
	}
}

func TestAssemblyBuildRefusesUnownedOutputCollision(t *testing.T) {
	dir := t.TempDir()
	fixture := writeFile(t, dir, "cases.json", assemblyFixtureJSON)
	outDir := filepath.Join(dir, "traces")
	if err := os.Mkdir(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	collision := filepath.Join(outDir, "case-metadata-flat.json")
	const original = "valuable existing output\n"
	if err := os.WriteFile(collision, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	err := assemblyBuild(context.Background(), fixture, outDir)
	if err == nil || !strings.Contains(err.Error(), "unowned output") {
		t.Fatalf("assemblyBuild error = %v, want unowned output refusal", err)
	}
	if got := string(mustReadAssemblyFile(t, collision)); got != original {
		t.Fatalf("unowned output overwritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(outDir, "case-metadata-progressive.json")); !os.IsNotExist(err) {
		t.Fatalf("builder wrote another trace before preflight failed: %v", err)
	}
}

func TestAssemblyBuildPreflightsStaleFilesBeforeWriting(t *testing.T) {
	var cases []assemblyCase
	if err := json.Unmarshal([]byte(assemblyFixtureJSON), &cases); err != nil {
		t.Fatal(err)
	}
	first, err := json.Marshal(cases[:1])
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fixture := writeFile(t, dir, "cases.json", string(first))
	outDir := filepath.Join(dir, "traces")
	if err := assemblyBuild(context.Background(), fixture, outDir); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(outDir, "case-metadata-flat.json")
	if err := os.WriteFile(stale, []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(cases[1:])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, second, 0o644); err != nil {
		t.Fatal(err)
	}

	err = assemblyBuild(context.Background(), fixture, outDir)
	if err == nil || !strings.Contains(err.Error(), "modified output") {
		t.Fatalf("assemblyBuild error = %v, want modified output refusal", err)
	}
	for _, name := range []string{"case-content-flat.json", "case-content-progressive.json"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); !os.IsNotExist(err) {
			t.Fatalf("builder wrote %s before stale-file preflight failed: %v", name, err)
		}
	}
}

func TestAssemblyBuildRefusesSymlinkOutput(t *testing.T) {
	dir := t.TempDir()
	fixture := writeFile(t, dir, "cases.json", assemblyFixtureJSON)
	outDir := filepath.Join(dir, "traces")
	if err := assemblyBuild(context.Background(), fixture, outDir); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dir, "sentinel")
	const original = "do not overwrite\n"
	if err := os.WriteFile(sentinel, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(outDir, "case-metadata-flat.json")
	if err := os.Remove(output); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, output); err != nil {
		t.Fatal(err)
	}

	err := assemblyBuild(context.Background(), fixture, outDir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("assemblyBuild error = %v, want symlink refusal", err)
	}
	if got := string(mustReadAssemblyFile(t, sentinel)); got != original {
		t.Fatalf("symlink target overwritten: %q", got)
	}
}

func TestAssemblyBuildRejectsEmptyFixtureWithoutDeletingOutput(t *testing.T) {
	dir := t.TempDir()
	fixture := writeFile(t, dir, "cases.json", assemblyFixtureJSON)
	outDir := filepath.Join(dir, "traces")
	if err := assemblyBuild(context.Background(), fixture, outDir); err != nil {
		t.Fatal(err)
	}
	keptTrace := filepath.Join(outDir, "case-content-flat.json")
	if err := os.WriteFile(fixture, []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := assemblyBuild(context.Background(), fixture, outDir); err == nil {
		t.Fatal("empty fixture accepted")
	}
	if _, err := os.Stat(keptTrace); err != nil {
		t.Fatalf("existing traces changed after rejected fixture: %v", err)
	}
}

func TestAssemblyBuildEndLinesMatchRenderedContent(t *testing.T) {
	const fixtureJSON = `[{
		"id":"line-range",
		"category":"content_only",
		"question":"What is the second line?",
		"golden":{"final_answer_criteria":"States beta."},
		"max_tokens":512,
		"sources":[{"path":"two.txt","content":"alpha\nbeta\n","language":"text"}]
	}]`
	dir := t.TempDir()
	fixture := writeFile(t, dir, "cases.json", fixtureJSON)
	outDir := filepath.Join(dir, "traces")
	if err := assemblyBuild(context.Background(), fixture, outDir); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		file   string
		header string
	}{
		{"line-range-flat.json", "--- two.txt (lines 1-2,"},
		{"line-range-progressive.json", `--- source: "two.txt" (lines 1-2,`},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			var tr Trace
			readAssemblyJSON(t, filepath.Join(outDir, tc.file), &tr)
			if got := tr.Turns[0].Content; !strings.Contains(got, tc.header) {
				t.Fatalf("rendered context missing %q:\n%s", tc.header, got)
			}
		})
	}
}

func TestAssemblyChunkIDUnambiguousAndFullWidth(t *testing.T) {
	a := assemblyChunkID("a", "\x00b")
	b := assemblyChunkID("a\x00", "b")
	if a == b {
		t.Fatalf("distinct source/content pairs collided: %q", a)
	}
	if len(a) != 64 || len(b) != 64 {
		t.Fatalf("chunk IDs are not full SHA-256 digests: %q / %q", a, b)
	}
}

func assemblyArtifact(pair string, mode AssemblyMode, model string, tokens int, ids []string) Artifact {
	tr := validAssemblyTrace(mode)
	tr.ID = pair + "-" + string(mode)
	tr.AssemblyEval.PairID = pair
	tr.AssemblyEval.CandidateIDs = ids
	tr.AssemblyEval.EstimatedPromptTokens = tokens
	a := Artifact{TraceID: tr.ID, CandidateModel: model, Trace: tr,
		ActualFinalAnswer: "answer"}
	a.ArtifactHash = artifactHash(a)
	return a
}

func labelFor(a Artifact, quality float64) Label {
	return Label{TraceID: a.TraceID, CandidateModel: a.CandidateModel,
		ArtifactHash: a.ArtifactHash, ExpectedAnswerQuality: quality,
		Labeler: "test"}
}

func TestAssemblyReportPairingAndDecision(t *testing.T) {
	ids := []string{"c1", "c2"}
	var arts []Artifact
	var labels []Label
	// 60 pairs meets the pre-registered minimum; progressive is one legal
	// rubric step better with 50% token reduction.
	for i := 0; i < assemblyMinimumPairsPerModel; i++ {
		pair := fmt.Sprintf("case-%d", i)
		f := assemblyArtifact(pair, AssemblyFlat, "m", 1000, ids)
		p := assemblyArtifact(pair, AssemblyProgressive, "m", 500, ids)
		arts = append(arts, f, p)
		labels = append(labels, labelFor(f, 0.5), labelFor(p, 1.0))
	}
	rep, err := computeAssemblyReport(arts, labels, 1, 10000)
	if err != nil {
		t.Fatalf("computeAssemblyReport: %v", err)
	}
	if len(rep.Models) != 1 {
		t.Fatalf("models=%d, want 1", len(rep.Models))
	}
	model := rep.Models[0]
	if model.Pairs != assemblyMinimumPairsPerModel || model.InvalidPairs != 0 {
		t.Fatalf("pairs=%d invalid=%d, want %d/0",
			model.Pairs, model.InvalidPairs, assemblyMinimumPairsPerModel)
	}
	if model.MedianTokenReduction < 0.49 || model.MedianTokenReduction > 0.51 {
		t.Fatalf("median token reduction = %v, want ~0.5", model.MedianTokenReduction)
	}
	if model.Decision != "quality-improved" {
		t.Fatalf("decision = %q, want quality-improved (uniform +0.5 deltas)", model.Decision)
	}

	// Candidate mismatch invalidates the case, never skews it. The extra
	// pair uses a FRESH pair ID ("case-mismatch", not one of case-0..59):
	// reusing an existing pair ID would trip the duplicate-arm error, which
	// is a different contract tested separately.
	ids2 := []string{"other"}
	badFlat := assemblyArtifact("case-mismatch", AssemblyFlat, "m", 1000, ids)
	bad := assemblyArtifact("case-mismatch", AssemblyProgressive, "m", 500, ids2)
	arts2 := append(append([]Artifact{}, arts...), badFlat, bad)
	labels2 := append(append([]Label{}, labels...), labelFor(badFlat, 0.5), labelFor(bad, 0.5))
	rep2, err := computeAssemblyReport(arts2, labels2, 1, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Models[0].Pairs != assemblyMinimumPairsPerModel || rep2.Models[0].InvalidPairs != 1 {
		t.Fatalf("pairs=%d invalid=%d, want %d/1",
			rep2.Models[0].Pairs, rep2.Models[0].InvalidPairs, assemblyMinimumPairsPerModel)
	}

	// A pair missing one arm is a pairing gap, excluded and counted.
	// Fresh pair ID for the same reason as above.
	orphan := assemblyArtifact("case-orphan", AssemblyFlat, "m", 1000, ids)
	rep3, err := computeAssemblyReport(append(arts, orphan), append(labels, labelFor(orphan, 0.5)), 1, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if rep3.Models[0].Pairs != assemblyMinimumPairsPerModel || rep3.Models[0].PairingGaps != 1 {
		t.Fatalf("pairs=%d gaps=%d, want %d/1",
			rep3.Models[0].Pairs, rep3.Models[0].PairingGaps, assemblyMinimumPairsPerModel)
	}

	// A statistically favorable result remains explicitly ineligible below
	// the corpus target; the report never prints "improved" on a seed corpus.
	small, err := computeAssemblyReport(arts[:10], labels[:10], 1, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if small.Models[0].Decision != "insufficient-corpus" {
		t.Fatalf("small-corpus decision = %q", small.Models[0].Decision)
	}
}

// TestAssemblyReportPerModelSeparation falsifies pooling: two models share
// the same pair IDs with OPPOSITE label patterns. A pooled implementation
// would average the two families to ~0 and report neither effect.
func TestAssemblyReportPerModelSeparation(t *testing.T) {
	ids := []string{"c1", "c2"}
	var arts []Artifact
	var labels []Label
	for i := 0; i < assemblyMinimumPairsPerModel; i++ {
		pair := fmt.Sprintf("case-%d", i)
		mf := assemblyArtifact(pair, AssemblyFlat, "m", 1000, ids)
		mp := assemblyArtifact(pair, AssemblyProgressive, "m", 500, ids)
		nf := assemblyArtifact(pair, AssemblyFlat, "n", 1000, ids)
		np := assemblyArtifact(pair, AssemblyProgressive, "n", 500, ids)
		arts = append(arts, mf, mp, nf, np)
		labels = append(labels,
			labelFor(mf, 0.5), labelFor(mp, 1.0), // m: progressive better
			labelFor(nf, 1.0), labelFor(np, 0.5)) // n: progressive worse
	}
	rep, err := computeAssemblyReport(arts, labels, 1, 10000)
	if err != nil {
		t.Fatalf("computeAssemblyReport: %v", err)
	}
	if len(rep.Models) != 2 {
		t.Fatalf("models=%d, want 2", len(rep.Models))
	}
	m, n := rep.Models[0], rep.Models[1]
	if m.CandidateModel != "m" || n.CandidateModel != "n" {
		t.Fatalf("model order = %q, %q; want sorted m, n", m.CandidateModel, n.CandidateModel)
	}
	if m.Pairs != assemblyMinimumPairsPerModel || n.Pairs != assemblyMinimumPairsPerModel {
		t.Fatalf("pairs = %d/%d, want %d each", m.Pairs, n.Pairs, assemblyMinimumPairsPerModel)
	}
	if m.MeanDelta < 0.49 || m.MeanDelta > 0.51 {
		t.Fatalf("model m mean delta = %v, want ~+0.5", m.MeanDelta)
	}
	if n.MeanDelta > -0.49 || n.MeanDelta < -0.51 {
		t.Fatalf("model n mean delta = %v, want ~-0.5", n.MeanDelta)
	}
	if m.Decision != "quality-improved" {
		t.Fatalf("model m decision = %q, want quality-improved", m.Decision)
	}
	if n.Decision != "regressed" {
		t.Fatalf("model n decision = %q, want regressed", n.Decision)
	}
}

func TestAssemblyReportErrors(t *testing.T) {
	ids := []string{"c1"}
	dupA := assemblyArtifact("p1", AssemblyFlat, "m", 1000, ids)
	dupB := assemblyArtifact("p1", AssemblyFlat, "m", 900, ids) // distinct hash, same arm
	orphan := assemblyArtifact("p2", AssemblyFlat, "m", 1000, ids)
	cases := []struct {
		name    string
		arts    []Artifact
		labels  []Label
		wantErr string
	}{
		{"duplicate flat arm", []Artifact{dupA, dupB}, []Label{labelFor(dupA, 0.5)}, "duplicate flat arm"},
		{"no complete labeled pairs", []Artifact{orphan}, []Label{labelFor(orphan, 0.5)}, "no complete labeled pairs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := computeAssemblyReport(tc.arts, tc.labels, 1, 10000)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestAssemblyDecisionRuleStatuses(t *testing.T) {
	cases := []struct {
		name       string
		ciLo, ciHi float64
		reduction  float64
		want       string
	}{
		{"improved", 0.05, 0.40, 0.30, "quality-improved"},
		{"noninferior", -0.05, 0.20, 0.30, "efficient-noninferior"},
		{"regressed", -0.60, -0.15, 0.30, "regressed"},
		{"inconclusive-ci", -0.20, 0.30, 0.30, "inconclusive"},
		{"improved-but-no-savings", 0.05, 0.40, 0.10, "inconclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := assemblyDecision(tc.ciLo, tc.ciHi, tc.reduction); got != tc.want {
				t.Fatalf("assemblyDecision(%v, %v, %v) = %q, want %q",
					tc.ciLo, tc.ciHi, tc.reduction, got, tc.want)
			}
		})
	}
}

func TestAssemblyBuildRejectsUnsafeOrDuplicateCaseIDs(t *testing.T) {
	for _, body := range []string{
		`[{"id":"../escape","category":"metadata","question":"q","golden":{"final_answer_criteria":"answer"},"max_tokens":64,"sources":[{"path":"a.go","content":"x"}]}]`,
		`[{"id":"dup","category":"metadata","question":"q","golden":{"final_answer_criteria":"answer"},"max_tokens":64,"sources":[{"path":"a.go","content":"x"}]},{"id":"dup","category":"metadata","question":"q","golden":{"final_answer_criteria":"answer"},"max_tokens":64,"sources":[{"path":"b.go","content":"y"}]},{"id":"dup2","category":"metadata","question":"q","golden":{"final_answer_criteria":"answer"},"max_tokens":64,"sources":[{"path":"c.go","content":"z"}]}]`,
		`[{"id":"case","category":"metadata","question":"q","golden":{"final_answer_criteria":"answer"},"max_tokens":64,"sources":[{"path":"a.go","content":"x"}]},{"id":"CASE","category":"metadata","question":"q","golden":{"final_answer_criteria":"answer"},"max_tokens":64,"sources":[{"path":"b.go","content":"y"}]}]`,
	} {
		dir := t.TempDir()
		fixture := writeFile(t, dir, "cases.json", body)
		outDir := filepath.Join(dir, "out")
		if err := assemblyBuild(context.Background(), fixture, outDir); err == nil {
			t.Fatalf("invalid fixture accepted: %s", body)
		}
		if _, err := os.Stat(outDir); !os.IsNotExist(err) {
			t.Fatalf("invalid fixture created output directory: %v", err)
		}
	}
}
