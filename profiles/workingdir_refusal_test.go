package profiles

import (
	"context"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
)

// Saving a working-directory Document as a profile is consent (#776 D1):
// the key-reference refusal lifts once its origin becomes the profile.
// The config package's own save legs live in config/workingdir_refusal_test.go.
func TestSaveElsewhereLiftsRefusal(t *testing.T) {
	const canary = "CANARY-776-a91f3c06d2e8b745"
	t.Setenv("X", canary)
	d, err := config.ParseDocument([]byte(storeFixtureBody),
		config.Origin{Source: config.OriginWorkingDir, Path: "models.json"}, config.DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = d.SetProviderAPIKey("local", "${X}")
	if diag, _ := config.DiagnosticOf(err); diag.Code != config.CodeKeyReferenceRefused {
		t.Fatalf("precondition: want refusal, got %v", err)
	}
	if strings.Contains(err.Error(), "CANARY-776") {
		t.Fatalf("refusal leaks the secret: %v", err)
	}

	out, err := NewStore(t.TempDir()).SaveAs(context.Background(), "user/x", d, "")
	if err != nil || !out.Persisted {
		t.Fatalf("SaveAs: out=%+v err=%v", out, err)
	}
	if err := d.SetProviderAPIKey("local", "${X}"); err != nil {
		t.Fatalf("mutation after SaveAs: %v", err)
	}
	if d.Config().Providers["local"].APIKey != canary {
		t.Fatal("reference not expanded after SaveAs")
	}
}
