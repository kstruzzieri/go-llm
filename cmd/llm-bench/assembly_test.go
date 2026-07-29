package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	if len(entries) != 4 { // 2 cases x 2 arms
		t.Fatalf("wrote %d traces, want 4", len(entries))
	}
	byPair := map[string][]Trace{}
	for _, e := range entries {
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
	for _, e := range entries {
		b1 := mustReadAssemblyFile(t, filepath.Join(outDir, e.Name()))
		b2 := mustReadAssemblyFile(t, filepath.Join(outDir2, e.Name()))
		if !bytes.Equal(b1, b2) {
			t.Fatalf("non-deterministic build for %s", e.Name())
		}
	}
}

func TestAssemblyBuildRejectsUnsafeOrDuplicateCaseIDs(t *testing.T) {
	for _, body := range []string{
		`[{"id":"../escape","category":"metadata","question":"q","golden":{"final_answer_criteria":"answer"},"max_tokens":64,"sources":[{"path":"a.go","content":"x"}]}]`,
		`[{"id":"dup","category":"metadata","question":"q","golden":{"final_answer_criteria":"answer"},"max_tokens":64,"sources":[{"path":"a.go","content":"x"}]},{"id":"dup","category":"metadata","question":"q","golden":{"final_answer_criteria":"answer"},"max_tokens":64,"sources":[{"path":"b.go","content":"y"}]},{"id":"dup2","category":"metadata","question":"q","golden":{"final_answer_criteria":"answer"},"max_tokens":64,"sources":[{"path":"c.go","content":"z"}]}]`,
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
