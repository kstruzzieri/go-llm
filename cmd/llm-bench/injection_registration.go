package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

// injectionRegistrationSchema is the pre-registration file schema (#452). The
// registration is written and committed BEFORE any capture; capture binds to
// its digest, so a run cannot quietly change the census, decoding or policy.
const injectionRegistrationSchema = "llm-bench.injection-registration/v1"

// injectionRegistration is the loaded pre-registration. Every field is
// required and unknown fields are rejected.
type injectionRegistration struct {
	Schema           string                        `json:"schema"`
	CanaryGUID       string                        `json:"canary_guid"`
	CasesSHA256      string                        `json:"cases_sha256"`
	BuilderRevision  string                        `json:"builder_revision"`
	ScorerRevision   string                        `json:"scorer_revision"`
	Census           []registeredTrace             `json:"census"`
	Models           []string                      `json:"models"`
	Baseline         string                        `json:"baseline"`
	Decoding         map[string]registeredDecoding `json:"decoding"`
	DecoderPolicy    registeredDecoderPolicy       `json:"decoder_policy"`
	Estimand         string                        `json:"estimand"`
	IncompletePolicy string                        `json:"incomplete_policy"`
	RetryPolicy      string                        `json:"retry_policy"`
	DeferredTrigger  float64                       `json:"deferred_trigger"`
	ClaimGates       []string                      `json:"claim_gates"`
}

type registeredTrace struct {
	ID         string `json:"id"`
	SHA256     string `json:"sha256"` // canonicalTraceHash
	Population string `json:"population"`
}

type registeredDecoding struct {
	Temperature float64  `json:"temperature"`
	TopP        *float64 `json:"top_p,omitempty"`
	TopK        *int     `json:"top_k,omitempty"`
	NumPredict  int      `json:"num_predict"`
	Think       *bool    `json:"think,omitempty"`
	// ThinkMode is the model's registered inline-think parser mode (none,
	// always, toggle, auto). Required: capture must never fall back to a
	// transport default that differs from Golem's configured think_mode.
	ThinkMode    string            `json:"think_mode"`
	ThinkEffort  string            `json:"think_effort,omitempty"`
	ServingStack string            `json:"serving_stack"`
	Hosted       *registeredHosted `json:"hosted,omitempty"`
}

type registeredHosted struct {
	Endpoint            string `json:"endpoint"`
	CaptureWindow       string `json:"capture_window"`
	ThinkFields         string `json:"think_fields"`
	ParserPolicy        string `json:"parser_policy"`
	ToolCallEnvelope    string `json:"tool_call_envelope"`
	TimeLimit           string `json:"time_limit"`
	SessionHeaderPolicy string `json:"session_header_policy"`
	// HostFixedDecoding documents decoding values the host imposes (for
	// example a temperature it refuses to vary); the registered temperature
	// must be the host-fixed value, recorded truthfully.
	HostFixedDecoding   string   `json:"host_fixed_decoding"`
	UnavailableControls []string `json:"unavailable_controls"`
}

type registeredDecoderPolicy struct {
	Encodings     []string `json:"encodings"`
	MaxValueBytes int      `json:"max_value_bytes"`
	MaxTotalBytes int      `json:"max_total_bytes"`
	MaxDepth      int      `json:"max_depth"`
	MinToken      int      `json:"min_token"`
}

// providerOptions is the wire decoding the registration pins for one model.
func (d registeredDecoding) providerOptions() provider.ModelOptions {
	temp := d.Temperature
	return provider.ModelOptions{
		Temperature: &temp,
		TopP:        d.TopP,
		TopK:        d.TopK,
		NumPredict:  d.NumPredict,
		Think:       d.Think,
		ThinkEffort: d.ThinkEffort,
	}
}

// injectionDecoding is the registered decoding plus parser policy for one
// model: what Runner.InjectionDecoding carries per selector.
type injectionDecoding struct {
	Options   provider.ModelOptions
	ThinkMode provider.ThinkMode
}

// decoding returns the wire options and the parser mode the registration pins.
func (d registeredDecoding) decoding() (injectionDecoding, error) {
	mode, err := provider.ParseThinkModeStrict(d.ThinkMode)
	if err != nil {
		return injectionDecoding{}, err
	}
	return injectionDecoding{Options: d.providerOptions(), ThinkMode: mode}, nil
}

// corpusIDs returns the census entries of the scored corpus population.
func (r injectionRegistration) corpusIDs() []string {
	var ids []string
	for _, c := range r.Census {
		if c.Population == "corpus" {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// loadInjectionRegistration reads and validates a registration file and
// returns it with the "sha256:"-prefixed digest of the file bytes.
func loadInjectionRegistration(path string) (injectionRegistration, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return injectionRegistration{}, "", fmt.Errorf("registration: read %q: %w", path, err)
	}
	var r injectionRegistration
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return injectionRegistration{}, "", fmt.Errorf("registration: decode %q: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return injectionRegistration{}, "", fmt.Errorf("registration: %q has trailing data", path)
	}
	if err := r.validate(); err != nil {
		return injectionRegistration{}, "", fmt.Errorf("registration: %q: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	return r, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (r injectionRegistration) validate() error {
	if r.Schema != injectionRegistrationSchema {
		return fmt.Errorf("schema %q (want %q)", r.Schema, injectionRegistrationSchema)
	}
	if r.CanaryGUID != injectionCanaryGUID {
		return errors.New("canary_guid does not match the corpus canary")
	}
	for name, v := range map[string]string{
		"cases_sha256": r.CasesSHA256, "builder_revision": r.BuilderRevision, "scorer_revision": r.ScorerRevision,
		"estimand": r.Estimand, "incomplete_policy": r.IncompletePolicy, "retry_policy": r.RetryPolicy,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if r.DeferredTrigger <= 0 || r.DeferredTrigger >= 1 {
		return fmt.Errorf("deferred_trigger %v outside (0,1)", r.DeferredTrigger)
	}
	if len(r.ClaimGates) == 0 {
		return errors.New("claim_gates is required")
	}
	if len(r.Census) == 0 {
		return errors.New("census is empty")
	}
	seen := make(map[string]struct{}, len(r.Census))
	for i, c := range r.Census {
		if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.SHA256) == "" {
			return fmt.Errorf("census row %d: id and sha256 are required", i)
		}
		if c.Population != "corpus" && c.Population != "control" {
			return fmt.Errorf("census row %d (%s): population %q (want corpus or control)", i, c.ID, c.Population)
		}
		if _, dup := seen[c.ID]; dup {
			return fmt.Errorf("census row %d: duplicate id %q", i, c.ID)
		}
		seen[c.ID] = struct{}{}
	}
	if len(r.Models) == 0 {
		return errors.New("models is empty")
	}
	if !slices.Contains(r.Models, r.Baseline) {
		return fmt.Errorf("baseline %q is not in models", r.Baseline)
	}
	if len(r.Decoding) != len(r.Models) {
		return fmt.Errorf("decoding has %d entries for %d models (one per model)", len(r.Decoding), len(r.Models))
	}
	for _, m := range r.Models {
		d, ok := r.Decoding[m]
		if !ok {
			return fmt.Errorf("model %q has no decoding", m)
		}
		if err := d.validate(m); err != nil {
			return err
		}
	}
	want := registeredDecoderPolicy{
		Encodings: []string{"literal", "base64", "hex"}, MaxValueBytes: decodeMaxValueBytes,
		MaxTotalBytes: decodeMaxTotalBytes, MaxDepth: decodeMaxDepth, MinToken: decodeMinToken,
	}
	got := r.DecoderPolicy
	if !slices.Equal(got.Encodings, want.Encodings) || got.MaxValueBytes != want.MaxValueBytes ||
		got.MaxTotalBytes != want.MaxTotalBytes || got.MaxDepth != want.MaxDepth || got.MinToken != want.MinToken {
		return errors.New("decoder_policy does not equal the compiled decoder constants")
	}
	return nil
}

// hosted reports whether d's serving stack is a hosted service.
func (d registeredDecoding) hosted() bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(d.ServingStack)), "hosted")
}

// validate checks model m's registered decoding: the rules both the #452 and
// the #517 registration hold every model to.
func (d registeredDecoding) validate(m string) error {
	if d.NumPredict <= 0 || strings.TrimSpace(d.ServingStack) == "" {
		return fmt.Errorf("model %q decoding needs num_predict > 0 and serving_stack", m)
	}
	hosted := d.hosted()
	// Local models are pinned at temperature 0. A hosted model may register
	// the temperature its host fixes (hosted.host_fixed_decoding says why).
	switch {
	case hosted && (math.IsNaN(d.Temperature) || math.IsInf(d.Temperature, 0) || d.Temperature < 0):
		return fmt.Errorf("decoding for %q: hosted temperature must be a finite value >= 0; got %v", m, d.Temperature)
	case !hosted && d.Temperature != 0:
		return fmt.Errorf("decoding for %q: temperature must be 0 for a local serving stack (#452 global constraint); got %v", m, d.Temperature)
	}
	mode, err := provider.ParseThinkModeStrict(d.ThinkMode)
	if err != nil {
		return fmt.Errorf("model %q decoding: %w", m, err)
	}
	if mode == provider.ThinkNone && (d.Think != nil || d.ThinkEffort != "") {
		return fmt.Errorf("model %q decoding: think_mode none cannot set think or think_effort (production clears them)", m)
	}
	if mode == provider.ThinkToggle && d.Think == nil {
		return fmt.Errorf("model %q decoding: think_mode toggle requires an explicit think (an unset toggle parses as auto in production)", m)
	}
	if hosted && d.Hosted == nil {
		return fmt.Errorf("model %q serving_stack %q is hosted but has no hosted block", m, d.ServingStack)
	}
	if h := d.Hosted; h != nil {
		for name, v := range map[string]string{
			"endpoint": h.Endpoint, "capture_window": h.CaptureWindow, "think_fields": h.ThinkFields,
			"parser_policy": h.ParserPolicy, "tool_call_envelope": h.ToolCallEnvelope,
			"time_limit": h.TimeLimit, "session_header_policy": h.SessionHeaderPolicy,
			"host_fixed_decoding": h.HostFixedDecoding,
		} {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("model %q hosted.%s is required", m, name)
			}
		}
		if h.UnavailableControls == nil {
			return fmt.Errorf("model %q hosted.unavailable_controls must be present (may be empty)", m)
		}
	}
	return nil
}

// refuseJudgeOnInjection rejects -scorer llm-judge over injection traces: the
// payload would reach the judge model.
func refuseJudgeOnInjection(scorer string, traces []Trace) error {
	if scorer != "llm-judge" {
		return nil
	}
	for _, t := range traces {
		if t.Injection != nil {
			return fmt.Errorf("trace %q: llm-judge refuses injection traces (the payload would reach the judge); use exact-match or manual", t.ID)
		}
	}
	return nil
}

// anyInjectionTrace reports whether any trace carries an injection eval.
func anyInjectionTrace(traces []Trace) bool {
	for _, t := range traces {
		if t.Injection != nil {
			return true
		}
	}
	return false
}

// bindCapture fails closed unless every selected trace is in the census with
// its registered canonicalTraceHash and every target has a registered
// decoding. It returns the Runner.InjectionDecoding map keyed by selector.
func (r injectionRegistration) bindCapture(traces []Trace, targets []ModelTarget) (map[string]injectionDecoding, error) {
	census := make(map[string]string, len(r.Census))
	for _, c := range r.Census {
		census[c.ID] = strings.TrimPrefix(c.SHA256, "sha256:")
	}
	for _, t := range traces {
		want, ok := census[t.ID]
		if !ok {
			return nil, fmt.Errorf("registration: trace %q is not in the registered census", t.ID)
		}
		if got := canonicalTraceHash(t); got != want {
			return nil, fmt.Errorf("registration: trace %q hash %s does not match the registered %s", t.ID, got, want)
		}
	}
	out := make(map[string]injectionDecoding, len(targets))
	for _, tg := range targets {
		d, ok := r.Decoding[tg.Display]
		if !ok {
			return nil, fmt.Errorf("registration: model %q has no registered decoding", tg.Display)
		}
		dec, err := d.decoding()
		if err != nil {
			return nil, fmt.Errorf("registration: model %q: %w", tg.Display, err)
		}
		out[tg.Display] = dec
	}
	return out, nil
}
