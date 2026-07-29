package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/kstruzzieri/go-llm/rag"
)

// Assembly-eval support (#331 PR 3a): a paired flat/progressive corpus where
// both arms of a pair carry the SAME selected candidate set rendered two
// ways. Assembly mode is first-class trace metadata — never encoded in the
// model name, never folded into the paired report's model identity. Pairing
// is (PairID, CandidateModel), so the two arms are distinct traces and the
// existing (TraceID, CandidateModel) duplicate-cell rule is never tripped.

// AssemblyMode names how retrieval context was rendered for one trace arm.
type AssemblyMode string

const (
	AssemblyFlat        AssemblyMode = "flat"        // frozen BuildContext
	AssemblyProgressive AssemblyMode = "progressive" // RenderProgressive, same budget
)

// AssemblyEval is the per-trace assembly-eval metadata. Both arms of a pair
// share PairID and must carry identical CandidateIDs (asserted at report
// time; a mismatch invalidates the case rather than skewing it).
type AssemblyEval struct {
	PairID                string       `json:"pair_id"`
	Mode                  AssemblyMode `json:"mode"`
	CandidateIDs          []string     `json:"candidate_ids"`
	EstimatedPromptTokens int          `json:"estimated_prompt_tokens"`
}

// assemblyCase is one QA case in the corpus fixture. Chunks are whole-file
// sources: the builder writes each as one indexed source so provenance and
// summaries line up; selection happens ONCE via retrieval and both arms
// render the same result slice.
type assemblyCase struct {
	ID        string           `json:"id"`
	Category  string           `json:"category"` // content_only | metadata | distractor | no_answer
	Question  string           `json:"question"`
	Golden    Golden           `json:"golden"`
	MaxTokens int              `json:"max_tokens"`
	Sources   []assemblySource `json:"sources"`
}

type assemblySource struct {
	Path     string `json:"path"`    // e.g. "pkg/api/server.go"
	Content  string `json:"content"` // whole-source body
	Language string `json:"language"`
	Abstract string `json:"abstract"` // L0; blank => no summary row for this source
	Overview string `json:"overview"` // L1; must be set iff Abstract is set (atomic pair)
}

const assemblySystemPrompt = "Answer the question using ONLY the provided context. " +
	"If the context does not contain the answer, say so explicitly."

// assemblyFixedEpoch pins every store timestamp the rendered context can
// carry (summarized_at, indexed_at). Rendered output embeds both as RFC3339
// lines, so leaving either at time.Now() would make the build
// non-reproducible byte-for-byte.
const assemblyFixedEpoch = int64(1753574400)

// assemblyBuild renders every case's candidate set both ways and writes
// <out>/<case-id>-flat.json and <out>/<case-id>-progressive.json.
func assemblyBuild(ctx context.Context, fixturePath, outDir string) error {
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		return fmt.Errorf("assembly build: read fixture: %w", err)
	}
	var cases []assemblyCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		return fmt.Errorf("assembly build: parse fixture: %w", err)
	}
	seen := make(map[string]struct{}, len(cases))
	for _, c := range cases {
		if _, ok := seen[c.ID]; ok {
			return fmt.Errorf("assembly build: duplicate case id %q", c.ID)
		}
		seen[c.ID] = struct{}{}
		if err := validateAssemblyCase(c); err != nil {
			return fmt.Errorf("assembly build: case %q: %w", c.ID, err)
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("assembly build: %w", err)
	}
	for _, c := range cases {
		if err := buildAssemblyCase(ctx, c, outDir); err != nil {
			return fmt.Errorf("assembly build: case %q: %w", c.ID, err)
		}
	}
	return nil
}

func buildAssemblyCase(ctx context.Context, c assemblyCase, outDir string) error {
	store, err := rag.NewSQLiteStore(":memory:")
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// Seed provenance-complete sources: one chunk per source. The fixture's
	// source order is the intended retrieval order; deterministic unit vectors
	// make that order explicit instead of deriving accidental relevance from
	// content hashes.
	const vectorSpaceID = "assembly-fixture-space"
	for i, s := range c.Sources {
		lines := strings.Count(s.Content, "\n") + 1
		chunk := rag.Chunk{
			ID:        assemblyChunkID(s.Path, s.Content),
			Content:   s.Content,
			Source:    s.Path,
			StartLine: 1,
			EndLine:   lines,
			Language:  s.Language,
		}
		emb := assemblyRankEmbedding(i)
		if err := store.ReplaceSourceWithHashAndVectorSpaceID(
			ctx, s.Path, []rag.Chunk{chunk}, [][]float64{emb},
			assemblySourceSignature(s.Content), vectorSpaceID); err != nil {
			return fmt.Errorf("seed source %q: %w", s.Path, err)
		}
	}
	// The store stamps indexed_at with time.Now(); the progressive renderer
	// emits it as an "indexed:" RFC3339 line, so pin it (same move as
	// internal/rageval/outline_fixture.go) or byte-identity across builds
	// depends on the wall clock.
	if _, err := store.DB().ExecContext(ctx,
		"UPDATE chunks SET indexed_at = ?", assemblyFixedEpoch); err != nil {
		return fmt.Errorf("pin indexed_at: %w", err)
	}
	prov, err := store.SourceProvenanceBatch(ctx, sourcePaths(c.Sources))
	if err != nil {
		return err
	}
	for _, s := range c.Sources {
		if s.Abstract == "" {
			continue
		}
		if s.Overview == "" {
			return fmt.Errorf("source %q: abstract without overview (atomic pair)", s.Path)
		}
		p := prov[s.Path]
		if err := store.UpsertSourceSummary(ctx, rag.SourceSummary{
			Source: s.Path, ContentHash: p.ContentHash, VectorSpaceID: p.VectorSpaceID,
			Abstract: s.Abstract, Overview: s.Overview,
			SummaryModel: "assembly-fixture", FormatVersion: rag.SourceSummaryFormatVersion,
			SummarizedAt: assemblyFixedEpoch,
		}); err != nil {
			return err
		}
	}

	retr, err := rag.NewRetrieverWithEmbedder(assemblyQueryEmbedder(), store,
		rag.WithRetrieverModel("assembly-fixture"), rag.WithVectorOnly())
	if err != nil {
		return err
	}
	// Selection runs ONCE; both arms render this exact slice.
	results, err := retr.Retrieve(ctx, c.Question, len(c.Sources))
	if err != nil {
		return err
	}
	if len(results) == 0 {
		return fmt.Errorf("retrieval selected nothing")
	}

	flatCtx := retr.BuildContext(results, c.MaxTokens)
	progCtx, _, err := retr.RenderProgressive(ctx, rag.ProgressiveRenderRequest{
		// MaxBytes: 4 bytes/token, the inverse of the est heuristic below.
		Results: results, MaxTokens: c.MaxTokens, MaxBytes: c.MaxTokens * 4,
	})
	if err != nil {
		return err
	}

	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.Chunk.ID
	}
	// est mirrors rag's unexported defaultEstimate; trace metadata only, so
	// do not "deduplicate" by exporting it.
	est := func(s string) int { return (len(s) + 3) / 4 }
	arms := []struct {
		mode    AssemblyMode
		context string
	}{{AssemblyFlat, flatCtx}, {AssemblyProgressive, progCtx}}
	traces := make([]Trace, 0, len(arms))
	for _, arm := range arms {
		content := "Context:\n" + arm.context + "\n\nQuestion: " + c.Question
		tr := Trace{
			ID:     c.ID + "-" + string(arm.mode),
			Source: "assembly-corpus",
			System: assemblySystemPrompt,
			Turns:  []Turn{{Role: "user", Content: content}},
			Golden: c.Golden,
			AssemblyEval: &AssemblyEval{
				PairID: c.ID, Mode: arm.mode, CandidateIDs: ids,
				EstimatedPromptTokens: est(assemblySystemPrompt) + est(content),
			},
		}
		if err := validateTrace(tr); err != nil {
			return fmt.Errorf("built %s arm invalid: %w", arm.mode, err)
		}
		traces = append(traces, tr)
	}
	for _, tr := range traces {
		if err := writeTraceJSON(filepath.Join(outDir, tr.ID+".json"), tr); err != nil {
			return err
		}
	}
	return nil
}

func validateAssemblyCase(c assemblyCase) error {
	if c.ID == "" || filepath.Base(c.ID) != c.ID || c.ID == "." || c.ID == ".." {
		return fmt.Errorf("invalid id %q: must be one filename-safe segment", c.ID)
	}
	switch c.Category {
	case "content_only", "metadata", "distractor", "no_answer":
	default:
		return fmt.Errorf("invalid category %q", c.Category)
	}
	if strings.TrimSpace(c.Question) == "" || c.MaxTokens <= 0 || len(c.Sources) == 0 {
		return fmt.Errorf("question, positive max_tokens, and sources are required")
	}
	if strings.TrimSpace(c.Golden.FinalAnswerCriteria) == "" {
		return fmt.Errorf("golden.final_answer_criteria is required")
	}
	seen := make(map[string]struct{}, len(c.Sources))
	for i, s := range c.Sources {
		if strings.TrimSpace(s.Path) == "" || strings.TrimSpace(s.Content) == "" {
			return fmt.Errorf("source %d: path and content are required", i)
		}
		if _, ok := seen[s.Path]; ok {
			return fmt.Errorf("source %d: duplicate path %q", i, s.Path)
		}
		seen[s.Path] = struct{}{}
		if (s.Abstract == "") != (s.Overview == "") {
			return fmt.Errorf("source %q: abstract and overview must be both set or both blank", s.Path)
		}
	}
	return nil
}

func assemblyQueryEmbedder() rag.Embedder {
	return rag.EmbedderFunc(func(_ context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
		if len(inputs) != 1 {
			return rag.EmbedResult{}, fmt.Errorf("assembly embedder: got %d inputs, want 1", len(inputs))
		}
		return rag.EmbedResult{
			Embeddings: [][]float64{{1, 0}},
			Model:      "assembly-fixture", Provider: "fixture",
			VectorSpaceID: "assembly-fixture-space",
		}, nil
	})
}

// assemblyRankEmbedding returns a unit vector whose cosine similarity to the
// query vector (1,0) strictly decreases with rank, so fixture order IS
// retrieval order.
func assemblyRankEmbedding(rank int) []float64 {
	angle := float64(rank+1) * 0.01
	return []float64{math.Cos(angle), math.Sin(angle)}
}

func assemblyContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// assemblySourceSignature builds the stored source-signature document, not a
// bare hash. SourceProvenanceBatch parses version/content_hash from it (see
// rag.parseSourceSignature: version != 0 and content_hash != "" is the whole
// parse contract); using a bare digest would make every summary stale via
// unknown_content_hash.
func assemblySourceSignature(content string) string {
	raw, _ := json.Marshal(struct {
		Version     int    `json:"version"`
		ContentHash string `json:"content_hash"`
	}{Version: 2, ContentHash: assemblyContentHash(content)})
	return string(raw)
}

func assemblyChunkID(source, content string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + content))
	return hex.EncodeToString(sum[:])[:12]
}

func sourcePaths(srcs []assemblySource) []string {
	out := make([]string, len(srcs))
	for i, s := range srcs {
		out[i] = s.Path
	}
	return out
}

func writeTraceJSON(path string, tr Trace) error {
	raw, err := json.MarshalIndent(tr, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}
