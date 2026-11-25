package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// recallReportOptions is the recall report's input: the registration, the
// captures (artifact files, each with its manifest), the run number (1 or 2
// for a confirmatory registration; 0 for the pilot view and the headroom
// check) and whether to run only the headroom check.
type recallReportOptions struct {
	RegistrationPath string
	Captures         []string
	Run              int
	Headroom         bool
}

// recallContinue is the headroom check's verdict when the run may go on.
const recallContinue recallVerdict = "continue"

// recallCaptureSummary records one verified capture for the report header.
type recallCaptureSummary struct {
	Name, ManifestDigest, Endpoint string
	Models                         []string
	Artifacts, Failed, Unattempted int
}

// recallIndex finds an artifact by (model key, trace ID).
type recallIndex map[[2]string]Artifact

// recallIndexOf indexes arts; a later artifact for the same cell wins (the
// loader refuses duplicate cells, so callers never rely on that).
func recallIndexOf(arts []Artifact) recallIndex {
	ix := make(recallIndex, len(arts))
	for _, a := range arts {
		ix[[2]string{modelKey(a.CandidateModel), a.TraceID}] = a
	}
	return ix
}

// get returns the model's artifact for a trace ID.
func (ix recallIndex) get(model, id string) (Artifact, bool) {
	a, ok := ix[[2]string{modelKey(model), id}]
	return a, ok
}

// recallRegSet is the registered family and cell universe, derived from the
// registered trace IDs, never from the artifacts that happened to exist.
type recallRegSet struct {
	fams  []string
	cells map[string]map[string]bool
}

// recallRegisteredSet parses every registered trace ID into its family and
// cell; an ID that does not parse is an error.
func recallRegisteredSet(reg recallRegistration) (recallRegSet, error) {
	s := recallRegSet{cells: map[string]map[string]bool{}}
	for _, tr := range reg.Traces {
		fam, cell, err := parseRecallTraceID(tr.ID)
		if err != nil {
			return recallRegSet{}, err
		}
		if s.cells[fam] == nil {
			s.cells[fam] = map[string]bool{}
			s.fams = append(s.fams, fam)
		}
		s.cells[fam][cell] = true
	}
	sort.Strings(s.fams)
	return s, nil
}

// has reports whether the family's cell is registered.
func (s recallRegSet) has(fam, cell string) bool { return s.cells[fam][cell] }

// recallTraceID builds a trace ID from its family and cell.
func recallTraceID(fam, cell string) string { return "recall-" + fam + "-" + cell }

// recallACell names the Trace A cell for an arm; S7 is the arm 2 trace with
// the label on a record other than the relevant one.
func recallACell(arm, caseID string) string {
	if caseID == "S7" {
		return "A-2-s7"
	}
	return "A-" + arm
}

// recallBCell names the Trace B cell for a case and arm.
func recallBCell(arm, caseID string) string {
	return "B-" + strings.ToLower(caseID) + "-" + arm
}

// recallReply is the scored reply: the last assistant content plus the raw
// calls that decoded (a malformed call drops only itself). The transcript's
// calls count only when the artifact holds no raw calls at all (in recall
// captures both channels come from the same response, so this fallback does
// not change a real score). Thinking is never scored.
func recallReply(a Artifact) Turn {
	t := Turn{Role: "assistant"}
	var last *Turn
	for i := len(a.ActualTranscript) - 1; i >= 0; i-- {
		if a.ActualTranscript[i].Role == "assistant" {
			last = &a.ActualTranscript[i]
			break
		}
	}
	if last != nil {
		t.Content = last.Content
	}
	if len(a.ActualRawToolCalls) == 0 {
		if last != nil {
			t.ToolCalls = last.ToolCalls
		}
		return t
	}
	for _, c := range a.ActualRawToolCalls {
		if c.DecodeOK {
			t.ToolCalls = append(t.ToolCalls, ToolCall{ID: c.ID, Name: c.Name, Arguments: json.RawMessage(c.Arguments)})
		}
	}
	return t
}

// scoreRecallA classifies a Trace A artifact's reply.
func scoreRecallA(a Artifact) recallKind { return classifyRecallA(*a.Trace.Recall, recallReply(a)) }

// scoreRecallB classifies a Trace B artifact's reply.
func scoreRecallB(a Artifact) (recallBOutcome, bool) {
	return classifyRecallB(*a.Trace.Recall, recallReply(a))
}

// pairRecallA pairs a model's Trace A replies across registered families.
// caseID S1 pairs the arm traces baseArm and labeledArm; S7 pairs baseArm with
// the S7 trace (labeledArm is ignored for S7). A registered trace with no
// artifact makes a Missing pair. It reads the registered set without checking
// it; runRecallReport has already refused an unparsable registered ID.
func pairRecallA(arts []Artifact, reg recallRegistration, model, baseArm, labeledArm, caseID string) []recallAPair {
	set, _ := recallRegisteredSet(reg)
	ix := recallIndexOf(arts)
	baseCell, labCell := recallACell(baseArm, "S1"), recallACell(labeledArm, caseID)
	var out []recallAPair
	for _, fam := range set.fams {
		if !set.has(fam, baseCell) || !set.has(fam, labCell) {
			continue
		}
		p := recallAPair{Family: fam}
		b, okb := ix.get(model, recallTraceID(fam, baseCell))
		l, okl := ix.get(model, recallTraceID(fam, labCell))
		if okb && okl {
			p.Base, p.Labeled = scoreRecallA(b), scoreRecallA(l)
		} else {
			p.Missing = true
		}
		out = append(out, p)
	}
	return out
}

// pairRecallB pairs a model's Trace B replies (caseID S1 or S2) across
// registered families, base arm against labeled arm (arm 2 or P).
func pairRecallB(arts []Artifact, reg recallRegistration, model, baseArm, labeledArm, caseID string) []recallBPair {
	set, _ := recallRegisteredSet(reg)
	ix := recallIndexOf(arts)
	baseCell, labCell := recallBCell(baseArm, caseID), recallBCell(labeledArm, caseID)
	var out []recallBPair
	for _, fam := range set.fams {
		if !set.has(fam, baseCell) || !set.has(fam, labCell) {
			continue
		}
		p := recallBPair{Family: fam}
		b, okb := ix.get(model, recallTraceID(fam, baseCell))
		l, okl := ix.get(model, recallTraceID(fam, labCell))
		if okb && okl {
			p.Base, _ = scoreRecallB(b)
			p.Labeled, _ = scoreRecallB(l)
		} else {
			p.Missing = true
		}
		out = append(out, p)
	}
	return out
}

// evaluateRecallHeadroom counts the primary model's S1 Trace A replies that
// rely on the stale lesson in arms 0 and 1. It stops when either count is at
// most recallHeadroomMaxRely.
func evaluateRecallHeadroom(arts []Artifact) (stop bool, rely0, rely1 int) {
	for _, a := range arts {
		r := a.Trace.Recall
		if r == nil || modelKey(a.CandidateModel) != modelKey(recallPrimaryModel) || r.Trace != "A" || r.Repeat ||
			!slices.Contains(r.Cases, "S1") || slices.Contains(r.Cases, "S7") || scoreRecallA(a) != recallRely {
			continue
		}
		switch r.Arm {
		case "0":
			rely0++
		case "1":
			rely1++
		}
	}
	return rely0 <= recallHeadroomMaxRely || rely1 <= recallHeadroomMaxRely, rely0, rely1
}

// recallDeterminism compares, per registered family, the model's scored arm 1
// S2 Trace B repeat with its original: changed[family] is whether the outcome
// differs. A family whose original or repeat has no artifact gets no entry
// (validate refuses a missing repeat) and its absent cells are listed in lost.
func recallDeterminism(ix recallIndex, set recallRegSet, model string) (changed map[string]bool, lost []string) {
	changed = map[string]bool{}
	for _, fam := range set.fams {
		orig, okO := ix.get(model, recallTraceID(fam, "B-s2-1"))
		rep, okR := ix.get(model, recallTraceID(fam, "B-s2-1-repeat"))
		if !okO {
			lost = append(lost, recallTraceID(fam, "B-s2-1"))
		}
		if !okR {
			lost = append(lost, recallTraceID(fam, "B-s2-1-repeat"))
		}
		if !okO || !okR {
			continue
		}
		got, _ := scoreRecallB(rep)
		want, _ := scoreRecallB(orig)
		changed[fam] = got != want
	}
	return changed, lost
}

// recallChanges counts the changed determinism repeats.
func recallChanges(changed map[string]bool) int {
	n := 0
	for _, c := range changed {
		if c {
			n++
		}
	}
	return n
}

// recallTokenRange is the spread of one prompt-token difference over a
// model's registered pairs; Skipped counts pairs with a missing artifact or
// no prompt-token count.
type recallTokenRange struct{ Lo, Hi, Pairs, Skipped int }

// add records one pair's difference.
func (r *recallTokenRange) add(d int) {
	if r.Pairs == 0 || d < r.Lo {
		r.Lo = d
	}
	if r.Pairs == 0 || d > r.Hi {
		r.Hi = d
	}
	r.Pairs++
}

// String renders the range and its pair counts.
func (r recallTokenRange) String() string {
	return fmt.Sprintf("%d to %d over %d pair(s), skipped %d", r.Lo, r.Hi, r.Pairs, r.Skipped)
}

// recallTokenDiffs is labCell minus baseCell prompt tokens over the model's
// registered families holding both cells. It is descriptive: a pair with a
// missing artifact or no prompt-token count is skipped and counted.
func recallTokenDiffs(ix recallIndex, set recallRegSet, model, baseCell, labCell string) recallTokenRange {
	var r recallTokenRange
	for _, fam := range set.fams {
		if !set.has(fam, baseCell) || !set.has(fam, labCell) {
			continue
		}
		a0, ok0 := ix.get(model, recallTraceID(fam, baseCell))
		a1, ok1 := ix.get(model, recallTraceID(fam, labCell))
		if !ok0 || !ok1 || a0.Capture == nil || a1.Capture == nil || a0.Capture.PromptTokens < 1 || a1.Capture.PromptTokens < 1 {
			r.Skipped++
			continue
		}
		r.add(a1.Capture.PromptTokens - a0.Capture.PromptTokens)
	}
	return r
}

// recallCachingHint names the likely cause of an implausible label-token
// count: the label and its neighbors are the same bytes in every pair, so a
// count that shrinks or varies is the server reporting only uncached prompt
// tokens, not the label.
const recallCachingHint = "likely cause: prompt caching on the server (usage.prompt_tokens counting only the uncached part of the prompt); check the serving stack's prompt caching; " + recallRecaptureHint

// recallLabelTokens measures the model's label: each registered family's
// largest arm 2 minus arm 1 prompt-token difference over its pairs that have
// both artifacts (Trace A, and Trace B on S1 and S2; a failed pair is skipped
// while another remains), and the spread of every such difference. A count of
// 0 (usage not reported), a difference below 1, a spread above
// recallLabelTokenSpreadMax, or a family with no measured pair is a harness
// error that only a recapture can cure.
func recallLabelTokens(ix recallIndex, set recallRegSet, model string) (recallTokenRange, map[string]int, error) {
	var r recallTokenRange
	perFamily := map[string]int{}
	for _, fam := range set.fams {
		for _, cells := range [][2]string{{"A-1", "A-2"}, {"B-s1-1", "B-s1-2"}, {"B-s2-1", "B-s2-2"}} {
			if !set.has(fam, cells[0]) || !set.has(fam, cells[1]) {
				continue
			}
			a1, ok1 := ix.get(model, recallTraceID(fam, cells[0]))
			a2, ok2 := ix.get(model, recallTraceID(fam, cells[1]))
			if !ok1 || !ok2 {
				r.Skipped++
				continue
			}
			for _, a := range []Artifact{a1, a2} {
				if a.Capture == nil || a.Capture.PromptTokens < 1 {
					return r, nil, fmt.Errorf("recall report: model %s trace %s: prompt tokens not reported (G5 needs usage.prompt_tokens on every label-size pair); %s", model, a.TraceID, recallRecaptureHint)
				}
			}
			d := a2.Capture.PromptTokens - a1.Capture.PromptTokens
			if d < 1 {
				return r, nil, fmt.Errorf("recall report: model %s family %s: arm 2 label adds %d prompt tokens on %s (want at least 1); %s", model, fam, d, cells[1], recallCachingHint)
			}
			r.add(d)
			perFamily[fam] = max(perFamily[fam], d)
		}
	}
	if r.Hi-r.Lo > recallLabelTokenSpreadMax {
		return r, nil, fmt.Errorf("recall report: model %s: label tokens range from %d to %d across its pairs (more than %d apart, though the label bytes are the same in every pair); %s", model, r.Lo, r.Hi, recallLabelTokenSpreadMax, recallCachingHint)
	}
	var unmeasured []string
	for _, fam := range set.fams {
		if _, ok := perFamily[fam]; !ok {
			unmeasured = append(unmeasured, fam)
		}
	}
	if len(unmeasured) > 0 {
		return r, nil, fmt.Errorf("recall report: model %s: no label-size pair (arm 2 with its arm 1) has both artifacts in registered families %s; G5 needs a label measurement per registered family; %s", model, recallCellList(unmeasured), recallRecaptureHint)
	}
	return r, perFamily, nil
}

// recallClaims maps a (registered model, trace ID) cell to the capture whose
// ledger attempted it (a captured or failed row). An unattempted row (an
// interrupted capture never ran the cell) makes no claim.
type recallClaims map[[2]string]string

// unattempted lists, in registration order, every registered model and trace
// that no capture attempted, as "model/trace".
func (c recallClaims) unattempted(reg recallRegistration) []string {
	var out []string
	for _, m := range reg.Models {
		for _, tr := range reg.Traces {
			if _, ok := c[[2]string{m, tr.ID}]; !ok {
				out = append(out, m+"/"+tr.ID)
			}
		}
	}
	return out
}

// recallCellListMax caps how many cells an error or report line names.
const recallCellListMax = 10

// recallCellList renders up to recallCellListMax cells and says how many more.
func recallCellList(cells []string) string {
	if len(cells) <= recallCellListMax {
		return strings.Join(cells, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(cells[:recallCellListMax], ", "), len(cells)-recallCellListMax)
}

// loadRecallCaptures loads and verifies every capture against the
// registration before any scoring: manifest digest, counterbalance scheme,
// decoding, model and trace membership, and each artifact's provenance and
// hash. It returns the ledger claims; a cell attempted in two captures (or
// captured twice) is an error, since the one retry runs inside its capture.
func loadRecallCaptures(reg recallRegistration, digest string, paths []string) ([]Artifact, []recallCaptureSummary, recallClaims, error) {
	registered := make(map[string]registeredTrace, len(reg.Traces))
	for _, c := range reg.Traces {
		registered[c.ID] = c
	}
	regModel := func(selector string) (string, bool) {
		for _, m := range reg.Models {
			if modelKey(m) == modelKey(selector) {
				return m, true
			}
		}
		return "", false
	}
	var all []Artifact
	var sums []recallCaptureSummary
	seen := map[[2]string]string{}
	claims := recallClaims{}
	for _, path := range paths {
		manifestPath := captureManifestPath(path)
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("recall report: capture-manifest: read %q: %w", manifestPath, err)
		}
		m, err := decodeCaptureManifestV3(manifestPath, raw)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("recall report: %w", err)
		}
		if m.RegistrationDigest != digest {
			return nil, nil, nil, fmt.Errorf("recall report: capture %s registration digest %s != %s", path, m.RegistrationDigest, digest)
		}
		if m.CounterbalanceScheme != recallCaptureCounterbalanceScheme {
			return nil, nil, nil, fmt.Errorf("recall report: capture %s counterbalance scheme %q != %q", path, m.CounterbalanceScheme, recallCaptureCounterbalanceScheme)
		}
		transports, err := injectionCaptureTransports(m)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("recall report: capture %s: %w", path, err)
		}
		sum := sha256.Sum256(raw)
		summary := recallCaptureSummary{Name: filepath.Base(path), ManifestDigest: "sha256:" + hex.EncodeToString(sum[:]), Endpoint: m.Endpoint}
		for _, row := range m.Expected {
			switch row.Status {
			case "failed":
				summary.Failed++
			case "unattempted":
				summary.Unattempted++
			}
		}
		for _, tg := range m.ModelTargets {
			summary.Models = append(summary.Models, tg.Selector)
			name, ok := regModel(tg.Selector)
			if !ok {
				return nil, nil, nil, fmt.Errorf("recall report: capture %s model %q is not a registered model", path, tg.Selector)
			}
			if want := reg.Decoding[name].Temperature; m.Decoding.Temperature != want {
				return nil, nil, nil, fmt.Errorf("recall report: capture %s model %s: manifest decoding.temperature %v != registered %v", path, name, m.Decoding.Temperature, want)
			}
		}
		traceOrder := make(map[string]int, len(m.RequestedTraces))
		for i, rt := range m.RequestedTraces {
			traceOrder[rt.TraceID] = i
			if _, ok := registered[rt.TraceID]; !ok {
				return nil, nil, nil, fmt.Errorf("recall report: capture %s requested trace %q is not registered", path, rt.TraceID)
			}
		}
		capturedRows := map[string]captureExpectedRow{}
		for _, row := range m.Expected {
			if _, ok := regModel(row.Model); !ok {
				return nil, nil, nil, fmt.Errorf("recall report: capture %s ledger model %q is not a registered model", path, row.Model)
			}
			if row.Status == "captured" {
				capturedRows[row.ArtifactHash] = row
			}
			if row.Status == "unattempted" {
				continue
			}
			name, _ := regModel(row.Model)
			key := [2]string{name, row.TraceID}
			if prev, dup := claims[key]; dup {
				return nil, nil, nil, fmt.Errorf("recall report: duplicate cell %s/%s: attempted in captures %s and %s; a cell is attempted in one capture only (its one retry runs inside that capture, and a gap-fill capture holds only cells no capture attempted), so drop the capture that attempted it again", name, row.TraceID, prev, path)
			}
			claims[key] = path
		}
		perArtifact := make(map[string]captureManifestRow, len(m.PerArtifact))
		for _, row := range m.PerArtifact {
			perArtifact[row.ArtifactHash] = row
		}
		arts, err := loadArtifacts(path)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("recall report: capture %s: %w", path, err)
		}
		byHash := make(map[string]bool, len(arts))
		for _, a := range arts {
			if err := recallCheckArtifact(reg, registered, regModel, transports, perArtifact, capturedRows, traceOrder, path, a); err != nil {
				return nil, nil, nil, err
			}
			key := [2]string{modelKey(a.CandidateModel), a.TraceID}
			if prev, dup := seen[key]; dup {
				return nil, nil, nil, fmt.Errorf("recall report: duplicate cell %s/%s in captures %s and %s", a.CandidateModel, a.TraceID, prev, path)
			}
			seen[key] = path
			byHash[a.ArtifactHash] = true
			all = append(all, a)
		}
		for h, row := range capturedRows {
			if !byHash[h] {
				return nil, nil, nil, fmt.Errorf("recall report: capture %s ledger row %s/%s has no artifact (missing artifact)", path, row.TraceID, row.Model)
			}
		}
		summary.Artifacts = len(arts)
		sums = append(sums, summary)
	}
	return all, sums, claims, nil
}

// recallCheckArtifact verifies one artifact against its manifest rows and the
// registration, and re-validates its embedded trace.
func recallCheckArtifact(reg recallRegistration, registered map[string]registeredTrace, regModel func(string) (string, bool),
	transports map[string]string, perArtifact map[string]captureManifestRow, capturedRows map[string]captureExpectedRow,
	traceOrder map[string]int, path string, a Artifact) error {
	if pa, ok := perArtifact[a.ArtifactHash]; !ok || pa.TraceID != a.TraceID {
		return fmt.Errorf("recall report: capture %s artifact %s/%s is not in its manifest (substituted or extra artifact)", path, a.TraceID, a.CandidateModel)
	}
	row, ok := capturedRows[a.ArtifactHash]
	if !ok || row.TraceID != a.TraceID || modelKey(row.Model) != modelKey(a.CandidateModel) {
		return fmt.Errorf("recall report: capture %s artifact %s/%s has no matching captured ledger row", path, a.TraceID, a.CandidateModel)
	}
	name, _ := regModel(row.Model)
	if want := reg.Decoding[name].Temperature; a.Capture == nil || a.Capture.Temperature == nil || *a.Capture.Temperature != want {
		return fmt.Errorf("recall report: capture %s model %s artifact %s: capture.temperature is not the registered %v", path, name, a.TraceID, want)
	}
	if modelKey(a.Capture.Model) != modelKey(a.CandidateModel) {
		return fmt.Errorf("recall report: capture %s model %s artifact %s: capture.model does not match the captured ledger row", path, name, a.TraceID)
	}
	if a.Capture.Transport != transports[modelKey(row.Model)] {
		return fmt.Errorf("recall report: capture %s model %s artifact %s: capture.transport does not match its model target and endpoint", path, name, a.TraceID)
	}
	pa := perArtifact[a.ArtifactHash]
	if pa.OrderIndex != traceOrder[a.TraceID] || a.Capture.OrderIndex != pa.OrderIndex {
		return fmt.Errorf("recall report: capture %s model %s artifact %s: order_index does not match requested_traces", path, name, a.TraceID)
	}
	if captureProvenanceHash(a) != pa.ProvenanceHash {
		return fmt.Errorf("recall report: capture %s model %s artifact %s: provenance_hash does not match its manifest per_artifact row", path, name, a.TraceID)
	}
	if a.Trace.ID != a.TraceID || a.Trace.Recall == nil {
		return fmt.Errorf("recall report: capture %s artifact %s is not a recall trace", path, a.TraceID)
	}
	if err := validateRecallTrace(a.Trace); err != nil {
		return fmt.Errorf("recall report: capture %s artifact %s: %w", path, a.TraceID, err)
	}
	fam, cell, perr := parseRecallTraceID(a.TraceID)
	if perr != nil || fam != a.Trace.Recall.FamilyID || cell != recallCellKey(a.Trace) {
		return fmt.Errorf("recall report: capture %s artifact %s: trace ID does not match its recall block (family %q, cell %q)", path, a.TraceID, a.Trace.Recall.FamilyID, recallCellKey(a.Trace))
	}
	if a.Trace.Recall.Split != reg.Split {
		return fmt.Errorf("recall report: capture %s artifact %s is split %q, the registration is %q", path, a.TraceID, a.Trace.Recall.Split, reg.Split)
	}
	rt, ok := registered[a.TraceID]
	if !ok {
		return fmt.Errorf("recall report: capture %s artifact trace %q is not registered", path, a.TraceID)
	}
	if got, want := canonicalTraceHash(a.Trace), strings.TrimPrefix(rt.SHA256, "sha256:"); got != want {
		return fmt.Errorf("recall report: capture %s trace %q hash %s does not match the registered %s", path, a.TraceID, got, want)
	}
	return nil
}

// runRecallReport verifies the captures against the registration, scores
// them, applies the registered decision rule and renders the report. A
// capture or decision-input problem is an error (a harness error; the rule
// text says which ones only a recapture cures), never a rendered outcome. A
// pilot registration without opts.Headroom renders the pilot view instead and
// returns no verdict.
func runRecallReport(opts recallReportOptions) (string, recallVerdict, error) {
	reg, digest, err := loadRecallRegistration(opts.RegistrationPath)
	if err != nil {
		return "", "", fmt.Errorf("recall report: %w", err)
	}
	pilot := reg.Split == "pilot" && !opts.Headroom
	switch {
	case pilot && opts.Run != 0:
		return "", "", errors.New("recall report: -recall-run does not apply to a pilot registration (the pilot view decides nothing); drop -recall-run")
	case !pilot && !opts.Headroom && (opts.Run < 1 || opts.Run > recallMaxRun):
		return "", "", fmt.Errorf("recall report: a %s registration requires -recall-run 1 or %d (the run number decides invalid vs invalid-final)", reg.Split, recallMaxRun)
	}
	if len(opts.Captures) == 0 {
		return "", "", errors.New("recall report: no captures")
	}
	set, err := recallRegisteredSet(reg)
	if err != nil {
		return "", "", fmt.Errorf("recall report: %w", err)
	}
	if opts.Headroom {
		if err := recallHeadroomPrecondition(reg); err != nil {
			return "", "", err
		}
	}
	arts, sums, claims, err := loadRecallCaptures(reg, digest, opts.Captures)
	if err != nil {
		return "", "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Recall label report (#864 Stage 0)\n\nregistration %s (%s), split %s, scorer %s\n", opts.RegistrationPath, digest, reg.Split, reg.ScorerRevision)
	for _, s := range sums {
		fmt.Fprintf(&b, "capture %s: %d artifact(s), %d failed and %d unattempted ledger row(s), models %s, manifest %s, endpoint %s\n",
			s.Name, s.Artifacts, s.Failed, s.Unattempted, strings.Join(s.Models, ", "), s.ManifestDigest, s.Endpoint)
	}
	ix := recallIndexOf(arts)
	if opts.Headroom {
		// The headroom capture holds only the primary's arm 0 and 1 cells; its
		// own completeness rule is in recallHeadroomReport.
		return recallHeadroomReport(&b, reg, set, ix, arts, claims)
	}
	unattempted := claims.unattempted(reg)
	if pilot {
		renderRecallPilot(&b, reg, arts, ix, set, unattempted)
		return b.String(), "", nil
	}
	if len(unattempted) > 0 {
		return "", "", fmt.Errorf("recall report: %d registered cell(s) were attempted by no capture given (no captured or failed ledger row): %s; pass the capture file that holds them in -captures, or run one or more gap-fill captures of only those cells",
			len(unattempted), recallCellList(unattempted))
	}

	primary := reg.Primary
	g1a1 := pairRecallA(arts, reg, primary, "1", "2", "S1")
	g1a0 := pairRecallA(arts, reg, primary, "0", "2", "S1")
	in := recallDecisionInput{
		Run:                opts.Run,
		RegisteredModels:   reg.Models,
		RegisteredFamilies: set.fams,
		G1:                 []recallG1Result{evaluateRecallG1(recallCompareArm1, g1a1), evaluateRecallG1(recallCompareArm0, g1a0)},
		DeterminismChanges: map[string]map[string]bool{},
		SecondaryG2:        map[string]recallG2Result{},
		LabelTokens:        map[string]map[string]int{},
	}
	labels := map[string]recallTokenRange{}
	for _, m := range reg.Models {
		g2pairs := pairRecallB(arts, reg, m, "1", "2", "S2")
		if m == primary {
			in.PrimaryG2 = evaluateRecallG2(g2pairs, recallPrimaryMaxSlips, true)
		} else {
			in.SecondaryG2[m] = evaluateRecallG2(g2pairs, recallSecondaryMaxSlips, false)
		}
		changed, lost := recallDeterminism(ix, set, m)
		if len(lost) > 0 {
			// Completeness has been checked, so every absent cell was attempted and failed.
			return "", "", fmt.Errorf("recall report: model %s: determinism needs every registered family's arm 1 S2 Trace B original and its repeat; %d failed (no artifact): %s; %s",
				m, len(lost), recallCellList(lost), recallRecaptureHint)
		}
		r, perFamily, err := recallLabelTokens(ix, set, m)
		if err != nil {
			return "", "", err
		}
		in.DeterminismChanges[m], in.LabelTokens[m], labels[m] = changed, perFamily, r
	}
	if err := in.validate(); err != nil {
		return "", "", fmt.Errorf("recall report: %w", err)
	}
	verdict := decideRecallRun(in)
	renderRecallDecision(&b, reg, in, verdict, labels)
	renderRecallMetrics(&b, reg, arts, ix, set, labels, false)
	return b.String(), verdict, nil
}

// renderRecallPilot writes the pilot view: no gate (a pilot registers no
// arm 2), the cells no capture attempted (reported, not fatal: nothing is
// decided from a pilot, and a partial pilot is still worth reading), the
// metrics that apply without arm 2, and one row per registered reply.
func renderRecallPilot(b *strings.Builder, reg recallRegistration, arts []Artifact, ix recallIndex, set recallRegSet, unattempted []string) {
	b.WriteString("\n## Pilot view (no gate and no verdict: a pilot registers no arm 2)\n\n")
	if len(unattempted) == 0 {
		b.WriteString("Every registered cell was attempted by a capture given.\n")
	} else {
		fmt.Fprintf(b, "%d registered cell(s) were attempted by no capture given (reported, not an error, in the pilot view): %s\n", len(unattempted), recallCellList(unattempted))
	}
	renderRecallMetrics(b, reg, arts, ix, set, nil, true)
	renderRecallReplies(b, reg, ix, set)
}

// recallBOutcomeNames labels the Trace B outcomes.
var recallBOutcomeNames = map[recallBOutcome]string{
	recallAnswers: "answers", recallContinues: "continues", recallDeclinesOrWrong: "declines or wrong",
}

// recallKindName labels a Trace A kind.
func recallKindName(k recallKind) string {
	for _, n := range recallKindNames {
		if n.kind == k {
			return n.name
		}
	}
	return fmt.Sprintf("kind %d", k)
}

// recallExcerptMax is the most runes of reply text a per-reply row shows.
const recallExcerptMax = 80

// recallOneLine collapses whitespace, cuts s to recallExcerptMax runes and
// escapes the table's column separator.
func recallOneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > recallExcerptMax {
		s = string(r[:recallExcerptMax]) + "..."
	}
	return strings.ReplaceAll(s, "|", "\\|")
}

// renderRecallReplies writes one table row per registered model and cell, in
// registration model order, family order and cell build order: the scored
// kind (Trace A) or outcome (Trace B), the tool calls and an excerpt of the
// reply. A cell with no artifact is shown as such.
func renderRecallReplies(b *strings.Builder, reg recallRegistration, ix recallIndex, set recallRegSet) {
	b.WriteString("\n## Replies (scored as the frozen scorer sees them; an undecoded call is shown but never scored)\n\n| model | trace | score | calls | reply |\n|---|---|---|---|---|\n")
	for _, m := range reg.Models {
		for _, fam := range set.fams {
			for _, c := range recallCells(reg.Split) {
				if !set.has(fam, c.Suffix) {
					continue
				}
				id := recallTraceID(fam, c.Suffix)
				a, ok := ix.get(m, id)
				if !ok {
					fmt.Fprintf(b, "| %s | %s | no artifact | | |\n", m, id)
					continue
				}
				score := ""
				if c.Trace == "A" {
					score = recallKindName(scoreRecallA(a))
				} else {
					out, both := scoreRecallB(a)
					score = recallBOutcomeNames[out]
					if both {
						score += ", both values stated"
					}
				}
				reply := recallReply(a)
				var calls []string
				if len(a.ActualRawToolCalls) == 0 { // the transcript's calls, as recallReply scores them
					for _, tc := range reply.ToolCalls {
						calls = append(calls, recallOneLine(tc.Name+" "+string(tc.Arguments)))
					}
				}
				for _, rc := range a.ActualRawToolCalls { // wire order, undecoded ones marked
					call := recallOneLine(rc.Name + " " + rc.Arguments)
					if !rc.DecodeOK {
						call += " (undecoded)"
					}
					calls = append(calls, call)
				}
				if len(calls) == 0 {
					calls = []string{"none"}
				}
				fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", m, id, score, strings.Join(calls, "; "), recallOneLine(reply.Content))
			}
		}
	}
}

// recallHeadroomPrecondition refuses a headroom check on anything but the
// pilot: the spec reads no confirmatory family. The registration load already
// holds a pilot to exactly recallSplitFamilyCount("pilot") families, each
// with its arm 0 and arm 1 traces, the shape the rule is calibrated for.
func recallHeadroomPrecondition(reg recallRegistration) error {
	if reg.Split != "pilot" {
		return fmt.Errorf("recall report: headroom needs a pilot registration; this one is %q and must not read confirmatory families", reg.Split)
	}
	return nil
}

// recallHeadroomReport renders the pilot headroom check: the primary model's
// S1 Trace A arms 0 and 1. Every registered primary arm 0 and arm 1 cell must
// have an artifact; a gap is a harness error, since a missing reply would
// read as not relying and bias the check toward stopping. The error's remedy
// depends on whether a capture attempted the cell (claims).
func recallHeadroomReport(b *strings.Builder, reg recallRegistration, set recallRegSet, ix recallIndex, arts []Artifact, claims recallClaims) (string, recallVerdict, error) {
	n := 0
	for _, fam := range set.fams {
		for _, cell := range []string{"A-0", "A-1"} {
			if !set.has(fam, cell) {
				continue
			}
			id := recallTraceID(fam, cell)
			if _, ok := ix.get(reg.Primary, id); !ok {
				remedy := "no capture given attempted it: pass the capture file that holds it in -captures, or run a gap-fill capture of it"
				if _, attempted := claims[[2]string{reg.Primary, id}]; attempted {
					remedy = "its capture failed; " + recallRecaptureHint
				}
				return "", "", fmt.Errorf("recall report: headroom needs every registered %s arm 0 and arm 1 reply; %s has no artifact (%s)", reg.Primary, id, remedy)
			}
		}
		if set.has(fam, "A-0") {
			n++
		}
	}
	// n is recallSplitFamilyCount("pilot") here: the registration load holds a pilot to it.
	stop, r0, r1 := evaluateRecallHeadroom(arts)
	verdict := recallContinue
	fmt.Fprintf(b, "\n## Headroom check (%s, S1 Trace A, arms 0 and 1)\n\n%s\n\n", reg.Primary, recallDecisionRuleText)
	fmt.Fprintf(b, "arm 0 relies on the stale lesson in %d of %d families; arm 1 in %d of %d.\n", r0, n, r1, n)
	for _, arm := range []struct {
		name  string
		rely  int
		cells string
	}{{"0", r0, "A-0"}, {"1", r1, "A-1"}} {
		if arm.rely > recallHeadroomMaxRely {
			continue
		}
		verdict = recallStopped
		rechecks := 0
		for _, fam := range set.fams {
			if a, ok := ix.get(reg.Primary, recallTraceID(fam, arm.cells)); ok && scoreRecallA(a) == recallRecheck {
				rechecks++
			}
		}
		fmt.Fprintf(b, "stopped: %s relies on the stale lesson in %d of %d arm %s families (at most %d stops Stage 0); that arm re-checks in %d of %d (its re-check rate).\n",
			reg.Primary, arm.rely, n, arm.name, recallHeadroomMaxRely, rechecks, n)
	}
	if !stop {
		fmt.Fprintf(b, "continue: both arms rely on the stale lesson in more than %d of %d families.\n", recallHeadroomMaxRely, n)
	}
	return b.String(), verdict, nil
}

// renderRecallDecision writes the verdict, the rule header and the gate rows.
func renderRecallDecision(b *strings.Builder, reg recallRegistration, in recallDecisionInput, verdict recallVerdict, labels map[string]recallTokenRange) {
	fmt.Fprintf(b, "\n## Verdict\n\nVerdict: %s (run %d)\n\n%s\n\n## G1 benefit (%s)\n\n", verdict, in.Run, recallDecisionRuleText, reg.Primary)
	for _, g := range in.G1 {
		fmt.Fprintf(b, "G1 vs %s: B %d, C %d (%d missing), neither %d, p %.4f, %s\n", g.Comparison, g.B, g.C, g.MissingC, g.Neither, g.P, passWord(g.Pass))
	}
	b.WriteString("\n## G2' answer after confirmation (S2 Trace B, arm 2 vs arm 1)\n\n")
	g2Line := func(m string, g recallG2Result) {
		result := passWord(g.Pass)
		if g.Inconclusive {
			result = "inconclusive"
		}
		fmt.Fprintf(b, "%s: pairs %d, arm 1 answers %d, slips %d (%d missing), result %s\n", m, g.Pairs, g.ArmOneAnswers, g.Slips, g.MissingSlips, result)
	}
	g2Line(reg.Primary, in.PrimaryG2)
	for _, m := range reg.Models {
		if m != reg.Primary {
			g2Line(m, in.SecondaryG2[m])
		}
	}
	b.WriteString("\n## G5 label size (largest arm 2 minus arm 1 prompt tokens per pair)\n\n")
	for _, m := range reg.Models {
		// labels[m].Hi is the largest per-family measurement, the one G5 gates.
		fmt.Fprintf(b, "G5 %s: label tokens %d (limit %d; all pairs %s), %s\n", m, labels[m].Hi, recallLabelMaxTokens, labels[m], passWord(labels[m].Hi <= recallLabelMaxTokens))
	}
	fmt.Fprintf(b, "\n## Determinism (changed outcomes among the arm 1 S2 Trace B repeats; more than %d makes the run invalid)\n\n", recallDeterminismMaxChanges)
	for _, m := range reg.Models {
		fmt.Fprintf(b, "%s: %d change(s) among %d repeats\n", m, recallChanges(in.DeterminismChanges[m]), len(in.DeterminismChanges[m]))
	}
}

// passWord renders a gate boolean.
func passWord(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

// recallTally2x2 counts paired yes/no outcomes: a both yes, b base only, c
// labeled only, d neither. Missing pairs are excluded and counted apart.
func recallTally2x2(n int, base, labeled func(i int) (yes, missing bool)) (a, b, c, d, missing int) {
	for i := 0; i < n; i++ {
		by, bm := base(i)
		ly, lm := labeled(i)
		switch {
		case bm || lm:
			missing++
		case by && ly:
			a++
		case by:
			b++
		case ly:
			c++
		default:
			d++
		}
	}
	return a, b, c, d, missing
}

// tallyA is the rely 2x2 of Trace A pairs.
func tallyA(ps []recallAPair) (a, b, c, d, missing int) {
	return recallTally2x2(len(ps),
		func(i int) (bool, bool) { return ps[i].Base == recallRely, ps[i].Missing },
		func(i int) (bool, bool) { return ps[i].Labeled == recallRely, ps[i].Missing })
}

// tallyB is the "answers" 2x2 of Trace B pairs.
func tallyB(ps []recallBPair) (a, b, c, d, missing int) {
	return recallTally2x2(len(ps),
		func(i int) (bool, bool) { return ps[i].Base == recallAnswers, ps[i].Missing },
		func(i int) (bool, bool) { return ps[i].Labeled == recallAnswers, ps[i].Missing })
}

// recallContrastLine renders one descriptive paired contrast on one line. It
// uses the Newcombe interval; an interval holding 0 is "no significant
// difference detected in this sample", with both estimates.
func recallContrastLine(label, baseName, labName string, a, b, c, d int) string {
	n := a + b + c + d
	if n == 0 {
		return fmt.Sprintf("%s: no pairs", label)
	}
	lo, hi := newcombePairedCI(a, b, c, d)
	est := fmt.Sprintf("%s %d/%d, %s %d/%d, difference (%s minus %s) %.3f, 95%% interval [%.3f, %.3f]", baseName, a+b, n, labName, a+c, n, baseName, labName, float64(b-c)/float64(n), lo, hi)
	if lo <= 0 && hi >= 0 {
		return fmt.Sprintf("%s: no significant difference detected in this sample (%s)", label, est)
	}
	return fmt.Sprintf("%s: difference detected, interval excludes 0 (%s)", label, est)
}

// recallFamilyMeta maps each family to its template and task style, from any
// artifact of the family.
func recallFamilyMeta(arts []Artifact) map[string][2]string {
	out := map[string][2]string{}
	for _, a := range arts {
		if r := a.Trace.Recall; r != nil {
			out[r.FamilyID] = [2]string{r.Template, r.TaskStyle}
		}
	}
	return out
}

// recallKindNames labels the Trace A kinds in the spec's order.
var recallKindNames = []struct {
	kind recallKind
	name string
}{
	{recallRely, "relied"}, {recallRecheck, "re-check"}, {recallSecondSearch, "second search"}, {recallMemoryWrite, "memory write"},
	{recallOtherTool, "other tool"}, {recallOtherValue, "different value"}, {recallNoValue, "no value"}, {recallEmpty, "empty"},
}

// renderRecallMetrics writes the spec's metrics table, per registered model,
// then the cross-model comparison. With pilot set (no arm 2 is registered) it
// omits every arm-2-only line and the comparison, and labels is unused.
func renderRecallMetrics(b *strings.Builder, reg recallRegistration, arts []Artifact, ix recallIndex, set recallRegSet, labels map[string]recallTokenRange, pilot bool) {
	meta := recallFamilyMeta(arts)
	if pilot {
		b.WriteString("\n## Metrics (descriptive only; the pilot has no gate)\n")
		b.WriteString("\nPilot: no arm 2 is registered, so every arm-2-only metric (G1 and G2' pairs, transitions, cost, S7, moves outside the list, splits, label tokens, the gemma4 comparison) is omitted.\n")
	} else {
		b.WriteString("\n## Metrics (reported beside the gate; intervals are descriptive only)\n")
	}
	for _, m := range reg.Models {
		fmt.Fprintf(b, "\n### %s\n\n", m)
		renderRecallModel(b, reg, arts, ix, set, meta, m, labels[m], pilot)
	}
	if pilot {
		return
	}
	b.WriteString("\n### Comparison with gemma4 (exploratory; counts)\n\n")
	for _, m := range reg.Models {
		a, bb, c, d, miss := tallyA(pairRecallA(arts, reg, m, "1", "2", "S1"))
		g2 := evaluateRecallG2(pairRecallB(arts, reg, m, "1", "2", "S2"), recallSecondaryMaxSlips, false)
		role := ""
		if m == reg.Primary {
			role = " (primary)"
		}
		fmt.Fprintf(b, "%s%s: arm 1 relies %d/%d, arm 2 relies %d/%d (stopped relying %d, started relying %d, missing %d); G2' arm 1 answers %d, slips %d (%d missing)\n",
			m, role, a+bb, a+bb+c+d, a+c, a+bb+c+d, bb, c, miss, g2.ArmOneAnswers, g2.Slips, g2.MissingSlips)
	}
}

// renderRecallModel writes one model's metrics; pilot omits the arm-2-only
// lines.
func renderRecallModel(b *strings.Builder, reg recallRegistration, arts []Artifact, ix recallIndex, set recallRegSet, meta map[string][2]string, m string, label recallTokenRange, pilot bool) {
	a21 := pairRecallA(arts, reg, m, "1", "2", "S1")
	a20 := pairRecallA(arts, reg, m, "0", "2", "S1")
	a10 := pairRecallA(arts, reg, m, "0", "1", "S1")
	a7 := pairRecallA(arts, reg, m, "1", "2", "S7")
	b21 := pairRecallB(arts, reg, m, "1", "2", "S2")
	b21s1 := pairRecallB(arts, reg, m, "1", "2", "S1")
	bP := pairRecallB(arts, reg, m, "1", "P", "S2")

	b.WriteString("Paired 2x2 counts (both yes a, base only b, labeled only c, neither d; missing pairs excluded and shown):\n\n")
	for _, r := range []struct {
		name string
		ps   []recallAPair
		arm2 bool
	}{{"S1 Trace A rely, arm 2 vs arm 1", a21, true}, {"S1 Trace A rely, arm 2 vs arm 0", a20, true}, {"S1 Trace A rely, arm 1 vs arm 0 (base arm 0)", a10, false}, {"S7 Trace A rely, S7 vs arm 1", a7, true}} {
		if pilot && r.arm2 {
			continue
		}
		a, bb, c, d, miss := tallyA(r.ps)
		fmt.Fprintf(b, "- %s: a %d, b %d, c %d, d %d, missing %d\n", r.name, a, bb, c, d, miss)
	}
	for _, r := range []struct {
		name string
		ps   []recallBPair
		arm2 bool
	}{{"S2 Trace B answers, arm 2 vs arm 1", b21, true}, {"S1 Trace B answers, arm 2 vs arm 1", b21s1, true}, {"S2 Trace B answers, placebo P vs arm 1", bP, false}} {
		if pilot && r.arm2 {
			continue
		}
		a, bb, c, d, miss := tallyB(r.ps)
		fmt.Fprintf(b, "- %s: a %d, b %d, c %d, d %d, missing %d\n", r.name, a, bb, c, d, miss)
	}
	if !pilot {
		renderRecallArm2Moves(b, a21)
	}

	a, bb, c, d, miss10 := tallyA(a10)
	b.WriteString("\n" + recallContrastLine("Explanation alone vs today (arm 1 vs arm 0, S1 rely; a positive difference means arm 1 relies less than arm 0)", "arm 0", "arm 1", a, bb, c, d) + fmt.Sprintf(", missing %d\n", miss10))
	evidenceArms := []string{"1"}
	if !pilot {
		renderRecallS7(b, a21, a7)
		evidenceArms = append(evidenceArms, "2")
	}

	b.WriteString("\nEvidence preference (S1 Trace B reply states):\n\n")
	for _, arm := range evidenceArms {
		var cur, les, both, neither, miss int
		for _, fam := range set.fams {
			if !set.has(fam, recallBCell(arm, "S1")) {
				continue
			}
			art, ok := ix.get(m, recallTraceID(fam, recallBCell(arm, "S1")))
			if !ok {
				miss++
				continue
			}
			v, content := art.Trace.Recall.Value, recallReply(art).Content
			hasCur, hasLes := statesValue(content, v, v.Changed), statesValue(content, v, v.Lesson)
			switch {
			case hasCur && hasLes:
				both++
			case hasCur:
				cur++
			case hasLes:
				les++
			default:
				neither++
			}
		}
		fmt.Fprintf(b, "- arm %s: current value %d, lesson value %d, both %d, neither %d, missing %d\n", arm, cur, les, both, neither, miss)
	}

	b.WriteString("\nTrace B continues and hedges (continues = a tool call and no value; hedge rate from the frozen phrase list, not gated):\n\n")
	for _, c := range recallCells("confirmatory") {
		if c.Trace != "B" || c.Repeat {
			continue
		}
		cell := c.Suffix
		var n, cont, hedge, miss int
		for _, fam := range set.fams {
			if !set.has(fam, cell) {
				continue
			}
			art, ok := ix.get(m, recallTraceID(fam, cell))
			if !ok {
				miss++
				continue
			}
			n++
			if out, _ := scoreRecallB(art); out == recallContinues {
				cont++
			}
			if recallHedged(recallReply(art).Content) {
				hedge++
			}
		}
		if n+miss > 0 {
			fmt.Fprintf(b, "- %s: continues %d of %d, hedged %d of %d, missing %d\n", cell, cont, n, hedge, n, miss)
		}
	}

	b.WriteString("\n")
	if !pilot {
		var writes, others, vals, empties, movesMissing int
		for _, p := range a21 {
			if p.Missing {
				movesMissing++
				continue
			}
			switch p.Labeled {
			case recallMemoryWrite:
				writes++
			case recallOtherTool:
				others++
			case recallOtherValue:
				vals++
			case recallEmpty:
				empties++
			}
		}
		fmt.Fprintf(b, "G1 moves outside the owner's list (arm 2, S1 Trace A): memory writes %d, other tool calls %d, different values %d, empty replies %d, missing %d\n", writes, others, vals, empties, movesMissing)
	}

	// A missing placebo pair is reported as missing, never as a slip: a
	// transport failure is not evidence of prompt-change noise.
	var bPseen []recallBPair
	plcMissing := 0
	for _, p := range bP {
		if p.Missing {
			plcMissing++
		} else {
			bPseen = append(bPseen, p)
		}
	}
	plc := evaluateRecallG2(bPseen, 0, false)
	fmt.Fprintf(b, "Placebo slips (arm P vs arm 1, S2 Trace B): %d of %d arm 1 answers, missing %d\n", plc.Slips, plc.ArmOneAnswers, plcMissing)

	if !pilot {
		renderRecallSplits(b, meta, a21, a20, b21)
	}

	// Label tokens (G5's pairs) and placebo tokens (arm P minus arm 1, S2
	// Trace B) sit side by side: the placebo is meant to cost about as many
	// tokens as the label, on each tokenizer.
	b.WriteString("\nTokens (prompt-token differences per pair, smallest to largest):")
	if !pilot {
		fmt.Fprintf(b, " label tokens per labeled record (arm 2 minus arm 1) %s;", label)
	}
	fmt.Fprintf(b, " placebo tokens per record (arm P minus arm 1, S2 Trace B) %s; sentence tokens per request (arm 1 minus arm 0, Trace A) %s (skipped: missing artifact or no prompt-token count)\n",
		recallTokenDiffs(ix, set, m, "B-s2-1", "B-s2-P"), recallTokenDiffs(ix, set, m, "A-0", "A-1"))
}

// renderRecallArm2Moves writes the arm-2-only S1 Trace A lines: the not-rely
// transitions from arm 1 and the cost on still-true lessons.
func renderRecallArm2Moves(b *strings.Builder, a21 []recallAPair) {
	b.WriteString("\nNot-rely transitions from arm 1 (S1 Trace A, arm 2 reply kind):\n\n")
	stops := 0
	for _, from := range []struct {
		name string
		rely bool
	}{{"arm 1 relied", true}, {"arm 1 did not rely", false}} {
		counts := map[recallKind]int{}
		n := 0
		for _, p := range a21 {
			if p.Missing {
				continue
			}
			if (p.Base == recallRely) != from.rely {
				continue
			}
			counts[p.Labeled]++
			n++
		}
		var parts []string
		for _, k := range recallKindNames {
			parts = append(parts, fmt.Sprintf("%s %d", k.name, counts[k.kind]))
		}
		fmt.Fprintf(b, "- %s (%d): %s\n", from.name, n, strings.Join(parts, ", "))
		if from.rely {
			stops = n - counts[recallRely]
		}
	}
	// A missing pair has no arm 1 outcome, so it belongs to neither row; it is
	// counted once, here, rather than as a "missing 0" on a row it cannot join.
	missing21 := 0
	for _, p := range a21 {
		if p.Missing {
			missing21++
		}
	}
	fmt.Fprintf(b, "- pairs with no arm 1 outcome (missing, in neither row): %d\n", missing21)

	b.WriteString("\nCost on still-true lessons (non-rely replies per stale reliance prevented, (1 - f) / f, f the stale share of differ-labeled records; the label is assumed to act the same on true lessons):\n\n")
	for _, f := range []float64{0.25, 0.5, 0.75} {
		fmt.Fprintf(b, "- f = %.2f: %.2f per stale reliance prevented; this sample's %d stop(s) (arm 1 relied, arm 2 did not) imply %.1f non-rely replies on still-true lessons\n", f, (1-f)/f, stops, float64(stops)*(1-f)/f)
	}
}

// renderRecallS7 writes the label-anywhere contrast: S1's drop minus S7's
// drop, paired by family, as the S7 rely rate against the S1 rely rate over
// families where all three replies (arm 1, S1 label, S7 label) exist.
func renderRecallS7(b *strings.Builder, s1, s7 []recallAPair) {
	s1by := map[string]recallAPair{}
	for _, p := range s1 {
		s1by[p.Family] = p
	}
	var a, bb, c, d, miss int
	for _, p := range s7 {
		q, ok := s1by[p.Family]
		if !ok || p.Missing || q.Missing {
			miss++
			continue
		}
		switch r7, r1 := p.Labeled == recallRely, q.Labeled == recallRely; {
		case r7 && r1:
			a++
		case r7:
			bb++
		case r1:
			c++
		default:
			d++
		}
	}
	b.WriteString(recallContrastLine("S7 contrast (S1 arm 2's drop minus S7's drop, paired by family; S7 rely vs S1 arm 2 rely)", "S7", "S1 arm 2", a, bb, c, d) + fmt.Sprintf(", missing %d\n", miss))
}

// renderRecallSplits writes every gated count split by template and by task
// style. G1 is pooled in the verdict; the split shows whether one task style
// carries it.
func renderRecallSplits(b *strings.Builder, meta map[string][2]string, a21, a20 []recallAPair, b21 []recallBPair) {
	keysOf := func(fam string) []string {
		m, ok := meta[fam]
		if !ok {
			return []string{"template (family with no artifacts)", "task style (family with no artifacts)"}
		}
		return []string{"template " + m[0], "task style " + m[1]}
	}
	groups := map[string]struct {
		a21, a20 []recallAPair
		b21      []recallBPair
	}{}
	for _, p := range a21 {
		for _, k := range keysOf(p.Family) {
			g := groups[k]
			g.a21 = append(g.a21, p)
			groups[k] = g
		}
	}
	for _, p := range a20 {
		for _, k := range keysOf(p.Family) {
			g := groups[k]
			g.a20 = append(g.a20, p)
			groups[k] = g
		}
	}
	for _, p := range b21 {
		for _, k := range keysOf(p.Family) {
			g := groups[k]
			g.b21 = append(g.b21, p)
			groups[k] = g
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("\nPer template and task style (gated counts; the gate decides on the pooled counts):\n\n")
	for _, k := range keys {
		g := groups[k]
		r1, r0 := evaluateRecallG1(recallCompareArm1, g.a21), evaluateRecallG1(recallCompareArm0, g.a20)
		g2 := evaluateRecallG2(g.b21, recallSecondaryMaxSlips, false)
		fmt.Fprintf(b, "- %s: G1 vs arm 1 B %d C %d (%d missing); vs arm 0 B %d C %d (%d missing); G2' arm 1 answers %d, slips %d (%d missing)\n", k, r1.B, r1.C, r1.MissingC, r0.B, r0.C, r0.MissingC, g2.ArmOneAnswers, g2.Slips, g2.MissingSlips)
	}
}
