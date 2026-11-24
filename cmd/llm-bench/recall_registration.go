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
	"regexp"
	"slices"
	"sort"
	"strings"
)

// recallRegistrationSchema is the recall-label pre-registration file schema
// (#864). Like the injection registration it is committed BEFORE any capture
// and capture binds to its digest.
const recallRegistrationSchema = "recall-label-registration/v1"

// recallRegistration is the loaded recall pre-registration. Every field is
// required and unknown fields are rejected. CasesSHA256 is recallCasesDigest of
// the cases file for Split (the line "cases_sha256 ..." the corpus builder
// prints), stable when families of the other split are appended later.
type recallRegistration struct {
	Schema         string                        `json:"schema"`
	Split          string                        `json:"split"`
	CasesSHA256    string                        `json:"cases_sha256"`
	ScorerRevision string                        `json:"scorer_revision"`
	RuleText       string                        `json:"rule_text"`
	Primary        string                        `json:"primary"`
	Models         []string                      `json:"models"`
	Decoding       map[string]registeredDecoding `json:"decoding"`
	Traces         []registeredTrace             `json:"traces"`
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
	if len(r.Traces) == 0 {
		return errors.New("traces is empty")
	}
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
	}
	return nil
}

// bindCapture fails closed unless every selected trace is a recall trace of the
// registered split, registered with its canonicalTraceHash, and every target
// has a registered decoding. Like the injection bindCapture it accepts a subset
// of the registered traces (a gap-fill capture) and a subset of the models. It returns
// the Runner.InjectionDecoding map keyed by selector.
func (r recallRegistration) bindCapture(traces []Trace, targets []ModelTarget) (map[string]injectionDecoding, error) {
	reg := make(map[string]string, len(r.Traces))
	for _, c := range r.Traces {
		reg[c.ID] = strings.TrimPrefix(c.SHA256, "sha256:")
	}
	for _, t := range traces {
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
		if got := canonicalTraceHash(t); got != want {
			return nil, fmt.Errorf("recall registration: trace %q hash %s does not match the registered %s", t.ID, got, want)
		}
	}
	out := make(map[string]injectionDecoding, len(targets))
	for _, tg := range targets {
		if !slices.Contains(r.Models, tg.Display) {
			return nil, fmt.Errorf("recall registration: model %q is not registered", tg.Display)
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
	var out []Trace
	byFamily := map[string][]Trace{}
	var fams []string
	for _, t := range traces {
		id := t.Recall.FamilyID
		if _, ok := byFamily[id]; !ok {
			fams = append(fams, id)
		}
		byFamily[id] = append(byFamily[id], t)
	}
	sort.Slice(fams, func(i, j int) bool {
		hi, hj := pairIDHash(fams[i]), pairIDHash(fams[j])
		if hi != hj {
			return hi < hj
		}
		return fams[i] < fams[j]
	})
	var repeats []Trace
	for pos, id := range fams {
		var cells []Trace
		for _, t := range byFamily[id] {
			if t.Recall.Repeat {
				repeats = append(repeats, t)
			} else {
				cells = append(cells, t)
			}
		}
		sort.SliceStable(cells, func(i, j int) bool {
			return slices.Index(recallBaseOrder, recallCellKey(cells[i])) < slices.Index(recallBaseOrder, recallCellKey(cells[j]))
		})
		if n := len(cells); n > 0 {
			k := pos % n
			cells = append(cells[k:len(cells):len(cells)], cells[:k]...)
		}
		out = append(out, cells...)
	}
	return append(out, repeats...)
}
