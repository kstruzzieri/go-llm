package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// blindFillMarker delimits the human-fill region inside a worksheet block. The
// ingest parser only reads score:/notes: lines that appear AFTER this marker,
// so candidate output text that happens to contain "score:" cannot be
// misparsed.
const blindFillMarker = "--- fill below (score: 0 | 0.5 | 1) ---"

// blindEndMarker terminates a worksheet block; the human-fill region runs
// from blindFillMarker to this line. Renderer and ingest parser must agree on
// this literal, so it lives in one place.
const blindEndMarker = "=== END ==="

// blindDupPair pairs a primary score with its DUP re-score for one artifact
// (the intra-rater control). Written as JSONL via -dups-out.
type blindDupPair struct {
	ArtifactHash string  `json:"artifact_hash"`
	PrimaryScore float64 `json:"primary_score"`
	DupScore     float64 `json:"dup_score"`
}

// blindIngestResult is everything one -blind-ingest parse produces: the
// emitted labels, the unscored-block count, the grounding-check flagged hashes
// (sorted; pending adjudication), and the intra-rater dup pairing. DUP-block
// scores NEVER appear in Labels — they exist only in DupPairs/DupUnpaired.
type blindIngestResult struct {
	Labels        []Label
	Skipped       int
	FlaggedHashes []string
	DupPairs      []blindDupPair
	DupUnpaired   []string
}

// blindDupSummary reduces the intra-rater pairs to the ingest summary:
// exact-agreement and disagreement counts plus the mean absolute score delta.
func blindDupSummary(pairs []blindDupPair) (agree, disagree int, meanAbsDelta float64) {
	if len(pairs) == 0 {
		return 0, 0, 0
	}
	var sum float64
	for _, p := range pairs {
		d := math.Abs(p.PrimaryScore - p.DupScore)
		if d == 0 {
			agree++
		} else {
			disagree++
		}
		sum += d
	}
	return agree, disagree, sum / float64(len(pairs))
}

// ingestBlindWorksheet parses a filled blind worksheet, rejoining trace_id +
// candidate_model from arts on artifact_hash (R-D3 — the untouched artifacts
// file is the join key, so no separate restore map). Blocks with a blank score
// are skipped (partial labeling allowed) and counted. A score outside
// {0, 0.5, 1.0}, duplicate worksheet block, or hash absent from arts is a loud
// error. labeler and labeled_at are stamped on every emitted label.
//
// Promptless blocks (legacy/mixed/topline) may carry a flag: line. A
// grounding-check flag prefixes the label's notes with groundingCheckPrefix so
// downstream sees the pending adjudication; any other non-blank value, a flag
// on a non-promptless block, an unscored block, or a DUP block is a loud
// error. DUP blocks ("=== ARTIFACT <hash> DUP ===") never emit labels: their
// scores pair with the primary block's score into DupPairs; a DUP whose
// primary (or itself) is unscored lands in DupUnpaired.
//
// The block parser only recognizes "=== ARTIFACT " while outside a block, so
// candidate output can contain worksheet-looking sentinel text without stealing
// the human-entered score from the real artifact block.
func ingestBlindWorksheet(worksheet string, arts []Artifact, labeler string) (blindIngestResult, error) {
	var res blindIngestResult
	artByHash := make(map[string]Artifact, len(arts))
	for _, a := range arts {
		artByHash[a.ArtifactHash] = a
	}

	var hash, score, notes, flagVal string
	isDup := false
	afterMarker := false
	seenWorksheetHash := map[string]struct{}{}
	seenDupHash := map[string]struct{}{}
	primaryScore := map[string]float64{}
	dupScore := map[string]float64{}
	var dupHashes []string
	flush := func() error {
		if hash == "" {
			return nil
		}
		defer func() {
			hash, score, notes, flagVal, isDup, afterMarker = "", "", "", "", false, false
		}()
		a, ok := artByHash[hash]
		if !ok {
			return fmt.Errorf("worksheet references unknown artifact_hash %q (not in -artifacts)", hash)
		}
		flag := strings.TrimSpace(flagVal)
		if isDup {
			if _, dup := seenDupHash[hash]; dup {
				return fmt.Errorf("worksheet contains duplicate DUP block for artifact_hash %q", hash)
			}
			seenDupHash[hash] = struct{}{}
			if a.Trace.AssemblyEval == nil ||
				(a.Trace.AssemblyEval.Mode != AssemblyLegacy && a.Trace.AssemblyEval.Mode != AssemblyMixed) {
				return fmt.Errorf("DUP block for artifact_hash %q: only legacy/mixed artifacts are dup-eligible", hash)
			}
			if flag != "" {
				return fmt.Errorf("DUP block for artifact_hash %q: flag: has no effect on a duplicate; flag the primary block", hash)
			}
			dupHashes = append(dupHashes, hash)
			if strings.TrimSpace(score) == "" {
				return nil // unscored dup: reported as unpaired after the parse
			}
			q, perr := parseBlindScore(score)
			if perr != nil {
				return fmt.Errorf("DUP block %s: %w", hash, perr)
			}
			dupScore[hash] = q
			return nil
		}
		if _, dup := seenWorksheetHash[hash]; dup {
			return fmt.Errorf("worksheet contains duplicate artifact_hash %q", hash)
		}
		seenWorksheetHash[hash] = struct{}{}
		switch flag {
		case "", groundingCheckFlag:
		default:
			return fmt.Errorf("artifact %s: unknown flag %q (want blank or %q)", hash, flag, groundingCheckFlag)
		}
		if flag == groundingCheckFlag && !prefilledAssemblyMode(a.Trace) {
			return fmt.Errorf("artifact %s: flag %q applies only to promptless (legacy/mixed/topline) blocks", hash, flag)
		}
		if strings.TrimSpace(score) == "" {
			if flag == groundingCheckFlag {
				return fmt.Errorf("artifact %s: flag %q on an unscored block (score the block before flagging it)", hash, flag)
			}
			res.Skipped++
			return nil
		}
		q, perr := parseBlindScore(score)
		if perr != nil {
			return fmt.Errorf("artifact %s: %w", hash, perr)
		}
		primaryScore[hash] = q
		labelNotes := strings.TrimSpace(notes)
		if flag == groundingCheckFlag {
			labelNotes = groundingCheckPrefix + labelNotes
			res.FlaggedHashes = append(res.FlaggedHashes, hash)
		}
		res.Labels = append(res.Labels, Label{
			TraceID:               a.TraceID,
			CandidateModel:        a.CandidateModel,
			ArtifactHash:          hash,
			ExpectedAnswerQuality: q,
			LabelNotes:            labelNotes,
			LabeledAt:             time.Now().UTC(),
			Labeler:               labeler,
		})
		return nil
	}

	inBlock := false
	for _, line := range strings.Split(worksheet, "\n") {
		switch {
		case !inBlock && strings.HasPrefix(line, "=== ARTIFACT "):
			body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "=== ARTIFACT "), " ==="))
			isDup = strings.HasSuffix(body, " DUP")
			hash = strings.TrimSpace(strings.TrimSuffix(body, " DUP"))
			score, notes, flagVal, afterMarker, inBlock = "", "", "", false, true
		case inBlock && strings.HasPrefix(line, blindFillMarker):
			afterMarker = true
		case inBlock && strings.HasPrefix(line, blindEndMarker):
			// The fill region ends at the block terminator: a stray score:/notes:
			// line between "=== END ===" and the next block must not attach to
			// the previous block (it would be reported as unscored instead).
			if ferr := flush(); ferr != nil {
				return blindIngestResult{}, ferr
			}
			inBlock = false
		case inBlock && afterMarker && strings.HasPrefix(line, "score:"):
			score = strings.TrimSpace(strings.TrimPrefix(line, "score:"))
		case inBlock && afterMarker && strings.HasPrefix(line, "notes:"):
			notes = strings.TrimSpace(strings.TrimPrefix(line, "notes:"))
		case inBlock && afterMarker && strings.HasPrefix(line, "flag:"):
			flagVal = strings.TrimSpace(strings.TrimPrefix(line, "flag:"))
		}
	}
	if inBlock {
		if ferr := flush(); ferr != nil {
			return blindIngestResult{}, ferr
		}
	}
	if ferr := flush(); ferr != nil {
		return blindIngestResult{}, ferr
	}

	sort.Strings(res.FlaggedHashes)
	sort.Strings(dupHashes)
	for _, h := range dupHashes {
		ds, dupScored := dupScore[h]
		ps, primaryScored := primaryScore[h]
		if !dupScored || !primaryScored {
			res.DupUnpaired = append(res.DupUnpaired, h)
			continue
		}
		res.DupPairs = append(res.DupPairs, blindDupPair{ArtifactHash: h, PrimaryScore: ps, DupScore: ds})
	}
	return res, nil
}

// parseBlindScore accepts only the three legal label values.
func parseBlindScore(s string) (float64, error) {
	switch strings.TrimSpace(s) {
	case "0", "0.0":
		return 0, nil
	case "0.5":
		return 0.5, nil
	case "1", "1.0":
		return 1, nil
	default:
		return 0, fmt.Errorf("invalid score %q (want 0, 0.5, or 1)", s)
	}
}

// renderBlindWorksheet emits one fill-in block per artifact for blind labeling
// (R-D3): the committed rubric and candidate final answer are shown, but the
// model identity is withheld. Normal blocks are ordered by
// (trace_id, artifact_hash) so all candidates for one prompt sit together while
// their order is model-independent. Assembly-eval trace IDs encode the arm, so
// those blocks omit the trace line and sort by artifact hash instead. The
// labeler fills score:/notes:;
// -blind-ingest then rejoins the true model on artifact_hash from the untouched
// artifacts file.
//
// Prefilled-mode blocks (legacy/mixed/topline, #331 slice 3c) are PROMPTLESS:
// they render [question] (the trace's final user turn) instead of [prompt] —
// zero prompt bytes reach the primary labeler — and their fill region gains an
// optional flag: line for marking a block grounding-check. Non-assembly and 3a
// flat/progressive blocks stay byte-identical to the pre-3c rendering,
// [prompt] sections included.
//
// dupN > 0 appends that many DUP blocks (intra-rater controls) AFTER all
// primary blocks: eligible artifacts (legacy/mixed only) sorted by hash,
// selected at every ceil(len/N)-th index. The registered formula can yield
// fewer than N when the stride overshoots; the ingest summary reports the
// actual pair count.
//
// Worksheet block contract (shared with ingestBlindWorksheet): "=== ARTIFACT
// <hash> ===" opens a block and "=== END ===" closes it; the human-fill region
// is gated by blindFillMarker. score:/notes:/flag: are read only after the
// marker, so those tokens inside candidate output are safely ignored. The
// renderer cannot stop a model answer from itself containing an "=== ARTIFACT "
// line, so the ingest parser is responsible for not mis-splitting on a sentinel
// embedded in candidate output; its unknown-artifact_hash check is the loud
// backstop.
func renderBlindWorksheet(arts []Artifact, dupN int) (string, error) {
	ordered := make([]Artifact, len(arts))
	copy(ordered, arts)
	sort.SliceStable(ordered, func(i, j int) bool {
		iAssembly := ordered[i].Trace.AssemblyEval != nil
		jAssembly := ordered[j].Trace.AssemblyEval != nil
		if iAssembly != jAssembly {
			return !iAssembly
		}
		if iAssembly {
			return ordered[i].ArtifactHash < ordered[j].ArtifactHash
		}
		if ordered[i].TraceID != ordered[j].TraceID {
			return ordered[i].TraceID < ordered[j].TraceID
		}
		return ordered[i].ArtifactHash < ordered[j].ArtifactHash
	})
	dups, err := selectBlindDups(ordered, dupN)
	if err != nil {
		return "", err
	}
	hasPromptless := false
	for _, a := range ordered {
		if prefilledAssemblyMode(a.Trace) {
			hasPromptless = true
			break
		}
	}

	var b strings.Builder
	fmt.Fprintln(&b, "# llm-bench — blind labeling worksheet")
	fmt.Fprintln(&b, "#")
	fmt.Fprintln(&b, "# Score each candidate output from the text alone. Model identity is hidden.")
	fmt.Fprintln(&b, "# For each block, fill the score: line with 0, 0.5, or 1, and notes: optionally.")
	fmt.Fprintln(&b, "# Leave score: blank to skip a block (it will be reported as unscored, not labeled).")
	// These doc lines must not contain the literal "[prompt]" or " DUP ===":
	// the whole-worksheet zero-prompt-bytes guarantee (and the DUP block
	// grammar) is asserted over every byte of a promptless worksheet.
	if hasPromptless {
		fmt.Fprintln(&b, "# Blocks that show a [question] instead of a prompt are promptless: judge the")
		fmt.Fprintln(&b, "# [candidate output] against the [question] and [rubric] alone. Their optional")
		fmt.Fprintln(&b, "# flag: line marks a block for the prompt-visible adjudication pass — write")
		fmt.Fprintln(&b, "# grounding-check to flag it, or leave it blank.")
	}
	if len(dups) > 0 {
		fmt.Fprintln(&b, "# ARTIFACT blocks marked DUP are intra-rater duplicates: score them")
		fmt.Fprintln(&b, "# independently; DUP scores never become labels.")
	}
	fmt.Fprintln(&b, "# Then run: llm-bench -blind-ingest -worksheet <this file> -artifacts <artifacts.jsonl> -labels-out <labels.jsonl>")
	fmt.Fprintln(&b)
	for _, a := range ordered {
		writeBlindBlock(&b, a, false)
	}
	for _, a := range dups {
		writeBlindBlock(&b, a, true)
	}
	return redactPaths(b.String()), nil
}

// writeBlindBlock renders one worksheet block. Promptless (prefilled-mode)
// artifacts get [question] + a flag: fill line; everything else keeps the
// pre-3c [prompt] body byte-for-byte. DUP blocks reuse the promptless body
// under the DUP header (only legacy/mixed artifacts are dup-eligible).
func writeBlindBlock(b *strings.Builder, a Artifact, dup bool) {
	if dup {
		fmt.Fprintf(b, "=== ARTIFACT %s DUP ===\n", a.ArtifactHash)
	} else {
		fmt.Fprintf(b, "=== ARTIFACT %s ===\n", a.ArtifactHash)
	}
	promptless := prefilledAssemblyMode(a.Trace)
	if promptless {
		fmt.Fprintln(b, "[question]")
		fmt.Fprintln(b, strings.TrimSpace(blindQuestion(a.Trace)))
	} else {
		if a.Trace.AssemblyEval == nil {
			fmt.Fprintf(b, "trace: %s\n\n", a.TraceID)
		}
		writeBlindPromptBody(b, a.Trace)
	}
	fmt.Fprintln(b)
	fmt.Fprintln(b, "[rubric]")
	fmt.Fprintln(b, strings.TrimSpace(a.Trace.Golden.FinalAnswerCriteria))
	fmt.Fprintln(b)
	fmt.Fprintln(b, "[candidate output]")
	fmt.Fprintln(b, strings.TrimSpace(a.ActualFinalAnswer))
	fmt.Fprintln(b)
	fmt.Fprintln(b, blindFillMarker)
	fmt.Fprintln(b, "score: ")
	fmt.Fprintln(b, "notes: ")
	if promptless {
		fmt.Fprintln(b, "flag: ")
	}
	fmt.Fprintln(b, blindEndMarker)
	fmt.Fprintln(b)
}

// writeBlindPromptBody renders the full-prompt section ([prompt], trimmed
// system, each non-empty turn). Shared by the non-promptless worksheet block
// and the adjudication worksheet so the two spellings can never drift.
func writeBlindPromptBody(b *strings.Builder, t Trace) {
	fmt.Fprintln(b, "[prompt]")
	fmt.Fprintln(b, strings.TrimSpace(t.System))
	for _, turn := range t.Turns {
		content := strings.TrimSpace(turn.Content)
		if content == "" {
			continue
		}
		fmt.Fprintf(b, "\n<%s>\n%s\n", turn.Role, content)
	}
}

// blindQuestion is the question a promptless block shows: the trace's final
// turn content (prefilled traces end on the user question; topline traces are
// a single user turn, shown as-is).
func blindQuestion(t Trace) string {
	if len(t.Turns) == 0 {
		return ""
	}
	return t.Turns[len(t.Turns)-1].Content
}

// selectBlindDups picks the intra-rater duplicate artifacts for -blind-dups:
// dup-eligible artifacts (assembly legacy/mixed only — topline is unpaired and
// 3a/plain artifacts are out of scope) sorted by hash, then every
// ceil(len/N)-th taken until N. Deterministic by construction. Asking for more
// dups than there are eligible artifacts is a loud error.
func selectBlindDups(arts []Artifact, n int) ([]Artifact, error) {
	if n == 0 {
		return nil, nil
	}
	if n < 0 {
		return nil, fmt.Errorf("blind-dups %d is negative", n)
	}
	var eligible []Artifact
	for _, a := range arts {
		if a.Trace.AssemblyEval == nil {
			continue
		}
		switch a.Trace.AssemblyEval.Mode {
		case AssemblyLegacy, AssemblyMixed:
			eligible = append(eligible, a)
		}
	}
	if n > len(eligible) {
		return nil, fmt.Errorf("blind-dups %d exceeds the %d dup-eligible legacy/mixed artifact(s)", n, len(eligible))
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].ArtifactHash < eligible[j].ArtifactHash })
	step := (len(eligible) + n - 1) / n
	var dups []Artifact
	for i := 0; i < len(eligible) && len(dups) < n; i += step {
		dups = append(dups, eligible[i])
	}
	return dups, nil
}

// writeLabelsJSONL writes labels as JSONL (one Label per line). LabeledAt
// stamping is the caller's responsibility (ingestBlindWorksheet already stamps
// it). Mirrors the artifacts/manifest writers.
func writeLabelsJSONL(path string, labels []Label) error {
	return writeJSONLRows(path, "labels", labels)
}

// writeJSONLRows writes rows as JSONL (one row per line); kind prefixes error
// messages. Shared by the labels, forced-choice, and dup-pair writers.
func writeJSONLRows[T any](path, kind string, rows []T) (retErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("%s: mkdir: %w", kind, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("%s: open output: %w", kind, err)
	}
	defer func() {
		if closeErr := f.Close(); retErr == nil && closeErr != nil {
			retErr = fmt.Errorf("%s: close output: %w", kind, closeErr)
		}
	}()
	enc := json.NewEncoder(f)
	for i, row := range rows {
		if err := enc.Encode(row); err != nil {
			return fmt.Errorf("%s: encode row %d: %w", kind, i, err)
		}
	}
	return nil
}
