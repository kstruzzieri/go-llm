package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// priceCfg builds a one-provider Config whose single role "agent" carries p.
func priceCfg(p *ModelPrice) *Config {
	return &Config{
		Providers: map[string]ProviderConfig{"p": {BaseURL: "http://localhost:1234"}},
		Models:    map[string]ModelConfig{"agent": {Name: "m", Provider: "p", Type: "dense", Price: p}},
		Defaults:  map[string]string{"agent": "agent"},
	}
}

func TestModelPriceValidation(t *testing.T) {
	tests := []struct {
		name  string
		price *ModelPrice
		want  string // "" = valid, else substring naming the rejected field
	}{
		{"no price", nil, ""},
		{"full price", &ModelPrice{Input: 3, Output: 15, CachedInput: provider.Ptr(0.3), CacheWrite: provider.Ptr(3.75)}, ""},
		{"optional fields absent", &ModelPrice{Input: 3, Output: 15}, ""},
		{"zero is a legal price", &ModelPrice{Input: 0, Output: 0, CachedInput: provider.Ptr(0.0), CacheWrite: provider.Ptr(0.0)}, ""},
		{"negative input", &ModelPrice{Input: -1, Output: 15}, "price.input"},
		{"negative output", &ModelPrice{Input: 3, Output: -0.01}, "price.output"},
		{"negative cached_input", &ModelPrice{Input: 3, Output: 15, CachedInput: provider.Ptr(-0.3)}, "price.cached_input"},
		{"negative cache_write", &ModelPrice{Input: 3, Output: 15, CacheWrite: provider.Ptr(-3.75)}, "price.cache_write"},
		{"+Inf input", &ModelPrice{Input: math.Inf(1), Output: 15}, "price.input"},
		{"-Inf output", &ModelPrice{Input: 3, Output: math.Inf(-1)}, "price.output"},
		{"NaN input", &ModelPrice{Input: math.NaN(), Output: 15}, "price.input"},
		{"NaN cached_input", &ModelPrice{Input: 3, Output: 15, CachedInput: provider.Ptr(math.NaN())}, "price.cached_input"},
		{"+Inf cache_write", &ModelPrice{Input: 3, Output: 15, CacheWrite: provider.Ptr(math.Inf(1))}, "price.cache_write"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A constructed Config (not decoded from JSON) is the only way to
			// reach NaN and Inf, so validation must not live in the decoder.
			err := priceCfg(tt.price).validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			assertDiag(t, err, CodeModelInvalid, SubjectRole, "agent")
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

func TestModelPriceLoadDecodesAndRejects(t *testing.T) {
	const tmpl = `{
  "providers": {"p": {"base_url": "http://localhost:1234"}},
  "models": {"agent": {"name": "m", "provider": "p", "type": "dense", "price": %s}},
  "defaults": {"agent": "agent"}
}`
	t.Run("optional fields stay nil when absent", func(t *testing.T) {
		cfg, err := Load(writeTempJSON(t, fmt.Sprintf(tmpl, `{"input": 3, "output": 15}`)))
		if err != nil {
			t.Fatal(err)
		}
		p := cfg.Models["agent"].Price
		if p == nil || p.Input != 3 || p.Output != 15 || p.CachedInput != nil || p.CacheWrite != nil {
			t.Fatalf("price = %+v, want {3 15 nil nil}", p)
		}
	})
	t.Run("explicit zero cached_input is kept distinct from absent", func(t *testing.T) {
		cfg, err := Load(writeTempJSON(t, fmt.Sprintf(tmpl, `{"input": 3, "output": 15, "cached_input": 0}`)))
		if err != nil {
			t.Fatal(err)
		}
		p := cfg.Models["agent"].Price
		if p == nil || p.CachedInput == nil || *p.CachedInput != 0 || p.CacheWrite != nil {
			t.Fatalf("price = %+v, want cached_input pointing at 0 and cache_write nil", p)
		}
	})
	t.Run("negative value fails Load with the role", func(t *testing.T) {
		_, err := Load(writeTempJSON(t, fmt.Sprintf(tmpl, `{"input": -3, "output": 15}`)))
		assertDiag(t, err, CodeModelInvalid, SubjectRole, "agent")
	})
}

// A price must name both rates and only known members: "{}", a missing
// rate or a typo'd member would otherwise decode to a legal $0 price and
// print a dollar total with no exclusion. The refusal names the role under
// CodeModelInvalid on both decode paths (Load and Document); a malformed
// value stays a parse error, as before.
func TestModelPriceStrictDecode(t *testing.T) {
	const tmpl = `{
  "providers": {"p": {"base_url": "http://localhost:1234"}},
  "models": {"agent": {"name": "m", "provider": "p", "type": "dense", "price": %s}},
  "defaults": {"agent": "agent"}
}`
	for _, tc := range []struct {
		name, price string
		code        ErrorCode // "" = accepted
		want        string    // substring of the error
	}{
		{"empty object", `{}`, CodeModelInvalid, "price.input is required"},
		{"typo'd member", `{"input_per_mtok": 3, "output": 15}`, CodeModelInvalid, `price has unknown member "input_per_mtok"`},
		{"unknown member beside both rates", `{"input": 3, "output": 15, "future_price_field": "x"}`, CodeModelInvalid, `price has unknown member "future_price_field"`},
		{"case variant is not the member", `{"Input": 3, "output": 15}`, CodeModelInvalid, `price has unknown member "Input"`},
		{"missing output", `{"input": 3}`, CodeModelInvalid, "price.output is required"},
		{"null input", `{"input": null, "output": 15}`, CodeModelInvalid, "price.input is required"},
		{"wrong type stays a parse error", `{"input": "3", "output": 15}`, CodeParseError, "price"},
		{"explicit free price", `{"input": 0, "output": 0}`, "", ""},
		{"both rates, optional absent", `{"input": 3, "output": 15}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(tmpl, tc.price)
			cfg, loadErr := Load(writeTempJSON(t, body))
			_, docErr := NewDocumentFromBytes([]byte(body), Origin{Source: OriginExplicit})
			for path, err := range map[string]error{"Load": loadErr, "Document": docErr} {
				if tc.code == "" {
					if err != nil {
						t.Fatalf("%s: %v, want accepted", path, err)
					}
					continue
				}
				subjectKind, subject := SubjectRole, "agent"
				if tc.code == CodeParseError {
					subjectKind, subject = SubjectNone, ""
				}
				assertDiag(t, err, tc.code, subjectKind, subject)
				if !strings.Contains(err.Error(), tc.want) || (tc.code == CodeModelInvalid && !strings.Contains(err.Error(), `"agent"`)) {
					t.Fatalf("%s: error %q must name %q and the role", path, err, tc.want)
				}
			}
			if tc.code == "" {
				if p := cfg.Models["agent"].Price; p == nil {
					t.Fatal("accepted price decoded to nil")
				}
			}
		})
	}
}

// selectorPriceCfg puts two roles on the SAME provider/model selector.
func selectorPriceCfg(a, b *ModelPrice) *Config {
	return &Config{
		Providers: map[string]ProviderConfig{"p": {BaseURL: "http://localhost:1234"}},
		Models: map[string]ModelConfig{
			"agent": {Name: "m", Provider: "p", Type: "dense", Price: a},
			"chat":  {Name: "m", Provider: "p", Type: "dense", Price: b},
		},
		Defaults: map[string]string{"agent": "agent", "chat": "chat"},
	}
}

func TestModelPriceSelectorConflict(t *testing.T) {
	tests := []struct {
		name     string
		a, b     *ModelPrice
		conflict bool
	}{
		{"neither role prices", nil, nil, false},
		{"nil vs set is no conflict", nil, &ModelPrice{Input: 3, Output: 15}, false},
		{"set vs nil is no conflict", &ModelPrice{Input: 3, Output: 15}, nil, false},
		{"equal values behind distinct pointers", &ModelPrice{Input: 3, Output: 15, CachedInput: provider.Ptr(0.3)}, &ModelPrice{Input: 3, Output: 15, CachedInput: provider.Ptr(0.3)}, false},
		{"input differs", &ModelPrice{Input: 3, Output: 15}, &ModelPrice{Input: 4, Output: 15}, true},
		{"output differs", &ModelPrice{Input: 3, Output: 15}, &ModelPrice{Input: 3, Output: 16}, true},
		{"cached_input differs", &ModelPrice{Input: 3, Output: 15, CachedInput: provider.Ptr(0.3)}, &ModelPrice{Input: 3, Output: 15, CachedInput: provider.Ptr(0.4)}, true},
		{"cached_input present vs absent", &ModelPrice{Input: 3, Output: 15, CachedInput: provider.Ptr(0.3)}, &ModelPrice{Input: 3, Output: 15}, true},
		{"cache_write differs", &ModelPrice{Input: 3, Output: 15, CacheWrite: provider.Ptr(3.75)}, &ModelPrice{Input: 3, Output: 15, CacheWrite: provider.Ptr(4.0)}, true},
		{"explicit zero vs absent cache_write", &ModelPrice{Input: 3, Output: 15, CacheWrite: provider.Ptr(0.0)}, &ModelPrice{Input: 3, Output: 15}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := selectorPriceCfg(tt.a, tt.b).validate()
			if !tt.conflict {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			assertDiag(t, err, CodeSelectorConflict, SubjectRole, "agent")
			if !strings.Contains(err.Error(), "price") || !strings.Contains(err.Error(), `"chat"`) {
				t.Fatalf("error %q must name the price field and both roles", err)
			}
		})
	}

	t.Run("different selectors may price differently", func(t *testing.T) {
		cfg := selectorPriceCfg(&ModelPrice{Input: 3, Output: 15}, &ModelPrice{Input: 4, Output: 16})
		m := cfg.Models["chat"]
		m.Name = "other"
		cfg.Models["chat"] = m
		if err := cfg.validate(); err != nil {
			t.Fatalf("validate() = %v, want nil", err)
		}
	})
	t.Run("implicit ollama collides with explicit ollama", func(t *testing.T) {
		cfg := &Config{
			Providers: map[string]ProviderConfig{"ollama": {BaseURL: "http://localhost:11434"}},
			Models: map[string]ModelConfig{
				"agent": {Name: "m", Type: "dense", Price: &ModelPrice{Input: 3, Output: 15}},
				"chat":  {Name: "m", Provider: "ollama", Type: "dense", Price: &ModelPrice{Input: 4, Output: 15}},
			},
			Defaults: map[string]string{"agent": "agent"},
		}
		assertDiag(t, cfg.validate(), CodeSelectorConflict, SubjectRole, "agent")
	})
}

// Config.clone is the generic deep copy that Document.Config and
// ForkRoleModel rely on; the price and its optional pointers must not alias.
func TestModelPriceCloneIsDeep(t *testing.T) {
	orig := priceCfg(&ModelPrice{Input: 3, Output: 15, CachedInput: provider.Ptr(0.3), CacheWrite: provider.Ptr(3.75)})
	cp := orig.clone()

	got := cp.Models["agent"].Price
	if got == nil || got == orig.Models["agent"].Price {
		t.Fatalf("clone price = %p, want a distinct non-nil pointer", got)
	}
	got.Input, got.Output = 99, 99
	*got.CachedInput, *got.CacheWrite = 99, 99

	want := ModelPrice{Input: 3, Output: 15, CachedInput: provider.Ptr(0.3), CacheWrite: provider.Ptr(3.75)}
	if !reflect.DeepEqual(*orig.Models["agent"].Price, want) {
		t.Fatalf("mutating the clone changed the original: %+v", *orig.Models["agent"].Price)
	}
}

// pricedFixture has one priced role plus an unknown role member, so
// raw-subtree preservation is observable. An unknown member inside price is
// refused at load (TestModelPriceStrictDecode).
const pricedFixture = `{
  "providers": {"local": {"base_url": "http://localhost:1", "api_format": "openai-compat"}},
  "models": {
    "agent": {"name": "m1", "provider": "local", "type": "dense",
      "future_role_field": true,
      "price": {"input": 3, "output": 15, "cached_input": 0.3, "cache_write": 3.75}}
  },
  "defaults": {"agent": "agent"}
}`

// pricedWant is the fixture price as the writer emits it: compact, keys
// sorted, every member exactly as loaded.
const pricedWant = `{"cache_write":3.75,"cached_input":0.3,"input":3,"output":15}`

// renderedRole renders d through the real writer path and returns one role's
// raw members, so a test can assert a member is absent or has exact numbers.
func renderedRole(t *testing.T, d *Document, role string) map[string]json.RawMessage {
	t.Helper()
	var doc struct {
		Models map[string]map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal([]byte(renderToString(t, d)), &doc); err != nil {
		t.Fatal(err)
	}
	members, ok := doc.Models[role]
	if !ok {
		t.Fatalf("rendered document has no role %q", role)
	}
	return members
}

// requireRenderedPrice asserts the role's written price is byte-for-byte
// want once the writer's indentation is compacted away.
func requireRenderedPrice(t *testing.T, members map[string]json.RawMessage, want string) {
	t.Helper()
	raw, ok := members["price"]
	if !ok {
		t.Fatalf("rendered role has no price member: %v", members)
	}
	var got bytes.Buffer
	if err := json.Compact(&got, raw); err != nil {
		t.Fatal(err)
	}
	if got.String() != want {
		t.Errorf("rendered price = %s, want %s", got.String(), want)
	}
}

func TestModelPriceRoundTripsThroughDocument(t *testing.T) {
	d := loadTestDoc(t, pricedFixture)

	p := d.Config().Models["agent"].Price
	if p == nil || p.Input != 3 || p.Output != 15 || p.CachedInput == nil || *p.CachedInput != 0.3 ||
		p.CacheWrite == nil || *p.CacheWrite != 3.75 {
		t.Fatalf("loaded price = %+v, want {3 15 0.3 3.75}", p)
	}

	members := renderedRole(t, d, "agent")
	requireRenderedPrice(t, members, pricedWant)
	if string(members["future_role_field"]) != "true" {
		t.Errorf("unknown role member lost: %v", members)
	}

	// Rendered bytes parse back to the same price and re-render identically.
	first := renderToString(t, d)
	again, err := NewDocumentFromBytes([]byte(first), Origin{Source: OriginExplicit})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Config().Models["agent"].Price, p) {
		t.Errorf("reparsed price = %+v, want %+v", again.Config().Models["agent"].Price, p)
	}
	if second := renderToString(t, again); second != first {
		t.Errorf("render not idempotent:\n%s\n--- vs ---\n%s", first, second)
	}
}

// An explicit free price survives the writer exactly: both zero members
// stay present (absent would now be refused) and no optional rate appears.
func TestModelPriceFreeRoundTrip(t *testing.T) {
	d := loadTestDoc(t, strings.Replace(pricedFixture,
		`{"input": 3, "output": 15, "cached_input": 0.3, "cache_write": 3.75}`, `{"input": 0, "output": 0}`, 1))
	requireRenderedPrice(t, renderedRole(t, d, "agent"), `{"input":0,"output":0}`)
	again, err := NewDocumentFromBytes([]byte(renderToString(t, d)), Origin{Source: OriginExplicit})
	if err != nil {
		t.Fatalf("rendered free price does not reload: %v", err)
	}
	if p := again.Config().Models["agent"].Price; p == nil || p.Input != 0 || p.Output != 0 || p.CachedInput != nil || p.CacheWrite != nil {
		t.Fatalf("reloaded price = %+v, want {0 0 nil nil}", p)
	}
}

func TestSetRoleModelPrice(t *testing.T) {
	t.Run("different model clears price and reports it", func(t *testing.T) {
		d := loadTestDoc(t, pricedFixture)
		res, err := d.SetRoleModel("agent", agentFacts(), SetRoleModelOpts{ConfirmUnknown: true}) // local/m9
		if err != nil {
			t.Fatal(err)
		}
		if !res.DroppedPrice {
			t.Error("DroppedPrice = false, want true")
		}
		if got := d.authored.Models["agent"].Price; got != nil {
			t.Errorf("authored price = %+v after moving to another model, want nil", got)
		}
		// The written JSON must not keep the old model's price either: the
		// raw authored subtree still holds it.
		members := renderedRole(t, d, "agent")
		if raw, ok := members["price"]; ok {
			t.Errorf("written role still has price %s", raw)
		}
		if string(members["future_role_field"]) != "true" {
			t.Errorf("unrelated unknown member lost: %v", members)
		}
	})
	t.Run("different provider with the same model name clears price", func(t *testing.T) {
		d := loadTestDoc(t, strings.Replace(pricedFixture,
			`"providers": {"local": {"base_url": "http://localhost:1", "api_format": "openai-compat"}}`,
			`"providers": {"local": {"base_url": "http://localhost:1", "api_format": "openai-compat"}, "other": {"base_url": "http://localhost:2", "api_format": "openai-compat"}}`, 1))
		facts := agentFacts()
		facts.Key = provider.ModelKey{Provider: "other", Model: "m1"}
		res, err := d.SetRoleModel("agent", facts, SetRoleModelOpts{ConfirmUnknown: true})
		if err != nil {
			t.Fatal(err)
		}
		if !res.DroppedPrice || d.authored.Models["agent"].Price != nil {
			t.Errorf("DroppedPrice=%v price=%+v, want true and nil", res.DroppedPrice, d.authored.Models["agent"].Price)
		}
	})
	t.Run("same model keeps exact price", func(t *testing.T) {
		d := loadTestDoc(t, pricedFixture)
		facts := agentFacts()
		facts.Key = provider.ModelKey{Provider: "local", Model: "m1"}
		res, err := d.SetRoleModel("agent", facts, SetRoleModelOpts{ConfirmUnknown: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.DroppedPrice {
			t.Error("DroppedPrice = true, want false for the same model")
		}
		p := d.authored.Models["agent"].Price
		if p == nil || p.Input != 3 || p.Output != 15 || *p.CachedInput != 0.3 || *p.CacheWrite != 3.75 {
			t.Errorf("authored price = %+v, want {3 15 0.3 3.75} kept", p)
		}
		requireRenderedPrice(t, renderedRole(t, d, "agent"), pricedWant)
	})
	t.Run("implicit ollama role counts as ollama for the same-model rule", func(t *testing.T) {
		d := loadTestDoc(t, `{
  "providers": {"ollama": {"base_url": "http://localhost:1"}},
  "models": {"agent": {"name": "m9", "type": "dense", "price": {"input": 3, "output": 15}}},
  "defaults": {"agent": "agent"}
}`)
		facts := agentFacts()
		facts.Key = provider.ModelKey{Provider: "ollama", Model: "m9"}
		res, err := d.SetRoleModel("agent", facts, SetRoleModelOpts{ConfirmUnknown: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.DroppedPrice || d.authored.Models["agent"].Price == nil {
			t.Errorf("DroppedPrice=%v price=%+v, want false and kept", res.DroppedPrice, d.authored.Models["agent"].Price)
		}
	})
	t.Run("unpriced role reports nothing dropped", func(t *testing.T) {
		d := loadTestDoc(t, roleFixture)
		res, err := d.SetRoleModel("fast", agentFacts(), SetRoleModelOpts{ConfirmUnknown: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.DroppedPrice {
			t.Error("DroppedPrice = true for a role without a price")
		}
	})
}

func TestForkRoleModelPrice(t *testing.T) {
	t.Run("different model clears price in memory and in the written JSON", func(t *testing.T) {
		d := loadTestDoc(t, pricedFixture)
		// No ConfirmDrops entry for price: the closed vocabulary stays
		// slots|think_tags, so the fork must succeed without one.
		if err := d.ForkRoleModel("agent", "agent-m", forkFacts(), forkOpts()); err != nil { // local/m7
			t.Fatal(err)
		}
		if got := d.authored.Models["agent-m"].Price; got != nil {
			t.Errorf("fork price = %+v, want nil", got)
		}
		// The fork copies the source's raw subtree, "price" included; the
		// writer must not resurrect it.
		fork := renderedRole(t, d, "agent-m")
		if raw, ok := fork["price"]; ok {
			t.Errorf("written fork still has price %s", raw)
		}
		if string(fork["future_role_field"]) != "true" {
			t.Errorf("fork lost the source's unknown member: %v", fork)
		}
		// The source keeps its price, exactly.
		requireRenderedPrice(t, renderedRole(t, d, "agent"), pricedWant)
	})
	t.Run("same model keeps exact price", func(t *testing.T) {
		d := loadTestDoc(t, pricedFixture)
		facts := forkFacts()
		facts.Key = provider.ModelKey{Provider: "local", Model: "m1"}
		if err := d.ForkRoleModel("agent", "agent-m", facts, forkOpts()); err != nil {
			t.Fatal(err)
		}
		p := d.authored.Models["agent-m"].Price
		if p == nil || p.Input != 3 || p.Output != 15 || *p.CachedInput != 0.3 || *p.CacheWrite != 3.75 {
			t.Errorf("fork price = %+v, want {3 15 0.3 3.75}", p)
		}
		requireRenderedPrice(t, renderedRole(t, d, "agent-m"), pricedWant)
		requireRenderedPrice(t, renderedRole(t, d, "agent"), pricedWant)
	})
	t.Run("fork does not alias the source price", func(t *testing.T) {
		d := loadTestDoc(t, pricedFixture)
		facts := forkFacts()
		facts.Key = provider.ModelKey{Provider: "local", Model: "m1"}
		if err := d.ForkRoleModel("agent", "agent-m", facts, forkOpts()); err != nil {
			t.Fatal(err)
		}
		fp := d.authored.Models["agent-m"].Price
		fp.Input = 99
		*fp.CachedInput = 99
		if sp := d.authored.Models["agent"].Price; sp.Input != 3 || *sp.CachedInput != 0.3 {
			t.Errorf("mutating the fork's price changed the source: %+v", sp)
		}
	})
}

// A selector's price may live on one role only; removing that role would
// silently unprice the survivors, so RemoveRole refuses like it does for
// quality_ctx_ceiling.
func TestRemoveRolePriceSurvivor(t *testing.T) {
	const tmpl = `{
  "providers": {"local": {"base_url": "http://localhost:1", "api_format": "openai-compat"}},
  "models": {
    "live": {"name": "shared", "provider": "local", "type": "dense"%s},
    "carrier": {"name": "shared", "provider": "local", "type": "dense"%s}
  },
  "defaults": {"agent": "live"}
}`
	const price = `, "price": {"input": 3, "output": 15}`
	t.Run("refuses to drop the only carrier", func(t *testing.T) {
		d := loadTestDoc(t, fmt.Sprintf(tmpl, "", price))
		err := d.RemoveRole("carrier")
		assertDiag(t, err, CodeRoleInUse, SubjectRole, "live")
		if !strings.Contains(err.Error(), "price") {
			t.Errorf("error %q must name price", err)
		}
		if _, ok := d.authored.Models["carrier"]; !ok {
			t.Error("carrier removed despite the refusal")
		}
	})
	t.Run("allows removal when a survivor carries an equal price", func(t *testing.T) {
		d := loadTestDoc(t, fmt.Sprintf(tmpl, price, price))
		if err := d.RemoveRole("carrier"); err != nil {
			t.Fatalf("RemoveRole = %v, want nil", err)
		}
	})
	t.Run("allows removing an unpriced role", func(t *testing.T) {
		d := loadTestDoc(t, fmt.Sprintf(tmpl, price, ""))
		if err := d.RemoveRole("carrier"); err != nil {
			t.Fatalf("RemoveRole = %v, want nil", err)
		}
	})
}

// validate makes a survivor with a different price unreachable through a
// Document, so the "equal price" half of the survivor check is pinned by
// calling selectorStateDrop on a constructed Config.
func TestSelectorStateDropPriceNeedsEqualSurvivor(t *testing.T) {
	cfg := func(survivor *ModelPrice) *Config {
		return &Config{Models: map[string]ModelConfig{
			"live":    {Name: "shared", Provider: "local", Type: "dense", Price: survivor},
			"carrier": {Name: "shared", Provider: "local", Type: "dense", Price: &ModelPrice{Input: 3, Output: 15}},
		}}
	}
	for _, tt := range []struct {
		name      string
		survivor  *ModelPrice
		wantField string
	}{
		{"survivor unpriced", nil, "price"},
		{"survivor priced differently", &ModelPrice{Input: 4, Output: 15}, "price"},
		{"survivor differs only in cached_input", &ModelPrice{Input: 3, Output: 15, CachedInput: provider.Ptr(0.3)}, "price"},
		{"survivor priced equally", &ModelPrice{Input: 3, Output: 15}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			who, field := selectorStateDrop(cfg(tt.survivor), "carrier")
			if field != tt.wantField || (field != "" && who != "live") {
				t.Fatalf("selectorStateDrop = (%q, %q), want (live, %q)", who, field, tt.wantField)
			}
		})
	}
}
