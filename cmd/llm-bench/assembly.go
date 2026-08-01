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
	"reflect"
	"sort"
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

	// Slice 3c (#331) agent-State arms: legacy vs mixed assembly of the SAME
	// frozen State under one input budget; topline is the unpaired
	// full-State ceiling arm (descriptive only, never enters pairing).
	AssemblyLegacy  AssemblyMode = "legacy"
	AssemblyMixed   AssemblyMode = "mixed"
	AssemblyTopline AssemblyMode = "topline"
)

// AssemblyEval is the per-trace assembly-eval metadata. Both arms of a pair
// share PairID and must carry identical CandidateIDs (asserted at report
// time; a mismatch invalidates the case rather than skewing it). The
// legacy-mixed kind additionally pair-checks StateDigest and Budget.
type AssemblyEval struct {
	PairID                string       `json:"pair_id"`
	Mode                  AssemblyMode `json:"mode"`
	CandidateIDs          []string     `json:"candidate_ids"`
	EstimatedPromptTokens int          `json:"estimated_prompt_tokens"`

	// Slice 3c (legacy/mixed/topline) metadata, filled by the 3c builder.
	// All omitempty so existing 3a trace JSON stays byte-identical.
	StateDigest     string   `json:"state_digest,omitempty"`     // sha256 of the canonical pre-assembly State
	Subjects        []string `json:"subjects,omitempty"`         // ORDERED, non-deduplicated "(callID|domain|subjectID)" entries
	Stratum         string   `json:"stratum,omitempty"`          // corpus stratum (drives the stratified bootstrap)
	AnswerHome      string   `json:"answer_home,omitempty"`      // where the answer-bearing evidence lives
	ScenarioFamily  string   `json:"scenario_family,omitempty"`  // bootstrap cluster ID; empty = own cluster
	TwinGroup       string   `json:"twin_group,omitempty"`       // descriptive lane-bias label; may span strata, never a clustering unit
	Control         bool     `json:"control,omitempty"`          // negative-control pair, excluded from the verdict
	Budget          int      `json:"budget,omitempty"`           // TokenBudget.Input, identical across both arms
	RawStateTokens  int      `json:"raw_state_tokens,omitempty"` // pre-assembly State size
	PressureLevel   string   `json:"pressure_level,omitempty"`   // per-arm pressure tier
	ShedMessages    int      `json:"shed_messages,omitempty"`    // full-state msg count minus assembled
	ShedBytes       int      `json:"shed_bytes,omitempty"`       // full-state byte count minus assembled
	OmittedSubjects int      `json:"omitted_subjects,omitempty"` // mixed arm only
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
	// AnswerLiteral is the OPTIONAL anchored answer string the builder
	// requires at least one rendered arm to contain. Anchor it — "claimBatch
	// = 25", not "25" — or unrelated trace text produces a false reachable
	// reading. Blank means "no reachability claim": no_answer cases have no
	// answer by design and must stay unaffected.
	//
	// There is deliberately no "must NOT appear" inverse for no_answer cases.
	// Their risk is near-synonym-shaped — the README's own authoring rule says
	// to grep for near-synonyms, not just the literal — and no substring-
	// absence check can prove a synonym absent. A negative literal would
	// certify one exact string missing while the real leak walks past under
	// another name: false confidence, not a weaker guarantee.
	AnswerLiteral string `json:"answer_literal,omitempty"`
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

const assemblyManifestName = ".assembly-manifest"

type assemblyManifest struct {
	Version int               `json:"version"`
	Files   map[string]string `json:"files"`
}

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
	if len(cases) == 0 {
		return fmt.Errorf("assembly build: no cases")
	}
	seen := make(map[string]struct{}, len(cases))
	expected := make(map[string]struct{}, len(cases)*2)
	for _, c := range cases {
		if _, ok := seen[c.ID]; ok {
			return fmt.Errorf("assembly build: duplicate case id %q", c.ID)
		}
		seen[c.ID] = struct{}{}
		if err := validateAssemblyCase(c); err != nil {
			return fmt.Errorf("assembly build: case %q: %w", c.ID, err)
		}
		expected[c.ID+"-flat.json"] = struct{}{}
		expected[c.ID+"-progressive.json"] = struct{}{}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("assembly build: %w", err)
	}
	previous, err := readAssemblyManifest(outDir)
	if err != nil {
		return fmt.Errorf("assembly build: read output manifest: %w", err)
	}
	if err := preflightAssemblyOutput(outDir, previous.Files, expected); err != nil {
		return fmt.Errorf("assembly build: preflight output: %w", err)
	}
	// ponytail: publication is atomic per file, not for the whole corpus. An
	// interrupted build may leave an unmanifested trace that preflight refuses;
	// the README documents remove-and-retry recovery. Add directory staging
	// only if corpus-level transactions become a requirement.
	for _, c := range cases {
		if err := buildAssemblyCase(ctx, c, outDir); err != nil {
			return fmt.Errorf("assembly build: case %q: %w", c.ID, err)
		}
	}
	if err := removeStaleAssemblyTraces(outDir, previous.Files, expected); err != nil {
		return fmt.Errorf("assembly build: reconcile output: %w", err)
	}
	if err := writeAssemblyManifest(outDir, expected); err != nil {
		return fmt.Errorf("assembly build: write output manifest: %w", err)
	}
	return nil
}

// assemblyRig is the seeded 3a fixture store plus the retriever built over
// it. Shared by the 3a case builder and the 3c mixed-state builder so both
// corpora retrieve from an identically seeded store; chunkIDs maps each source
// path to its seeded chunk ID (one chunk per source by construction), which
// the mixed builder uses to restate rag group subjects (source paths) in the
// 3a chunk-ID candidate vocabulary.
type assemblyRig struct {
	store    *rag.SQLiteStore
	retr     *rag.Retriever
	chunkIDs map[string]string // source path -> seeded chunk ID
}

// Close releases the underlying in-memory store.
func (r *assemblyRig) Close() error { return r.store.Close() }

// seedAssemblyRig seeds an in-memory rag store from fixture sources exactly as
// the 3a builder always has (provenance-complete sources, rank embeddings,
// pinned indexed_at epoch, atomic abstract/overview summaries) and returns the
// retriever over it. Extracted from buildAssemblyCase unchanged; 3a behavior
// stays byte-identical.
func seedAssemblyRig(ctx context.Context, sources []assemblySource) (*assemblyRig, error) {
	chunkIDs := make([]string, len(sources))
	byPath := make(map[string]string, len(sources))
	seenChunkIDs := make(map[string]string, len(sources))
	for i, source := range sources {
		chunkIDs[i] = assemblyChunkID(source.Path, source.Content)
		if previous, ok := seenChunkIDs[chunkIDs[i]]; ok {
			return nil, fmt.Errorf("source %q: chunk ID collides with source %q", source.Path, previous)
		}
		seenChunkIDs[chunkIDs[i]] = source.Path
		byPath[source.Path] = chunkIDs[i]
	}

	store, err := rag.NewSQLiteStore(":memory:")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = store.Close()
		}
	}()

	// Seed provenance-complete sources: one chunk per source. The fixture's
	// source order is the intended retrieval order; deterministic unit vectors
	// make that order explicit instead of deriving accidental relevance from
	// content hashes.
	const vectorSpaceID = "assembly-fixture-space"
	for i, s := range sources {
		chunk := rag.Chunk{
			ID:        chunkIDs[i],
			Content:   s.Content,
			Source:    s.Path,
			StartLine: 1,
			EndLine:   assemblySourceLineCount(s.Content),
			Language:  s.Language,
		}
		emb := assemblyRankEmbedding(i)
		if err := store.ReplaceSourceWithHashAndVectorSpaceID(
			ctx, s.Path, []rag.Chunk{chunk}, [][]float64{emb},
			assemblySourceSignature(s.Content), vectorSpaceID); err != nil {
			return nil, fmt.Errorf("seed source %q: %w", s.Path, err)
		}
	}
	// The store stamps indexed_at with time.Now(); the progressive renderer
	// emits it as an "indexed:" RFC3339 line, so pin it (same move as
	// internal/rageval/outline_fixture.go) or byte-identity across builds
	// depends on the wall clock.
	if _, err := store.DB().ExecContext(ctx,
		"UPDATE chunks SET indexed_at = ?", assemblyFixedEpoch); err != nil {
		return nil, fmt.Errorf("pin indexed_at: %w", err)
	}
	prov, err := store.SourceProvenanceBatch(ctx, sourcePaths(sources))
	if err != nil {
		return nil, err
	}
	for _, s := range sources {
		if s.Abstract == "" {
			continue
		}
		if s.Overview == "" {
			return nil, fmt.Errorf("source %q: abstract without overview (atomic pair)", s.Path)
		}
		p := prov[s.Path]
		if err := store.UpsertSourceSummary(ctx, rag.SourceSummary{
			Source: s.Path, ContentHash: p.ContentHash, VectorSpaceID: p.VectorSpaceID,
			Abstract: s.Abstract, Overview: s.Overview,
			SummaryModel: "assembly-fixture", FormatVersion: rag.SourceSummaryFormatVersion,
			SummarizedAt: assemblyFixedEpoch,
		}); err != nil {
			return nil, err
		}
	}

	retr, err := rag.NewRetrieverWithEmbedder(assemblyQueryEmbedder(), store,
		rag.WithRetrieverModel("assembly-fixture"), rag.WithVectorOnly())
	if err != nil {
		return nil, err
	}
	ok = true
	return &assemblyRig{store: store, retr: retr, chunkIDs: byPath}, nil
}

func buildAssemblyCase(ctx context.Context, c assemblyCase, outDir string) error {
	rig, err := seedAssemblyRig(ctx, c.Sources)
	if err != nil {
		return err
	}
	defer func() { _ = rig.Close() }()
	retr := rig.retr
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

	// Reachability gate. A case whose answer reaches NEITHER arm is dead: it
	// contributes a guaranteed zero delta to the paired report, and its golden
	// criteria score a model 0 for correctly answering "not in the provided
	// context". This is a hard error, not a warning, because -assembly-build
	// regenerates a COMMITTED corpus — a warning can be scrolled past and
	// committed, an error cannot.
	if c.AnswerLiteral != "" {
		switch reach := assemblyAnswerReach(c.AnswerLiteral, flatCtx, progCtx); len(reach) {
		case 0:
			return fmt.Errorf("answer_literal %q reaches neither arm: either the "+
				"literal is not verbatim in any source, or the answer-bearing source "+
				"sits past both arms' reach at max_tokens=%d (move it to index 0 or 1)",
				c.AnswerLiteral, c.MaxTokens)
		case 1:
			// Single-arm reach is legal and expected (content_only cases are
			// usually flat-only), but it is the shape the author otherwise
			// derives by hand, so surface it instead of making them grep.
			_, _ = fmt.Fprintf(os.Stderr, "assembly build: %s: answer_literal reaches the %s arm only\n",
				c.ID, reach[0])
		}
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
	if !validAssemblyCaseID(c.ID) {
		return fmt.Errorf("invalid id %q: use lowercase ASCII letters, digits, and non-leading hyphens", c.ID)
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
	// Blank means "no reachability claim"; whitespace-only is an authoring
	// slip that would match nearly any rendered context and report a false
	// reachable reading.
	if c.AnswerLiteral != "" && strings.TrimSpace(c.AnswerLiteral) == "" {
		return fmt.Errorf("answer_literal is whitespace-only; omit it or anchor it")
	}
	return validateAssemblySources(c.Sources)
}

// validateAssemblySources is the per-source validation shared by the 3a case
// fixture (Sources) and the 3c mixed fixture (RagSources): non-blank path and
// content, unique paths, and the atomic abstract/overview pair.
func validateAssemblySources(sources []assemblySource) error {
	seen := make(map[string]struct{}, len(sources))
	for i, s := range sources {
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

// assemblyAnswerReach names the arms whose RENDERED CONTEXT contains lit, in
// flat-then-progressive order. The question text is deliberately not searched:
// a question that leaks its own answer would otherwise mask a case no arm can
// actually answer.
func assemblyAnswerReach(lit, flatCtx, progCtx string) []AssemblyMode {
	var reach []AssemblyMode
	if strings.Contains(flatCtx, lit) {
		reach = append(reach, AssemblyFlat)
	}
	if strings.Contains(progCtx, lit) {
		reach = append(reach, AssemblyProgressive)
	}
	return reach
}

func validAssemblyCaseID(id string) bool {
	if id == "" {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && c == '-' {
			continue
		}
		return false
	}
	return true
}

func readAssemblyManifest(outDir string) (assemblyManifest, error) {
	path := filepath.Join(outDir, assemblyManifestName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return assemblyManifest{Version: 1, Files: map[string]string{}}, nil
	}
	if err != nil {
		return assemblyManifest{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return assemblyManifest{}, fmt.Errorf("%s is not a regular file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return assemblyManifest{}, err
	}
	var manifest assemblyManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return assemblyManifest{}, err
	}
	if manifest.Version != 1 || manifest.Files == nil {
		return assemblyManifest{}, fmt.Errorf("unsupported or incomplete manifest")
	}
	for name, digest := range manifest.Files {
		if !validAssemblyTraceFilename(name) || len(digest) != sha256.Size*2 {
			return assemblyManifest{}, fmt.Errorf("invalid manifest entry %q", name)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return assemblyManifest{}, fmt.Errorf("invalid manifest digest for %q", name)
		}
	}
	return manifest, nil
}

func validAssemblyTraceFilename(name string) bool {
	// 3a flat/progressive plus the 3c mixed-corpus arms; the two corpora live
	// in separate directories with separate manifests, but share this shape
	// check (case ID + known arm suffix).
	for _, suffix := range []string{
		"-flat.json", "-progressive.json",
		"-legacy.json", "-mixed.json", "-topline.json",
	} {
		if strings.HasSuffix(name, suffix) {
			return validAssemblyCaseID(strings.TrimSuffix(name, suffix))
		}
	}
	return false
}

func preflightAssemblyOutput(outDir string, previous map[string]string, expected map[string]struct{}) error {
	for name, digest := range previous {
		if _, err := verifyOwnedAssemblyOutput(outDir, name, digest); err != nil {
			return err
		}
	}
	for name := range expected {
		if _, owned := previous[name]; owned {
			continue
		}
		_, err := os.Lstat(filepath.Join(outDir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("refuse to overwrite unowned output %q", name)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if _, owned := previous[entry.Name()]; owned {
			continue
		}
		if _, wanted := expected[entry.Name()]; wanted {
			continue
		}
		return fmt.Errorf("refuse unowned JSON output %q", entry.Name())
	}
	return nil
}

func verifyOwnedAssemblyOutput(outDir, name, digest string) (bool, error) {
	path := filepath.Join(outDir, name)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("refuse to change symlink output %q", name)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("refuse to change modified output %q", name)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != digest {
		return false, fmt.Errorf("refuse to change modified output %q", name)
	}
	return true, nil
}

func removeStaleAssemblyTraces(outDir string, previous map[string]string, expected map[string]struct{}) error {
	for name, digest := range previous {
		if _, ok := expected[name]; ok {
			continue
		}
		exists, err := verifyOwnedAssemblyOutput(outDir, name, digest)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if err := os.Remove(filepath.Join(outDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func writeAssemblyManifest(outDir string, expected map[string]struct{}) error {
	manifest := assemblyManifest{Version: 1, Files: make(map[string]string, len(expected))}
	for name := range expected {
		raw, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		manifest.Files[name] = hex.EncodeToString(sum[:])
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(outDir, assemblyManifestName), append(raw, '\n'))
}

var assemblyLineBreakReplacer = strings.NewReplacer(
	"\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n",
	"\u0085", "\n", "\u2028", "\n", "\u2029", "\n",
)

func assemblySourceLineCount(content string) int {
	return strings.Count(strings.TrimSuffix(assemblyLineBreakReplacer.Replace(content), "\n"), "\n") + 1
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
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%s", len(source), source, content)))
	return hex.EncodeToString(sum[:])
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
	return writeFileAtomic(path, append(raw, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refuse to overwrite symlink %q", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refuse to overwrite non-regular file %q", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// Decision-rule constants, pre-registered (#331 spec 3.8). AnswerQuality
// labels are on the harness's 0..1 scale, so the non-inferiority margin is
// -0.10 (10% of range) and the minimum useful median token reduction is 20%.
const (
	assemblyNonInferiorityMargin = -0.10
	assemblyMinTokenReduction    = 0.20
	assemblyMinimumPairsPerModel = 60
)

// assemblyDecision applies the pre-registered rule to the paired-delta CI
// (progressive - flat) and the median token reduction. Only a lower CI bound
// above zero may use the word "improved".
func assemblyDecision(ciLo, ciHi, medianReduction float64) string {
	switch {
	case ciLo > 0 && medianReduction >= assemblyMinTokenReduction:
		return "quality-improved"
	case ciHi < assemblyNonInferiorityMargin:
		return "regressed"
	case ciLo > assemblyNonInferiorityMargin && medianReduction >= assemblyMinTokenReduction:
		return "efficient-noninferior"
	default:
		return "inconclusive"
	}
}

// AssemblyReport is the -assembly-report output.
type AssemblyReport struct {
	SchemaVersion        string                `json:"schema_version"` // "llm-bench-assembly/v1"
	MinimumPairsPerModel int                   `json:"minimum_pairs_per_model"`
	DecisionRule         string                `json:"decision_rule"`
	Models               []AssemblyModelReport `json:"models"`
	BootstrapSeed        int64                 `json:"bootstrap_seed"`
	BootstrapN           int                   `json:"bootstrap_n"`

	// Slice 3c sections (legacy-mixed kind + topline ceiling). All omitempty
	// so a flat-progressive-only report stays byte-identical to the 3a schema.
	LegacyMixedDecisionRule string                     `json:"legacy_mixed_decision_rule,omitempty"`
	LegacyMixedModels       []AssemblyMixedModelReport `json:"legacy_mixed_models,omitempty"`
	Topline                 []AssemblyToplineReport    `json:"topline,omitempty"`
}

// AssemblyModelReport keeps assembly effects separate by candidate model.
// Pooling several models would pseudo-replicate the same cases and could hide
// a regression that affects one model family.
type AssemblyModelReport struct {
	CandidateModel       string    `json:"candidate_model"`
	Pairs                int       `json:"pairs"`
	InvalidPairs         int       `json:"invalid_pairs"`
	PairingGaps          int       `json:"pairing_gaps"`
	MeanDelta            float64   `json:"mean_delta"`
	DeltaCILow           float64   `json:"delta_ci_low"`
	DeltaCIHigh          float64   `json:"delta_ci_high"`
	MedianTokenReduction float64   `json:"median_token_reduction"`
	Decision             string    `json:"decision"`
	Deltas               []float64 `json:"deltas"` // lexicographic pair-ID order ("case-10" < "case-2")
}

// computeAssemblyReport pairs artifacts on (kind, PairID, CandidateModel) —
// kind derives from each artifact's mode (flat/progressive => the 3a
// flat-progressive kind, legacy/mixed => the 3c legacy-mixed kind) so one
// PairID may appear under both kinds without collision — joins human labels
// by ArtifactHash, and applies each kind's decision rule. Topline artifacts
// never pair; they feed the descriptive ceiling section. Flat-progressive
// cases with unequal candidate sets are invalid; cases missing an arm or a
// label are pairing gaps. Both are excluded from the deltas and counted.
func computeAssemblyReport(arts []Artifact, labels []Label, seed int64, bootstrapN int, fcPrefs []FCPreference) (*AssemblyReport, error) {
	matched, _, err := matchLabels(labels, arts)
	if err != nil {
		return nil, fmt.Errorf("assembly report: match labels: %w", err)
	}
	quality := make(map[string]float64, len(matched)) // ArtifactHash -> quality
	for _, m := range matched {
		quality[m.Artifact.ArtifactHash] = m.Label.ExpectedAnswerQuality
	}
	pairs := map[assemblyPairKey]*assemblyArmSet{}
	var keys []assemblyPairKey
	var topline []*Artifact
	for i := range arts {
		a := &arts[i]
		if a.Trace.AssemblyEval == nil {
			continue // non-assembly artifact in the same directory: ignore
		}
		if err := validateTrace(a.Trace); err != nil {
			return nil, fmt.Errorf("assembly report: artifact %q: %w", a.TraceID, err)
		}
		if modelKey(a.CandidateModel) == "" {
			return nil, fmt.Errorf("assembly report: artifact %q has blank candidate model", a.TraceID)
		}
		mode := a.Trace.AssemblyEval.Mode
		if mode == AssemblyTopline {
			topline = append(topline, a)
			continue
		}
		k := assemblyPairKey{assemblyModeKind(mode), a.Trace.AssemblyEval.PairID, modelKey(a.CandidateModel)}
		s, ok := pairs[k]
		if !ok {
			s = &assemblyArmSet{}
			pairs[k] = s
			keys = append(keys, k)
		}
		switch mode {
		case AssemblyFlat, AssemblyLegacy:
			if s.base != nil {
				return nil, fmt.Errorf("assembly report: duplicate %s arm for pair %q model %q", mode, k.pair, k.model)
			}
			s.base = a
		case AssemblyProgressive, AssemblyMixed:
			if s.treat != nil {
				return nil, fmt.Errorf("assembly report: duplicate %s arm for pair %q model %q", mode, k.pair, k.model)
			}
			s.treat = a
		default:
			return nil, fmt.Errorf("assembly report: pair %q model %q has invalid mode %q",
				k.pair, k.model, mode)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].model != keys[j].model {
			return keys[i].model < keys[j].model
		}
		if keys[i].kind != keys[j].kind {
			return keys[i].kind < keys[j].kind
		}
		return keys[i].pair < keys[j].pair
	})

	rep := &AssemblyReport{
		SchemaVersion:        "llm-bench-assembly/v1",
		MinimumPairsPerModel: assemblyMinimumPairsPerModel,
		BootstrapSeed:        seed,
		BootstrapN:           bootstrapN,
		DecisionRule: fmt.Sprintf(
			"minimum %d complete pairs per model; quality-improved: CI low > 0 and median token reduction >= %.0f%%; "+
				"efficient-noninferior: CI low > %.2f and reduction >= %.0f%%; "+
				"regressed: CI high < %.2f; else inconclusive",
			assemblyMinimumPairsPerModel, assemblyMinTokenReduction*100, assemblyNonInferiorityMargin,
			assemblyMinTokenReduction*100, assemblyNonInferiorityMargin),
	}
	type modelAccumulator struct {
		report     AssemblyModelReport
		reductions []float64
	}
	models := map[string]*modelAccumulator{}
	var modelOrder []string
	totalPairs := 0
	hasMixedEvidence := len(topline) > 0
	for _, k := range keys {
		if k.kind != assemblyKindFlatProgressive {
			hasMixedEvidence = true
			continue
		}
		acc, ok := models[k.model]
		if !ok {
			acc = &modelAccumulator{report: AssemblyModelReport{CandidateModel: k.model}}
			models[k.model] = acc
			modelOrder = append(modelOrder, k.model)
		}
		s := pairs[k]
		if s.base == nil || s.treat == nil {
			acc.report.PairingGaps++
			continue
		}
		qf, okF := quality[s.base.ArtifactHash]
		qp, okP := quality[s.treat.ArtifactHash]
		if !okF || !okP {
			acc.report.PairingGaps++
			continue
		}
		if !reflect.DeepEqual(s.base.Trace.AssemblyEval.CandidateIDs, s.treat.Trace.AssemblyEval.CandidateIDs) {
			acc.report.InvalidPairs++
			continue
		}
		acc.report.Pairs++
		totalPairs++
		acc.report.Deltas = append(acc.report.Deltas, qp-qf)
		// validateTrace enforced tokens > 0 for every paired artifact.
		ft := float64(s.base.Trace.AssemblyEval.EstimatedPromptTokens)
		pt := float64(s.treat.Trace.AssemblyEval.EstimatedPromptTokens)
		acc.reductions = append(acc.reductions, 1-pt/ft)
	}
	// A pure-3a input with nothing labeled still hard-errors exactly as
	// before; any 3c evidence (legacy/mixed/topline artifacts) makes the
	// report worth emitting even when the flat-progressive side is empty.
	if totalPairs == 0 && !hasMixedEvidence {
		return nil, fmt.Errorf("assembly report: no complete labeled pairs")
	}
	for _, model := range modelOrder {
		acc := models[model]
		if acc.report.Pairs > 0 {
			var sum float64
			for _, delta := range acc.report.Deltas {
				sum += delta
			}
			acc.report.MeanDelta = sum / float64(acc.report.Pairs)
			acc.report.DeltaCILow, acc.report.DeltaCIHigh =
				bootstrapDeltaCI(acc.report.Deltas, seed, bootstrapN)
			sort.Float64s(acc.reductions)
			// len(reductions) == Pairs > 0 inside this branch.
			if n := len(acc.reductions); n%2 == 1 {
				acc.report.MedianTokenReduction = acc.reductions[n/2]
			} else {
				acc.report.MedianTokenReduction =
					(acc.reductions[n/2-1] + acc.reductions[n/2]) / 2
			}
		}
		if acc.report.Pairs < assemblyMinimumPairsPerModel {
			acc.report.Decision = "insufficient-corpus"
		} else {
			acc.report.Decision = assemblyDecision(
				acc.report.DeltaCILow, acc.report.DeltaCIHigh,
				acc.report.MedianTokenReduction)
		}
		rep.Models = append(rep.Models, acc.report)
	}
	rep.LegacyMixedModels = computeAssemblyMixedSection(keys, pairs, quality, seed, bootstrapN)
	if len(rep.LegacyMixedModels) > 0 {
		rep.LegacyMixedDecisionRule = assemblyMixedDecisionRuleText()
	}
	rep.Topline = computeAssemblyTopline(topline, quality)
	// Forced-choice is attached AFTER every Decision above is final: the
	// registered sign test is a secondary analysis and must never feed the
	// primary decision. fcPrefs == nil means -fc-preferences was not set (an
	// empty file still attaches zero-count sections).
	if fcPrefs != nil {
		artHashes := make(map[string]struct{}, len(arts))
		for i := range arts {
			artHashes[arts[i].ArtifactHash] = struct{}{}
		}
		if err := attachAssemblyForcedChoice(rep.LegacyMixedModels, fcPrefs, pairs, artHashes); err != nil {
			return nil, fmt.Errorf("assembly report: %w", err)
		}
	}
	return rep, nil
}

// runAssemblyReport loads the (labels, artifacts) pair — plus, when
// fcPrefsPath is set, the -fc-ingest preference sidecar — and renders the
// assembly report as indented JSON.
func runAssemblyReport(labelsPath, artifactsPath, fcPrefsPath string) (string, error) {
	arts, err := loadArtifacts(artifactsPath)
	if err != nil {
		return "", err
	}
	labels, err := loadLabels(labelsPath)
	if err != nil {
		return "", err
	}
	var fcPrefs []FCPreference
	if strings.TrimSpace(fcPrefsPath) != "" {
		fcPrefs, err = loadFCPreferences(fcPrefsPath)
		if err != nil {
			return "", err
		}
		if fcPrefs == nil {
			fcPrefs = []FCPreference{} // empty sidecar still means "flag set"
		}
	}
	report, err := computeAssemblyReport(arts, labels, pairedBootstrapSeed, pairedBootstrapN, fcPrefs)
	if err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	return string(append(raw, '\n')), nil
}
