package config

import (
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

func newTestDoc(t *testing.T) *Document {
	t.Helper()
	d, err := ParseDocument([]byte(rawValid), Origin{Source: OriginExplicit}, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func renderToString(t *testing.T, d *Document) string {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	out, err := d.canonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestAddProvider(t *testing.T) {
	d := newTestDoc(t)
	if err := d.AddProvider("q", ProviderSpec{BaseURL: "http://other:1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Config().Providers["q"]; !ok {
		t.Fatal("provider q missing")
	}
	assertDiag(t, d.AddProvider("q", ProviderSpec{BaseURL: "http://x"}),
		CodeProviderExists, SubjectProvider, "q")
	assertDiag(t, d.AddProvider("bad/name", ProviderSpec{BaseURL: "http://x"}),
		CodeProviderNameInvalid, SubjectProvider, "bad/name")

	before := renderToString(t, d)
	assertDiag(t, d.AddProvider("r", ProviderSpec{BaseURL: "not a url"}),
		CodeProviderEndpointInvalid, SubjectProvider, "r")
	if renderToString(t, d) != before {
		t.Fatal("failed AddProvider changed the draft")
	}
}

func TestProviderSpecRoundTripsAllFields(t *testing.T) {
	d := newTestDoc(t)
	full := ProviderSpec{
		BaseURL:       "http://gov:9",
		Timeout:       Duration{Duration: 90 * time.Second},
		APIFormat:     "openai-compat",
		SlotDiscovery: true,
	}
	if err := d.AddProvider("gov", full); err != nil {
		t.Fatal(err)
	}
	got, err := d.AuthoredProvider("gov")
	if err != nil {
		t.Fatal(err)
	}
	if got != full {
		t.Fatalf("Add round-trip dropped a field: got %+v want %+v", got, full)
	}
	updated := full
	updated.Timeout = Duration{Duration: 30 * time.Second}
	updated.SlotDiscovery = false
	if err := d.UpdateProvider("gov", updated); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.AuthoredProvider("gov"); got != updated {
		t.Fatalf("Update round-trip dropped a field: got %+v want %+v", got, updated)
	}
}

func TestUpdateProviderPreservesHiddenStateAndIsTransactional(t *testing.T) {
	raw := strings.Replace(rawValid, `"base_url": "http://localhost:1234"`,
		`"base_url":"http://localhost:1234/path?q=1","api_key":"${K}","x_vendor":{"a":1}`, 1)
	snapshot := map[string]string{"K": "value"}
	d, err := ParseDocument([]byte(raw), Origin{Source: OriginExplicit},
		DocumentOptions{LookupEnv: func(k string) (string, bool) { v, ok := snapshot[k]; return v, ok }})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateProvider("p", ProviderSpec{
		BaseURL: "http://moved:2/path?q=1", APIFormat: "openai-compat",
	}); err != nil {
		t.Fatal(err)
	}
	authored, err := d.AuthoredProvider("p")
	if err != nil {
		t.Fatal(err)
	}
	if authored.BaseURL != "http://moved:2/path?q=1" || authored.APIFormat != "openai-compat" {
		t.Fatal("authored update not applied")
	}
	out := renderToString(t, d)
	for _, want := range []string{`"api_key": "${K}"`, `"x_vendor"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("UpdateProvider lost required authored member %s", want)
		}
	}
	assertDiag(t, d.UpdateProvider("ghost", ProviderSpec{BaseURL: "http://h"}),
		CodeProviderNotFound, SubjectProvider, "ghost")

	before := renderToString(t, d)
	assertDiag(t, d.UpdateProvider("p", ProviderSpec{BaseURL: "not a url"}),
		CodeProviderEndpointInvalid, SubjectProvider, "p")
	if renderToString(t, d) != before {
		t.Fatal("failed UpdateProvider changed the draft")
	}
}

func TestRemoveProvider(t *testing.T) {
	d := newTestDoc(t)
	assertDiag(t, d.RemoveProvider("p"), CodeProviderInUse, SubjectRole, "agent")
	if err := d.AddProvider("q", ProviderSpec{BaseURL: "http://other:1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetRoleModel("agent",
		ModelFacts{Key: provider.ModelKey{Provider: "q", Model: "m"}, Type: "dense"},
		SetRoleModelOpts{ConfirmUnknown: true}); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveProvider("p"); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Config().Providers["p"]; ok {
		t.Fatal("provider p survived removal")
	}
	// The last provider is still referenced, so provider_in_use wins.
	assertDiag(t, d.RemoveProvider("q"), CodeProviderInUse, SubjectRole, "agent")
}

// Removing the last provider of an unreferenced (zero-model) config fails at
// the FINALIZE stage (provider_required), and that failure is transactional:
// the draft renders identically before and after.
func TestRemoveProviderFinalizeFailureIsTransactional(t *testing.T) {
	d, err := ParseDocument([]byte(`{"providers":{"p":{"base_url":"http://h"}}}`),
		Origin{Source: OriginExplicit}, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := renderToString(t, d)
	assertDiag(t, d.RemoveProvider("p"), CodeProviderRequired, SubjectNone, "")
	if renderToString(t, d) != before {
		t.Fatal("failed RemoveProvider changed the draft")
	}
}

func TestAuthoredProviderIsAPIKeyFreeAuthoredViewAndWorksReadOnly(t *testing.T) {
	d := newTestDoc(t)
	spec, err := d.AuthoredProvider("p")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Timeout.Duration != 0 || spec.APIFormat != "" {
		t.Fatal("effective defaults leaked into authored view")
	}
	_, err = d.AuthoredProvider("ghost")
	assertDiag(t, err, CodeProviderNotFound, SubjectProvider, "ghost")

	roRaw := strings.Replace(rawValid, `"base_url": "http://localhost:1234"`,
		`"base_url":"http://localhost:1234","BASE_URL":"http://localhost:1234"`, 1)
	ro, err := parseRaw(t, roRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.AuthoredProvider("p"); err != nil {
		t.Fatalf("read-only document must still expose authored backend base: %v", err)
	}
}

func TestSetClearProviderAPIKey(t *testing.T) {
	t.Setenv("GO_LLM_TEST_UNSET_VAR", "")
	t.Setenv("KEY_REF", "expanded-test-value")
	// Seed an unknown member on the provider entry: key mutations must
	// carry authored members they do not understand.
	raw := strings.Replace(rawValid, `"base_url": "http://localhost:1234"`,
		`"base_url":"http://localhost:1234","x_vendor":{"a":1}`, 1)
	d, perr := parseRaw(t, raw)
	if perr != nil {
		t.Fatal(perr)
	}
	assertDiag(t, d.SetProviderAPIKey("p", ""), CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.SetProviderAPIKey("ghost", "value"),
		CodeProviderNotFound, SubjectProvider, "ghost")

	if err := d.SetProviderAPIKey("p", "sk-test-only"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(renderToString(t, d), `"api_key": "sk-test-only"`) {
		t.Fatal("raw key did not enter authored render")
	}
	if !strings.Contains(renderToString(t, d), `"x_vendor"`) {
		t.Fatal("SetProviderAPIKey lost unknown authored member")
	}

	before := renderToString(t, d)
	err := d.SetProviderAPIKey("p", "${GO_LLM_TEST_UNSET_VAR}")
	assertDiag(t, err, CodeKeyReferenceUnavailable, SubjectProvider, "p")
	if renderToString(t, d) != before {
		t.Fatal("failed SetProviderAPIKey changed the draft")
	}
	if strings.Contains(err.Error(), "sk-test-only") {
		t.Fatal("prior key leaked into error text")
	}
	errMalformed := d.SetProviderAPIKey("p", "prefix${broken")
	assertDiag(t, errMalformed, CodeKeyReferenceMalformed, SubjectProvider, "p")
	if strings.Contains(errMalformed.Error(), "prefix${broken") {
		t.Fatal("submitted key value leaked into error text")
	}

	if err := d.SetProviderAPIKey("p", "${KEY_REF}"); err != nil {
		t.Fatal(err)
	}
	if d.Config().Providers["p"].APIKey != "expanded-test-value" {
		t.Fatal("effective view did not expand the authored environment reference")
	}
	rendered := renderToString(t, d)
	if !strings.Contains(rendered, "${KEY_REF}") ||
		strings.Contains(rendered, "expanded-test-value") {
		t.Fatal("render did not preserve the authored environment reference")
	}

	if err := d.ClearProviderAPIKey("p"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(renderToString(t, d), `"api_key"`) {
		t.Fatal("cleared key survived render")
	}
	if !strings.Contains(renderToString(t, d), `"x_vendor"`) {
		t.Fatal("ClearProviderAPIKey lost unknown authored member")
	}
	if err := d.ClearProviderAPIKey("p"); err != nil {
		t.Fatalf("clearing a keyless provider must succeed: %v", err)
	}
}
