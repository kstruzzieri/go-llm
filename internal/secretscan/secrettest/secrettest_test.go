package secrettest

import (
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/secretscan"
)

// Expected sets are spelled out here, not read from the package under test.
var (
	specClasses = []string{"expected-sensitive", "synthetic-fixture", "benign-lookalike", "benign", "unclassified"}
	scanKinds   = []string{"openai_token", "github_token", "gitlab_token", "slack_token", "npm_token", "bearer_token", "secret_assignment", "private_key", "payment_card"}
	pemLabels   = []string{"PRIVATE KEY", "ENCRYPTED PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY", "DSA PRIVATE KEY", "OPENSSH PRIVATE KEY"}
)

func TestCasesValidate(t *testing.T) {
	t.Parallel()
	cases, err := Cases()
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("Cases returned an empty table")
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.ID] {
			t.Errorf("duplicate case ID %q", c.ID)
		}
		seen[c.ID] = true
		if !slices.Contains(specClasses, string(c.Class)) {
			t.Errorf("case %s: class %q is not a spec §5.2 class", c.ID, c.Class)
		}
		if c.Mandatory && c.Class != "expected-sensitive" {
			t.Errorf("case %s: mandatory with class %q", c.ID, c.Class)
		}
		if c.CoverageGap && c.ExpectAny {
			t.Errorf("case %s: coverage gap expects a finding", c.ID)
		}
		if n := strings.Count(c.Template, "{{VALUE}}"); n != 1 {
			t.Errorf("case %s: template holds {{VALUE}} %d times, want 1", c.ID, n)
		}
		if c.Kind != "" && !slices.Contains(scanKinds, c.Kind) {
			t.Errorf("case %s: kind %q is not a secretscan kind", c.ID, c.Kind)
		}
	}

	// The validator inside Cases rejects each violation on its own.
	base := Case{
		ID: "base", Class: Benign, Role: "role", Provenance: "provenance", Rationale: "rationale",
		Carriers: []Carrier{Observation}, Template: "a {{VALUE}}", Generator: "literal", Params: map[string]string{"text": "b"},
	}
	table := func(cs ...Case) []byte {
		data, err := json.Marshal(map[string]any{"version": 1, "cases": cs})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if _, err := parseCases(table(base)); err != nil {
		t.Fatalf("parseCases(valid base) = %v, want nil", err)
	}
	for _, tt := range []struct {
		name   string
		mutate func(*Case)
	}{
		{"missing role", func(c *Case) { c.Role = "" }},
		{"unknown class", func(c *Case) { c.Class = "secret" }},
		{"mandatory not sensitive", func(c *Case) { c.Mandatory, c.ExpectAny, c.ExpectKinds = true, true, []string{"openai_token"} }},
		{"mandatory without expectation", func(c *Case) { c.Class, c.Mandatory = ExpectedSensitive, true }},
		{"coverage gap expecting", func(c *Case) { c.CoverageGap, c.ExpectAny, c.ExpectKinds = true, true, []string{"openai_token"} }},
		{"template without marker", func(c *Case) { c.Template = "a" }},
		{"template marker twice", func(c *Case) { c.Template = "{{VALUE}}{{VALUE}}" }},
		{"unknown kind", func(c *Case) { c.Kind = "aws_key" }},
		{"unknown expected kind", func(c *Case) { c.ExpectAny, c.ExpectKinds = true, []string{"aws_key"} }},
		{"expect any without kinds", func(c *Case) { c.ExpectAny = true }},
		{"kinds without expect any", func(c *Case) { c.ExpectKinds = []string{"openai_token"} }},
		{"no carriers", func(c *Case) { c.Carriers = nil }},
		{"unknown carrier", func(c *Case) { c.Carriers = []Carrier{"stdin"} }},
		{"repeated carrier", func(c *Case) { c.Carriers = []Carrier{Observation, Observation} }},
		{"unknown generator", func(c *Case) { c.Generator = "jwt" }},
		{"seed param", func(c *Case) { c.Params["seed"] = "x" }},
	} {
		c := base
		c.Params = maps.Clone(base.Params)
		tt.mutate(&c)
		if _, err := parseCases(table(c)); err == nil {
			t.Errorf("parseCases(%s) = nil, want error", tt.name)
		}
	}
	for name, data := range map[string]string{
		"duplicate id":  string(table(base, base)),
		"unknown field": `{"version":1,"cases":[],"extra":true}`,
		"wrong version": `{"version":2,"cases":[]}`,
		"trailing data": `{"version":1,"cases":[]} {}`,
	} {
		if _, err := parseCases([]byte(data)); err == nil {
			t.Errorf("parseCases(%s) = nil, want error", name)
		}
	}
}

// TestCasesMeetDetectionContract checks every authored expectation against
// Scan of the rendered value. Failures name kinds only, never the value.
func TestCasesMeetDetectionContract(t *testing.T) {
	t.Parallel()
	cases, err := Cases()
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	for _, c := range cases {
		value, err := c.Render()
		if err != nil {
			t.Errorf("case %s: Render: %v", c.ID, err)
			continue
		}
		if again, _ := c.Render(); again != value {
			t.Errorf("case %s: Render is not deterministic", c.ID)
		}
		var kinds []string
		for _, f := range secretscan.Scan(value) {
			if !slices.Contains(kinds, string(f.Kind)) {
				kinds = append(kinds, string(f.Kind))
			}
		}
		if (len(kinds) > 0) != c.ExpectAny || !slices.Equal(kinds, c.ExpectKinds) {
			t.Errorf("case %s: Scan kinds = %v, want %v", c.ID, kinds, c.ExpectKinds)
		}
	}
}

// TestCasesCoverStrata pins the spec §5.3 strata so that a dropped case is red.
func TestCasesCoverStrata(t *testing.T) {
	t.Parallel()
	cases, err := Cases()
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	mandatoryKinds := map[string]bool{}
	assignmentKeys := map[string]bool{}
	gapShapes := map[string]bool{}
	exempt := map[string]bool{}
	pem := map[string]bool{}
	for _, c := range cases {
		p := c.Params
		if c.Mandatory {
			mandatoryKinds[c.Kind] = true
			if c.Generator == "assignment" {
				key := strings.ToLower(strings.ReplaceAll(strings.Trim(p["key"], `"'`), "-", "_"))
				assignmentKeys[key] = true
			}
		}
		if c.CoverageGap {
			gapShapes[p["prefix_a"]+p["prefix_b"]+p["key"]] = true
		}
		if c.Generator == "card" && p["test_pan"] != "" {
			exempt[p["test_pan"]+"/"+p["sep"]] = true
		}
		if c.Generator == "pem" {
			pem[p["label"]+"/"+p["complete"]] = true
		}
	}
	for _, kind := range scanKinds {
		if !mandatoryKinds[kind] {
			t.Errorf("no mandatory case for kind %s", kind)
		}
	}
	for _, key := range []string{"api_key", "apikey", "auth_token", "access_token", "refresh_token", "id_token", "token", "secret", "password", "passwd", "private_key", "authorization"} {
		if !assignmentKeys[key] {
			t.Errorf("no mandatory assignment case for key %s", key)
		}
	}
	for _, family := range []string{"gho_", "ghu_", "ghs_", "ghr_", "xapp-", "sk_live_", "AKIA", "aws_secret_access_key", "client_secret", "_authToken"} {
		found := false
		for shape := range gapShapes {
			found = found || strings.Contains(shape, family)
		}
		if !found {
			t.Errorf("no coverage-gap case for %s", family)
		}
	}
	for _, name := range []string{"visa_4111", "visa_4242", "mastercard", "amex", "discover", "diners", "jcb"} {
		for _, sep := range []string{" ", "-"} {
			if !exempt[name+"/"+sep] {
				t.Errorf("no exempt-PAN case for %s with separator %q", name, sep)
			}
		}
	}
	for _, label := range pemLabels {
		for _, complete := range []string{"true", "false"} {
			if !pem[label+"/"+complete] {
				t.Errorf("no PEM case for %s complete=%s", label, complete)
			}
		}
	}
}

func TestGeneratorsHitBoundaries(t *testing.T) {
	t.Parallel()
	gen := func(name string, params map[string]string) string {
		t.Helper()
		v, err := Generate(name, params)
		if err != nil {
			t.Fatalf("Generate(%s): %v", name, err)
		}
		return v
	}
	check := func(label, value string, want []secretscan.Finding) {
		t.Helper()
		if got := secretscan.Scan(value); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Scan = %v, want %v", label, got, want)
		}
	}
	for _, tt := range []struct {
		prefix  string
		kind    secretscan.Kind
		minimum int
	}{
		{"sk-", secretscan.OpenAIToken, 17},
		{"ghp_", secretscan.GitHubToken, 16},
		{"github_pat_", secretscan.GitHubToken, 16},
		{"glpat-", secretscan.GitLabToken, 16},
		{"npm_", secretscan.NPMToken, 16},
		{"xoxb-", secretscan.SlackToken, 10},
		{"xoxa-", secretscan.SlackToken, 10},
		{"xoxp-", secretscan.SlackToken, 10},
		{"xoxr-", secretscan.SlackToken, 10},
		{"xoxs-", secretscan.SlackToken, 10},
	} {
		params := func(n int) map[string]string {
			return map[string]string{"prefix_a": tt.prefix[:1], "prefix_b": tt.prefix[1:], "len": strconv.Itoa(n), "charset": "alnum", "seed": tt.prefix}
		}
		at := gen("provider", params(tt.minimum))
		if len(at) != len(tt.prefix)+tt.minimum || !strings.HasPrefix(at, tt.prefix) {
			t.Errorf("provider %s at minimum: %d bytes, want prefix plus %d", tt.prefix, len(at), tt.minimum)
		}
		check("provider "+tt.prefix+" at minimum", at, []secretscan.Finding{{Kind: tt.kind, Start: 0, End: len(tt.prefix) + tt.minimum}})
		check("provider "+tt.prefix+" one below", gen("provider", params(tt.minimum-1)), nil)
	}

	bearer := func(n int, entropy string) string {
		return gen("bearer", map[string]string{"len": strconv.Itoa(n), "entropy": entropy, "seed": "bearer"})
	}
	check("bearer high at 20", bearer(20, "high"), []secretscan.Finding{{Kind: secretscan.BearerToken, Start: 7, End: 27}})
	check("bearer high at 19", bearer(19, "high"), nil)
	check("bearer low at 40", bearer(40, "low"), nil)

	assignment := func(n int, quote string) string {
		return gen("assignment", map[string]string{"key": "token", "quote": quote, "len": strconv.Itoa(n), "seed": "assignment"})
	}
	check("assignment at 20", assignment(20, ""), []secretscan.Finding{{Kind: secretscan.SecretAssignment, Start: 6, End: 26}})
	check("assignment quoted at 20", assignment(20, `"`), []secretscan.Finding{{Kind: secretscan.SecretAssignment, Start: 7, End: 27}})
	check("assignment at 19", assignment(19, ""), nil)
	if got := gen("assignment", map[string]string{"key": "password", "sep": ": ", "quote": "'", "len": "20", "seed": "s"}); !strings.HasPrefix(got, "password: '") || !strings.HasSuffix(got, "'") || len(got) != 32 {
		t.Errorf("assignment with sep and quote: %d bytes, want password: '<20>'", len(got))
	}

	for _, label := range pemLabels {
		complete := gen("pem", map[string]string{"label": label, "complete": "true", "seed": label})
		check("pem "+label+" complete", complete, []secretscan.Finding{{Kind: secretscan.PrivateKey, Start: 0, End: len(complete)}})
		check("pem "+label+" truncated", gen("pem", map[string]string{"label": label, "complete": "false", "seed": label}), nil)
	}
	check("pem certificate", gen("pem", map[string]string{"label": "CERTIFICATE", "complete": "true", "seed": "c"}), nil)
	indented := gen("pem", map[string]string{"label": "PRIVATE KEY", "complete": "true", "lines": "2", "sep": "\n    ", "seed": "i"})
	if lines := strings.Split(indented, "\n    "); len(lines) != 4 {
		t.Errorf("pem with lines=2 and indented sep: %d lines, want 4", len(lines))
	}
	if elided := gen("pem", map[string]string{"label": "PRIVATE KEY", "complete": "false", "lines": "0", "seed": "e"}); !strings.HasSuffix(elided, "-----\n...") {
		t.Error("pem with lines=0 does not end in an elision line")
	}

	if got := gen("literal", map[string]string{"text": "plain words", "seed": "x"}); got != "plain words" {
		t.Errorf("literal = %q, want the text verbatim", got)
	}
	for _, bad := range []struct {
		name   string
		params map[string]string
	}{
		{"nope", nil},
		{"provider", map[string]string{"len": "4", "charset": "alnum", "color": "red"}},
		{"provider", map[string]string{"len": "4", "charset": "emoji"}},
		{"provider", map[string]string{"len": "-1", "charset": "alnum"}},
		{"bearer", map[string]string{"len": "20", "entropy": "medium"}},
		{"pem", map[string]string{"label": "PRIVATE KEY", "complete": "maybe"}},
		{"card", map[string]string{"digits": "16", "valid": "true", "lead": "4x"}},
		{"card", map[string]string{"test_pan": "unknown"}},
		{"literal", map[string]string{}},
	} {
		if _, err := Generate(bad.name, bad.params); err == nil {
			t.Errorf("Generate(%s, %v) = nil error, want error", bad.name, bad.params)
		}
	}
}

func TestCardGenerator(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		digits     int
		lead, sep  string
		group      int
		wantGroups []int
	}{
		{16, "4", "", 0, nil},
		{16, "4", " ", 0, []int{4, 4, 4, 4}},
		{16, "5", "-", 0, []int{4, 4, 4, 4}},
		{13, "4", "", 0, nil},
		{15, "37", " ", 0, []int{4, 4, 4, 3}},
		{19, "17297712", "", 0, nil},
		{13, "", " ", 1, []int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}},
	} {
		for _, valid := range []bool{true, false} {
			params := map[string]string{"digits": strconv.Itoa(tt.digits), "valid": strconv.FormatBool(valid), "lead": tt.lead, "sep": tt.sep, "seed": tt.lead + tt.sep}
			if tt.group != 0 {
				params["group"] = strconv.Itoa(tt.group)
			}
			label := "card " + strconv.Itoa(tt.digits) + " lead " + tt.lead + " sep " + strconv.Quote(tt.sep) + " valid " + strconv.FormatBool(valid)
			value, err := Generate("card", params)
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			digits := strings.NewReplacer(" ", "", "-", "").Replace(value)
			if len(digits) != tt.digits || !strings.HasPrefix(digits, tt.lead) || strings.Trim(digits, "0123456789") != "" {
				t.Errorf("%s: %d digits, want %d starting with the lead", label, len(digits), tt.digits)
			}
			if luhn(digits) != valid {
				t.Errorf("%s: Luhn = %t", label, !valid)
			}
			if tt.sep != "" {
				var groups []int
				for _, g := range strings.Split(value, tt.sep) {
					groups = append(groups, len(g))
				}
				if !slices.Equal(groups, tt.wantGroups) {
					t.Errorf("%s: groups %v, want %v", label, groups, tt.wantGroups)
				}
			}
			var want []secretscan.Finding
			if valid {
				want = []secretscan.Finding{{Kind: secretscan.PaymentCard, Start: 0, End: len(value)}}
			}
			if got := secretscan.Scan(value); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: Scan = %v, want %v", label, got, want)
			}
		}
	}
	for name, pan := range map[string]string{
		"visa_4111": "41111111" + "11111111", "visa_4242": "42424242" + "42424242", "mastercard": "55555555" + "55554444",
		"amex": "37828224" + "6310005", "discover": "60111111" + "11111117", "diners": "3056930" + "9025904", "jcb": "35660020" + "20360505",
	} {
		for _, sep := range []string{" ", "-"} {
			value, err := Generate("card", map[string]string{"test_pan": name, "sep": sep})
			if err != nil {
				t.Fatalf("exempt %s: %v", name, err)
			}
			if got := strings.ReplaceAll(value, sep, ""); got != pan || !strings.Contains(value, sep) || !luhn(got) {
				t.Errorf("exempt %s with %q: rendered digits differ from the published test number or are not separated", name, sep)
			}
			for _, f := range secretscan.Scan(value) {
				if f.Kind == secretscan.PaymentCard {
					t.Errorf("exempt %s with %q: Scan reported payment_card", name, sep)
				}
			}
		}
	}
}

// luhn is the test's own check, independent of the scanner and the generator.
func luhn(digits string) bool {
	sum := 0
	for i := range len(digits) {
		d := int(digits[len(digits)-1-i] - '0')
		if i%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

func TestCommittedArtifactsCarryNoSecretShapes(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"cases.json"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if findings := secretscan.Scan(string(data)); len(findings) != 0 {
			t.Errorf("%s carries secret-shaped spans: %v", path, findings)
		}
	}
}
