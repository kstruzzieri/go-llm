package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func parseRaw(t *testing.T, raw string) (*Document, error) {
	t.Helper()
	return ParseDocument([]byte(raw), Origin{Source: OriginExplicit}, DocumentOptions{})
}

const rawValid = `{
  "providers": {"p": {"base_url": "http://localhost:1234"}},
  "models": {"agent": {"name": "m", "type": "dense", "provider": "p"}},
  "defaults": {"agent": "agent"}
}`

func TestReadOnlyDetection(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		kind    SubjectKind
		subject string
	}{
		{"top-level exact duplicate",
			`{"providers":{"p":{"base_url":"http://h"}},"providers":{"p":{"base_url":"http://h"}},"models":{"agent":{"name":"m","type":"dense","provider":"p"}},"defaults":{"agent":"agent"}}`,
			SubjectNone, ""},
		{"top-level known-tag alias",
			strings.Replace(rawValid, `"providers":`, `"Providers":{"p":{"base_url":"http://h"}},"providers":`, 1),
			SubjectNone, ""},
		{"providers-map exact duplicate",
			`{"providers":{"p":{"base_url":"http://h"},"p":{"base_url":"http://h"}},"models":{"agent":{"name":"m","type":"dense","provider":"p"}},"defaults":{"agent":"agent"}}`,
			SubjectProvider, "p"},
		{"provider-entry known-tag alias",
			strings.Replace(rawValid, `"base_url": "http://localhost:1234"`,
				`"base_url":"http://localhost:1234","BASE_URL":"http://localhost:1234"`, 1),
			SubjectProvider, "p"},
		{"provider-entry exact duplicate",
			strings.Replace(rawValid, `"base_url": "http://localhost:1234"`,
				`"base_url":"http://localhost:1234","base_url":"http://localhost:1234"`, 1),
			SubjectProvider, "p"},
		{"models-map exact duplicate",
			`{"providers":{"p":{"base_url":"http://h"}},"models":{"agent":{"name":"m","type":"dense","provider":"p"},"agent":{"name":"m","type":"dense","provider":"p"}},"defaults":{"agent":"agent"}}`,
			SubjectRole, "agent"},
		{"model-entry known-tag alias",
			strings.Replace(rawValid, `"type": "dense"`, `"type":"dense","Type":"dense"`, 1),
			SubjectRole, "agent"},
		{"model-entry exact duplicate",
			strings.Replace(rawValid, `"type": "dense"`, `"type":"dense","type":"dense"`, 1),
			SubjectRole, "agent"},
		{"defaults-map exact duplicate",
			`{"providers":{"p":{"base_url":"http://h"}},"models":{"agent":{"name":"m","type":"dense","provider":"p"}},"defaults":{"agent":"agent","agent":"agent"}}`,
			SubjectUseCase, "agent"},
		{"options known-tag alias",
			strings.Replace(rawValid, `"provider": "p"`,
				`"provider":"p","options":{"top_p":0.5,"TOP_P":0.5}`, 1),
			SubjectRole, "agent"},
		{"think-tags known-tag alias",
			strings.Replace(rawValid, `"provider": "p"`,
				`"provider":"p","think_tags":{"open":"<t>","close":"</t>","Open":"<t>"}`, 1),
			SubjectRole, "agent"},
		{"nested known-struct exact duplicate",
			strings.Replace(rawValid, `"provider": "p"`,
				`"provider":"p","think_tags":{"open":"<t>","open":"<t>","close":"</t>"}`, 1),
			SubjectRole, "agent"},
		{"first collision in document order wins",
			`{"models":{"agent":{"name":"m","type":"dense","provider":"p","think_tags":{"open":"<t>","open":"<t>","close":"</t>"}}},"providers":{"p":{"base_url":"http://h"}},"defaults":{"agent":"agent"},"defaults":{"agent":"agent"}}`,
			SubjectRole, "agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := parseRaw(t, tc.raw)
			if err != nil {
				t.Fatalf("collision documents still load: %v", err)
			}
			diag, readOnly := d.ReadOnly()
			if !readOnly {
				t.Fatal("expected read-only")
			}
			if diag.Code != CodeDuplicateKeys || diag.SubjectKind != tc.kind || diag.Subject != tc.subject {
				t.Fatalf("got %+v", diag)
			}
		})
	}
}

func TestReadOnlyExemptionsAndUnknownDuplicatePreservation(t *testing.T) {
	// Map keys are case-sensitive role identities.
	d, err := parseRaw(t, strings.Replace(rawValid,
		`"agent": {"name": "m", "type": "dense", "provider": "p"}`,
		`"agent":{"name":"m","type":"dense","provider":"p"},"Agent":{"name":"m","type":"dense","provider":"p"}`, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, readOnly := d.ReadOnly(); readOnly {
		t.Fatal("map-level case variants are distinct entries")
	}

	// Unknown siblings differing only by case are not aliases of schema tags.
	d, err = parseRaw(t, strings.Replace(rawValid,
		`"base_url": "http://localhost:1234"`,
		`"base_url":"http://localhost:1234","x_vendor":1,"X_VENDOR":2`, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, readOnly := d.ReadOnly(); readOnly {
		t.Fatal("unknown case variants must stay writable")
	}

	// Duplicates inside an unknown subtree are exempt AND both occurrences
	// survive canonical save/reload.
	d, err = parseRaw(t, strings.Replace(rawValid, `"defaults": {"agent": "agent"}`,
		`"defaults":{"agent":"agent"},"x_custom":{"k":1,"k":2}`, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, readOnly := d.ReadOnly(); readOnly {
		t.Fatal("unknown-subtree duplicates are exempt")
	}
	path := filepath.Join(t.TempDir(), "unknown-duplicates.json")
	if err := d.SaveNew(path); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(saved), `"k"`) != 2 {
		t.Fatal("canonical save lost an unknown-subtree duplicate")
	}
	reloaded, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, readOnly := reloaded.ReadOnly(); readOnly {
		t.Fatal("saved unknown subtree became read-only")
	}
}

func TestReadOnlyGatesExistingMutatorsAndEverySave(t *testing.T) {
	roRaw := strings.Replace(rawValid, `"base_url": "http://localhost:1234"`,
		`"base_url":"http://localhost:1234","BASE_URL":"http://localhost:1234"`, 1)
	newRO := func(t *testing.T) *Document {
		t.Helper()
		d, err := parseRaw(t, roRaw)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	dir := t.TempDir()
	stale := filepath.Join(dir, "stale.json")
	if err := os.WriteFile(stale, []byte(rawValid), 0o600); err != nil {
		t.Fatal(err)
	}
	facts := ModelFacts{Key: provider.ModelKey{Provider: "p", Model: "m"}, Type: "dense"}
	cases := []struct {
		name string
		run  func(*Document) error
	}{
		{"BindUseCase", func(d *Document) error { return d.BindUseCase("chat", "agent") }},
		{"BindUseCase-empty", func(d *Document) error { return d.BindUseCase("", "agent") }},
		{"SetRoleModel-empty-facts", func(d *Document) error {
			_, err := d.SetRoleModel("agent", ModelFacts{Type: "dense"}, SetRoleModelOpts{})
			return err
		}},
		{"SetRoleModel", func(d *Document) error {
			_, err := d.SetRoleModel("agent", facts, SetRoleModelOpts{ConfirmUnknown: true})
			return err
		}},
		{"SaveNew", func(d *Document) error { return d.SaveNew(filepath.Join(dir, "new.json")) }},
		{"SaveNewAs", func(d *Document) error { return d.SaveNewAs(filepath.Join(dir, "new-as.json"), OriginProfile) }},
		{"SaveReplace-missing", func(d *Document) error {
			return d.SaveReplace(filepath.Join(dir, "missing.json"), "stale")
		}},
		{"SaveReplace-stale", func(d *Document) error { return d.SaveReplace(stale, "stale") }},
		{"SaveReplaceAs", func(d *Document) error { return d.SaveReplaceAs(stale, "stale", OriginProfile) }},
		{"AddProvider", func(d *Document) error { return d.AddProvider("q", ProviderSpec{BaseURL: "http://h"}) }},
		{"AddProvider-invalid-name", func(d *Document) error { return d.AddProvider("bad/name", ProviderSpec{BaseURL: "http://h"}) }},
		{"UpdateProvider", func(d *Document) error { return d.UpdateProvider("p", ProviderSpec{BaseURL: "http://h"}) }},
		{"RemoveProvider", func(d *Document) error { return d.RemoveProvider("p") }},
		{"SetProviderAPIKey", func(d *Document) error { return d.SetProviderAPIKey("p", "k") }},
		{"SetProviderAPIKey-empty", func(d *Document) error { return d.SetProviderAPIKey("p", "") }},
		{"ClearProviderAPIKey", func(d *Document) error { return d.ClearProviderAPIKey("p") }},
		{"AddRoleModel", func(d *Document) error {
			return d.AddRoleModel("x", facts, SetRoleModelOpts{ConfirmUnknown: true})
		}},
		{"AddRoleModel-empty", func(d *Document) error {
			return d.AddRoleModel("", ModelFacts{}, SetRoleModelOpts{})
		}},
		{"ForkRoleModel", func(d *Document) error {
			return d.ForkRoleModel("agent", "x", facts, ForkRoleModelOpts{
				SetRoleModelOpts: SetRoleModelOpts{ConfirmUnknown: true}})
		}},
		{"ForkRoleModel-bad-drops", func(d *Document) error {
			return d.ForkRoleModel("agent", "x", facts, ForkRoleModelOpts{
				ConfirmDrops: []string{"bogus"}})
		}},
		{"UnbindUseCase", func(d *Document) error { return d.UnbindUseCase("agent") }},
		{"UnbindUseCase-empty", func(d *Document) error { return d.UnbindUseCase("") }},
		{"RemoveRole", func(d *Document) error { return d.RemoveRole("agent") }},
		{"RemoveRole-empty", func(d *Document) error { return d.RemoveRole("") }},
		{"SetRoleOverrides", func(d *Document) error {
			return d.SetRoleOverrides(provider.ModelKey{Provider: "p", Model: "m"},
				RoleOverrides{}, SetRoleOverridesOpts{ConfirmUnknown: true})
		}},
		{"SetRoleOverrides-empty", func(d *Document) error {
			return d.SetRoleOverrides(provider.ModelKey{}, RoleOverrides{}, SetRoleOverridesOpts{})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDiag(t, tc.run(newRO(t)), CodeDuplicateKeys, SubjectProvider, "p")
		})
	}
	t.Run("ClearAllProviderAPIKeys", func(t *testing.T) {
		err := newRO(t).ClearAllProviderAPIKeys()
		assertDiag(t, err, CodeDuplicateKeys, SubjectNone, "")
		if strings.Contains(err.Error(), `provider "p"`) {
			t.Errorf("ClearAllProviderAPIKeys() error = %q, want no provider identity", err)
		}
		for cause := errors.Unwrap(err); cause != nil; cause = errors.Unwrap(cause) {
			if diag, ok := DiagnosticOf(cause); ok && diag.Subject != "" {
				t.Errorf("ClearAllProviderAPIKeys() unwrap diagnostic = %+v, want no subject", diag)
			}
			if strings.Contains(cause.Error(), `provider "p"`) {
				t.Errorf("ClearAllProviderAPIKeys() unwrap error = %q, want no provider identity", cause)
			}
		}
	})

	d := newRO(t)
	d.mu.Lock()
	_, err := d.canonicalBytes()
	d.mu.Unlock()
	assertDiag(t, err, CodeDuplicateKeys, SubjectProvider, "p")
}

func TestSectionEntryKindCoversAllRootSections(t *testing.T) {
	root := configSchema()
	for tag, child := range root.known {
		if child != nil && child.isMap() {
			if sectionEntryKind(tag) == SubjectNone {
				t.Fatalf("map section %q has no entry SubjectKind: extend sectionEntryKind consciously", tag)
			}
		}
	}
}
