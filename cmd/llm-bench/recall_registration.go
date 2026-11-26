package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// recallRegistrationSchema is the recall-label pre-registration file schema
// (#864). Like the injection registration it is committed BEFORE any capture
// and capture binds to its digest.
const recallRegistrationSchema = "recall-label-registration/v1"

// recallDecisionSourceSHA256 identifies the scoring, ordering and decision
// code in recallScorerRevision. TestRecallScoringAndDecisionCodePinned
// recomputes it from source. Keep this constant outside the hashed declarations.
const recallDecisionSourceSHA256 = "0a9e9f4c286c8269dbd75f892d432f9b913699d5d96ad5d5532199ddb380ae12"

// recallFixtureRegenCommand regenerates the committed recall fixture's traces
// and registrations (TestRecallRegistrationsLoad); errors that a stale
// fixture can cause name it.
const recallFixtureRegenCommand = "go test ./cmd/llm-bench -run TestRecallRegistrationsLoad -update-recall-registration"

// recallRegistration is the loaded recall pre-registration. Unknown fields are rejected.
// CasesSHA256 is recallCasesDigest of the cases file for Split (the line
// "cases_sha256 ..." the corpus builder prints), stable when families of the other split are appended later.
type recallRegistration struct {
	Schema         string                        `json:"schema"`
	Split          string                        `json:"split"`
	CasesSHA256    string                        `json:"cases_sha256"`
	ScorerRevision string                        `json:"scorer_revision"`
	RuleText       string                        `json:"rule_text"`
	Endpoint       string                        `json:"endpoint,omitempty"` // omitted in v1 means the Stage 0 llama-swap endpoint
	Primary        string                        `json:"primary"`
	Models         []string                      `json:"models"`
	Decoding       map[string]registeredDecoding `json:"decoding"`
	Traces         []registeredTrace             `json:"traces"`
}

// recallStageModels is the Stage 0 lineup for both registered splits.
var recallStageModels = [recallModelCount]string{
	recallPrimaryModel, "openai-compat/qwen3-coder-next:latest",
	"openai-compat/qwen3.6:35b-a3b", "openai-compat/qwen3.8:27b",
}

func recallSplitFamilyCount(split string) int {
	if split == "pilot" {
		return 12
	}
	return recallFamilyCount
}

// recallNumPredict is the registered num_predict for every recall model.
const recallNumPredict = 4096

// recallDigestPattern is the cases_sha256 shape.
var recallDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// loadRecallRegistration reads and validates a recall registration and returns
// it with the "sha256:"-prefixed digest of the file bytes.
func loadRecallRegistration(path string) (recallRegistration, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return recallRegistration{}, "", fmt.Errorf("recall registration: read %q: %w", path, err)
	}
	var r recallRegistration
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return recallRegistration{}, "", fmt.Errorf("recall registration: decode %q: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return recallRegistration{}, "", fmt.Errorf("recall registration: %q has trailing data", path)
	}
	if err := r.validate(); err != nil {
		return recallRegistration{}, "", fmt.Errorf("recall registration: %q: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	return r, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// validate checks the registration against the compiled rule text, scorer
// revision and primary model, and every model's decoding.
func (r recallRegistration) validate() error {
	if r.Schema != recallRegistrationSchema {
		return fmt.Errorf("schema %q (want %q)", r.Schema, recallRegistrationSchema)
	}
	if r.Split != "pilot" && r.Split != "confirmatory" {
		return fmt.Errorf("split %q (want pilot or confirmatory)", r.Split)
	}
	if !recallDigestPattern.MatchString(r.CasesSHA256) {
		return fmt.Errorf("cases_sha256 %q (want sha256: plus 64 lowercase hex digits)", r.CasesSHA256)
	}
	if r.RuleText != recallDecisionRuleText {
		return errors.New("rule_text does not equal the compiled decision rule")
	}
	if r.ScorerRevision != recallScorerRevision {
		return fmt.Errorf("scorer_revision %q (want %q)", r.ScorerRevision, recallScorerRevision)
	}
	if r.Primary != recallPrimaryModel {
		return fmt.Errorf("primary %q (want %q)", r.Primary, recallPrimaryModel)
	}
	if !slices.Contains(r.Models, r.Primary) {
		return fmt.Errorf("primary %q is not in models", r.Primary)
	}
	seenModel := make(map[string]struct{}, len(r.Models))
	for _, m := range r.Models {
		if _, dup := seenModel[m]; dup {
			return fmt.Errorf("model %q is listed twice", m)
		}
		seenModel[m] = struct{}{}
	}
	for m := range r.Decoding {
		if _, ok := seenModel[m]; !ok {
			return fmt.Errorf("decoding for %q, which is not in models", m)
		}
	}
	for _, m := range r.Models {
		d, ok := r.Decoding[m]
		if !ok {
			return fmt.Errorf("model %q has no decoding", m)
		}
		if err := d.validate(m); err != nil {
			return err
		}
		// The spec's regime (#864): local models only, temperature 0, thinking
		// off, num_predict 4096. Conclusions are limited to it. (The local
		// temperature 0 is held by registeredDecoding.validate above.)
		switch {
		case d.hosted():
			return fmt.Errorf("model %q: hosted serving stacks are out of scope for the recall registration", m)
		case d.Think == nil || *d.Think:
			return fmt.Errorf("model %q: think must be false", m)
		case d.NumPredict != recallNumPredict:
			return fmt.Errorf("model %q: num_predict must be %d; got %d", m, recallNumPredict, d.NumPredict)
		}
	}
	if len(r.Models) != len(recallStageModels) {
		return fmt.Errorf("models must contain the %d Stage 0 models", len(recallStageModels))
	}
	for _, model := range recallStageModels {
		if _, ok := seenModel[model]; !ok {
			return fmt.Errorf("models is missing Stage 0 model %q", model)
		}
	}
	dest, err := registeredRecallDestination(r.Endpoint)
	if err != nil {
		return err
	}
	for model, d := range r.Decoding {
		const legacyStack = "llama.cpp via llama-swap " + recallDefaultHost
		if r.Endpoint == "" && d.ServingStack != legacyStack {
			return fmt.Errorf("model %q: an explicit loopback endpoint is required for this serving_stack", model)
		}
		if d.ServingStack == legacyStack && dest.BaseURL() != recallDefaultEndpoint {
			return fmt.Errorf("model %q: endpoint disagrees with serving_stack", model)
		}
	}
	return r.validateTraceInventory()
}

func (r recallRegistration) validateTraceInventory() error {
	if len(r.Traces) == 0 {
		return errors.New("traces is empty")
	}
	cells := recallCells(r.Split)
	allowed := make(map[string]bool, len(cells))
	for _, c := range cells {
		allowed[c.Suffix] = true
	}
	counts := map[string]int{}
	seen := make(map[string]struct{}, len(r.Traces))
	for i, c := range r.Traces {
		if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.SHA256) == "" {
			return fmt.Errorf("trace row %d: id and sha256 are required", i)
		}
		if c.Population != "" {
			return fmt.Errorf("trace row %d (%s): population is not part of the recall registration", i, c.ID)
		}
		if _, dup := seen[c.ID]; dup {
			return fmt.Errorf("trace row %d: duplicate id %q", i, c.ID)
		}
		seen[c.ID] = struct{}{}
		if !recallDigestPattern.MatchString("sha256:" + strings.TrimPrefix(c.SHA256, "sha256:")) {
			return fmt.Errorf("trace %q: invalid sha256", c.ID)
		}
		family, cell, err := parseRecallTraceID(c.ID)
		if err != nil || !injectionNamePattern.MatchString(family) || !allowed[cell] {
			return fmt.Errorf("trace %q is not a %s family/cell ID", c.ID, r.Split)
		}
		counts[family]++
	}
	for family, n := range counts {
		if n != len(cells) {
			return fmt.Errorf("family %q has %d registered cells, want %d", family, n, len(cells))
		}
	}
	if n := recallSplitFamilyCount(r.Split); len(counts) != n {
		return fmt.Errorf("registration has %d %s families, want %d", len(counts), r.Split, n)
	}
	return nil
}

// bindCapture fails closed unless every selected trace is a recall trace of the
// registered split, registered with its canonicalTraceHash, and every target
// has a registered decoding. Like the injection bindCapture it accepts a subset
// of the registered traces (a gap-fill capture) and a subset of the models.
// When paths are supplied, a .json filename naming a registered cell must contain
// that cell. Exact names take precedence over case variants. Other filenames
// are aliases. It returns the Runner.InjectionDecoding map keyed by selector.
func (r recallRegistration) bindCapture(traces []Trace, targets []ModelTarget, paths ...string) (map[string]injectionDecoding, error) {
	if len(paths) != 0 && len(paths) != len(traces) {
		return nil, fmt.Errorf("recall registration: got %d input paths for %d traces", len(paths), len(traces))
	}
	reg := make(map[string]string, len(r.Traces))
	foldedNames := make(map[string][]string, len(r.Traces))
	for _, c := range r.Traces {
		reg[c.ID] = strings.TrimPrefix(c.SHA256, "sha256:")
		folded := strings.ToLower(c.ID + ".json")
		foldedNames[folded] = append(foldedNames[folded], c.ID)
	}
	for i, t := range traces {
		if len(paths) > 0 {
			base := filepath.Base(paths[i])
			named, jsonFile := strings.CutSuffix(base, ".json")
			if _, registered := reg[named]; !jsonFile || !registered {
				matches := foldedNames[strings.ToLower(base)]
				switch len(matches) {
				case 0:
					named = ""
				case 1:
					named = matches[0]
				default:
					return nil, fmt.Errorf("recall registration: filename %q matches multiple registered traces; use the exact registered filename", paths[i])
				}
			}
			if named != "" && named != t.ID {
				return nil, fmt.Errorf("recall registration: filename %q selects trace %q but contains %q", paths[i], named, t.ID)
			}
		}
		if t.Recall == nil {
			return nil, fmt.Errorf("recall registration: trace %q is not a recall trace", t.ID)
		}
		if t.Recall.Split != r.Split {
			return nil, fmt.Errorf("recall registration: trace %q is split %q, the registration is %q", t.ID, t.Recall.Split, r.Split)
		}
		want, ok := reg[t.ID]
		if !ok {
			return nil, fmt.Errorf("recall registration: trace %q is not registered", t.ID)
		}
		family, cell, err := parseRecallTraceID(t.ID)
		if err != nil || family != t.Recall.FamilyID || cell != recallCellKey(t) {
			return nil, fmt.Errorf("recall registration: trace %q disagrees with its family or cell", t.ID)
		}
		if got := canonicalTraceHash(t); got != want {
			return nil, fmt.Errorf("recall registration: trace %q hash %s does not match the registered %s: the registration and the corpus disagree; for the committed fixture (cmd/llm-bench/testdata/recall-label) regenerate both with %s before the freeze only (after it, revert the change), otherwise rebuild the corpus with -build-recall-corpus and register it again",
				t.ID, got, want, recallFixtureRegenCommand)
		}
	}
	out := make(map[string]injectionDecoding, len(targets))
	for _, tg := range targets {
		if !slices.Contains(r.Models, tg.Display) {
			return nil, fmt.Errorf("recall registration: model %q is not registered", tg.Display)
		}
		if tg.Provider != openAICompatTransport || tg.Display != tg.Provider+"/"+tg.Model {
			return nil, fmt.Errorf("recall registration: model %q disagrees with its transport or model ID", tg.Display)
		}
		// validate guarantees every registered model has a decoding.
		dec, err := r.Decoding[tg.Display].decoding()
		if err != nil {
			return nil, fmt.Errorf("recall registration: model %q: %w", tg.Display, err)
		}
		out[tg.Display] = dec
	}
	return out, nil
}

// recallBaseOrder is the within-family capture order of non-repeat cells: the
// non-repeat cells of recallCells in their build order.
var recallBaseOrder = func() []string {
	var order []string
	for _, c := range recallCells("confirmatory") {
		if !c.Repeat {
			order = append(order, c.Suffix)
		}
	}
	return order
}()

// recallCellKey names t's cell: the recallCells suffix it was built under,
// which parseRecallTraceID returns from its ID.
func recallCellKey(t Trace) string {
	r := t.Recall
	var key string
	switch {
	case r.Trace == "A" && slices.Equal(r.Cases, []string{"S7"}):
		key = "A-2-s7"
	case r.Trace == "A":
		key = "A-" + r.Arm
	case slices.Equal(r.Cases, []string{"S2"}):
		key = "B-s2-" + r.Arm
	default:
		key = "B-s1-" + r.Arm
	}
	if r.Repeat {
		key += "-repeat"
	}
	return key
}

// recallCaptureCounterbalanceScheme names the recall capture order for the v3
// manifest. Single source: recallCaptureOrder implements exactly this
// sentence (runCalibrateCapture refuses any non-recall trace in a recall set).
const recallCaptureCounterbalanceScheme = "families sorted by fnv1a64(family_id) ascending; each family's non-repeat cells in the base order A-0, A-1, A-2, A-2-s7, B-s1-1, B-s1-2, B-s2-1, B-s2-2, B-s2-P rotated left by the family position modulo the cells present; repeat cells last in family order"

// recallCaptureOrder is the registered recall capture order (see
// recallCaptureCounterbalanceScheme): families by pairIDHash(FamilyID)
// ascending, each family's non-repeat cells in recallBaseOrder rotated left by
// the family's position modulo the cells present, then every repeat cell in
// family order. Every trace must carry a recall block (runCalibrateCapture
// refuses a set that mixes in any other trace).
func recallCaptureOrder(traces []Trace) []Trace {
	trials := make([]recallTrial, len(traces))
	for i, t := range traces {
		trials[i] = recallTrial{family: t.Recall.FamilyID, cell: recallCellKey(t)}
	}
	out := make([]Trace, len(traces))
	for i, source := range recallTrialOrder(trials) {
		out[i] = traces[source]
	}
	return out
}

type recallTrial struct {
	family, cell string
}

// recallTrialOrder returns the indices of the selected trials in registered
// order. Capture takes the identities from the traces; report independently
// derives them from the registration, including trials with no artifact.
func recallTrialOrder(trials []recallTrial) []int {
	byFamily := make(map[string][]int)
	var fams []string
	for i, trial := range trials {
		if _, ok := byFamily[trial.family]; !ok {
			fams = append(fams, trial.family)
		}
		byFamily[trial.family] = append(byFamily[trial.family], i)
	}
	sort.Slice(fams, func(i, j int) bool {
		hi, hj := pairIDHash(fams[i]), pairIDHash(fams[j])
		if hi != hj {
			return hi < hj
		}
		return fams[i] < fams[j]
	})
	out := make([]int, 0, len(trials))
	var repeats []int
	for pos, id := range fams {
		var cells []int
		for _, i := range byFamily[id] {
			if strings.HasSuffix(trials[i].cell, "-repeat") {
				repeats = append(repeats, i)
			} else {
				cells = append(cells, i)
			}
		}
		sort.SliceStable(cells, func(i, j int) bool {
			return slices.Index(recallBaseOrder, trials[cells[i]].cell) < slices.Index(recallBaseOrder, trials[cells[j]].cell)
		})
		if n := len(cells); n > 0 {
			k := pos % n
			out = append(out, cells[k:]...)
			out = append(out, cells[:k]...)
		}
	}
	return append(out, repeats...)
}
