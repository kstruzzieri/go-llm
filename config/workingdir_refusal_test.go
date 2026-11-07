package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// wdCanary is the secret every #776 test plants; it must never appear in
// any error text.
const wdCanary = "CANARY-776-5e0b9d41c7a2f368"

// wdConfig is a one-provider models.json whose provider "p" carries apiKey.
func wdConfig(apiKey string) string {
	return fmt.Sprintf(`{
  "providers": {"p": {"base_url": "http://localhost:8080", "api_format": "openai-compat", "api_key": %q}},
  "models": {"agent": {"name": "m1", "provider": "p", "type": "dense"}},
  "defaults": {"agent": "agent"}
}`, apiKey)
}

// literalKey swaps the shared fixtures' ${DOC_TEST_KEY} reference for a
// literal, for tests that load them as a working-directory config (which
// refuses references).
func literalKey(body string) string {
	return strings.ReplaceAll(body, "${DOC_TEST_KEY}", "sk-literal-776")
}

// isolateDiscovery leaves discoverConfigPath only the files a test writes:
// GO_LLM_CONFIG unset and the user-config and legacy locations under an
// empty temporary home. It returns the user-config models.json path.
func isolateDiscovery(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	unsetEnvForTest(t, "GO_LLM_CONFIG")
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "go-llm", "models.json")
}

// workingDirWith writes body as ./models.json in a fresh directory, isolates
// discovery, and makes that directory the working directory.
func workingDirWith(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	isolateDiscovery(t)
	t.Chdir(dir)
}

func assertNoCanary(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), "CANARY-776") {
		t.Fatalf("error leaks the secret: %v", err)
	}
}

func assertRefusalMessage(t *testing.T, err error, name string) {
	t.Helper()
	assertNoCanary(t, err)
	msg := err.Error()
	for _, want := range []string{fmt.Sprintf("environment variable %q", name), "working directory", "GO_LLM_CONFIG"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
}

// countingLookup returns the canary for every name and counts the calls.
func countingLookup(calls *int) func(string) (string, bool) {
	return func(string) (string, bool) {
		*calls++
		return wdCanary, true
	}
}

func TestDefaultRefusesWorkingDirKeyReference(t *testing.T) {
	workingDirWith(t, wdConfig("${CANARY_VAR}"))
	t.Setenv("CANARY_VAR", wdCanary)
	_, err := Default()
	assertDiag(t, err, CodeKeyReferenceRefused, SubjectProvider, "p")
	assertRefusalMessage(t, err, "CANARY_VAR")
}

func TestDefaultDocumentRefusesWorkingDirKeyReference(t *testing.T) {
	workingDirWith(t, wdConfig("${CANARY_VAR}"))
	t.Setenv("CANARY_VAR", wdCanary)
	_, err := DefaultDocument()
	assertDiag(t, err, CodeKeyReferenceRefused, SubjectProvider, "p")
	assertRefusalMessage(t, err, "CANARY_VAR")
}

// The origin is the contract, not the entry point (acceptance 9). The
// explicit-origin leg proves the counter sees lookups when they happen.
func TestParseDocumentWorkingDirOriginRefusesWithoutLookup(t *testing.T) {
	data := []byte(wdConfig("${CANARY_VAR}"))
	calls := 0
	_, err := ParseDocument(data, Origin{Source: OriginWorkingDir, Path: "models.json"},
		DocumentOptions{LookupEnv: countingLookup(&calls)})
	assertDiag(t, err, CodeKeyReferenceRefused, SubjectProvider, "p")
	assertRefusalMessage(t, err, "CANARY_VAR")
	if calls != 0 {
		t.Fatalf("refused reference was looked up %d times, want 0", calls)
	}

	d, err := ParseDocument(data, Origin{Source: OriginExplicit, Path: "models.json"},
		DocumentOptions{LookupEnv: countingLookup(&calls)})
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 || d.Config().Providers["p"].APIKey != wdCanary {
		t.Fatalf("explicit origin did not expand the reference (calls=%d)", calls)
	}
}

// A malformed reference in a discovered file is refused, not reported as
// malformed, and nothing is looked up (Review Focus 2).
func TestWorkingDirMalformedReferenceIsRefused(t *testing.T) {
	for _, key := range []string{"abc${", "${not valid}"} {
		t.Run(key, func(t *testing.T) {
			calls := 0
			_, err := ParseDocument([]byte(wdConfig(key)), Origin{Source: OriginWorkingDir},
				DocumentOptions{LookupEnv: countingLookup(&calls)})
			assertDiag(t, err, CodeKeyReferenceRefused, SubjectProvider, "p")
			assertNoCanary(t, err)
			if strings.Contains(err.Error(), "environment variable \"") {
				t.Fatalf("malformed reference named a variable: %v", err)
			}
			if calls != 0 {
				t.Fatalf("lookup called %d times, want 0", calls)
			}
		})
	}
}

// Review Focus 4: the first reference in a key, and the first provider in
// sorted order.
func TestWorkingDirRefusalNamesFirstReference(t *testing.T) {
	_, err := ParseDocument([]byte(wdConfig("${A}${B}")), Origin{Source: OriginWorkingDir}, DocumentOptions{})
	assertDiag(t, err, CodeKeyReferenceRefused, SubjectProvider, "p")
	assertRefusalMessage(t, err, "A")
	if strings.Contains(err.Error(), `"B"`) {
		t.Fatalf("refusal named the second reference: %v", err)
	}

	two := `{
  "providers": {
    "b": {"base_url": "http://localhost:8080", "api_key": "${B}"},
    "a": {"base_url": "http://localhost:8081", "api_key": "${A}"}
  },
  "models": {"agent": {"name": "m1", "provider": "b", "type": "dense"}},
  "defaults": {"agent": "agent"}
}`
	_, err = ParseDocument([]byte(two), Origin{Source: OriginWorkingDir}, DocumentOptions{})
	assertDiag(t, err, CodeKeyReferenceRefused, SubjectProvider, "a")
}

func TestWorkingDirLiteralKeyLoads(t *testing.T) {
	workingDirWith(t, wdConfig("sk-literal-776"))
	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers["p"].APIKey; got != "sk-literal-776" {
		t.Fatalf("Default api_key = %q", got)
	}
	d, err := DefaultDocument()
	if err != nil {
		t.Fatal(err)
	}
	if d.Origin().Source != OriginWorkingDir || d.Config().Providers["p"].APIKey != "sk-literal-776" {
		t.Fatalf("DefaultDocument origin %+v, api_key %q", d.Origin(), d.Config().Providers["p"].APIKey)
	}
}

// Naming the file is consent: explicit paths and GO_LLM_CONFIG, including a
// relative GO_LLM_CONFIG pointing at the same working-directory file
// (acceptance 2, Review Focus 1).
func TestExplicitAndEnvOverrideLoadReferences(t *testing.T) {
	workingDirWith(t, wdConfig("${CANARY_VAR}"))
	t.Setenv("CANARY_VAR", wdCanary)

	cfg, err := Load("models.json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["p"].APIKey != wdCanary {
		t.Fatal("Load did not expand the reference")
	}
	d, err := LoadDocument("models.json")
	if err != nil {
		t.Fatal(err)
	}
	if d.Config().Providers["p"].APIKey != wdCanary {
		t.Fatal("LoadDocument did not expand the reference")
	}

	t.Setenv("GO_LLM_CONFIG", "models.json")
	cfg, err = Default()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["p"].APIKey != wdCanary {
		t.Fatal("Default with GO_LLM_CONFIG did not expand the reference")
	}
	d, err = DefaultDocument()
	if err != nil {
		t.Fatal(err)
	}
	if d.Origin().Source != OriginEnvOverride || d.Config().Providers["p"].APIKey != wdCanary {
		t.Fatalf("DefaultDocument with GO_LLM_CONFIG: origin %+v", d.Origin())
	}
}

func TestUserConfigReferenceStillExpands(t *testing.T) {
	userPath := isolateDiscovery(t)
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte(wdConfig("${CANARY_VAR}")), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("CANARY_VAR", wdCanary)

	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["p"].APIKey != wdCanary {
		t.Fatal("Default did not expand the user config reference")
	}
	d, err := DefaultDocument()
	if err != nil {
		t.Fatal(err)
	}
	if d.Origin().Source != OriginUserConfig {
		t.Fatalf("origin = %+v, want user-config", d.Origin())
	}
}

// Running from inside the user config directory: ./models.json IS the
// user's own config, so it keeps the user-config origin (Review Focus 3).
func TestWorkingDirIsUserConfigDir(t *testing.T) {
	userPath := isolateDiscovery(t)
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte(wdConfig("${CANARY_VAR}")), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(userPath))
	t.Setenv("CANARY_VAR", wdCanary)

	d, err := DefaultDocument()
	if err != nil {
		t.Fatal(err)
	}
	if d.Origin().Source != OriginUserConfig {
		t.Fatalf("origin = %+v, want user-config", d.Origin())
	}
	if d.Config().Providers["p"].APIKey != wdCanary {
		t.Fatal("user config reference not expanded")
	}
	if _, err := Default(); err != nil {
		t.Fatal(err)
	}
}

// A DIFFERENT ./models.json still beats the user config: only the same file
// takes the user-config origin.
func TestWorkingDirBeatsDifferentUserConfig(t *testing.T) {
	userPath := isolateDiscovery(t)
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte(wdConfig("sk-user-776")), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(wdConfig("sk-wd-776")), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	d, err := DefaultDocument()
	if err != nil {
		t.Fatal(err)
	}
	if d.Origin().Source != OriginWorkingDir || d.Config().Providers["p"].APIKey != "sk-wd-776" {
		t.Fatalf("origin %+v, api_key %q; want working-dir, sk-wd-776", d.Origin(), d.Config().Providers["p"].APIKey)
	}
	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["p"].APIKey != "sk-wd-776" {
		t.Fatalf("Default api_key = %q, want sk-wd-776", cfg.Providers["p"].APIKey)
	}
}

// A working-dir Document refuses a mutation that introduces a reference
// and is left exactly as it was (acceptance 4, 10); the same mutation on an
// explicit-origin Document of the same file succeeds.
func TestWorkingDirMutationRefusedAndUnchanged(t *testing.T) {
	workingDirWith(t, wdConfig("sk-literal-776"))
	t.Setenv("X", wdCanary)
	d, err := DefaultDocument()
	if err != nil {
		t.Fatal(err)
	}
	beforeCfg, beforeRev, beforeOrigin := d.Config(), d.Revision(), d.Origin()
	beforeBytes, err := d.canonicalBytes()
	if err != nil {
		t.Fatal(err)
	}

	err = d.SetProviderAPIKey("p", "${X}")
	assertDiag(t, err, CodeKeyReferenceRefused, SubjectProvider, "p")
	assertRefusalMessage(t, err, "X")

	if !reflect.DeepEqual(d.Config(), beforeCfg) {
		t.Fatal("refused mutation changed the effective config")
	}
	afterBytes, err := d.canonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterBytes, beforeBytes) {
		t.Fatal("refused mutation changed the rendered bytes")
	}
	if d.Revision() != beforeRev || d.Origin() != beforeOrigin {
		t.Fatalf("refused mutation changed revision or origin: %q %+v", d.Revision(), d.Origin())
	}

	explicit, err := LoadDocument("models.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := explicit.SetProviderAPIKey("p", "${X}"); err != nil {
		t.Fatalf("explicit-origin mutation refused: %v", err)
	}
}

// Saving elsewhere is consent (D1); saving to its own path is not
// (acceptance 4). The profiles.Store.SaveAs leg lives in package profiles.
func TestSaveElsewhereLiftsRefusal(t *testing.T) {
	workingDirDoc := func(t *testing.T) *Document {
		t.Helper()
		workingDirWith(t, wdConfig("sk-literal-776"))
		t.Setenv("X", wdCanary)
		d, err := DefaultDocument()
		if err != nil {
			t.Fatal(err)
		}
		err = d.SetProviderAPIKey("p", "${X}")
		assertDiag(t, err, CodeKeyReferenceRefused, SubjectProvider, "p")
		assertNoCanary(t, err)
		return d
	}
	for _, tc := range []struct {
		name string
		save func(t *testing.T, d *Document) error
	}{
		{"SaveNew elsewhere", func(t *testing.T, d *Document) error {
			return d.SaveNew(filepath.Join(t.TempDir(), "models.json"))
		}},
		{"SaveReplace elsewhere", func(t *testing.T, d *Document) error {
			other := filepath.Join(t.TempDir(), "models.json")
			if err := os.WriteFile(other, []byte(wdConfig("sk-literal-776")), 0o600); err != nil {
				t.Fatal(err)
			}
			return d.SaveReplace(other, d.Revision())
		}},
		{"SaveNewAs explicit elsewhere", func(t *testing.T, d *Document) error {
			return d.SaveNewAs(filepath.Join(t.TempDir(), "models.json"), OriginExplicit)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := workingDirDoc(t)
			if err := tc.save(t, d); err != nil {
				t.Fatal(err)
			}
			if err := d.SetProviderAPIKey("p", "${X}"); err != nil {
				t.Fatalf("mutation after save elsewhere: %v", err)
			}
			if d.Config().Providers["p"].APIKey != wdCanary {
				t.Fatal("reference not expanded after save elsewhere")
			}
		})
	}

	t.Run("save to own path", func(t *testing.T) {
		d := workingDirDoc(t)
		if err := d.SaveReplace("models.json", d.Revision()); err != nil {
			t.Fatal(err)
		}
		if d.Origin().Source != OriginWorkingDir {
			t.Fatalf("origin = %+v, want working-dir", d.Origin())
		}
		err := d.SetProviderAPIKey("p", "${X}")
		assertDiag(t, err, CodeKeyReferenceRefused, SubjectProvider, "p")
		assertNoCanary(t, err)
	})
}
