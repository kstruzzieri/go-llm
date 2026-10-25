// Package secrettest holds the classified Secrets case table (#517, spec §5.1)
// and the deterministic generators that expand its secret-shaped values at
// test time. Like httptest, it is a helper imported only by tests and the
// interceptor census. The committed table carries no secret-shaped bytes:
// No live credentials; generated fixtures minimize committed secret-shaped
// material; GitHub enforcement is verified separately.
package secrettest

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/kstruzzieri/go-llm/internal/secretscan"
)

//go:embed cases.json
var casesJSON []byte

// Class is a case's label, assigned from its documented role before any
// scanning (spec §5.2).
type Class string

// The spec §5.2 classes.
const (
	ExpectedSensitive Class = "expected-sensitive"
	SyntheticFixture  Class = "synthetic-fixture"
	BenignLookalike   Class = "benign-lookalike"
	Benign            Class = "benign"
	Unclassified      Class = "unclassified"
)

// Carrier is the surface that delivers a case's rendered value.
type Carrier string

// Carriers: a tool observation's text, or a JSON string field in tool-call
// arguments.
const (
	Observation Carrier = "observation"
	ToolArgs    Carrier = "tool-args"
)

// Case is one row of the table. Class is the accuracy truth: it is assigned
// from Role, Provenance and Rationale before any scanning, and Score turns it
// and an observed result into a confusion cell. Kind is the secretscan
// stratum the case probes, or empty. ExpectAny and ExpectKinds are a
// conformance prediction, not truth: they pin what the current rules report
// for the rendered value scanned as plain text (kinds in source order without
// repeats), so a scanner change shows up as a diff. They never decide TP, FN,
// FP or TN. Mandatory marks the supported expected-sensitive set of spec §5.6
// item 1. CoverageGap marks a family the scanner has no rule for at all (no
// prefix, kind or key); only those rows leave the confusion matrix.
type Case struct {
	ID          string            `json:"id"`
	Class       Class             `json:"class"`
	Role        string            `json:"role"`
	Provenance  string            `json:"provenance"`
	Rationale   string            `json:"rationale"`
	Kind        string            `json:"kind,omitempty"`
	Carriers    []Carrier         `json:"carriers"`
	Template    string            `json:"template"`
	Generator   string            `json:"generator"`
	Params      map[string]string `json:"params"`
	ExpectAny   bool              `json:"expect_any"`
	ExpectKinds []string          `json:"expect_kinds,omitempty"`
	Mandatory   bool              `json:"mandatory,omitempty"`
	CoverageGap bool              `json:"coverage_gap,omitempty"`
}

const valueMarker = "{{VALUE}}"

// Cases decodes the embedded table, rejecting unknown fields, and validates it.
func Cases() ([]Case, error) { return parseCases(casesJSON) }

func parseCases(data []byte) ([]Case, error) {
	var table struct {
		Version int    `json:"version"`
		Cases   []Case `json:"cases"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&table); err != nil {
		return nil, fmt.Errorf("secrettest: decode cases: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("secrettest: trailing data after the case table")
	}
	if table.Version != 1 {
		return nil, fmt.Errorf("secrettest: case table version %d, want 1", table.Version)
	}
	seen := map[string]bool{}
	for _, c := range table.Cases {
		if seen[c.ID] {
			return nil, fmt.Errorf("secrettest: duplicate case %q", c.ID)
		}
		seen[c.ID] = true
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("secrettest: case %q: %w", c.ID, err)
		}
	}
	return table.Cases, nil
}

var kinds = []secretscan.Kind{
	secretscan.OpenAIToken, secretscan.GitHubToken, secretscan.GitLabToken, secretscan.SlackToken, secretscan.NPMToken,
	secretscan.BearerToken, secretscan.SecretAssignment, secretscan.PrivateKey, secretscan.PaymentCard,
}

func knownKind(k string) bool { return slices.Contains(kinds, secretscan.Kind(k)) }

func (c Case) validate() error {
	switch {
	case c.ID == "" || c.Role == "" || c.Provenance == "" || c.Rationale == "":
		return errors.New("id, role, provenance and rationale are required")
	case !slices.Contains([]Class{ExpectedSensitive, SyntheticFixture, BenignLookalike, Benign, Unclassified}, c.Class):
		return fmt.Errorf("unknown class %q", c.Class)
	case c.Kind != "" && !knownKind(c.Kind):
		return fmt.Errorf("unknown kind %q", c.Kind)
	case strings.Count(c.Template, valueMarker) != 1:
		return errors.New("template must hold {{VALUE}} exactly once")
	case c.Mandatory && c.Class != ExpectedSensitive:
		// A mandatory case may predict a miss (expect_any false): conformance
		// then pins the miss and the evidence report records item 1 as FAIL.
		return errors.New("mandatory requires class expected-sensitive")
	case c.Mandatory && c.Kind == "":
		// A coverage gap has no kind, so this also rejects a mandatory gap.
		return errors.New("a mandatory case needs a supported kind, so it cannot be a coverage gap")
	case c.CoverageGap && (c.ExpectAny || c.Kind != ""):
		return errors.New("a coverage gap has no supported kind and cannot expect a finding")
	case c.ExpectAny != (len(c.ExpectKinds) > 0):
		return errors.New("expect_any must hold exactly when expect_kinds is non-empty")
	case len(c.Carriers) == 0:
		return errors.New("at least one carrier is required")
	case generators[c.Generator].run == nil:
		return fmt.Errorf("unknown generator %q", c.Generator)
	}
	if _, ok := c.Params["seed"]; ok {
		return errors.New("params must not set seed; Render seeds from the ID")
	}
	for _, k := range c.ExpectKinds {
		if !knownKind(k) {
			return fmt.Errorf("unknown expected kind %q", k)
		}
	}
	for i, carrier := range c.Carriers {
		if carrier != Observation && carrier != ToolArgs || slices.Contains(c.Carriers[:i], carrier) {
			return fmt.Errorf("unknown or repeated carrier %q", carrier)
		}
	}
	return nil
}

// Score places the case in the confusion matrix, given whether any sensitive
// finding fired on it. Truth is the Class alone: expected-sensitive scores
// "TP" or "FN", and benign scores "FP" or "TN". Coverage gaps and every other
// class score "": lookalikes and fixtures are tabulated separately, and
// unclassified rows nowhere. The ExpectAny prediction never enters.
func (c Case) Score(fired bool) string {
	if c.CoverageGap {
		return ""
	}
	switch c.Class {
	case ExpectedSensitive:
		if fired {
			return "TP"
		}
		return "FN"
	case Benign:
		if fired {
			return "FP"
		}
		return "TN"
	}
	return ""
}

// Render generates the case's value, seeded from its ID, and substitutes it
// for {{VALUE}} in Template. The result is an expanded value: never commit or
// print it.
func (c Case) Render() (string, error) {
	params := maps.Clone(c.Params)
	if params == nil {
		params = map[string]string{}
	}
	params["seed"] = c.ID
	value, err := Generate(c.Generator, params)
	if err != nil {
		return "", fmt.Errorf("secrettest: case %q: %w", c.ID, err)
	}
	return strings.Replace(c.Template, valueMarker, value, 1), nil
}

type generator struct {
	params []string
	run    func(src *stream, p map[string]string) (string, error)
}

// Generators and their params. Every param is optional unless noted.
//   - provider: prefix_a + prefix_b (joined here so the table never holds a
//     whole prefix), then len (required) bytes drawn from charset (required).
//   - pem: label (required), complete (required, "true" or "false"), lines of
//     base64 key material (default 4; 0 writes a "..." elision line), and the
//     line separator sep (default "\n").
//   - card: digits (required) with lead fixed and a Luhn check digit that is
//     correct when valid is "true" and off by one when "false"; or test_pan,
//     a published processor test number by name. sep joins groups of group
//     digits (default 4).
//   - assignment: key (required, verbatim), sep (default "="), quote (around
//     the value), and len (required) bytes of high-entropy value.
//   - bearer: "Bearer " then len (required) bytes. entropy (required) is
//     "high", which keeps bytes distinct, or "low", which draws from eight
//     letters (at most 3 bits per byte).
//   - literal: text (required), verbatim.
var generators = map[string]generator{
	"provider":   {[]string{"prefix_a", "prefix_b", "len", "charset"}, genProvider},
	"pem":        {[]string{"label", "complete", "lines", "sep"}, genPEM},
	"card":       {[]string{"digits", "valid", "lead", "sep", "group", "test_pan"}, genCard},
	"assignment": {[]string{"key", "sep", "quote", "len"}, genAssignment},
	"bearer":     {[]string{"len", "entropy"}, genBearer},
	"literal":    {[]string{"text"}, func(_ *stream, p map[string]string) (string, error) { return required(p, "text") }},
}

// Generate expands the named generator. The optional param "seed" selects the
// deterministic stream (see stream), so equal inputs give equal bytes; Render
// passes the case ID. Every other param must be one the generator documents.
func Generate(name string, params map[string]string) (string, error) {
	g, ok := generators[name]
	if !ok {
		return "", fmt.Errorf("secrettest: unknown generator %q", name)
	}
	for k := range params {
		if k != "seed" && !slices.Contains(g.params, k) {
			return "", fmt.Errorf("secrettest: generator %s: unknown param %q", name, k)
		}
	}
	value, err := g.run(&stream{seed: params["seed"]}, params)
	if err != nil {
		return "", fmt.Errorf("secrettest: generator %s: %w", name, err)
	}
	return value, nil
}

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

var charsets = map[string]string{
	"alnum":        alnum,
	"base64":       alnum + "+/",
	"base64url":    alnum + "-_",
	"hex":          "0123456789abcdef",
	"upper_digits": "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
	"x":            "x",
}

func genProvider(src *stream, p map[string]string) (string, error) {
	n, err := count(p, "len", "")
	if err != nil {
		return "", err
	}
	set, ok := charsets[p["charset"]]
	if !ok {
		return "", fmt.Errorf("unknown charset %q", p["charset"])
	}
	return p["prefix_a"] + p["prefix_b"] + draw(src, set, n), nil
}

func genPEM(src *stream, p map[string]string) (string, error) {
	label, err := required(p, "label")
	if err != nil {
		return "", err
	}
	complete, err := boolean(p, "complete")
	if err != nil {
		return "", err
	}
	lines, err := count(p, "lines", "4")
	if err != nil {
		return "", err
	}
	sep := p["sep"]
	if sep == "" {
		sep = "\n"
	}
	parts := []string{"-----BEGIN " + label + "-----"}
	for range lines {
		parts = append(parts, draw(src, charsets["base64"], 64))
	}
	if lines == 0 {
		parts = append(parts, "...")
	}
	if complete {
		parts = append(parts, "-----END "+label+"-----")
	}
	return strings.Join(parts, sep), nil
}

// testPANs are the published processor test numbers that scanner.go exempts,
// split as scanner.go writes them.
var testPANs = map[string]string{
	"visa_4111":  "41111111" + "11111111",
	"visa_4242":  "42424242" + "42424242",
	"mastercard": "55555555" + "55554444",
	"amex":       "37828224" + "6310005",
	"discover":   "60111111" + "11111117",
	"diners":     "3056930" + "9025904",
	"jcb":        "35660020" + "20360505",
}

func genCard(src *stream, p map[string]string) (string, error) {
	var digits string
	if name, ok := p["test_pan"]; ok {
		if digits, ok = testPANs[name]; !ok {
			return "", fmt.Errorf("unknown test_pan %q", name)
		}
	} else {
		n, err := count(p, "digits", "")
		if err != nil {
			return "", err
		}
		valid, err := boolean(p, "valid")
		if err != nil {
			return "", err
		}
		lead := p["lead"]
		if strings.Trim(lead, "0123456789") != "" || len(lead) >= n {
			return "", fmt.Errorf("lead must be fewer than %d digits", n)
		}
		ds := []byte(lead)
		for len(ds) < n-1 {
			d := byte('0' + src.Uint64()%10)
			if len(ds) == 0 && d == '0' {
				d = '1'
			}
			ds = append(ds, d)
		}
		check := luhnCheckDigit(ds)
		if !valid {
			check = '0' + (check-'0'+1)%10
		}
		digits = string(append(ds, check))
	}
	sep := p["sep"]
	if sep == "" {
		return digits, nil
	}
	group, err := count(p, "group", "4")
	if err != nil || group == 0 {
		return "", errors.New("group must be a positive count")
	}
	var groups []string
	for len(digits) > group {
		groups, digits = append(groups, digits[:group]), digits[group:]
	}
	return strings.Join(append(groups, digits), sep), nil
}

// luhnCheckDigit returns the digit that, appended to ds, makes it pass Luhn.
func luhnCheckDigit(ds []byte) byte {
	sum := 0
	for i := range ds {
		d := int(ds[len(ds)-1-i] - '0')
		if i%2 == 0 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return byte('0' + (10-sum%10)%10)
}

func genAssignment(src *stream, p map[string]string) (string, error) {
	key, err := required(p, "key")
	if err != nil {
		return "", err
	}
	n, err := count(p, "len", "")
	if err != nil {
		return "", err
	}
	sep, ok := p["sep"]
	if !ok {
		sep = "="
	}
	return key + sep + p["quote"] + distinct(src, alnum, n) + p["quote"], nil
}

func genBearer(src *stream, p map[string]string) (string, error) {
	n, err := count(p, "len", "")
	if err != nil {
		return "", err
	}
	switch p["entropy"] {
	case "high":
		return "Bearer " + distinct(src, alnum, n), nil
	case "low":
		return "Bearer " + draw(src, "abcdefgh", n), nil
	}
	return "", fmt.Errorf("entropy %q, want high or low", p["entropy"])
}

// stream is a deterministic byte source: block i is sha256(seed || i), with i
// a big-endian uint64 counter. Render seeds it with the case ID, so a
// regeneration is byte-identical on every platform and Go release.
type stream struct {
	seed    string
	counter uint64
	buf     []byte
}

// Uint64 returns the next eight bytes of the stream.
func (s *stream) Uint64() uint64 {
	if len(s.buf) < 8 {
		block := binary.BigEndian.AppendUint64([]byte(s.seed), s.counter)
		s.counter++
		sum := sha256.Sum256(block)
		s.buf = append(s.buf, sum[:]...)
	}
	v := binary.BigEndian.Uint64(s.buf)
	s.buf = s.buf[8:]
	return v
}

// draw returns n bytes drawn from set (modulo bias is irrelevant here).
func draw(src *stream, set string, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = set[src.Uint64()%uint64(len(set))]
	}
	return string(out)
}

// distinct returns n bytes from successive shuffles of set, so byte counts
// differ by at most one and entropy is at least log2(min(n, len(set))) bits
// per byte.
func distinct(src *stream, set string, n int) string {
	var out []byte
	for len(out) < n {
		block := []byte(set)
		for i := len(block) - 1; i > 0; i-- {
			j := src.Uint64() % uint64(i+1)
			block[i], block[j] = block[j], block[i]
		}
		out = append(out, block...)
	}
	return string(out[:n])
}

func required(p map[string]string, key string) (string, error) {
	if p[key] == "" {
		return "", fmt.Errorf("param %s is required", key)
	}
	return p[key], nil
}

func count(p map[string]string, key, fallback string) (int, error) {
	s, ok := p[key]
	if !ok || s == "" {
		s = fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("param %s must be a non-negative count", key)
	}
	return n, nil
}

func boolean(p map[string]string, key string) (bool, error) {
	switch p[key] {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("param %s must be true or false", key)
}
