package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// interceptorRegistrationSchema is the #517 benign pre-registration schema
// (spec §6.7, BC6): its own strict schema, committed before any capture. Each
// run records the registration digest. #452's registration stays untouched.
const interceptorRegistrationSchema = "llm-bench.interceptor-registration/v1"

// interceptorCensusRel is the census directory relative to the registration's
// directory, in slash form. The loader reads cases.json and traces/ beside the
// registration and the census here, so it works from any working directory:
// the repository holds the registration in docs/llm/traces/interceptor-benign/
// and the census in docs/llm/census/, and a copy works wherever it keeps that
// docs/llm layout.
const interceptorCensusRel = "../../census"

// interceptorPrimaryEndpoint is the primary contrast's endpoint (spec §6.8),
// on the extraction stratum.
const interceptorPrimaryEndpoint = "answer_quality"

// interceptorEndpointStrata maps each registered endpoint to the stratum its
// contrasts run on (spec §6.8): AnswerQuality and restraint on extraction,
// whose goldens call no tool, and delegated-instruction correctness on
// delegated.
var interceptorEndpointStrata = map[string]string{
	"answer_quality": benignExtraction, "restraint": benignExtraction, "delegated_correctness": benignDelegated,
}

// interceptorK1Budgets are K1's budgets (spec §2.1), the only ones a
// registration may record.
var interceptorK1Budgets = registeredBudgets{Delta: 0.10, TaskFailure: 0, MCPBlockPerStratum: 0.01}

// interceptorRegistration is the loaded #517 registration. Every field is
// required and unknown fields are rejected. Hosted fields travel inside
// Decoding[model].Hosted in the #452 v1 shape; the hosted model is descriptive
// only.
type interceptorRegistration struct {
	Schema      string `json:"schema"`
	CasesSHA256 string `json:"cases_sha256"`
	// BuilderRevision is a source digest, because a branch commit does not
	// survive the squash merge: the sha256 of the sha256sum listing
	// ("<hex>  <name>\n", two spaces) of cmd/llm-bench/injection_build.go,
	// benign.go, arms.go and interceptor_v1.go in that order, as
	// `cd cmd/llm-bench && shasum -a 256 injection_build.go benign.go arms.go interceptor_v1.go | shasum -a 256`
	// prints it. The loader checks its form only; TestRegistrationRegen holds
	// it to the current sources (Ruling P30).
	BuilderRevision string `json:"builder_revision"`
	// ScorerRevision is "deferred (K5=b)": no benign scorer exists until the
	// flip PR.
	ScorerRevision string `json:"scorer_revision"`
	// Traces registers every committed trace once; Population is its benign
	// stratum, which fixes its expected identity-gate cell (extraction and
	// delegated: a trailer under G+D; nonfiring: identical).
	Traces   []registeredTrace             `json:"traces"`
	Chains   []registeredChain             `json:"chains"`
	Census   registeredCensus              `json:"census"`
	Budgets  registeredBudgets             `json:"budgets"`
	Power    registeredPower               `json:"power"`
	Decoding map[string]registeredDecoding `json:"decoding"`
	Estimand string                        `json:"estimand"`
	// DelegatedMatch is the delegated-instruction matching rule.
	DelegatedMatch string   `json:"delegated_match"`
	RetryPolicy    string   `json:"retry_policy"`
	ClaimGates     []string `json:"claim_gates"`
	CaptureTiming  string   `json:"capture_timing"`
}

// registeredChain is one arm: it must equal interceptorArms() name for name
// and member for member.
type registeredChain struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

// registeredCensus binds the census: the manifest and census-results.json
// bytes, and the full output's digest that census-results.json records.
type registeredCensus struct {
	ManifestSHA256 string `json:"manifest_sha256"`
	ResultsSHA256  string `json:"results_sha256"`
	FullSHA256     string `json:"full_sha256"`
}

// registeredBudgets are K1's budgets: δ on the primary contrast, the
// task-failure budget, and the unnecessary-block budget on benign MCP
// observations per corpus stratum.
type registeredBudgets struct {
	Delta              float64 `json:"delta"`
	TaskFailure        int     `json:"task_failure"`
	MCPBlockPerStratum float64 `json:"mcp_block_per_stratum"`
}

// registeredPower records how informative the rule is (spec §6.7, §6.8):
// under a true Δ of 0, the primary contrast passes with probability
// H0PassPrimary = h0PassProbability(Clusters, AssumedQ, δ), Clusters the
// extraction task count; the AnswerQuality contrasts of all local models pass
// together with H0PassAllLocal = H0PassPrimary^(the decoding map's non-hosted
// models); each secondary contrast passes with its own H0Pass; and all of them
// together with H0PassAllContrasts, the product over the primary and
// Secondary.
type registeredPower struct {
	AssumedQ        float64 `json:"assumed_q"`
	Clusters        int     `json:"clusters"`
	PrimaryModel    string  `json:"primary_model"`
	PrimaryEndpoint string  `json:"primary_endpoint"`
	H0PassPrimary   float64 `json:"h0_pass_primary"`
	H0PassAllLocal  float64 `json:"h0_pass_all_local"`
	// Secondary is every local model on every endpoint except the primary
	// contrast.
	Secondary          []registeredContrast `json:"secondary"`
	H0PassAllContrasts float64              `json:"h0_pass_all_contrasts"`
}

// registeredContrast is one secondary contrast: a local model, an endpoint,
// the endpoint's stratum and its task count, and h0PassProbability over them.
type registeredContrast struct {
	Model    string  `json:"model"`
	Endpoint string  `json:"endpoint"`
	Stratum  string  `json:"stratum"`
	Clusters int     `json:"clusters"`
	H0Pass   float64 `json:"h0_pass"`
}

// h0PassProbability is the probability that one non-inferiority contrast over
// n tasks passes (lower 95% bound of the paired Δ at least -δ) when the true Δ
// is 0, by the normal approximation: Φ((δ − 1.96·√(q/n)) / √(q/n)), q the
// assumed discordant share (a task's paired-difference variance at Δ = 0).
func h0PassProbability(n int, q, delta float64) float64 {
	se := math.Sqrt(q / float64(n))
	z := (delta - 1.96*se) / se
	return 0.5 * (1 + math.Erf(z/math.Sqrt2))
}

// zeroLossBound is the exact one-sided 97.5% upper bound on a loss rate when
// none of n tasks shows a loss: 1 − 0.025^(1/n) (spec §6.8, F14).
func zeroLossBound(n int) float64 {
	return 1 - math.Pow(0.025, 1/float64(n))
}

// loadInterceptorRegistration reads and validates a registration file, and
// the files it binds (interceptorCensusRel), and returns it with the
// "sha256:"-prefixed digest of the file bytes. Every mismatch names its field.
func loadInterceptorRegistration(path string) (interceptorRegistration, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return interceptorRegistration{}, "", fmt.Errorf("registration: read %q: %w", path, err)
	}
	var r interceptorRegistration
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return interceptorRegistration{}, "", fmt.Errorf("registration: decode %q: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return interceptorRegistration{}, "", fmt.Errorf("registration: %q has trailing data", path)
	}
	if err := r.validate(filepath.Dir(path)); err != nil {
		return interceptorRegistration{}, "", fmt.Errorf("registration: %q: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	return r, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// isSHA256Hex reports whether s is a lowercase hex sha256 digest.
func isSHA256Hex(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// checkDigest reports, under field, a read error or a mismatch between want
// and the hex sha256 of the bytes at path.
func checkDigest(field, path, want string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("%s %s does not match sha256(%s) %x", field, want, filepath.Base(path), sum)
	}
	return nil
}

// validate checks r's own fields first, then the files beside dir.
func (r interceptorRegistration) validate(dir string) error {
	if r.Schema != interceptorRegistrationSchema {
		return fmt.Errorf("schema %q (want %q)", r.Schema, interceptorRegistrationSchema)
	}
	for _, f := range []struct{ name, v string }{
		{"cases_sha256", r.CasesSHA256}, {"scorer_revision", r.ScorerRevision}, {"estimand", r.Estimand},
		{"delegated_match", r.DelegatedMatch}, {"retry_policy", r.RetryPolicy}, {"capture_timing", r.CaptureTiming},
	} {
		if strings.TrimSpace(f.v) == "" {
			return fmt.Errorf("%s is required", f.name)
		}
	}
	if !isSHA256Hex(r.BuilderRevision) {
		return fmt.Errorf("builder_revision %q is not a lowercase hex sha256 source digest", r.BuilderRevision)
	}
	if !isSHA256Hex(r.Census.FullSHA256) {
		return fmt.Errorf("census.full_sha256 %q is not a lowercase hex sha256 digest", r.Census.FullSHA256)
	}
	if len(r.ClaimGates) == 0 || slices.ContainsFunc(r.ClaimGates, func(g string) bool { return strings.TrimSpace(g) == "" }) {
		return errors.New("claim_gates is required and holds no blank gate")
	}
	if r.Budgets != interceptorK1Budgets {
		return fmt.Errorf("budgets %+v are not K1's %+v", r.Budgets, interceptorK1Budgets)
	}
	arms := interceptorArms()
	if len(r.Chains) != len(arms) {
		return fmt.Errorf("chains has %d entries, want the %d of interceptorArms()", len(r.Chains), len(arms))
	}
	for i, a := range arms {
		if c := r.Chains[i]; c.Name != a.Name || !slices.Equal(c.Members, a.Members) {
			return fmt.Errorf("chains[%d] is %q %v, want interceptorArms() %q %v", i, c.Name, c.Members, a.Name, a.Members)
		}
	}
	if len(r.Decoding) == 0 {
		return errors.New("decoding is empty")
	}
	if len(r.Traces) == 0 {
		return errors.New("traces is empty")
	}
	var locals []string
	for _, m := range slices.Sorted(maps.Keys(r.Decoding)) {
		d := r.Decoding[m]
		if err := d.validate(m); err != nil {
			return fmt.Errorf("decoding: %w", err)
		}
		if !d.hosted() {
			locals = append(locals, m)
		}
	}
	p := r.Power
	if !slices.Contains(locals, p.PrimaryModel) {
		return fmt.Errorf("power.primary_model %q is not a local model in decoding", p.PrimaryModel)
	}
	if p.PrimaryEndpoint != interceptorPrimaryEndpoint {
		return fmt.Errorf("power.primary_endpoint %q, want %q", p.PrimaryEndpoint, interceptorPrimaryEndpoint)
	}
	if !(p.AssumedQ > 0 && p.AssumedQ < 1) {
		return fmt.Errorf("power.assumed_q %v outside (0,1)", p.AssumedQ)
	}

	if err := checkDigest("cases_sha256", filepath.Join(dir, "cases.json"), r.CasesSHA256); err != nil {
		return err
	}
	cases, err := loadBenignCases(filepath.Join(dir, "cases.json"))
	if err != nil {
		return err
	}
	if n := countStratum(cases.Tasks, benignExtraction); p.Clusters != n {
		return fmt.Errorf("power.clusters %d, want the %d extraction tasks of cases.json", p.Clusters, n)
	}
	// ponytail: 1e-12 absorbs last-bit math differences across architectures
	// (FMA fusion); the registered values come from these functions.
	if want := h0PassProbability(p.Clusters, p.AssumedQ, r.Budgets.Delta); math.Abs(p.H0PassPrimary-want) > 1e-12 {
		return fmt.Errorf("power.h0_pass_primary %v, want h0PassProbability(%d, %v, %v) = %v", p.H0PassPrimary, p.Clusters, p.AssumedQ, r.Budgets.Delta, want)
	}
	if want := math.Pow(p.H0PassPrimary, float64(len(locals))); math.Abs(p.H0PassAllLocal-want) > 1e-12 {
		return fmt.Errorf("power.h0_pass_all_local %v, want h0_pass_primary^%d (the local models) = %v", p.H0PassAllLocal, len(locals), want)
	}
	if err := p.checkSecondary(cases, locals, r.Budgets.Delta); err != nil {
		return err
	}

	census := filepath.Join(dir, filepath.FromSlash(interceptorCensusRel))
	for _, f := range []struct{ name, file, want string }{
		{"census.manifest_sha256", "manifest.json", r.Census.ManifestSHA256},
		{"census.results_sha256", "census-results.json", r.Census.ResultsSHA256},
	} {
		if err := checkDigest(f.name, filepath.Join(census, f.file), f.want); err != nil {
			return err
		}
	}
	results, err := loadCensusResults(filepath.Join(census, "census-results.json"))
	if err != nil {
		return err
	}
	switch {
	case results.ManifestSHA256 != r.Census.ManifestSHA256:
		return fmt.Errorf("census.manifest_sha256 %s, but census-results.json binds manifest %s", r.Census.ManifestSHA256, results.ManifestSHA256)
	case cases.Sampling.CensusManifestSHA256 != r.Census.ManifestSHA256:
		return fmt.Errorf("census.manifest_sha256 %s, but cases.json was drafted from manifest %s", r.Census.ManifestSHA256, cases.Sampling.CensusManifestSHA256)
	case results.FullSHA256 != r.Census.FullSHA256:
		return fmt.Errorf("census.full_sha256 %s does not match census-results.json full_sha256 %s", r.Census.FullSHA256, results.FullSHA256)
	}
	return r.checkTraces(dir)
}

// checkSecondary holds p.Secondary to every local model on every registered
// endpoint except the primary contrast, once each, with its stratum's task
// count in cases and its h0PassProbability, and H0PassAllContrasts to the
// product over the primary and every secondary contrast.
func (p registeredPower) checkSecondary(cases benignCases, locals []string, delta float64) error {
	seen := map[[3]string]bool{{p.PrimaryModel, interceptorPrimaryEndpoint, benignExtraction}: true}
	all := p.H0PassPrimary
	for i, c := range p.Secondary {
		stratum, known := interceptorEndpointStrata[c.Endpoint]
		key := [3]string{c.Model, c.Endpoint, c.Stratum}
		switch {
		case !known:
			return fmt.Errorf("power.secondary[%d].endpoint %q is not a registered endpoint", i, c.Endpoint)
		case c.Stratum != stratum:
			return fmt.Errorf("power.secondary[%d].stratum %q, want %s for endpoint %s", i, c.Stratum, stratum, c.Endpoint)
		case !slices.Contains(locals, c.Model):
			return fmt.Errorf("power.secondary[%d].model %q is not a local model in decoding", i, c.Model)
		case seen[key]:
			return fmt.Errorf("power.secondary[%d] (%s %s) is a duplicate or the primary contrast", i, c.Model, c.Endpoint)
		}
		seen[key] = true
		if n := countStratum(cases.Tasks, c.Stratum); c.Clusters != n {
			return fmt.Errorf("power.secondary[%d].clusters %d, want the %d %s tasks of cases.json", i, c.Clusters, n, c.Stratum)
		}
		if want := h0PassProbability(c.Clusters, p.AssumedQ, delta); math.Abs(c.H0Pass-want) > 1e-12 {
			return fmt.Errorf("power.secondary[%d].h0_pass %v, want h0PassProbability(%d, %v, %v) = %v", i, c.H0Pass, c.Clusters, p.AssumedQ, delta, want)
		}
		all *= c.H0Pass
	}
	if want := len(locals)*len(interceptorEndpointStrata) - 1; len(p.Secondary) != want {
		return fmt.Errorf("power.secondary has %d contrasts, want %d: every local model on every endpoint but the primary", len(p.Secondary), want)
	}
	if math.Abs(p.H0PassAllContrasts-all) > 1e-12 {
		return fmt.Errorf("power.h0_pass_all_contrasts %v, want the product over the primary and power.secondary %v", p.H0PassAllContrasts, all)
	}
	return nil
}

// checkTraces holds the committed traces in dir/traces to r.Traces: each file
// registered exactly once with its canonicalTraceHash and its benign stratum.
func (r interceptorRegistration) checkTraces(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "traces", "*.json"))
	if err != nil {
		return err
	}
	traces, err := loadTraces(files)
	if err != nil {
		return fmt.Errorf("traces: %w", err)
	}
	onDisk := make(map[string]Trace, len(traces))
	for _, t := range traces {
		if _, dup := onDisk[t.ID]; dup {
			return fmt.Errorf("traces: two trace files hold id %s", t.ID)
		}
		onDisk[t.ID] = t
	}
	seen := make(map[string]bool, len(r.Traces))
	for i, rt := range r.Traces {
		t, ok := onDisk[rt.ID]
		switch {
		case seen[rt.ID]:
			return fmt.Errorf("traces[%d] (%s) is a duplicate registration", i, rt.ID)
		case !ok:
			return fmt.Errorf("traces[%d] (%s) has no trace file", i, rt.ID)
		case t.Benign == nil || t.Benign.Stratum != rt.Population:
			// loadTraces admits only the three benign strata, so this also
			// rejects any other population.
			return fmt.Errorf("traces[%d] (%s).population %q is not the trace's benign stratum", i, rt.ID, rt.Population)
		}
		if got := canonicalTraceHash(t); got != rt.SHA256 {
			return fmt.Errorf("traces[%d] (%s).sha256 %s does not match canonicalTraceHash %s", i, rt.ID, rt.SHA256, got)
		}
		seen[rt.ID] = true
	}
	for _, id := range slices.Sorted(maps.Keys(onDisk)) {
		if !seen[id] {
			return fmt.Errorf("traces: trace file %s is not registered", id)
		}
	}
	return nil
}
