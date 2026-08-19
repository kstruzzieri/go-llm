package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const docTestConfig = `{
  "providers": {
    "local": {"base_url": "http://localhost:8080", "api_format": "openai-compat", "api_key": "${DOC_TEST_KEY}"}
  },
  "models": {
    "agent": {"name": "m1", "provider": "local", "type": "dense"}
  },
  "defaults": {"chat": "agent", "agent": "agent"}
}`

func loadTestDoc(t *testing.T, body string) *Document {
	t.Helper()
	t.Setenv("DOC_TEST_KEY", "sekret-value")
	d, err := LoadDocument(writeTempConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestLoadDocumentOriginAndRevision(t *testing.T) {
	t.Setenv("DOC_TEST_KEY", "v")
	p := writeTempConfig(t, docTestConfig)
	d, err := LoadDocument(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Origin(); got.Source != OriginExplicit || got.Path != p {
		t.Fatalf("origin = %+v", got)
	}
	sum := sha256.Sum256([]byte(docTestConfig))
	if got, want := d.Revision(), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("revision = %q, want %q", got, want)
	}
}

// Effective is expanded; authored keeps the literal; Config() is a copy.
func TestLoadDocumentSecretsSplitAndCopy(t *testing.T) {
	d := loadTestDoc(t, docTestConfig)
	if got := d.Config().Providers["local"].APIKey; got != "sekret-value" {
		t.Fatalf("effective api_key = %q, want expanded", got)
	}
	if got := d.authored.Providers["local"].APIKey; got != "${DOC_TEST_KEY}" {
		t.Fatalf("authored api_key = %q, want literal", got)
	}
	c := d.Config()
	c.Defaults["chat"] = "hacked"
	if d.Config().Defaults["chat"] != "agent" {
		t.Fatal("Config() aliases document state")
	}
}

func TestLoadDocumentMissingFile(t *testing.T) {
	if _, err := LoadDocument(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("want error")
	}
}

// Invalid config (defaults naming a missing role) must fail exactly like Load.
func TestLoadDocumentValidatesEffective(t *testing.T) {
	t.Setenv("DOC_TEST_KEY", "v")
	bad := `{"providers":{},"models":{},"defaults":{"chat":"ghost"}}`
	if _, err := LoadDocument(writeTempConfig(t, bad)); err == nil {
		t.Fatal("want validation error")
	}
}

func TestDefaultDocumentEnvOverride(t *testing.T) {
	t.Setenv("DOC_TEST_KEY", "v")
	p := writeTempConfig(t, docTestConfig)
	t.Setenv("GO_LLM_CONFIG", p)
	d, err := DefaultDocument()
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Origin(); got.Source != OriginEnvOverride || got.Path != p {
		t.Fatalf("origin = %+v", got)
	}
}

func TestDefaultDocumentEnvSetButEmpty(t *testing.T) {
	t.Setenv("GO_LLM_CONFIG", "")
	if _, err := DefaultDocument(); err == nil {
		t.Fatal("want hard error for set-but-empty GO_LLM_CONFIG (Default parity)")
	}
}

// Working-dir winner, and env-beats-working-dir precedence. The user-config
// and legacy winners depend on os.UserConfigDir/os.UserHomeDir, which are not
// env-fakeable on darwin; they stay covered by discoverConfigPath being the
// single implementation shared with Default() and Default's existing tests.
func TestDefaultDocumentWorkingDirAndPrecedence(t *testing.T) {
	t.Setenv("DOC_TEST_KEY", "v")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(docTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	// Unset GO_LLM_CONFIG for the working-dir leg, restoring whatever the
	// invoking shell had (it is commonly exported on dev machines).
	if prev, ok := os.LookupEnv("GO_LLM_CONFIG"); ok {
		t.Cleanup(func() { _ = os.Setenv("GO_LLM_CONFIG", prev) })
	}
	if err := os.Unsetenv("GO_LLM_CONFIG"); err != nil {
		t.Fatal(err)
	}
	d, err := DefaultDocument()
	if err != nil {
		t.Fatal(err)
	}
	if d.Origin().Source != OriginWorkingDir {
		t.Fatalf("origin = %+v, want working-dir", d.Origin())
	}
	// env override beats the working-dir file
	p2 := writeTempConfig(t, docTestConfig)
	t.Setenv("GO_LLM_CONFIG", p2)
	d2, err := DefaultDocument()
	if err != nil {
		t.Fatal(err)
	}
	if d2.Origin().Source != OriginEnvOverride || d2.Origin().Path != p2 {
		t.Fatalf("precedence: origin = %+v, want env-override %s", d2.Origin(), p2)
	}
}
