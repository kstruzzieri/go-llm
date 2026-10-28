package config

import (
	"encoding/json"
	"maps"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// recordingEnv is an explicit LookupEnv fake: it answers from env and
// records every name it is asked for, in order. Tests never consult the
// real environment.
type recordingEnv struct {
	env   map[string]string
	calls []string
}

func (r *recordingEnv) lookup(name string) (string, bool) {
	r.calls = append(r.calls, name)
	v, ok := r.env[name]
	return v, ok
}

func (r *recordingEnv) seen() []string {
	set := map[string]bool{}
	for _, name := range r.calls {
		set[name] = true
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func parseSecretDoc(t *testing.T, raw string, env *recordingEnv) *Document {
	t.Helper()
	d, err := ParseDocument([]byte(raw), Origin{}, DocumentOptions{LookupEnv: env.lookup})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// wantMatch asserts the matcher's verdict for each input. Expectations are
// fixed literals; nothing here is derived from the production matcher.
func wantMatch(t *testing.T, label string, m func(string) bool, want bool, inputs ...string) {
	t.Helper()
	if m == nil {
		t.Fatalf("%s: SecretMatcher returned nil", label)
	}
	for _, in := range inputs {
		if got := m(in); got != want {
			t.Errorf("%s: matcher(%q) = %v, want %v", label, in, got, want)
		}
	}
}

const (
	secLiteralA   = "literal-key-0001"
	secCompA      = "comp-aaaa1111"
	secCompB      = "comp-bbbb2222"
	secComposedB  = "static-prefix-0001-comp-aaaa1111-comp-bbbb2222"
	secInactiveC  = "inactive-key-0003"
	secUnusedEnv  = "unused-sentinel-9999"
	secCompC      = "comp-cccc3333"
	secReplacedA  = "replaced-key-0004"
	secComposedC  = "x-comp-cccc3333"
	secRotatedA   = "comp-zzzz9999"
	secPlainLitB  = "plain-literal-0007"
	secSourcesRaw = `{
  "providers": {
    "a": {"base_url": "http://localhost:1", "api_key": "literal-key-0001"},
    "b": {"base_url": "http://localhost:2", "api_key": "static-prefix-0001-${GOLEM_T_A}-${GOLEM_T_B}"},
    "c": {"base_url": "http://localhost:3", "api_key": "inactive-key-0003"},
    "d": {"base_url": "http://localhost:4", "api_key": "literal-key-0001"}
  },
  "models": {
    "agent": {"name": "m", "type": "dense", "provider": "b"},
    "alt": {"name": "m2", "type": "dense", "provider": "d"}
  },
  "defaults": {"agent": "agent"}
}`
)

func sourcesEnv() *recordingEnv {
	return &recordingEnv{env: map[string]string{
		"GOLEM_T_A":    secCompA,
		"GOLEM_T_B":    secCompB,
		"SECRET_TOKEN": secUnusedEnv,
	}}
}

func TestSecretMatcherSources(t *testing.T) {
	env := sourcesEnv()
	d := parseSecretDoc(t, secSourcesRaw, env)
	m := d.Config().SecretMatcher()

	wantMatch(t, "sources", m, true,
		secLiteralA, secComposedB, secCompA, secCompB, secInactiveC,
		"prefix "+secLiteralA+" suffix", // contiguous substring, not equality
	)
	wantMatch(t, "sources", m, false,
		"", "static-prefix-0001-", "GOLEM_T_A", "${GOLEM_T_A}", "GOLEM_T_B", "SECRET_TOKEN", secUnusedEnv,
		"LITERAL-KEY-0001",             // case-sensitive
		"literal-key-000 1",            // contiguous only
		"comp-aaaa111", "omp-bbbb2222", // proper prefixes/suffixes of a member
	)
	if got, want := env.seen(), []string{"GOLEM_T_A", "GOLEM_T_B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lookup saw %v, want %v", got, want)
	}

	// d duplicates a's literal: removing a keeps the literal guarded.
	if err := d.RemoveProvider("a"); err != nil {
		t.Fatal(err)
	}
	wantMatch(t, "after RemoveProvider(a)", d.Config().SecretMatcher(), true,
		secLiteralA, secInactiveC, secComposedB, secCompA, secCompB)
}

func TestSecretMatcherThreshold(t *testing.T) {
	raw := `{
  "providers": {
    "p0": {"base_url": "http://localhost:1", "api_key": ""},
    "p7": {"base_url": "http://localhost:2", "api_key": "1234567"},
    "p8": {"base_url": "http://localhost:3", "api_key": "12345678"},
    "pw": {"base_url": "http://localhost:4", "api_key": "password"},
    "ru": {"base_url": "http://localhost:5", "api_key": "ключ"},
    "cm": {"base_url": "http://localhost:6", "api_key": "${S1}${S2}"}
  },
  "models": {"agent": {"name": "m", "type": "dense", "provider": "p8"}},
  "defaults": {"agent": "agent"}
}`
	env := &recordingEnv{env: map[string]string{"S1": "abcd", "S2": "efgh"}}
	m := parseSecretDoc(t, raw, env).Config().SecretMatcher()

	if len("ключ") != 8 {
		t.Fatalf("fixture: len(ключ) = %d, want 8", len("ключ"))
	}
	wantMatch(t, "threshold", m, true, "12345678", "password", "ключ", "abcdefgh")
	// An empty candidate would make every input match, so "" and
	// unrelated text pin its exclusion too.
	wantMatch(t, "threshold", m, false, "", "unrelated", "1234567", "abcd", "efgh")
}

func TestSecretMatcherNoRecursiveExpansion(t *testing.T) {
	raw := `{
  "providers": {"p": {"base_url": "http://localhost:1", "api_key": "${OUTER}"}},
  "models": {"agent": {"name": "m", "type": "dense", "provider": "p"}},
  "defaults": {"agent": "agent"}
}`
	env := &recordingEnv{env: map[string]string{"OUTER": "val-${INNER}-xyz", "INNER": "inner-value-0001"}}
	cfg := parseSecretDoc(t, raw, env).Config()
	if got := cfg.Providers["p"].APIKey; got != "val-${INNER}-xyz" {
		t.Fatalf("effective api_key = %q, want verbatim one-pass expansion", got)
	}
	m := cfg.SecretMatcher()
	wantMatch(t, "no recursion", m, true, "val-${INNER}-xyz")
	wantMatch(t, "no recursion", m, false, "inner-value-0001", "val-inner-value-0001-xyz")
	if got, want := env.calls, []string{"OUTER"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lookup calls = %v, want %v", got, want)
	}
}

func TestSecretMatcherOneResolutionTransaction(t *testing.T) {
	raw := `{
  "providers": {
    "p": {"base_url": "http://localhost:1", "api_key": "${K}"},
    "q": {"base_url": "http://localhost:2", "api_key": "pre-${K}-${K}"}
  },
  "models": {"agent": {"name": "m", "type": "dense", "provider": "p"}},
  "defaults": {"agent": "agent"}
}`
	env := &recordingEnv{env: map[string]string{"K": "shared-key-0001"}}
	d := parseSecretDoc(t, raw, env)
	if got, want := env.calls, []string{"K"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("parse lookups = %v, want %v (one per finalize)", got, want)
	}
	wantMatch(t, "transaction", d.Config().SecretMatcher(), true,
		"shared-key-0001", "pre-shared-key-0001-shared-key-0001")

	// A successful mutation is a new transaction: exactly one more lookup.
	if err := d.ClearProviderAPIKey("p"); err != nil {
		t.Fatal(err)
	}
	if got, want := env.calls, []string{"K", "K"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lookups after mutation = %v, want %v", got, want)
	}
}

const secLifecycleRaw = `{
  "providers": {
    "a": {"base_url": "http://localhost:1", "api_key": "literal-key-0001"},
    "b": {"base_url": "http://localhost:2", "api_key": "static-prefix-0001-${GOLEM_T_A}-${GOLEM_T_B}"},
    "c": {"base_url": "http://localhost:3", "api_key": "${GOLEM_T_A}"}
  },
  "models": {"agent": {"name": "m", "type": "dense", "provider": "c"}},
  "defaults": {"agent": "agent"}
}`

func lifecycleEnv() *recordingEnv {
	return &recordingEnv{env: map[string]string{
		"GOLEM_T_A": secCompA,
		"GOLEM_T_B": secCompB,
		"GOLEM_T_C": secCompC,
	}}
}

func TestSecretMatcherLifecycle(t *testing.T) {
	all := []string{secLiteralA, secComposedB, secCompA, secCompB}

	t.Run("Config clone keeps components", func(t *testing.T) {
		d := parseSecretDoc(t, secLifecycleRaw, lifecycleEnv())
		wantMatch(t, "Config()", d.Config().SecretMatcher(), true, all...)
		wantMatch(t, "clone of Config()", d.Config().clone().SecretMatcher(), true, all...)
	})

	t.Run("SetProviderAPIKey replaces membership", func(t *testing.T) {
		d := parseSecretDoc(t, secLifecycleRaw, lifecycleEnv())
		if err := d.SetProviderAPIKey("a", secReplacedA); err != nil {
			t.Fatal(err)
		}
		m := d.Config().SecretMatcher()
		wantMatch(t, "set literal", m, false, secLiteralA)
		wantMatch(t, "set literal", m, true, secReplacedA, secComposedB, secCompA, secCompB)

		if err := d.SetProviderAPIKey("b", "x-${GOLEM_T_C}"); err != nil {
			t.Fatal(err)
		}
		m = d.Config().SecretMatcher()
		// The old composed key still contains A, which provider c keeps
		// guarded, so only the unshared component B can show the drop.
		wantMatch(t, "set template", m, false, secCompB)
		wantMatch(t, "set template", m, true, secComposedC, secCompC, secCompA, secReplacedA)
	})

	t.Run("RemoveProvider drops unshared components", func(t *testing.T) {
		d := parseSecretDoc(t, secLifecycleRaw, lifecycleEnv())
		if err := d.RemoveProvider("b"); err != nil {
			t.Fatal(err)
		}
		m := d.Config().SecretMatcher()
		// The composed key contains A (still guarded via c); B shows the drop.
		wantMatch(t, "remove b", m, false, secCompB)
		wantMatch(t, "remove b", m, true, secCompA, secLiteralA)
	})

	t.Run("ClearProviderAPIKey drops only that provider", func(t *testing.T) {
		d := parseSecretDoc(t, secLifecycleRaw, lifecycleEnv())
		if err := d.ClearProviderAPIKey("a"); err != nil {
			t.Fatal(err)
		}
		m := d.Config().SecretMatcher()
		wantMatch(t, "clear a", m, false, secLiteralA)
		wantMatch(t, "clear a", m, true, secComposedB, secCompA, secCompB)
	})

	t.Run("ClearAllProviderAPIKeys is always-false", func(t *testing.T) {
		d := parseSecretDoc(t, secLifecycleRaw, lifecycleEnv())
		if err := d.ClearAllProviderAPIKeys(); err != nil {
			t.Fatal(err)
		}
		wantMatch(t, "clear all", d.Config().SecretMatcher(), false, append([]string{"", "x"}, all...)...)
	})

	t.Run("failed SetProviderAPIKey keeps previous matcher", func(t *testing.T) {
		d := parseSecretDoc(t, secLifecycleRaw, lifecycleEnv())
		// GOLEM_T_C resolves before GOLEM_T_MISSING fails: nothing from
		// the failed transaction may be published.
		err := d.SetProviderAPIKey("a", "${GOLEM_T_C}-${GOLEM_T_MISSING}")
		assertDiag(t, err, CodeKeyReferenceUnavailable, SubjectProvider, "a")
		if strings.Contains(err.Error(), secCompC) {
			t.Fatalf("error %q echoes a resolved value", err)
		}
		m := d.Config().SecretMatcher()
		wantMatch(t, "failed set", m, true, all...)
		wantMatch(t, "failed set", m, false, secCompC)
	})

	t.Run("reparse with different lookup value", func(t *testing.T) {
		d1 := parseSecretDoc(t, secLifecycleRaw, lifecycleEnv())
		env2 := lifecycleEnv()
		env2.env["GOLEM_T_A"] = secRotatedA
		d2 := parseSecretDoc(t, secLifecycleRaw, env2)
		if d1.Revision() != d2.Revision() {
			t.Fatal("same bytes produced different revisions")
		}
		m := d2.Config().SecretMatcher()
		wantMatch(t, "reparse", m, true, secRotatedA, "static-prefix-0001-"+secRotatedA+"-"+secCompB)
		// The old composed key contains the unchanged B, so only A shows it.
		wantMatch(t, "reparse", m, false, secCompA)
	})

	t.Run("environment change without re-finalization", func(t *testing.T) {
		env := lifecycleEnv()
		d := parseSecretDoc(t, secLifecycleRaw, env)
		m := d.Config().SecretMatcher()
		env.env["GOLEM_T_A"] = secRotatedA
		wantMatch(t, "rotated env, old matcher", m, true, secCompA, secComposedB)
		wantMatch(t, "rotated env, old matcher", m, false, secRotatedA)
		wantMatch(t, "rotated env, Config()", d.Config().SecretMatcher(), false, secRotatedA)
	})
}

func TestSecretMatcherDropsStaleComponents(t *testing.T) {
	raw := `{
  "providers": {
    "a": {"base_url": "http://localhost:1", "api_key": "literal-key-0001"},
    "b": {"base_url": "http://localhost:2", "api_key": "static-prefix-0001-${GOLEM_T_A}-${GOLEM_T_B}"}
  },
  "models": {"agent": {"name": "m", "type": "dense", "provider": "a"}},
  "defaults": {"agent": "agent"}
}`
	cfg := parseSecretDoc(t, raw, sourcesEnv()).Config()
	wantMatch(t, "baseline", cfg.SecretMatcher(), true, secCompA, secCompB)

	replaced := *cfg
	replaced.Providers = maps.Clone(cfg.Providers)
	p := replaced.Providers["b"]
	p.APIKey = secPlainLitB
	replaced.Providers["b"] = p
	m := replaced.SecretMatcher()
	wantMatch(t, "replaced b", m, false, secCompA, secCompB, secComposedB)
	wantMatch(t, "replaced b", m, true, secPlainLitB, secLiteralA)

	removed := *cfg
	removed.Providers = maps.Clone(cfg.Providers)
	delete(removed.Providers, "b")
	m = removed.SecretMatcher()
	wantMatch(t, "removed b", m, false, secCompA, secCompB, secComposedB)
	wantMatch(t, "removed b", m, true, secLiteralA)
}

// Re-finalization replaces the sidecar in full. Production finalizes only
// sidecar-free values, so this drives finalizeEnv directly on an effective
// copy: provider b's key is now a literal byte-identical to its old
// expansion, which an appended older generation would still pair with
// components A and B; c, the other source of A, becomes unrelated.
func TestSecretMatcherRefinalizeReplacesSidecar(t *testing.T) {
	cfg := parseSecretDoc(t, secLifecycleRaw, lifecycleEnv()).Config()
	if _, ok := cfg.resolved["b"]; !ok {
		t.Fatal("fixture: expected a sidecar entry for b")
	}
	p := cfg.Providers["c"]
	p.APIKey = secPlainLitB
	cfg.Providers["c"] = p
	env := &recordingEnv{}
	if err := cfg.finalizeEnv(env.lookup); err != nil {
		t.Fatal(err)
	}
	if len(env.calls) != 0 {
		t.Fatalf("literal-only finalize looked up %v", env.calls)
	}
	m := cfg.SecretMatcher()
	wantMatch(t, "refinalized", m, false, secCompA, secCompB)
	wantMatch(t, "refinalized", m, true, secComposedB, secLiteralA, secPlainLitB)
}

func TestSecretMatcherNotSerialized(t *testing.T) {
	cfg := parseSecretDoc(t, secLifecycleRaw, lifecycleEnv()).Config()
	if len(cfg.resolved) == 0 {
		t.Fatal("fixture: expected a resolved-component sidecar")
	}
	bare := *cfg
	bare.resolved = nil
	got, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(&bare)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("sidecar leaked into JSON:\n got %s\nwant %s", got, want)
	}

	// Formatting a matcher reveals nothing because of its type: fmt prints
	// an unnamed func value as a pointer and cannot reach its captures. A
	// format-string check could not fail, so pin the exact type instead. A
	// named matcher type stays assignable to func(string) bool yet could
	// carry a String or Format method, so assignability is not enough.
	if got, want := reflect.TypeOf((*Config)(nil).SecretMatcher()), reflect.TypeFor[func(string) bool](); got != want {
		t.Fatalf("SecretMatcher returns %v, want exactly %v", got, want)
	}
}

// The sidecar survives a copy only as an independent deep copy: writes to
// the copy's map or component slices must never reach the source. Both
// Config.clone and Document.Config are copy boundaries.
func TestSecretMatcherCloneDeepCopiesSidecar(t *testing.T) {
	want := []string{secCompA, secCompB}
	cases := []struct {
		name  string
		setup func(d *Document) (src *Config, copyOf func() *Config)
	}{
		{"Config.clone", func(d *Document) (*Config, func() *Config) {
			c := d.Config()
			return c, c.clone
		}},
		{"Document.Config", func(d *Document) (*Config, func() *Config) {
			return d.effective, d.Config
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh document per subtest and a fresh copy per check, so
			// each assertion fails on its own when sharing leaks.
			src, copyOf := tc.setup(parseSecretDoc(t, secLifecycleRaw, lifecycleEnv()))
			if got := src.resolved["b"].components; !reflect.DeepEqual(got, want) {
				t.Fatalf("fixture: source components[b] = %q, want %q", got, want)
			}
			written := copyOf()
			if _, ok := written.resolved["b"]; !ok {
				t.Fatal("copy lost the sidecar entry for b")
			}
			written.resolved["b"].components[0] = "zzzzzzzzzz"
			if got := src.resolved["b"].components; !reflect.DeepEqual(got, want) {
				t.Errorf("write to the copy's components reached the source: %q", got)
			}
			deleted := copyOf()
			delete(deleted.resolved, "b")
			if _, ok := src.resolved["b"]; !ok {
				t.Error("delete on the copy's sidecar removed the source entry")
			}
			// B is guarded only through b's components.
			wantMatch(t, tc.name, src.SecretMatcher(), true, secCompB)
		})
	}
}

func TestSecretMatcherNilAndProgrammatic(t *testing.T) {
	var nilCfg *Config
	wantMatch(t, "nil receiver", nilCfg.SecretMatcher(), false, "", "anything", secLiteralA)
	wantMatch(t, "empty config", (&Config{}).SecretMatcher(), false, "", "anything")

	// Programmatic configs supply literal keys; no expansion happens.
	prog := &Config{Providers: map[string]ProviderConfig{
		"p": {APIKey: "programmatic-0008"},
		"q": {APIKey: "${NOT_EXPANDED}"},
	}}
	m := prog.SecretMatcher()
	wantMatch(t, "programmatic", m, true, "programmatic-0008", "${NOT_EXPANDED}")
	wantMatch(t, "programmatic", m, false, "NOT_EXPANDED")
}

func TestSecretMatcherConcurrentUse(t *testing.T) {
	cfg := parseSecretDoc(t, secLifecycleRaw, lifecycleEnv()).Config()
	shared := cfg.SecretMatcher()
	var wg sync.WaitGroup
	errs := make(chan string, 32*4)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			own := cfg.SecretMatcher()
			for _, m := range []func(string) bool{shared, own} {
				if !m("x " + secCompA + " y") {
					errs <- "missed member"
				}
				if m("unrelated") {
					errs <- "matched non-member"
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}
