package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const namesCanaryA = "CANARY-776-a1b2c3d4e5f60718"
const namesCanaryB = "CANARY-776-0918f7e6d5c4b3a2"

func loadNamesConfig(t *testing.T, apiKey string) *Config {
	t.Helper()
	t.Setenv("NAMES_TEST_A", namesCanaryA)
	t.Setenv("NAMES_TEST_B", namesCanaryB)
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(wdConfig(apiKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAPIKeyRefNames(t *testing.T) {
	cfg := loadNamesConfig(t, "${NAMES_TEST_A}x${NAMES_TEST_B}")
	got := cfg.APIKeyRefNames("p")
	want := []string{"NAMES_TEST_A", "NAMES_TEST_B"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	if strings.Contains(strings.Join(got, ","), "CANARY") {
		t.Fatal("names contain a resolved value")
	}
	got[0] = "MUTATED"
	if again := cfg.APIKeyRefNames("p"); !reflect.DeepEqual(again, want) {
		t.Fatalf("returned slice aliases internal state: %v", again)
	}
	if cfg.APIKeyRefNames("nope") != nil {
		t.Fatal("unknown provider must be nil")
	}
	if lit := loadNamesConfig(t, "sk-literal"); lit.APIKeyRefNames("p") != nil {
		t.Fatal("literal key must be nil")
	}
}

func TestAPIKeyRefNamesSurviveClone(t *testing.T) {
	t.Setenv("NAMES_TEST_A", namesCanaryA)
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(wdConfig("${NAMES_TEST_A}")), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	c := doc.Config()
	if got := c.APIKeyRefNames("p"); !reflect.DeepEqual(got, []string{"NAMES_TEST_A"}) {
		t.Fatalf("clone names = %v", got)
	}
	// Mutating one clone's record must not leak into the next.
	c.resolved["p"].names[0] = "MUTATED"
	if got := doc.Config().APIKeyRefNames("p"); !reflect.DeepEqual(got, []string{"NAMES_TEST_A"}) {
		t.Fatalf("clone shares names: %v", got)
	}
}
