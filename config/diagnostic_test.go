package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestDiagnosticOfWrappedChain(t *testing.T) {
	base := fmt.Errorf("config: model %q: type is required", "agent")
	err := fmt.Errorf("outer: %w", diagWrap(CodeModelInvalid, SubjectRole, "agent", base))
	d, ok := DiagnosticOf(err)
	if !ok {
		t.Fatal("expected diagnostic")
	}
	if d.Code != CodeModelInvalid || d.SubjectKind != SubjectRole || d.Subject != "agent" {
		t.Fatalf("got %+v", d)
	}
}

func TestDiagnosticErrorTextUnchanged(t *testing.T) {
	base := fmt.Errorf("config: provider %q: base_url is required", "p")
	err := diagWrap(CodeProviderEndpointInvalid, SubjectProvider, "p", base)
	if err.Error() != base.Error() {
		t.Fatalf("wrapper altered message: %q", err.Error())
	}
	if !errors.Is(err, base) {
		t.Fatal("wrapper must unwrap to base")
	}
}

func TestDiagnosticOfSentinels(t *testing.T) {
	cases := []struct {
		err  error
		code ErrorCode
	}{
		{fmt.Errorf("x: %w", ErrRevisionConflict), CodeRevisionConflict},
		{fmt.Errorf("x: %w", ErrDurabilityUncertain), CodeDurabilityUncertain},
		{fmt.Errorf("x: %w", ErrConfigNotFound), CodeConfigNotFound},
	}
	for _, tc := range cases {
		d, ok := DiagnosticOf(tc.err)
		if !ok || d.Code != tc.code {
			t.Fatalf("%v: got %+v ok=%v", tc.err, d, ok)
		}
	}
	if _, ok := DiagnosticOf(errors.New("plain")); ok {
		t.Fatal("plain error must not classify")
	}
	if _, ok := DiagnosticOf(nil); ok {
		t.Fatal("nil must not classify")
	}
}

func TestSanitizeSubjectOrderAndBound(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"clean", "agent", "agent"},
		{"control", "a\x00b\u202ec", "a�b�c"}, // Cc then bidi Cf
		{"invalid-utf8", "a\xffb", "a�b"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := sanitizeSubject(tc.in); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
	// Growth-then-truncate: 62 ASCII + 1 control byte -> repair grows to 65
	// bytes (U+FFFD is 3 bytes), truncation walks back over the partial
	// U+FFFD to exactly the 62 ASCII bytes.
	if got := sanitizeSubject(strings.Repeat("a", 62) + "\x00"); got != strings.Repeat("a", 62) {
		t.Fatalf("growth case: got %q", got)
	}
	// 2-byte runes: 80 bytes in, the cut at 64 lands on a rune boundary.
	if got := sanitizeSubject(strings.Repeat("é", 40)); got != strings.Repeat("é", 32) {
		t.Fatalf("2-byte case: got %q", got)
	}
	// 4-byte runes: 68 bytes in, exactly 16 runes (64 bytes) survive.
	if got := sanitizeSubject(strings.Repeat("🜁", 17)); got != strings.Repeat("🜁", 16) {
		t.Fatalf("4-byte case: got %q", got)
	}
	// Max walk-back: 65 bytes in, the cut at 64 lands 3 bytes into the
	// final rune (which starts at byte 61), so truncation drops it whole.
	if got := sanitizeSubject("a" + strings.Repeat("🜁", 16)); got != "a"+strings.Repeat("🜁", 15) {
		t.Fatalf("max walk-back case: got %q", got)
	}
}

func TestDiagnosticOfPrecedence(t *testing.T) {
	inner := diagWrap(CodeModelInvalid, SubjectRole, "inner", errors.New("base"))
	outer := diagWrap(CodeProviderNotFound, SubjectProvider, "outer", inner)
	d, ok := DiagnosticOf(outer)
	if !ok || d.Code != CodeProviderNotFound || d.SubjectKind != SubjectProvider || d.Subject != "outer" {
		t.Fatalf("double wrap: got %+v ok=%v", d, ok)
	}
	// A diagError anywhere in the chain beats the sentinel fallback.
	mixed := diagWrap(CodeTargetExists, SubjectNone, "", fmt.Errorf("x: %w", ErrRevisionConflict))
	d, ok = DiagnosticOf(mixed)
	if !ok || d.Code != CodeTargetExists {
		t.Fatalf("mixed chain: got %+v ok=%v", d, ok)
	}
}

func TestDiagnosticSitesLoadAndSave(t *testing.T) {
	// discovery: GO_LLM_CONFIG set but empty
	t.Setenv("GO_LLM_CONFIG", "")
	_, err := DefaultDocument()
	assertDiag(t, err, CodeConfigDiscoveryInvalid, SubjectNone, "")

	// io: DefaultDocument read failure (env points at a missing file)
	t.Setenv("GO_LLM_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	_, err = DefaultDocument()
	assertDiag(t, err, CodeIO, SubjectNone, "")

	// parse failure
	_, err = NewDocumentFromBytes([]byte("{nope"), Origin{Source: OriginExplicit})
	assertDiag(t, err, CodeParseError, SubjectNone, "")

	// io: LoadDocument on a missing path
	missing := filepath.Join(t.TempDir(), "absent.json")
	_, err = LoadDocument(missing)
	assertDiag(t, err, CodeIO, SubjectNone, "")
	// Legacy Load is a Firn Phase-2 entry point too; it gets the same codes.
	_, err = Load(missing)
	assertDiag(t, err, CodeIO, SubjectNone, "")
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(bad)
	assertDiag(t, err, CodeParseError, SubjectNone, "")

	// target_exists: SaveNew onto an existing file
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "m.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	assertDiag(t, d.SaveNew(p), CodeTargetExists, SubjectNone, "")

	// revision_conflict via wrapped sentinel at source
	assertDiag(t, d.SaveReplace(p, "deadbeef"), CodeRevisionConflict, SubjectNone, "")
}

func TestDiagnosticSitesValidate(t *testing.T) {
	t.Setenv("GO_LLM_TEST_UNSET_VAR", "")
	// Each case: a minimal config JSON provoking exactly one site.
	cases := []struct {
		name    string
		mutate  func(m map[string]any) // applied to a valid base config map
		code    ErrorCode
		kind    SubjectKind
		subject string
	}{
		{"provider_required", func(m map[string]any) { m["providers"] = map[string]any{} }, CodeProviderRequired, SubjectNone, ""},
		{"provider_name_invalid", func(m map[string]any) {
			m["providers"].(map[string]any)["bad/name"] = map[string]any{"base_url": "http://h"}
		}, CodeProviderNameInvalid, SubjectProvider, "bad/name"},
		{"endpoint_missing", func(m map[string]any) {
			m["providers"].(map[string]any)["p"].(map[string]any)["base_url"] = ""
		}, CodeProviderEndpointInvalid, SubjectProvider, "p"},
		{"endpoint_parse", func(m map[string]any) {
			m["providers"].(map[string]any)["p"].(map[string]any)["base_url"] = "://"
		}, CodeProviderEndpointInvalid, SubjectProvider, "p"},
		{"endpoint_scheme_host", func(m map[string]any) {
			m["providers"].(map[string]any)["p"].(map[string]any)["base_url"] = "localhost:1234"
		}, CodeProviderEndpointInvalid, SubjectProvider, "p"},
		{"format_invalid", func(m map[string]any) {
			m["providers"].(map[string]any)["p"].(map[string]any)["api_format"] = "ollma"
		}, CodeProviderFormatInvalid, SubjectProvider, "p"},
		{"model_name", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["name"] = ""
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"model_type_missing", func(m map[string]any) {
			delete(m["models"].(map[string]any)["agent"].(map[string]any), "type")
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"model_type_invalid", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["type"] = "quantum"
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"context_window", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["context_window"] = -1
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"dimensions", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["dimensions"] = -1
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"temperature", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["options"] = map[string]any{"temperature": -1}
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"top_p", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["options"] = map[string]any{"top_p": 0}
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"top_k", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["options"] = map[string]any{"top_k": -1}
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"capability_unknown", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["capabilities"] = []string{"bogus"}
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"capability_noncanonical_alias", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["capabilities"] = []string{"completion"}
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"embedding_capability", func(m map[string]any) {
			model := m["models"].(map[string]any)["agent"].(map[string]any)
			model["type"], model["capabilities"] = "embedding", []string{"chat"}
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"slots_negative", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["slots"] = -1
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"think_mode", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["think_mode"] = "sometimes"
		}, CodeThinkInvalid, SubjectRole, "agent"},
		{"think_tags_missing", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["think_tags"] = map[string]any{"open": "<t>"}
		}, CodeThinkInvalid, SubjectRole, "agent"},
		{"think_tags_same", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["think_tags"] = map[string]any{"open": "<t>", "close": "<t>"}
		}, CodeThinkInvalid, SubjectRole, "agent"},
		{"think_tags_prefix", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["think_tags"] = map[string]any{"open": "t", "close": "</t>"}
		}, CodeThinkInvalid, SubjectRole, "agent"},
		{"implicit_provider_missing", func(m map[string]any) {
			delete(m["models"].(map[string]any)["agent"].(map[string]any), "provider")
		}, CodeProviderNotFound, SubjectRole, "agent"},
		{"model_provider_missing", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["provider"] = "ghost"
		}, CodeProviderNotFound, SubjectRole, "agent"},
		{"fallback_self", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["fallbacks"] = []string{"agent"}
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"fallback_unknown", func(m map[string]any) {
			m["models"].(map[string]any)["agent"].(map[string]any)["fallbacks"] = []string{"ghost"}
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"fallback_incompatible", func(m map[string]any) {
			models := m["models"].(map[string]any)
			models["embed"] = map[string]any{"name": "e", "type": "embedding", "provider": "p"}
			models["agent"].(map[string]any)["fallbacks"] = []string{"embed"}
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"fallback_cycle", func(m map[string]any) {
			models := m["models"].(map[string]any)
			models["agent"].(map[string]any)["fallbacks"] = []string{"b"}
			models["b"] = map[string]any{"name": "b", "type": "dense", "provider": "p", "fallbacks": []string{"agent"}}
		}, CodeModelInvalid, SubjectRole, "agent"},
		{"defaults_unknown_role", func(m map[string]any) {
			m["defaults"].(map[string]any)["chat"] = "ghost"
		}, CodeDefaultsInvalid, SubjectUseCase, "chat"},
		{"key_ref_malformed", func(m map[string]any) {
			m["providers"].(map[string]any)["p"].(map[string]any)["api_key"] = "${bad"
		}, CodeKeyReferenceMalformed, SubjectProvider, "p"},
		{"key_ref_bad_name", func(m map[string]any) {
			m["providers"].(map[string]any)["p"].(map[string]any)["api_key"] = "${1BAD}"
		}, CodeKeyReferenceMalformed, SubjectProvider, "p"},
		{"key_ref_unavailable", func(m map[string]any) {
			m["providers"].(map[string]any)["p"].(map[string]any)["api_key"] = "${GO_LLM_TEST_UNSET_VAR}"
		}, CodeKeyReferenceUnavailable, SubjectProvider, "p"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := validBaseConfigMap() // helper: provider "p", model "agent", defaults{"agent":"agent"}
			tc.mutate(base)
			raw, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			_, derr := NewDocumentFromBytes(raw, Origin{Source: OriginExplicit})
			assertDiag(t, derr, tc.code, tc.kind, tc.subject)
		})
	}
}

func TestDiagnosticSitesMutations(t *testing.T) {
	d := newTestDoc(t)
	assertDiag(t, d.BindUseCase("", "agent"), CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.BindUseCase("chat", "ghost"), CodeRoleNotFound, SubjectRole, "ghost")
	_, err := d.SetRoleModel("agent", ModelFacts{Type: "dense"}, SetRoleModelOpts{})
	assertDiag(t, err, CodeInvalidArgument, SubjectNone, "")
	_, err = d.SetRoleModel("ghost", ModelFacts{Key: provider.ModelKey{Provider: "p", Model: "m"}, Type: "dense"}, SetRoleModelOpts{})
	assertDiag(t, err, CodeRoleNotFound, SubjectRole, "ghost")
	_, err = d.SetRoleModel("agent", ModelFacts{Key: provider.ModelKey{Provider: "nope", Model: "m"}, Type: "dense"}, SetRoleModelOpts{})
	assertDiag(t, err, CodeProviderNotFound, SubjectProvider, "nope")
	_, err = d.SetRoleModel("agent", ModelFacts{Key: provider.ModelKey{Provider: "p", Model: "m"}, Type: "quantum"}, SetRoleModelOpts{})
	assertDiag(t, err, CodeInvalidArgument, SubjectNone, "")
	_, err = d.SetRoleModel("agent", ModelFacts{Key: provider.ModelKey{Provider: "p", Model: "m"}, Type: "dense"},
		SetRoleModelOpts{Capabilities: []string{"not-a-capability"}, ConfirmUnknown: true})
	assertDiag(t, err, CodeModelInvalid, SubjectRole, "agent")
	_, err = d.SetRoleModel("agent", ModelFacts{Key: provider.ModelKey{Provider: "p", Model: "m"}, Type: "dense"}, SetRoleModelOpts{}) // no ConfirmUnknown
	assertDiag(t, err, CodeEligibilityUnknown, SubjectRole, "agent")
	_, err = d.SetRoleModel("agent", ModelFacts{Key: provider.ModelKey{Provider: "p", Model: "m"}, Type: "dense"},
		SetRoleModelOpts{Requirements: map[string]provider.Capability{"agent": provider.CapChat}, KnownMask: provider.CapChat})
	assertDiag(t, err, CodeEligibilityIneligible, SubjectRole, "agent")

	facts := ModelFacts{Key: provider.ModelKey{Provider: "p", Model: "m"}, Type: "dense"}
	assertDiag(t, d.AddRoleModel("agent", facts, SetRoleModelOpts{ConfirmUnknown: true}),
		CodeRoleExists, SubjectRole, "agent")
	assertDiag(t, d.ForkRoleModel("agent", "agent", facts, ForkRoleModelOpts{}),
		CodeRoleExists, SubjectRole, "agent")
	assertDiag(t, d.ForkRoleModel("ghost", "x", facts, ForkRoleModelOpts{}),
		CodeRoleNotFound, SubjectRole, "ghost")
	assertDiag(t, d.UnbindUseCase("missing"),
		CodeUseCaseNotFound, SubjectUseCase, "missing")
	assertDiag(t, d.SetRoleOverrides(provider.ModelKey{Provider: "p", Model: "missing"},
		RoleOverrides{}, SetRoleOverridesOpts{ConfirmUnknown: true}),
		CodeRoleNotFound, SubjectNone, "")

	roles := loadTestDoc(t, roleFixture)
	err = roles.ForkRoleModel("agent", "agent-m", forkFacts(), forkOpts())
	assertDiag(t, err, CodeDropConfirmationRequired, SubjectRole, "agent")
	if drops, ok := DropSetOf(err); !ok ||
		!slices.Equal(drops, []string{"slots", "think_tags"}) {
		t.Fatalf("DropSetOf = %v, %v", drops, ok)
	}
	assertDiag(t, roles.RemoveRole("agent"),
		CodeRoleInUse, SubjectUseCase, "agent")
	if err := roles.UnbindUseCase("chat"); err != nil {
		t.Fatal(err)
	}
	assertDiag(t, roles.RemoveRole("fast"),
		CodeRoleInUse, SubjectRole, "agent")

	assertDiag(t, d.AddRoleModel("", facts, SetRoleModelOpts{}),
		CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.ForkRoleModel("agent", "x", facts,
		ForkRoleModelOpts{ConfirmDrops: []string{"bogus"}}),
		CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.SetRoleOverrides(provider.ModelKey{Provider: "p", Model: "m"},
		RoleOverrides{Capabilities: []string{}}, SetRoleOverridesOpts{}),
		CodeInvalidArgument, SubjectNone, "")

	ghostFacts := facts
	ghostFacts.Key.Provider = "ghost"
	assertDiag(t, d.AddRoleModel("x", ghostFacts, SetRoleModelOpts{ConfirmUnknown: true}),
		CodeProviderNotFound, SubjectProvider, "ghost")
	assertDiag(t, d.ForkRoleModel("agent", "x", ghostFacts, ForkRoleModelOpts{}),
		CodeProviderNotFound, SubjectProvider, "ghost")
	assertDiag(t, d.SetRoleOverrides(provider.ModelKey{Provider: "p", Model: "m"},
		RoleOverrides{Capabilities: []string{"bogus"}}, SetRoleOverridesOpts{}),
		CodeModelInvalid, SubjectRole, "agent")

	joinFacts := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m1"}, Type: "moe"}
	assertDiag(t, roles.AddRoleModel("joiner", joinFacts, SetRoleModelOpts{
		Requirements: map[string]provider.Capability{"agent": provider.CapChat},
		Capabilities: []string{"embed"}, ConfirmUnknown: true,
	}), CodeEligibilityIneligible, SubjectRole, "joiner")
	assertDiag(t, roles.SetRoleOverrides(
		provider.ModelKey{Provider: "local", Model: "m1"}, RoleOverrides{},
		SetRoleOverridesOpts{Requirements: map[string]provider.Capability{"agent": provider.CapChat}}),
		CodeEligibilityUnknown, SubjectRole, "agent")
}

// Site config.go's ParseCapsStrict round-trip is unreachable via config input
// (both checks share one vocabulary); this injection is its only coverage.
func TestDiagnosticCapabilityParserDriftGuard(t *testing.T) {
	// Mutates package state; config tests must stay non-parallel.
	validCapabilityNames["drift-only"] = true
	t.Cleanup(func() { delete(validCapabilityNames, "drift-only") })
	base := validBaseConfigMap()
	base["models"].(map[string]any)["agent"].(map[string]any)["capabilities"] = []string{"drift-only"}
	raw, _ := json.Marshal(base)
	_, err := NewDocumentFromBytes(raw, Origin{Source: OriginExplicit})
	assertDiag(t, err, CodeModelInvalid, SubjectRole, "agent")
}

// TestExportedErrorCodeSetMatchesContract pins the exported ErrorCode
// constant SET to the approved 31-code vocabulary by parsing the package
// source: adding, removing, renaming, or retyping an exported Code* const
// breaks this test. (parser.ParseDir is deprecated since Go 1.22; per-file
// ParseFile over os.ReadDir has the same semantics without SA1019.)
func TestExportedErrorCodeSetMatchesContract(t *testing.T) {
	want := map[string]bool{
		"config_not_found": true, "config_discovery_invalid": true,
		"io": true, "parse_error": true, "render_error": true,
		"duplicate_keys": true, "provider_required": true,
		"provider_name_invalid": true, "provider_endpoint_invalid": true,
		"provider_format_invalid": true, "slot_policy_invalid": true,
		"model_invalid": true, "think_invalid": true,
		"provider_not_found": true, "defaults_invalid": true,
		"key_reference_malformed": true, "key_reference_unavailable": true,
		"invalid_argument": true, "role_not_found": true,
		"provider_exists": true, "provider_in_use": true,
		"eligibility_ineligible": true, "eligibility_unknown": true,
		"selector_conflict": true, "target_exists": true,
		"revision_conflict": true, "durability_uncertain": true,
		"role_exists": true, "role_in_use": true,
		"use_case_not_found":         true,
		"drop_confirmation_required": true,
	}
	if len(want) != 31 {
		t.Fatalf("test contract has %d codes, want 31", len(want))
	}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				values := spec.(*ast.ValueSpec)
				typeName, explicitlyTyped := values.Type.(*ast.Ident)
				for i, name := range values.Names {
					// The exported Code* namespace is reserved for ErrorCode;
					// name unrelated consts differently.
					if !name.IsExported() || !strings.HasPrefix(name.Name, "Code") {
						continue
					}
					if !explicitlyTyped || typeName.Name != "ErrorCode" {
						t.Fatalf("%s must be explicitly typed ErrorCode", name.Name)
					}
					if i >= len(values.Values) {
						t.Fatalf("%s must have an explicit string literal", name.Name)
					}
					lit, ok := values.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s must have an explicit string literal", name.Name)
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					if got[value] {
						t.Fatalf("duplicate exported ErrorCode value %q", value)
					}
					got[value] = true
				}
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		var missing, extra []string
		for v := range want {
			if !got[v] {
				missing = append(missing, v)
			}
		}
		for v := range got {
			if !want[v] {
				extra = append(extra, v)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		t.Fatalf("exported ErrorCode set drifted (missing=%v extra=%v): a deliberate "+
			"vocabulary change must update this want set, the 31 count guard above, and the spec s6 code table",
			missing, extra)
	}
}

// DropSetOf extracts the computed drop set from a fork refusal through an
// arbitrary wrap chain, returns a copy (never an alias), and reports false
// for chains without a carrier.
func TestDropSetOf(t *testing.T) {
	base := diagWrap(CodeDropConfirmationRequired, SubjectRole, "agent",
		fmt.Errorf("config: fork role model %q: unconfirmed drops from %q: slots, think_tags", "agent-m", "agent"))
	in := []string{"slots", "think_tags"}
	err := fmt.Errorf("outer: %w", dropSetWrap(in, base))
	in[0] = "clobbered" // wrap must have copied; slices.Equal below goes red otherwise

	got, ok := DropSetOf(err)
	if !ok || !slices.Equal(got, []string{"slots", "think_tags"}) {
		t.Fatalf("DropSetOf = %v, %v", got, ok)
	}
	got[0] = "mutated"
	again, _ := DropSetOf(err)
	if again[0] != "slots" {
		t.Fatal("DropSetOf result aliases the carrier")
	}
	// The diagnostic survives the same chain, and the message is unchanged.
	assertDiag(t, err, CodeDropConfirmationRequired, SubjectRole, "agent")
	if !strings.Contains(err.Error(), "unconfirmed drops from") {
		t.Fatalf("wrapped message lost: %q", err.Error())
	}
	if _, ok := DropSetOf(fmt.Errorf("plain")); ok {
		t.Fatal("false positive on plain error")
	}
}

// TestDiagnosticRemainingSources provokes the two diagnostic sources no
// other test reaches: render_error (unparseable retained raw bytes) and the
// durability_uncertain source-wrap (dir-sync failure after publication).
func TestDiagnosticRemainingSources(t *testing.T) {
	_, err := renderCanonical([]byte("{"), &Config{})
	assertDiag(t, err, CodeRenderError, SubjectNone, "")

	d := loadTestDoc(t, rawWithUnknown)
	original := syncDir
	syncDir = func(string) error { return errors.New("injected sync failure") }
	t.Cleanup(func() { syncDir = original })
	err = d.SaveNew(filepath.Join(t.TempDir(), "durability.json"))
	assertDiag(t, err, CodeDurabilityUncertain, SubjectNone, "")
}

func validBaseConfigMap() map[string]any {
	return map[string]any{
		"providers": map[string]any{
			"p": map[string]any{"base_url": "http://localhost:1234"},
		},
		"models": map[string]any{
			"agent": map[string]any{"name": "m", "type": "dense", "provider": "p"},
		},
		"defaults": map[string]any{"agent": "agent"},
	}
}

func assertDiag(t *testing.T, err error, code ErrorCode, kind SubjectKind, subject string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error with code %s, got nil", code)
	}
	d, ok := DiagnosticOf(err)
	if !ok || d.Code != code || d.SubjectKind != kind || d.Subject != subject {
		t.Fatalf("err=%v: got %+v ok=%v want {%s %s %q}", err, d, ok, code, kind, subject)
	}
}
