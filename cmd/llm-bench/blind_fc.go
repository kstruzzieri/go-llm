package main

// Forced-choice sidecar (#331 slice 3c): -fc-render shows each complete
// legacy/mixed pair as two anonymous answers (A/B) to the same question;
// -fc-ingest emits FCPreference rows. The A/B->arm resolution is deliberately
// NOT written into the rows so the sidecar stays blind-stable on disk: side
// assignment comes from the sealed random side map (-fc-sidemap, W3 —
// REQUIRED on render and ingest), whose committed digest seals it before
// labeling. W5: the sidemap is also the ONLY hash carrier — worksheet PAIR
// headers carry pairID and modelKey alone, because a header hash joins the
// committed artifacts JSONL (whose AssemblyEval.Mode names the arm) and
// unblinds without the map. fcSideIsLegacyA remains ONLY as the report's
// default resolver for pre-sidemap worksheets.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"strings"
	"time"
)

// fcFillMarker delimits the human-fill region of a forced-choice block; only
// prefer: and arm_guess: lines after it are read by the ingest parser.
const fcFillMarker = "--- fill below (prefer: A | B | tie) ---"

// FCPreference is one forced-choice sidecar row (JSONL via -fc-out).
// Preference is "a", "b", or "tie" — a SIDE, not an arm: which assembly arm
// rendered as A is recoverable only through the sealed sidemap (or, for
// pre-sidemap worksheets, fcSideIsLegacyA), by design, so reading the
// sidecar alone unblinds nothing. ArmGuess is the optional blinding-audit
// guess ("a"/"b": which SIDE the labeler believes came from the
// reduced-context mixed arm; empty = no guess); it feeds only the
// descriptive arm_guess_accuracy section, never any decision.
type FCPreference struct {
	PairID         string    `json:"pair_id"`
	CandidateModel string    `json:"candidate_model"`
	ArtifactHashA  string    `json:"artifact_hash_a"`
	ArtifactHashB  string    `json:"artifact_hash_b"`
	Preference     string    `json:"preference"`
	ArmGuess       string    `json:"arm_guess,omitempty"`
	Labeler        string    `json:"labeler"`
	LabeledAt      time.Time `json:"labeled_at"`
}

// fcSideIsLegacyA is the LEGACY (pre-sidemap) A/B side assignment: FNV-1a-64
// over pairID+"|"+modelKey+"|fc"; an even sum renders the legacy arm as
// answer A, an odd sum the mixed arm. It survives ONLY as the report's
// default resolver for worksheets rendered before the sealed sidemap
// existed; new render/ingest runs require -fc-sidemap.
func fcSideIsLegacyA(pairID, modelKey string) bool {
	h := fnv.New64a()
	_, _ = h.Write([]byte(pairID + "|" + modelKey + "|fc"))
	return h.Sum64()%2 == 0
}

// fcSideResolver resolves which arm renders as answer A for one
// (pairID, modelKey): true = legacy is A. An error means the resolver has no
// assignment for the pair (a sidemap gap) and must abort the caller loudly.
type fcSideResolver func(pairID, modelKey string) (bool, error)

// fcParityResolver adapts the legacy hash-parity assignment to the resolver
// seam. Report-only default for pre-sidemap worksheets.
func fcParityResolver(pairID, modelKey string) (bool, error) {
	return fcSideIsLegacyA(pairID, modelKey), nil
}

// fcSidemapSchemaVersion pins the sealed side-map file schema. v2 (#331 W5)
// adds hash_a/hash_b per pair: the sidemap became the sidecar's ONLY hash
// carrier when worksheet headers went hash-free, so a v1 map can no longer
// drive render or ingest and is rejected at load.
const fcSidemapSchemaVersion = "fc-sidemap/v2"

// fcSidemapFile is the sealed random side map: one crypto/rand boolean per
// complete legacy/mixed pair, keyed "<pairID>|<modelKey>", plus the two arm
// artifact hashes in the A/B order that boolean dictates. SealedDigest is
// written empty — the seal is the file's OWN sha256, printed at generation
// time for the operator to commit BEFORE labeling and to verify at report
// time via -fc-sidemap-digest.
type fcSidemapFile struct {
	SchemaVersion string                    `json:"schema_version"`
	Pairs         map[string]fcSidemapEntry `json:"pairs"`
	SealedDigest  string                    `json:"sealed_digest"`
}

// fcSidemapEntry is one pair's sealed assignment: which arm renders as
// answer A, and the artifact hashes in A/B order (HashA is the legacy arm's
// hash iff LegacyIsA).
type fcSidemapEntry struct {
	LegacyIsA bool   `json:"legacy_is_a"`
	HashA     string `json:"hash_a"`
	HashB     string `json:"hash_b"`
}

func fcSidemapKey(pairID, modelKey string) string {
	return pairID + "|" + modelKey
}

// entry resolves one pair's sealed assignment; a pair absent from the map is
// a loud error, never a fallback.
func (m fcSidemapFile) entry(pairID, modelKey string) (fcSidemapEntry, error) {
	e, ok := m.Pairs[fcSidemapKey(pairID, modelKey)]
	if !ok {
		return fcSidemapEntry{}, fmt.Errorf("fc-sidemap has no entry for pair %q model %q (regenerate the sidemap over the current artifacts)", pairID, modelKey)
	}
	return e, nil
}

// generateFCSidemap builds the sealed random side map over the complete
// legacy/mixed pairs of arts, one crypto/rand boolean each, recording each
// pair's arm hashes in the drawn A/B order.
func generateFCSidemap(arts []Artifact) (fcSidemapFile, error) {
	pairs, err := collectFCPairs(arts)
	if err != nil {
		return fcSidemapFile{}, err
	}
	if len(pairs) == 0 {
		return fcSidemapFile{}, fmt.Errorf("fc-sidemap: no complete legacy/mixed pairs with non-empty answers in -artifacts")
	}
	random := make([]byte, len(pairs))
	if _, err := rand.Read(random); err != nil {
		return fcSidemapFile{}, fmt.Errorf("fc-sidemap: crypto/rand: %w", err)
	}
	m := fcSidemapFile{SchemaVersion: fcSidemapSchemaVersion, Pairs: make(map[string]fcSidemapEntry, len(pairs))}
	for i, p := range pairs {
		legacyIsA := random[i]&1 == 1
		hashA, hashB := p.legacy.ArtifactHash, p.mixed.ArtifactHash
		if !legacyIsA {
			hashA, hashB = hashB, hashA
		}
		m.Pairs[fcSidemapKey(p.pairID, p.model)] = fcSidemapEntry{LegacyIsA: legacyIsA, HashA: hashA, HashB: hashB}
	}
	return m, nil
}

// writeFCSidemap writes the sealed side map and returns the file's sha256
// hex digest (the commit-before-labeling seal).
func writeFCSidemap(path string, m fcSidemapFile) (string, error) {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("fc-sidemap: marshal: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("fc-sidemap: write: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// loadFCSidemap reads a sealed side map, returning the parsed map, its
// resolver, and the file's sha256 hex digest (for -fc-sidemap-digest
// verification). A missing pair is a loud error at resolution time.
func loadFCSidemap(path string) (fcSidemapFile, fcSideResolver, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fcSidemapFile{}, nil, "", fmt.Errorf("fc-sidemap: read %q: %w", path, err)
	}
	var m fcSidemapFile
	if err := json.Unmarshal(raw, &m); err != nil {
		return fcSidemapFile{}, nil, "", fmt.Errorf("fc-sidemap: decode %q: %w", path, err)
	}
	if m.SchemaVersion != fcSidemapSchemaVersion {
		return fcSidemapFile{}, nil, "", fmt.Errorf("fc-sidemap: %q schema_version %q (want %q)", path, m.SchemaVersion, fcSidemapSchemaVersion)
	}
	if len(m.Pairs) == 0 {
		return fcSidemapFile{}, nil, "", fmt.Errorf("fc-sidemap: %q has no pairs", path)
	}
	// v2 entries carry the pair's arm hashes (the only hash carrier for the
	// forced-choice flow); a blank or duplicated hash cannot join anything.
	for key, e := range m.Pairs {
		if strings.TrimSpace(e.HashA) == "" || strings.TrimSpace(e.HashB) == "" {
			return fcSidemapFile{}, nil, "", fmt.Errorf("fc-sidemap: %q entry %q is missing hash_a/hash_b (regenerate with -fc-sidemap-generate)", path, key)
		}
		if e.HashA == e.HashB {
			return fcSidemapFile{}, nil, "", fmt.Errorf("fc-sidemap: %q entry %q has identical hash_a and hash_b", path, key)
		}
	}
	sum := sha256.Sum256(raw)
	return m, m.resolver(), hex.EncodeToString(sum[:]), nil
}

// resolver returns the sidemap-backed fcSideResolver: a pair absent from the
// map is a loud error, never a parity fallback — a silent fallback would let
// a stale sidemap unblind or mis-assign new pairs.
func (m fcSidemapFile) resolver() fcSideResolver {
	return func(pairID, modelKey string) (bool, error) {
		e, err := m.entry(pairID, modelKey)
		if err != nil {
			return false, err
		}
		return e.LegacyIsA, nil
	}
}

// verifyFCSidemapDigest compares the loaded sidemap file digest against the
// operator-committed one (-fc-sidemap-digest). Case-insensitive hex compare;
// an optional "sha256:" prefix on the committed value is accepted.
func verifyFCSidemapDigest(gotHex, committed string) error {
	want := strings.TrimPrefix(strings.TrimSpace(committed), "sha256:")
	if !strings.EqualFold(gotHex, want) {
		return fmt.Errorf("fc-sidemap digest mismatch: file is sha256:%s, committed digest is sha256:%s", gotHex, want)
	}
	return nil
}

// loadFCPreferences reads a -fc-ingest sidecar JSONL. Join fields and the
// preference domain are validated at load so every downstream consumer
// (-assembly-report -fc-preferences) can trust the rows.
func loadFCPreferences(path string) ([]FCPreference, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	var out []FCPreference
	dec := json.NewDecoder(f)
	for dec.More() {
		var r FCPreference
		if err := dec.Decode(&r); err != nil {
			return nil, fmt.Errorf("decode fc-preference row %d: %w", len(out)+1, err)
		}
		if r.PairID == "" || r.CandidateModel == "" || r.ArtifactHashA == "" || r.ArtifactHashB == "" {
			return nil, fmt.Errorf("fc-preference row %d: blank join field (pair_id, candidate_model, artifact_hash_a, artifact_hash_b are all required)", len(out)+1)
		}
		switch r.Preference {
		case "a", "b", "tie":
		default:
			return nil, fmt.Errorf("fc-preference row %d (pair %q): invalid preference %q (want a, b, or tie)", len(out)+1, r.PairID, r.Preference)
		}
		switch r.ArmGuess {
		case "", "a", "b":
		default:
			return nil, fmt.Errorf("fc-preference row %d (pair %q): invalid arm_guess %q (want a, b, or empty)", len(out)+1, r.PairID, r.ArmGuess)
		}
		out = append(out, r)
	}
	return out, nil
}

// fcPair is one complete forced-choice pair: both arms present with non-empty
// final answers, keyed by (PairID, modelKey).
type fcPair struct {
	pairID, model string
	legacy, mixed Artifact
}

// collectFCPairs groups legacy/mixed artifacts by (PairID, modelKey) and keeps
// the complete pairs (both arms, both answers non-empty), sorted by
// (pairID, model). A duplicate arm for one key is a loud error; incomplete
// pairs are silently excluded (there is nothing to force a choice between).
func collectFCPairs(arts []Artifact) ([]fcPair, error) {
	type key struct{ pair, model string }
	type armSet struct{ legacy, mixed *Artifact }
	sets := map[key]*armSet{}
	var order []key
	for i := range arts {
		a := &arts[i]
		if a.Trace.AssemblyEval == nil {
			continue
		}
		mode := a.Trace.AssemblyEval.Mode
		if mode != AssemblyLegacy && mode != AssemblyMixed {
			continue
		}
		k := key{a.Trace.AssemblyEval.PairID, modelKey(a.CandidateModel)}
		s, ok := sets[k]
		if !ok {
			s = &armSet{}
			sets[k] = s
			order = append(order, k)
		}
		if mode == AssemblyLegacy {
			if s.legacy != nil {
				return nil, fmt.Errorf("forced-choice: duplicate legacy arm for pair %q model %q", k.pair, k.model)
			}
			s.legacy = a
		} else {
			if s.mixed != nil {
				return nil, fmt.Errorf("forced-choice: duplicate mixed arm for pair %q model %q", k.pair, k.model)
			}
			s.mixed = a
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].pair != order[j].pair {
			return order[i].pair < order[j].pair
		}
		return order[i].model < order[j].model
	})
	var pairs []fcPair
	for _, k := range order {
		s := sets[k]
		if s.legacy == nil || s.mixed == nil {
			continue
		}
		if strings.TrimSpace(s.legacy.ActualFinalAnswer) == "" || strings.TrimSpace(s.mixed.ActualFinalAnswer) == "" {
			continue
		}
		pairs = append(pairs, fcPair{pairID: k.pair, model: k.model, legacy: *s.legacy, mixed: *s.mixed})
	}
	return pairs, nil
}

// renderForcedChoiceWorksheet emits one A/B block per complete legacy/mixed
// pair. The header line carries pairID and modelKey ONLY (W5): a header hash
// would join the committed artifacts JSONL, whose AssemblyEval.Mode names
// the arm, and unblind the block without the sealed map — so the sidemap is
// the sole hash carrier and the ingest join runs through it. The question is
// the final turn content, identical across arms by construction; a mismatch
// is fixture corruption and a loud error. No mode names or hashes appear
// anywhere. sidemap is REQUIRED; a pair it cannot place, or an entry whose
// hashes disagree with the pair's arms under its own assignment, is a loud
// error.
func renderForcedChoiceWorksheet(arts []Artifact, sidemap fcSidemapFile) (string, error) {
	if len(sidemap.Pairs) == 0 {
		return "", fmt.Errorf("forced-choice: render requires the sealed side map (-fc-sidemap)")
	}
	pairs, err := collectFCPairs(arts)
	if err != nil {
		return "", err
	}
	if len(pairs) == 0 {
		return "", fmt.Errorf("forced-choice: no complete legacy/mixed pairs with non-empty answers in -artifacts")
	}
	var b strings.Builder
	fmt.Fprintln(&b, "# llm-bench — forced-choice worksheet")
	fmt.Fprintln(&b, "#")
	fmt.Fprintln(&b, "# Each block shows the same question answered twice by one candidate model")
	fmt.Fprintln(&b, "# under two hidden context-assembly arms. Fill prefer: with A, B, or tie; leave")
	fmt.Fprintln(&b, "# it blank to skip the block. Side assignment is a sealed random map committed")
	fmt.Fprintln(&b, "# before labeling; nothing in a block reveals which arm produced which side.")
	fmt.Fprintln(&b, "# arm_guess: is an optional blinding audit — if you believe you can tell which")
	fmt.Fprintln(&b, "# answer came from the reduced-context arm, write a or b; otherwise leave blank.")
	fmt.Fprintln(&b, "# It never affects any result; it only measures whether the blinding held.")
	fmt.Fprintln(&b, "# Then run: llm-bench -fc-ingest -worksheet <this file> -artifacts <artifacts.jsonl> -fc-sidemap <sidemap.json> -fc-out <preferences.jsonl>")
	fmt.Fprintln(&b)
	for _, p := range pairs {
		question := strings.TrimSpace(blindQuestion(p.legacy.Trace))
		if question != strings.TrimSpace(blindQuestion(p.mixed.Trace)) {
			return "", fmt.Errorf("forced-choice: pair %q model %q: question differs across arms (fixture corruption)", p.pairID, p.model)
		}
		e, err := sidemap.entry(p.pairID, p.model)
		if err != nil {
			return "", fmt.Errorf("forced-choice: %w", err)
		}
		sideA, sideB := p.legacy, p.mixed
		if !e.LegacyIsA {
			sideA, sideB = p.mixed, p.legacy
		}
		// Integrity: the sealed entry's hashes must be exactly this pair's arm
		// hashes in its own A/B order — a mismatch means the sidemap was
		// generated over different artifacts (or hand-edited).
		if e.HashA != sideA.ArtifactHash || e.HashB != sideB.ArtifactHash {
			return "", fmt.Errorf("forced-choice: pair %q model %q: sidemap hashes %q/%q disagree with the pair's arms under its registered side assignment (want %q/%q); regenerate the sidemap over the current artifacts",
				p.pairID, p.model, e.HashA, e.HashB, sideA.ArtifactHash, sideB.ArtifactHash)
		}
		// The PAIR header is space-delimited: any whitespace inside a field
		// would shift the ingest join columns, so refuse it at render time.
		for _, part := range []string{p.pairID, p.model} {
			if part == "" || strings.ContainsAny(part, " \t\r\n") {
				return "", fmt.Errorf("forced-choice: pair %q model %q: header field %q is empty or contains whitespace (the PAIR header is space-delimited)", p.pairID, p.model, part)
			}
		}
		fmt.Fprintf(&b, "=== PAIR %s %s ===\n", p.pairID, p.model)
		fmt.Fprintln(&b, "[question]")
		fmt.Fprintln(&b, question)
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "[rubric]")
		fmt.Fprintln(&b, strings.TrimSpace(p.legacy.Trace.Golden.FinalAnswerCriteria))
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "[answer A]")
		fmt.Fprintln(&b, strings.TrimSpace(sideA.ActualFinalAnswer))
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "[answer B]")
		fmt.Fprintln(&b, strings.TrimSpace(sideB.ActualFinalAnswer))
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, fcFillMarker)
		fmt.Fprintln(&b, "prefer: ")
		fmt.Fprintln(&b, "arm_guess: ")
		fmt.Fprintln(&b, blindEndMarker)
		fmt.Fprintln(&b)
	}
	return redactPaths(b.String()), nil
}

// ingestForcedChoiceWorksheet parses a filled forced-choice worksheet into
// sidecar rows, resolving each block's A/B artifact hashes from the sealed
// sidemap (W5: the worksheet carries none) and validating every block
// against arts: a pair absent from the sidemap, sidemap hashes absent from
// the artifacts or not belonging to the header's (pair, model), sidemap
// hashes disagreeing with its own side assignment, an invalid prefer: or
// arm_guess: value, or a duplicate pair block are loud errors. A blank
// prefer: skips the block (partial labeling allowed) and is counted — unless
// requireComplete is set (the registered workflow), which turns ANY blank
// prefer: into a loud error listing the unfilled pairs. sidemap is REQUIRED
// and -fc-sidemap-digest verification is the guard that render and ingest
// used the SAME sealed map. labeler and labeled_at are stamped on every row.
func ingestForcedChoiceWorksheet(worksheet string, arts []Artifact, labeler string, sidemap fcSidemapFile, requireComplete bool) (rows []FCPreference, skipped int, err error) {
	if len(sidemap.Pairs) == 0 {
		return nil, 0, fmt.Errorf("forced-choice worksheet: ingest requires the sealed side map (-fc-sidemap)")
	}
	artByHash := make(map[string]Artifact, len(arts))
	for _, a := range arts {
		artByHash[a.ArtifactHash] = a
	}
	now := time.Now().UTC()
	seen := map[string]struct{}{}
	var blankPairs []string

	var header []string
	var prefer, armGuess string
	flush := func() error {
		if header == nil {
			return nil
		}
		defer func() { header, prefer, armGuess = nil, "", "" }()
		pairID, model := header[0], header[1]
		seenKey := pairID + "\x00" + model
		if _, dup := seen[seenKey]; dup {
			return fmt.Errorf("forced-choice worksheet: duplicate block for pair %q model %q", pairID, model)
		}
		seen[seenKey] = struct{}{}
		e, err := sidemap.entry(pairID, model)
		if err != nil {
			return fmt.Errorf("forced-choice worksheet: %w", err)
		}
		hashA, hashB := e.HashA, e.HashB
		sideA, okA := artByHash[hashA]
		if !okA {
			return fmt.Errorf("forced-choice worksheet: pair %q model %q: sidemap hash_a %q not in -artifacts", pairID, model, hashA)
		}
		sideB, okB := artByHash[hashB]
		if !okB {
			return fmt.Errorf("forced-choice worksheet: pair %q model %q: sidemap hash_b %q not in -artifacts", pairID, model, hashB)
		}
		for _, side := range []Artifact{sideA, sideB} {
			ae := side.Trace.AssemblyEval
			if ae == nil || ae.PairID != pairID || modelKey(side.CandidateModel) != model {
				return fmt.Errorf("forced-choice worksheet: artifact %q does not belong to pair %q model %q", side.ArtifactHash, pairID, model)
			}
		}
		wantA, wantB := AssemblyMixed, AssemblyLegacy
		if e.LegacyIsA {
			wantA, wantB = AssemblyLegacy, AssemblyMixed
		}
		if sideA.Trace.AssemblyEval.Mode != wantA || sideB.Trace.AssemblyEval.Mode != wantB {
			return fmt.Errorf("forced-choice worksheet: pair %q model %q: sidemap hashes disagree with the registered side assignment (regenerate the sidemap over the current artifacts)", pairID, model)
		}
		g := strings.ToLower(strings.TrimSpace(armGuess))
		switch g {
		case "", "a", "b":
		default:
			return fmt.Errorf("forced-choice worksheet: pair %q model %q: invalid arm_guess %q (want a, b, or blank)", pairID, model, armGuess)
		}
		p := strings.ToLower(strings.TrimSpace(prefer))
		if p == "" {
			skipped++
			blankPairs = append(blankPairs, fmt.Sprintf("%s/%s", pairID, model))
			return nil
		}
		switch p {
		case "a", "b", "tie":
		default:
			return fmt.Errorf("forced-choice worksheet: pair %q model %q: invalid prefer %q (want A, B, or tie)", pairID, model, prefer)
		}
		rows = append(rows, FCPreference{
			PairID:         pairID,
			CandidateModel: model,
			ArtifactHashA:  hashA,
			ArtifactHashB:  hashB,
			Preference:     p,
			ArmGuess:       g,
			Labeler:        labeler,
			LabeledAt:      now,
		})
		return nil
	}

	grammar := worksheetGrammar{headerPrefixes: []string{"=== PAIR "}, fillMarker: fcFillMarker, fields: []string{"prefer", "arm_guess"}}
	err = scanWorksheetBlocks(worksheet, grammar,
		func(_, body string) error {
			fields := strings.Fields(body)
			if len(fields) != 2 {
				return fmt.Errorf("forced-choice worksheet: malformed header %q (want === PAIR <pair> <model> ===)", body)
			}
			header, prefer, armGuess = fields, "", ""
			return nil
		},
		func(field, value string) {
			if field == "prefer" {
				prefer = value
			} else {
				armGuess = value
			}
		},
		flush)
	if err != nil {
		return nil, 0, err
	}
	if requireComplete && len(blankPairs) > 0 {
		return nil, 0, fmt.Errorf("forced-choice worksheet: -fc-require-complete: %d block(s) left blank: %s",
			len(blankPairs), strings.Join(blankPairs, ", "))
	}
	return rows, skipped, nil
}
