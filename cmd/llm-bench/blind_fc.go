package main

// Forced-choice sidecar (#331 slice 3c): -fc-render shows each complete
// legacy/mixed pair as two anonymous answers (A/B) to the same question;
// -fc-ingest emits FCPreference rows. The A/B->arm resolution is deliberately
// NOT written into the rows so the sidecar stays blind-stable on disk: the
// later report task resolves sides by recomputing the hash parity via
// fcSideIsLegacyA, which is the single place the secret lives.

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"
)

// fcFillMarker delimits the human-fill region of a forced-choice block; only
// a prefer: line after it is read by the ingest parser.
const fcFillMarker = "--- fill below (prefer: A | B | tie) ---"

// FCPreference is one forced-choice sidecar row (JSONL via -fc-out).
// Preference is "a", "b", or "tie" — a SIDE, not an arm: which assembly arm
// rendered as A is recoverable only through fcSideIsLegacyA(PairID,
// CandidateModel), by design, so reading the sidecar alone unblinds nothing.
type FCPreference struct {
	PairID         string    `json:"pair_id"`
	CandidateModel string    `json:"candidate_model"`
	ArtifactHashA  string    `json:"artifact_hash_a"`
	ArtifactHashB  string    `json:"artifact_hash_b"`
	Preference     string    `json:"preference"`
	Labeler        string    `json:"labeler"`
	LabeledAt      time.Time `json:"labeled_at"`
}

// fcSideIsLegacyA is the registered A/B side assignment and the ONE place the
// parity secret is computed (render, ingest validation, and the later report's
// side resolution all call it): FNV-1a-64 over pairID+"|"+modelKey+"|fc"; an
// even sum renders the legacy arm as answer A, an odd sum the mixed arm.
func fcSideIsLegacyA(pairID, modelKey string) bool {
	h := fnv.New64a()
	_, _ = h.Write([]byte(pairID + "|" + modelKey + "|fc"))
	return h.Sum64()%2 == 0
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
// pair. The header line carries pairID, modelKey, and BOTH artifact hashes
// (hashA then hashB) for the ingest join — hashes do not leak arm identity
// because the side assignment is the hash-parity secret. The question is the
// final turn content, identical across arms by construction; a mismatch is
// fixture corruption and a loud error. No mode names appear anywhere.
func renderForcedChoiceWorksheet(arts []Artifact) (string, error) {
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
	fmt.Fprintln(&b, "# it blank to skip the block. Side assignment is a deterministic hash secret;")
	fmt.Fprintln(&b, "# nothing in a block reveals which arm produced which side.")
	fmt.Fprintln(&b, "# Then run: llm-bench -fc-ingest -worksheet <this file> -artifacts <artifacts.jsonl> -fc-out <preferences.jsonl>")
	fmt.Fprintln(&b)
	for _, p := range pairs {
		question := strings.TrimSpace(blindQuestion(p.legacy.Trace))
		if question != strings.TrimSpace(blindQuestion(p.mixed.Trace)) {
			return "", fmt.Errorf("forced-choice: pair %q model %q: question differs across arms (fixture corruption)", p.pairID, p.model)
		}
		sideA, sideB := p.legacy, p.mixed
		if !fcSideIsLegacyA(p.pairID, p.model) {
			sideA, sideB = p.mixed, p.legacy
		}
		fmt.Fprintf(&b, "=== PAIR %s %s %s %s ===\n", p.pairID, p.model, sideA.ArtifactHash, sideB.ArtifactHash)
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
		fmt.Fprintln(&b, blindEndMarker)
		fmt.Fprintln(&b)
	}
	return redactPaths(b.String()), nil
}

// ingestForcedChoiceWorksheet parses a filled forced-choice worksheet into
// sidecar rows, validating every block against arts: unknown hashes, artifacts
// that do not belong to the header's (pair, model), hash order disagreeing
// with the registered side assignment, an invalid prefer: value, or a
// duplicate pair block are loud errors. A blank prefer: skips the block
// (partial labeling allowed) and is counted. labeler and labeled_at are
// stamped on every row.
func ingestForcedChoiceWorksheet(worksheet string, arts []Artifact, labeler string) (rows []FCPreference, skipped int, err error) {
	artByHash := make(map[string]Artifact, len(arts))
	for _, a := range arts {
		artByHash[a.ArtifactHash] = a
	}
	now := time.Now().UTC()
	seen := map[string]struct{}{}

	var header []string
	var prefer string
	afterMarker := false
	flush := func() error {
		if header == nil {
			return nil
		}
		defer func() { header, prefer, afterMarker = nil, "", false }()
		pairID, model, hashA, hashB := header[0], header[1], header[2], header[3]
		seenKey := pairID + "\x00" + model
		if _, dup := seen[seenKey]; dup {
			return fmt.Errorf("forced-choice worksheet: duplicate block for pair %q model %q", pairID, model)
		}
		seen[seenKey] = struct{}{}
		sideA, okA := artByHash[hashA]
		if !okA {
			return fmt.Errorf("forced-choice worksheet: pair %q model %q: unknown artifact_hash_a %q (not in -artifacts)", pairID, model, hashA)
		}
		sideB, okB := artByHash[hashB]
		if !okB {
			return fmt.Errorf("forced-choice worksheet: pair %q model %q: unknown artifact_hash_b %q (not in -artifacts)", pairID, model, hashB)
		}
		for _, side := range []Artifact{sideA, sideB} {
			ae := side.Trace.AssemblyEval
			if ae == nil || ae.PairID != pairID || modelKey(side.CandidateModel) != model {
				return fmt.Errorf("forced-choice worksheet: artifact %q does not belong to pair %q model %q", side.ArtifactHash, pairID, model)
			}
		}
		wantA, wantB := AssemblyMixed, AssemblyLegacy
		if fcSideIsLegacyA(pairID, model) {
			wantA, wantB = AssemblyLegacy, AssemblyMixed
		}
		if sideA.Trace.AssemblyEval.Mode != wantA || sideB.Trace.AssemblyEval.Mode != wantB {
			return fmt.Errorf("forced-choice worksheet: pair %q model %q: hash order disagrees with the registered side assignment", pairID, model)
		}
		p := strings.ToLower(strings.TrimSpace(prefer))
		if p == "" {
			skipped++
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
			Labeler:        labeler,
			LabeledAt:      now,
		})
		return nil
	}

	inBlock := false
	for _, line := range strings.Split(worksheet, "\n") {
		switch {
		case !inBlock && strings.HasPrefix(line, "=== PAIR "):
			fields := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, "=== PAIR "), " ==="))
			if len(fields) != 4 {
				return nil, 0, fmt.Errorf("forced-choice worksheet: malformed header %q (want === PAIR <pair> <model> <hashA> <hashB> ===)", line)
			}
			header, prefer, afterMarker, inBlock = fields, "", false, true
		case inBlock && strings.HasPrefix(line, fcFillMarker):
			afterMarker = true
		case inBlock && strings.HasPrefix(line, blindEndMarker):
			if ferr := flush(); ferr != nil {
				return nil, 0, ferr
			}
			inBlock = false
		case inBlock && afterMarker && strings.HasPrefix(line, "prefer:"):
			prefer = strings.TrimSpace(strings.TrimPrefix(line, "prefer:"))
		}
	}
	if inBlock {
		if ferr := flush(); ferr != nil {
			return nil, 0, ferr
		}
	}
	return rows, skipped, nil
}
