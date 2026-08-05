package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

// worksheetEscapePrefix is the visible escape the renderers prefix onto any
// untrusted-text line (candidate output, question, rubric, prompt body) that
// collides with a worksheet sentinel — a fill marker, the end marker, a block
// header prefix, or a recognized field line. Without it a candidate answer
// containing a well-formed fill-marker/score/end-marker frame would score its
// own block and orphan the human's real fill region.
const worksheetEscapePrefix = "| "

// Blind worksheet header prefixes. Non-promptless blocks stay hash-addressed
// ("=== ARTIFACT <hash> ==="), byte-identical to the pre-3c grammar. W5:
// promptless (legacy/mixed/topline) blocks are addressed by an OPAQUE id
// instead ("=== BLOCK <opaqueID> ===") — an artifact hash in the header
// joins the committed artifacts JSONL, whose AssemblyEval.Mode names the
// arm, so a hash-headed promptless worksheet unblinds by inspection. The
// opaque id resolves back to the hash only through the render-emitted block
// map (-blind-blockmap-out / -blind-blockmap). The adjudication worksheet
// deliberately keeps hashes: it reveals the full prompt anyway.
const (
	blindArtifactHeaderPrefix = "=== ARTIFACT "
	blindBlockHeaderPrefix    = "=== BLOCK "
)

// blindBlockmapSchemaVersion pins the opaque block-map file schema (W5).
const blindBlockmapSchemaVersion = "blind-blockmap/v1"

// blindBlockmapFile is the sibling map a promptless -blind-render emits:
// salt plus opaqueID -> artifact hash for every promptless block. It is the
// ONLY join between worksheet BLOCK ids and artifacts, so -blind-ingest
// requires it whenever the worksheet holds BLOCK headers.
type blindBlockmapFile struct {
	SchemaVersion string            `json:"schema_version"`
	Salt          string            `json:"salt"`
	Blocks        map[string]string `json:"blocks"`
}

// blindOpaqueID derives a block's opaque worksheet id:
// sha256(artifactHash + "|" + salt), hex, truncated to 16 chars. The salt
// keeps the id non-derivable from the artifact hash alone.
func blindOpaqueID(artifactHash, salt string) string {
	sum := sha256.Sum256([]byte(artifactHash + "|" + salt))
	return hex.EncodeToString(sum[:])[:16]
}

// newBlindBlockSalt draws a fresh random block salt (16 crypto/rand bytes,
// hex).
func newBlindBlockSalt() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("blind-blockmap: crypto/rand: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// writeBlindBlockmap writes the opaque block map as indented JSON.
func writeBlindBlockmap(path string, bm blindBlockmapFile) error {
	raw, err := json.MarshalIndent(bm, "", "  ")
	if err != nil {
		return fmt.Errorf("blind-blockmap: marshal: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("blind-blockmap: write: %w", err)
	}
	return nil
}

// loadBlindBlockmap reads and validates an opaque block map: schema version,
// non-blank salt, non-empty blocks, and every id re-derivable from its hash
// under the file's salt (a hand-edited or mismatched map cannot slip in).
func loadBlindBlockmap(path string) (blindBlockmapFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return blindBlockmapFile{}, fmt.Errorf("blind-blockmap: read %q: %w", path, err)
	}
	var bm blindBlockmapFile
	if err := json.Unmarshal(raw, &bm); err != nil {
		return blindBlockmapFile{}, fmt.Errorf("blind-blockmap: decode %q: %w", path, err)
	}
	if bm.SchemaVersion != blindBlockmapSchemaVersion {
		return blindBlockmapFile{}, fmt.Errorf("blind-blockmap: %q schema_version %q (want %q)", path, bm.SchemaVersion, blindBlockmapSchemaVersion)
	}
	if strings.TrimSpace(bm.Salt) == "" {
		return blindBlockmapFile{}, fmt.Errorf("blind-blockmap: %q has a blank salt", path)
	}
	if len(bm.Blocks) == 0 {
		return blindBlockmapFile{}, fmt.Errorf("blind-blockmap: %q has no blocks", path)
	}
	for id, hash := range bm.Blocks {
		if want := blindOpaqueID(hash, bm.Salt); id != want {
			return blindBlockmapFile{}, fmt.Errorf("blind-blockmap: %q block id %q does not derive from its hash under the file's salt (want %q); the map is corrupt or hand-edited", path, id, want)
		}
	}
	return bm, nil
}

// worksheetGrammar describes one worksheet block dialect for
// scanWorksheetBlocks: the header prefixes that open a block (the blind
// grammar has two — hash-addressed ARTIFACT blocks and opaque-ID BLOCK
// blocks), the fill marker that opens the human-fill region, and the
// recognized fill-region fields.
type worksheetGrammar struct {
	headerPrefixes []string
	fillMarker     string
	fields         []string
}

// matchHeader reports the first grammar header prefix line starts with.
func (g worksheetGrammar) matchHeader(line string) (string, bool) {
	for _, p := range g.headerPrefixes {
		if strings.HasPrefix(line, p) {
			return p, true
		}
	}
	return "", false
}

// matchWorksheetField reports which recognized field a line starts with
// ("" when none). Shared by the fill-region parser and the pre-marker
// forged-field check.
func matchWorksheetField(fields []string, line string) string {
	for _, f := range fields {
		if strings.HasPrefix(line, f+":") {
			return f
		}
	}
	return ""
}

// worksheetSentinelCollision reports whether a line of UNTRUSTED text
// (candidate output, question, rubric, prompt body) collides with any
// worksheet sentinel across all three grammars: a fill marker, the shared end
// marker, a block-header prefix, or a recognized field line. The union is
// deliberate — one rule for every renderer beats three grammar-local ones,
// and over-escaping is harmless (the escape is rendering-only).
func worksheetSentinelCollision(line string) bool {
	for _, p := range []string{
		blindFillMarker, fcFillMarker, blindEndMarker,
		blindArtifactHeaderPrefix, blindBlockHeaderPrefix, fcPairHeaderPrefix,
		fcSidemapDigestMetadataPrefix,
	} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return matchWorksheetField([]string{"score", "notes", "flag", "prefer", "arm_guess", "reason"}, line) != ""
}

// escapeWorksheetText prefixes worksheetEscapePrefix onto every line of
// untrusted text that collides with a worksheet sentinel, so embedded
// sentinel-shaped text renders visibly but can never open, close, or score a
// block. Collision-free text is returned byte-identical (the pre-3c golden
// worksheets are pinned on that).
func escapeWorksheetText(s string) string {
	lines := strings.Split(s, "\n")
	changed := false
	for i, line := range lines {
		if worksheetSentinelCollision(line) {
			lines[i] = worksheetEscapePrefix + line
			changed = true
		}
	}
	if !changed {
		return s
	}
	return strings.Join(lines, "\n")
}

// scanWorksheetBlocks is the single block scanner behind all three worksheet
// parsers (blind, forced-choice, adjudication). Line rules: outside a block,
// only a header-prefix line opens one — so a forged header inside candidate
// output or an answer can never split a block; inside a block nothing is read
// until fillMarker; inside the fill region every non-blank line must start
// with a recognized field (loud error naming the block otherwise — this
// catches multi-line notes/reason continuations, leading spaces, and
// capitalized field typos that would otherwise silently drop data); a
// blindEndMarker line flushes the block, and a block still open at end of
// input is flushed once. open receives the matched prefix alongside the
// header body so a two-prefix grammar can tell its block kinds apart.
//
// Forged-frame hardening (external PR review P1): renderers escape untrusted
// text (escapeWorksheetText), so an UNESCAPED sentinel is always evidence of
// a forged or corrupted worksheet and errors loudly naming the block instead
// of silently mis-framing: a duplicate fill marker inside one block, an end
// marker before the fill marker, a recognized field line before the fill
// marker, and a fill or end marker outside any block (the reproduced attack
// closes the block early with a well-formed forged frame, orphaning the
// human's REAL fill region outside the block — the stray real marker is the
// loud evidence). Stray FIELD lines outside a block stay ignored: they
// cannot score anything, and the pre-hardening grammar pinned that.
func scanWorksheetBlocks(text string, g worksheetGrammar, open func(prefix, body string) error, setField func(field, value string), flush func() error) error {
	inBlock, afterMarker := false, false
	blockID := ""
	location := func() string {
		if blockID == "" && !inBlock {
			return "before any block"
		}
		if !inBlock {
			return fmt.Sprintf("after block %q", blockID)
		}
		return fmt.Sprintf("in block %q", blockID)
	}
	for _, line := range strings.Split(text, "\n") {
		prefix, isHeader := "", false
		if !inBlock {
			prefix, isHeader = g.matchHeader(line)
		}
		switch {
		case !inBlock && isHeader:
			body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, prefix), " ==="))
			if err := open(prefix, body); err != nil {
				return err
			}
			blockID = body
			afterMarker, inBlock = false, true
		case strings.HasPrefix(line, g.fillMarker):
			if !inBlock {
				return fmt.Errorf("stray fill marker outside any block (%s): unescaped sentinel text or a forged frame closed the block early", location())
			}
			if afterMarker {
				return fmt.Errorf("block %q: fill marker appears more than once (unescaped sentinel text or a forged frame)", blockID)
			}
			afterMarker = true
		case strings.HasPrefix(line, blindEndMarker):
			if !inBlock {
				return fmt.Errorf("stray end marker outside any block (%s): unescaped sentinel text or a forged frame", location())
			}
			if !afterMarker {
				return fmt.Errorf("block %q: end marker before the fill marker (unescaped sentinel text or a forged frame)", blockID)
			}
			// The fill region ends at the block terminator: a stray field line
			// between "=== END ===" and the next block must not attach to the
			// previous block.
			if err := flush(); err != nil {
				return err
			}
			inBlock, afterMarker = false, false
		case inBlock && !afterMarker && matchWorksheetField(g.fields, line) != "":
			return fmt.Errorf("block %q: field line %q appears before the fill marker (unescaped sentinel text or a forged frame)", blockID, line)
		case inBlock && afterMarker && strings.TrimSpace(line) != "":
			matched := matchWorksheetField(g.fields, line)
			if matched == "" {
				return fmt.Errorf("block %q: unrecognized fill-region line %q (recognized fields: %s)", blockID, line, strings.Join(g.fields, ", "))
			}
			setField(matched, strings.TrimSpace(strings.TrimPrefix(line, matched+":")))
		}
	}
	if inBlock {
		return flush()
	}
	return nil
}

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
// file is the join key, so no separate restore map). W5: promptless blocks
// are opaque-ID addressed ("=== BLOCK <id> ==="); their hash resolves
// through blockmap (-blind-blockmap), which is REQUIRED the moment any BLOCK
// header appears — an unknown id, a BLOCK id resolving to a non-promptless
// artifact, or an ARTIFACT header naming a promptless artifact (a pre-W5,
// unblinded worksheet) are loud errors. Blocks with a blank score
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
func ingestBlindWorksheet(worksheet string, arts []Artifact, labeler string, blockmap *blindBlockmapFile) (blindIngestResult, error) {
	var res blindIngestResult
	artByHash := make(map[string]Artifact, len(arts))
	for _, a := range arts {
		artByHash[a.ArtifactHash] = a
	}

	var hash, score, notes, flagVal string
	isDup, isOpaque := false, false
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
			hash, score, notes, flagVal, isDup, isOpaque = "", "", "", "", false, false
		}()
		a, ok := artByHash[hash]
		if !ok {
			return fmt.Errorf("worksheet references unknown artifact_hash %q (not in -artifacts)", hash)
		}
		// Header-kind / artifact-kind coherence: promptless artifacts are
		// worksheet-addressed by opaque BLOCK id only (a hash-headed
		// promptless block is a pre-W5 worksheet, unblinded by inspection),
		// and a BLOCK id must resolve to a promptless artifact.
		if promptless := prefilledAssemblyMode(a.Trace); promptless != isOpaque {
			if isOpaque {
				return fmt.Errorf("BLOCK id resolves to non-promptless artifact %q; the block map is stale or mismatched", hash)
			}
			return fmt.Errorf("worksheet addresses promptless artifact %q by hash; promptless blocks are opaque BLOCK blocks (re-render the worksheet)", hash)
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
		if flag == "" && strings.HasPrefix(labelNotes, groundingCheckPrefix) {
			// Spoofed-flag guard: the prefix is the downstream adjudication
			// selector, so it may only ever enter notes via the flag: line.
			return fmt.Errorf("artifact %s: notes begin with %q but flag: is blank; use flag: %s to request adjudication", hash, groundingCheckPrefix, groundingCheckFlag)
		}
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

	grammar := worksheetGrammar{
		headerPrefixes: []string{blindArtifactHeaderPrefix, blindBlockHeaderPrefix},
		fillMarker:     blindFillMarker,
		fields:         []string{"score", "notes", "flag"},
	}
	err := scanWorksheetBlocks(worksheet, grammar,
		func(prefix, body string) error {
			isDup = strings.HasSuffix(body, " DUP")
			isOpaque = prefix == blindBlockHeaderPrefix
			id := strings.TrimSpace(strings.TrimSuffix(body, " DUP"))
			if isOpaque {
				if blockmap == nil {
					return fmt.Errorf("worksheet holds opaque BLOCK blocks; -blind-blockmap (the render-emitted block map) is required to join them")
				}
				resolved, ok := blockmap.Blocks[id]
				if !ok {
					return fmt.Errorf("worksheet BLOCK id %q is not in the block map (wrong -blind-blockmap for this worksheet?)", id)
				}
				id = resolved
			}
			hash = id
			score, notes, flagVal = "", "", ""
			return nil
		},
		func(field, value string) {
			switch field {
			case "score":
				score = value
			case "notes":
				notes = value
			case "flag":
				flagVal = value
			}
		},
		flush)
	if err != nil {
		return blindIngestResult{}, err
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
// optional flag: line for marking a block grounding-check. W5: promptless
// blocks are additionally addressed by OPAQUE id ("=== BLOCK <id> ===",
// id = blindOpaqueID(hash, salt)) instead of the artifact hash — a header
// hash joins the committed artifacts JSONL and names the arm — and the
// returned block map (nil when no promptless block rendered) is the only
// id->hash join; the caller MUST persist it (-blind-blockmap-out) for
// ingest. salt "" draws a fresh random salt; tests pass a fixed one for
// deterministic worksheets. Non-assembly and 3a flat/progressive blocks stay
// byte-identical to the pre-3c rendering, [prompt] sections included.
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
func renderBlindWorksheet(arts []Artifact, dupN int, salt string) (string, *blindBlockmapFile, error) {
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
		return "", nil, err
	}
	hasPromptless := false
	for _, a := range ordered {
		if !prefilledAssemblyMode(a.Trace) {
			continue
		}
		hasPromptless = true
		// The [question] a promptless block shows is the final turn; if that
		// turn is not the user question (hand-built or corrupt artifact), the
		// block would leak prompt bytes into the primary pass.
		if turns := a.Trace.Turns; len(turns) == 0 || turns[len(turns)-1].Role != "user" {
			return "", nil, fmt.Errorf("promptless artifact %s: final turn must be a user question (worksheet would leak prompt bytes otherwise)", a.ArtifactHash)
		}
	}
	var bm *blindBlockmapFile
	if hasPromptless {
		if salt == "" {
			if salt, err = newBlindBlockSalt(); err != nil {
				return "", nil, err
			}
		}
		bm = &blindBlockmapFile{SchemaVersion: blindBlockmapSchemaVersion, Salt: salt, Blocks: map[string]string{}}
		for _, a := range ordered {
			if !prefilledAssemblyMode(a.Trace) {
				continue
			}
			id := blindOpaqueID(a.ArtifactHash, salt)
			if prev, dup := bm.Blocks[id]; dup && prev != a.ArtifactHash {
				// 64-bit truncated ids make this astronomically unlikely; a
				// re-render draws a fresh salt.
				return "", nil, fmt.Errorf("opaque block id collision between %s and %s; re-run -blind-render to draw a new salt", prev, a.ArtifactHash)
			}
			bm.Blocks[id] = a.ArtifactHash
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
		fmt.Fprintln(&b, "# grounding-check to flag it, or leave it blank. Promptless blocks carry an")
		fmt.Fprintln(&b, "# opaque id; pass the render-emitted block map to ingest via -blind-blockmap.")
	}
	if len(dups) > 0 {
		// Dup-eligible artifacts are legacy/mixed only, i.e. promptless BLOCK
		// blocks — so this doc line names BLOCK, and it may not contain the
		// literal " DUP ===" (the whole-worksheet DUP grammar is asserted).
		fmt.Fprintln(&b, "# BLOCK blocks marked DUP are intra-rater duplicates: score them")
		fmt.Fprintln(&b, "# independently; DUP scores never become labels.")
	}
	fmt.Fprintln(&b, "# Then run: llm-bench -blind-ingest -worksheet <this file> -artifacts <artifacts.jsonl> -labels-out <labels.jsonl>")
	fmt.Fprintln(&b)
	for _, a := range ordered {
		writeBlindBlock(&b, a, false, bm)
	}
	for _, a := range dups {
		writeBlindBlock(&b, a, true, bm)
	}
	return redactPaths(b.String()), bm, nil
}

// writeBlindBlock renders one worksheet block. Promptless (prefilled-mode)
// artifacts get an opaque BLOCK header (W5), [question], and a flag: fill
// line; everything else keeps the pre-3c hash-addressed [prompt] body
// byte-for-byte. DUP blocks reuse the promptless body under the DUP header
// minus the flag: line (only legacy/mixed artifacts are dup-eligible, and
// flags belong on the primary block).
func writeBlindBlock(b *strings.Builder, a Artifact, dup bool, bm *blindBlockmapFile) {
	promptless := prefilledAssemblyMode(a.Trace)
	header := blindArtifactHeaderPrefix + a.ArtifactHash
	if promptless {
		header = blindBlockHeaderPrefix + blindOpaqueID(a.ArtifactHash, bm.Salt)
	}
	if dup {
		fmt.Fprintf(b, "%s DUP ===\n", header)
	} else {
		fmt.Fprintf(b, "%s ===\n", header)
	}
	if promptless {
		fmt.Fprintln(b, "[question]")
		fmt.Fprintln(b, escapeWorksheetText(strings.TrimSpace(blindQuestion(a.Trace))))
	} else {
		if a.Trace.AssemblyEval == nil {
			fmt.Fprintf(b, "trace: %s\n\n", a.TraceID)
		}
		writeBlindPromptBody(b, a.Trace)
	}
	fmt.Fprintln(b)
	fmt.Fprintln(b, "[rubric]")
	fmt.Fprintln(b, escapeWorksheetText(strings.TrimSpace(a.Trace.Golden.FinalAnswerCriteria)))
	fmt.Fprintln(b)
	fmt.Fprintln(b, "[candidate output]")
	fmt.Fprintln(b, escapeWorksheetText(strings.TrimSpace(a.ActualFinalAnswer)))
	fmt.Fprintln(b)
	fmt.Fprintln(b, blindFillMarker)
	fmt.Fprintln(b, "score: ")
	fmt.Fprintln(b, "notes: ")
	// DUP blocks get no flag: line — flags belong on the primary block (a
	// flag on a DUP is an ingest error) so the duplicate cannot invite one.
	if promptless && !dup {
		fmt.Fprintln(b, "flag: ")
	}
	fmt.Fprintln(b, blindEndMarker)
	fmt.Fprintln(b)
}

// writeBlindPromptBody renders the full-prompt section ([prompt], trimmed
// system, each non-empty turn). Shared by the non-promptless worksheet block
// and the adjudication worksheet so the two spellings can never drift.
// System and turn content are untrusted text and sentinel-escaped.
func writeBlindPromptBody(b *strings.Builder, t Trace) {
	fmt.Fprintln(b, "[prompt]")
	fmt.Fprintln(b, escapeWorksheetText(strings.TrimSpace(t.System)))
	for _, turn := range t.Turns {
		content := strings.TrimSpace(turn.Content)
		if content == "" {
			continue
		}
		fmt.Fprintf(b, "\n<%s>\n%s\n", turn.Role, escapeWorksheetText(content))
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

// filterArtifactsByModel narrows a worksheet render to one candidate model
// (#331 W3, -model: the registered workflow labels one model at a time).
// Matching is modelKey equality so bare and provider-prefixed spellings
// agree. An empty selector keeps every artifact; a selector matching zero
// artifacts is a loud error (a silently empty worksheet would read as "no
// work left").
func filterArtifactsByModel(arts []Artifact, selector string) ([]Artifact, error) {
	if strings.TrimSpace(selector) == "" {
		return arts, nil
	}
	key := modelKey(selector)
	var out []Artifact
	for _, a := range arts {
		if modelKey(a.CandidateModel) == key {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-model %q matches no artifacts", selector)
	}
	return out, nil
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
