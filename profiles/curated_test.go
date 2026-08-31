package profiles

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/config"
)

// mustCuratedBytes is the test-side wrapper over curatedBytes — the
// production helper stays free of a testing import.
func mustCuratedBytes(t *testing.T, id ID) []byte {
	t.Helper()
	raw, err := curatedBytes(id)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return raw
}

// envRefRe accepts ONLY a whole-value single environment reference.
// (Declared in curated.go; tests pin it.)
func TestEnvRefRule(t *testing.T) {
	valid := []string{"${COMMANDCODE_KEY}", "${A1_B}"}
	invalid := []string{"literal-secret", "prefix${ENV}", "${ENV}suffix", "${lower}", "${}", "${A} ${B}"}
	for _, v := range valid {
		if !envRefRe.MatchString(v) {
			t.Fatalf("%q rejected, want accepted", v)
		}
	}
	for _, v := range invalid {
		if envRefRe.MatchString(v) {
			t.Fatal("invalid shape accepted") // no value in output
		}
	}
}

// ParseID error text is length-capped on a rune boundary — a multi-byte id
// must not leave a partial rune behind the cut. %q would launder a split
// byte into a literal \xNN escape, so absence of that artifact is the
// observable (and the raw capped text stays valid UTF-8).
func TestParseIDErrorTruncationRuneSafe(t *testing.T) {
	_, err := ParseID("a" + strings.Repeat("é", 60)) // byte 80 lands mid-rune
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), `\x`) {
		t.Fatalf("truncation split a rune: %q", err.Error())
	}
	if !utf8.ValidString(err.Error()) {
		t.Fatalf("invalid UTF-8 in error: %q", err.Error())
	}
}

// P0: no curated file may embed a credential — any nonempty api_key must be
// entirely one ${ENV} reference. Failures never print the value.
func TestCuratedFilesRejectEmbeddedCredentials(t *testing.T) {
	for _, id := range curatedIDs() {
		var cfg struct {
			Providers map[string]struct {
				APIKey string `json:"api_key"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(mustCuratedBytes(t, id), &cfg); err != nil {
			t.Fatalf("%s: parse: %v", id, err)
		}
		for name, p := range cfg.Providers {
			if p.APIKey != "" && !envRefRe.MatchString(p.APIKey) {
				t.Fatalf("curated %s provider %s: api_key is not a pure ${ENV} reference", id, name)
			}
		}
	}
}

// Every curated file must be loadable in a CLEAN environment once its
// (guaranteed pure) env references get placeholders. Uses config directly —
// the Store does not exist yet.
func TestCuratedFilesValidateClean(t *testing.T) {
	sawLocal := false
	for _, id := range curatedIDs() {
		raw := mustCuratedBytes(t, id)
		for _, envName := range envRefsIn(raw) {
			t.Setenv(envName, "ci-placeholder")
		}
		if _, err := config.NewDocumentFromBytes(raw, config.Origin{Source: config.OriginProfile, Path: "embedded:" + string(id)}); err != nil {
			t.Fatalf("curated %s does not validate: %v", id, err)
		}
		sawLocal = sawLocal || id == ID("curated/local")
	}
	if !sawLocal {
		t.Fatal("curated/local missing")
	}
}

// Metadata is pinned exactly: curated/local names its provenance.
func TestCuratedMetadata(t *testing.T) {
	info, ok := curatedInfo(ID("curated/local"))
	if !ok {
		t.Fatal("curated/local metadata missing")
	}
	if !strings.Contains(info.Description, "local llama-swap lineup") ||
		!strings.Contains(info.Description, "GOAT eval 2026-08-17") {
		t.Fatalf("description does not pin provenance: %q", info.Description)
	}
}

// Tripwire: every embedded file needs a curatedDescriptions row — a dropped
// file without one would silently vanish from List (Task 8 skips
// metadata-less ids). This is what makes "drop a sanitized file + one
// description row" the complete recipe.
func TestCuratedMetadataCoversAllFiles(t *testing.T) {
	for _, id := range curatedIDs() {
		info, ok := curatedInfo(id)
		if !ok {
			t.Fatalf("%s: embedded but has no curatedDescriptions row", id)
		}
		if !info.Curated || info.ID != id || info.Revision == "" {
			t.Fatalf("%s: malformed info: %+v", id, info)
		}
	}
}
