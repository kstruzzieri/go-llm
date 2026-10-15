package containmentinv

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateInventory = flag.Bool("update-inventory", false, "rewrite docs/security/containment-inventory.md from the inventory JSON")

func writeGo(t *testing.T, root, rel, src string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func containsErr(errs []error, sub string) bool {
	for _, err := range errs {
		if strings.Contains(err.Error(), sub) {
			return true
		}
	}
	return false
}

func TestInventoryMarkerMustSitInCitedFunction(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "pkg/a_test.go", `package pkg
import "testing"
func TestCited(t *testing.T) {}
func TestOther(t *testing.T) {
	// containment: PATH-ABS
	_ = 1
}`)
	inv := Inventory{Rows: []Row{{ID: "PATH-ABS", Status: "verified", Layer: "focused",
		Refs: []Ref{{Package: "./pkg", Test: "TestCited", File: "pkg/a_test.go", Func: "TestCited"}}}}}
	errs := Lint(inv, root, nil)
	if !containsErr(errs, "PATH-ABS") || !containsErr(errs, "TestCited") {
		t.Fatalf("marker in the wrong function passed lint: %v", errs)
	}
}

// boundInventory is a lint-clean inventory over the fixture written by
// boundTree: one verified row whose single ref has its marker.
func boundInventory() Inventory {
	return Inventory{
		Profiles: []Profile{{Name: "linux-host", GOOS: "linux"}},
		Rows: []Row{{ID: "PATH-ABS", Source: "issue-452:52", Requirement: "absolute paths rejected",
			Status: "verified", Layer: "focused", Profiles: []string{"linux-host"},
			Refs: []Ref{{Package: "./pkg", Test: "TestCited", File: "pkg/a_test.go", Func: "TestCited"}}}},
	}
}

const boundTree = `package pkg
import "testing"
func TestCited(t *testing.T) {
	// containment: PATH-ABS
	_ = 1
}
`

func TestInventoryOrphanMarkerFails(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "pkg/a_test.go", boundTree)
	if errs := Lint(boundInventory(), root, nil); len(errs) != 0 {
		t.Fatalf("clean fixture rejected: %v", errs)
	}
	writeGo(t, root, "pkg/b_test.go", `package pkg
import "testing"
func TestStray(t *testing.T) {
	// containment: PATH-GHOST
	_ = 1
}
`)
	// The orphan must be reported as an unknown ID, not merely as an unbound marker.
	errs := Lint(boundInventory(), root, nil)
	if len(errs) != 1 || !containsErr(errs, "PATH-GHOST") || !containsErr(errs, "names no inventory row") {
		t.Fatalf("marker naming no inventory row: got %v, want exactly one error naming PATH-GHOST as unknown", errs)
	}
}

func TestInventoryScenarioStatusDerived(t *testing.T) {
	inv := Inventory{Rows: []Row{{ID: "FP-SCRIPT-CONTENT", Scenarios: []string{"X3"}, Status: "verified"}}}
	errs := Lint(inv, t.TempDir(), []ScenarioDecl{{ID: "X3", Declared: "uncontained", Owner: "#420"}})
	if !containsErr(errs, "derived") {
		t.Fatalf("stored status disagreeing with the registry passed: %v", errs)
	}
}

func TestInventoryPendingNeedsOwnerAndNoRefs(t *testing.T) {
	pending := func() Row {
		return Row{ID: "QUARANTINE", Source: "issue-452:45", Requirement: "quarantine of untrusted content",
			Status: "pending(#434)", Owner: "#434", Layer: "focused"}
	}
	if errs := Lint(Inventory{Rows: []Row{pending()}}, t.TempDir(), nil); len(errs) != 0 {
		t.Fatalf("clean pending row rejected: %v", errs)
	}

	blank := pending()
	blank.Owner = ""
	if errs := Lint(Inventory{Rows: []Row{blank}}, t.TempDir(), nil); !containsErr(errs, "QUARANTINE") || !containsErr(errs, "owner") {
		t.Fatalf("pending row without an owner passed: %v", errs)
	}

	// The cited marker exists, so the only defect left is the ref itself.
	root := t.TempDir()
	writeGo(t, root, "pkg/a_test.go", strings.Replace(boundTree, "PATH-ABS", "QUARANTINE", 1))
	withRefs := pending()
	withRefs.Profiles = []string{"linux-host"}
	withRefs.Refs = []Ref{{Package: "./pkg", Test: "TestCited", File: "pkg/a_test.go", Func: "TestCited"}}
	inv := Inventory{Profiles: []Profile{{Name: "linux-host", GOOS: "linux"}}, Rows: []Row{withRefs}}
	if errs := Lint(inv, root, nil); !containsErr(errs, "QUARANTINE") || !containsErr(errs, "refs") {
		t.Fatalf("pending row with refs passed: %v", errs)
	}
}

// TestCommittedInventory is the inventory gate: the committed JSON lints clean
// against this tree and the scenario registry, every artifact exists, and the
// committed Markdown is the rendering of the JSON. MODEL-* artifacts land with
// the #452 model-resistance PR; GO_LLM_INVENTORY_PREMERGE=1 skips only those
// until this branch is rebased onto it. Never set it in CI or a script.
func TestCommittedInventory(t *testing.T) {
	root := filepath.Join("..", "..")
	inv, err := Load(filepath.Join(root, "docs", "security", "containment-inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := LoadScenarios(filepath.Join(root, "docs", "security", "containment-scenarios.json"))
	if err != nil {
		t.Fatal(err)
	}
	// containment: META-INVENTORY
	for _, err := range Lint(inv, root, scenarios) {
		t.Error(err)
	}
	premerge := os.Getenv("GO_LLM_INVENTORY_PREMERGE") == "1"
	for _, r := range inv.Rows {
		if premerge && strings.HasPrefix(r.ID, "MODEL-") {
			continue
		}
		for _, a := range r.Artifacts {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(a))); err != nil {
				t.Errorf("%s: artifact %s: %v", r.ID, a, err)
			}
		}
	}
	md := filepath.Join(root, "docs", "security", "containment-inventory.md")
	got := RenderMarkdown(inv, scenarios)
	if *updateInventory {
		if err := os.WriteFile(md, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(md)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("docs/security/containment-inventory.md is stale; regenerate with: go test ./internal/containmentinv -run TestCommittedInventory -update-inventory")
	}
}
