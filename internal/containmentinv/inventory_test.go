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
	errs := Lint(inv, root, nil, nil)
	if !containsErr(errs, "PATH-ABS") || !containsErr(errs, "TestCited") {
		t.Fatalf("marker in the wrong function passed lint: %v", errs)
	}
}

// boundInventory is a lint-clean inventory over the fixture written by
// boundTree: one verified row whose single ref has its marker. Its profile is
// windows because linux-host may allow no skip and the lint cases give the
// profile one.
func boundInventory() Inventory {
	return Inventory{
		Profiles: []Profile{{Name: "windows", GOOS: "windows"}},
		Rows: []Row{{ID: "PATH-ABS", Source: "issue-452:52", Requirement: "absolute paths rejected",
			Status: "verified", Layer: "focused", Profiles: []string{"windows"}, Mutations: []string{"M-001"},
			Refs: []Ref{{Package: "./pkg", Test: "TestCited", File: "pkg/a_test.go", Func: "TestCited"}}}},
	}
}

// fixtureLedger is the mutation ledger the fixtures cite.
var fixtureLedger = []string{"M-001"}

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
	if errs := Lint(boundInventory(), root, nil, fixtureLedger); len(errs) != 0 {
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
	errs := Lint(boundInventory(), root, nil, fixtureLedger)
	if len(errs) != 1 || !containsErr(errs, "PATH-GHOST") || !containsErr(errs, "names no inventory row") {
		t.Fatalf("marker naming no inventory row: got %v, want exactly one error naming PATH-GHOST as unknown", errs)
	}
}

func TestInventoryScenarioStatusDerived(t *testing.T) {
	inv := Inventory{Rows: []Row{{ID: "FP-SCRIPT-CONTENT", Scenarios: []string{"X3"}, Status: "verified"}}}
	errs := Lint(inv, t.TempDir(), []ScenarioDecl{{ID: "X3", Declared: "uncontained", Owner: "#420"}}, nil)
	if !containsErr(errs, "derived") {
		t.Fatalf("stored status disagreeing with the registry passed: %v", errs)
	}
}

func TestInventoryPendingNeedsOwnerAndNoRefs(t *testing.T) {
	pending := func() Row {
		return Row{ID: "QUARANTINE", Source: "issue-452:45", Requirement: "quarantine of untrusted content",
			Status: "pending(#434)", Owner: "#434", Layer: "focused"}
	}
	if errs := Lint(Inventory{Rows: []Row{pending()}}, t.TempDir(), nil, nil); len(errs) != 0 {
		t.Fatalf("clean pending row rejected: %v", errs)
	}

	blank := pending()
	blank.Owner = ""
	if errs := Lint(Inventory{Rows: []Row{blank}}, t.TempDir(), nil, nil); !containsErr(errs, "QUARANTINE") || !containsErr(errs, "owner") {
		t.Fatalf("pending row without an owner passed: %v", errs)
	}

	// The cited marker exists, so the only defect left is the ref itself.
	root := t.TempDir()
	writeGo(t, root, "pkg/a_test.go", strings.Replace(boundTree, "PATH-ABS", "QUARANTINE", 1))
	withRefs := pending()
	withRefs.Profiles = []string{"linux-host"}
	withRefs.Refs = []Ref{{Package: "./pkg", Test: "TestCited", File: "pkg/a_test.go", Func: "TestCited"}}
	inv := Inventory{Profiles: []Profile{{Name: "linux-host", GOOS: "linux"}}, Rows: []Row{withRefs}}
	if errs := Lint(inv, root, nil, nil); !containsErr(errs, "QUARANTINE") || !containsErr(errs, "refs") {
		t.Fatalf("pending row with refs passed: %v", errs)
	}
}

// scenarioTree and scenarioFixture are a lint-clean scenario-backed row: X3
// claims FP-SCRIPT-CONTENT, the row lists X3 and cites its TestContainment leaf,
// and the marker sits in the scenario function.
const scenarioTree = `package pkg
import "testing"
func TestContainment(t *testing.T) {}
func x3() {
	// containment: FP-SCRIPT-CONTENT
}
`

func scenarioFixture() (Inventory, []ScenarioDecl) {
	inv := Inventory{
		Profiles: []Profile{{Name: "linux-host", GOOS: "linux"}},
		Rows: []Row{{ID: "FP-SCRIPT-CONTENT", Source: "issue-452:56", Requirement: "script change needs approval",
			Layer: "scenario", Scenarios: []string{"X3"}, Profiles: []string{"linux-host"}, Mutations: []string{"M-001"},
			Refs: []Ref{{Package: "./pkg", Test: "TestContainment/X3", File: "pkg/a_test.go", Func: "x3"}}}},
	}
	return inv, []ScenarioDecl{{ID: "X3", Reqs: []string{"FP-SCRIPT-CONTENT"}, Declared: "uncontained", Owner: "#420"}}
}

// TestInventoryLintRules breaks one rule per case, starting from a fixture
// that lints clean, and requires the rule's own message.
func TestInventoryLintRules(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scenario bool   // start from scenarioFixture instead of boundInventory
		tree     string // replaces the fixture's pkg/a_test.go when set
		edit     func(inv *Inventory, sc *[]ScenarioDecl)
		want     string
	}{
		{name: "clean bound fixture", edit: func(*Inventory, *[]ScenarioDecl) {}},
		{name: "clean scenario fixture", scenario: true, edit: func(*Inventory, *[]ScenarioDecl) {}},
		{name: "ref fields", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Refs[0].Func = "" },
			want: "needs package, test, file and func"},
		{name: "ref package", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Refs[0].Package = "./other" },
			want: "package ./other, want ./pkg"},
		{name: "ref file", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Refs[0].File = "pkg/missing_test.go" },
			want: "pkg/missing_test.go is not a scanned _test.go file"},
		{name: "ref function", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Refs[0].Func = "TestNope" },
			want: "function TestNope not found"},
		{name: "ref top-level test", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Refs[0].Test = "TestMissing/leaf" },
			want: "top-level test TestMissing not found"},
		{name: "marker outside any function", tree: boundTree + "\n// containment: PATH-ABS\nvar _ = 1\n",
			edit: func(*Inventory, *[]ScenarioDecl) {}, want: "PATH-ABS marker sits outside any function"},
		{name: "duplicate row", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows = append(inv.Rows, inv.Rows[0]) },
			want: "PATH-ABS: duplicate row ID"},
		{name: "family", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows = append(inv.Rows, Row{ID: "BOGUS-1", Status: "verified", Layer: "focused", Artifacts: []string{"a.md"}})
		}, want: "BOGUS-1: ID belongs to no inventory family"},
		{name: "layer", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Layer = "unit" },
			want: `layer "unit"`},
		{name: "stored status form", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Status = "done" },
			want: `status "done", want verified`},
		{name: "owner mismatch", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows = append(inv.Rows, Row{ID: "QUARANTINE", Status: "pending(#434)", Owner: "#999", Layer: "focused"})
		}, want: `does not name the row owner "#999"`},
		{name: "evidence", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows = append(inv.Rows, Row{ID: "PATH-NUL", Status: "verified", Layer: "focused"})
		}, want: "PATH-NUL: verified row has no refs and no artifacts"},
		{name: "model artifacts", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows = append(inv.Rows, Row{ID: "MODEL-STRATA", Status: "verified", Layer: "model"})
		}, want: "MODEL-STRATA: model row lists no artifacts"},
		{name: "mandatory profile", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Profiles = nil },
			want: "lists no mandatory profile"},
		{name: "unknown profile", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Profiles = []string{"mars"} },
			want: `unknown profile "mars"`},
		{name: "profile name", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Profiles = append(inv.Profiles, Profile{Name: "windows"})
		}, want: "empty or duplicate name"},
		{name: "allowed skip form", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Profiles[0].AllowedSkips = []string{"TestCited"} },
			want: "want <leaf>=<reason>"},
		{name: "allowed skip leaf", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Profiles[0].AllowedSkips = []string{"TestCited=capability:bash", "TestOther=capability:bash"}
		}, want: "allowed skip for TestOther, which no row mandatory on windows cites"},
		{name: "allowed skip reason", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Profiles[0].AllowedSkips = []string{"TestCited=flaky on CI"}
		}, want: `allowed skip reason "flaky on CI", want capability:<cap>`},
		{name: "linux-host allows no skip", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Profiles = append(inv.Profiles, Profile{Name: "linux-host", GOOS: "linux", AllowedSkips: []string{"TestCited=capability:bash"}})
			inv.Rows[0].Profiles = append(inv.Rows[0].Profiles, "linux-host")
		}, want: "profile linux-host must allow no skip, allows TestCited=capability:bash"},
		{name: "scenario-backed owner", scenario: true, edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Owner = "#420" },
			want: "stores owner"},
		{name: "undeclared scenario", scenario: true, edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Scenarios = []string{"X3", "X9"} },
			want: "scenario X9 is not declared"},
		{name: "scenario outcome", scenario: true, edit: func(_ *Inventory, sc *[]ScenarioDecl) { (*sc)[0].Declared = "maybe" },
			want: `unknown outcome "maybe"`},
		{name: "scenario owner", scenario: true, edit: func(_ *Inventory, sc *[]ScenarioDecl) { (*sc)[0].Owner = "" },
			want: "X3 is uncontained without an owner or reason"},
		{name: "split row", scenario: true, edit: func(inv *Inventory, sc *[]ScenarioDecl) {
			inv.Rows[0].Scenarios, inv.Rows[0].Refs = []string{"S1", "O1"}, nil
			*sc = []ScenarioDecl{{ID: "S1", Reqs: []string{"FP-SCRIPT-CONTENT"}, Declared: "pending", Owner: "#584"},
				{ID: "O1", Reqs: []string{"FP-SCRIPT-CONTENT"}, Declared: "out-of-scope", Reason: "outside the guarantee"}}
		}, want: "split the row"},
		{name: "executable scenario ref", scenario: true, edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Refs = nil },
			want: "executable scenario X3 has no TestContainment/X3 ref"},
		{name: "ref to an unlisted scenario", scenario: true, edit: func(inv *Inventory, sc *[]ScenarioDecl) {
			inv.Rows[0].Refs = append(inv.Rows[0].Refs, Ref{Package: "./pkg", Test: "TestContainment/X4", File: "pkg/a_test.go", Func: "x3"})
		}, want: "ref TestContainment/X4 cites scenario X4, which this row does not list"},
		{name: "row scenario not claiming it", scenario: true, edit: func(_ *Inventory, sc *[]ScenarioDecl) {
			(*sc)[0].Reqs = []string{"FP-SCRIPT-CONTENT-OTHER"}
		}, want: "scenario X3 does not list this row in its reqs"},
		{name: "scenario claim not listed", scenario: true, edit: func(_ *Inventory, sc *[]ScenarioDecl) {
			*sc = append(*sc, ScenarioDecl{ID: "X4", Reqs: []string{"FP-SCRIPT-CONTENT"}, Declared: "uncontained", Owner: "#420"})
		}, want: "FP-SCRIPT-CONTENT: row does not list scenario X4"},
		{name: "scenario claims unknown row", scenario: true, edit: func(_ *Inventory, sc *[]ScenarioDecl) {
			*sc = append(*sc, ScenarioDecl{ID: "X5", Reqs: []string{"FP-GHOST"}, Declared: "contained"})
		}, want: "scenario X5 claims FP-GHOST, which has no inventory row"},
		{name: "mutation missing", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Mutations = nil },
			want: "PATH-ABS: verified row cites no mutation ledger entry"},
		{name: "mutation missing on uncontained", scenario: true, edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Mutations = nil },
			want: "FP-SCRIPT-CONTENT: uncontained row cites no mutation ledger entry"},
		{name: "mutation unknown", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Mutations = []string{"M-001", "M-999"} },
			want: "mutation M-999 is not an entry of the mutation ledger"},
		{name: "exempt row needs no mutation", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows = append(inv.Rows, Row{ID: "META-GATE", Status: "verified", Layer: "focused", Exempt: "administrative: fixture", Artifacts: []string{"a.md"}})
		}},
		{name: "exempt reason on a verified security row", edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Exempt = "administrative: fixture" },
			want: "PATH-ABS: verified row carries an exempt reason; only an administrative META- row may"},
		{name: "exempt reason on an uncontained row", scenario: true, edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Exempt = "owned by #420" },
			want: "FP-SCRIPT-CONTENT: uncontained row carries an exempt reason"},
		{name: "model row needs no mutation", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows = append(inv.Rows, Row{ID: "MODEL-STRATA", Status: "verified", Layer: "model", Artifacts: []string{"a.md"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv, sc, tree := boundInventory(), []ScenarioDecl(nil), boundTree
			inv.Profiles[0].AllowedSkips = []string{"TestCited=capability:bash"}
			if tc.scenario {
				inv, sc = scenarioFixture()
				tree = scenarioTree
			}
			if tc.tree != "" {
				tree = tc.tree
			}
			root := t.TempDir()
			writeGo(t, root, "pkg/a_test.go", tree)
			tc.edit(&inv, &sc)
			errs := Lint(inv, root, sc, fixtureLedger)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("clean fixture rejected: %v", errs)
				}
				return
			}
			if !containsErr(errs, tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, errs)
			}
		})
	}
}

func TestLoadLedger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.md")
	writeGo(t, dir, "ledger.md", "# Ledger\n\n## M-001 — one\ntext ## M-009 — inline\n### M-008 — deeper\n## M-002 — two\n")
	if ids, err := LoadLedger(path); err != nil || strings.Join(ids, ",") != "M-001,M-002" {
		t.Fatalf("LoadLedger = %v, %v, want [M-001 M-002]", ids, err)
	}
	writeGo(t, dir, "ledger.md", "## M-001 — one\n## M-001 — again\n")
	if _, err := LoadLedger(path); err == nil || !strings.Contains(err.Error(), "duplicate ledger entry M-001") {
		t.Fatalf("duplicate entry: err = %v, want duplicate ledger entry M-001", err)
	}
}

func TestInventoryLoadRejects(t *testing.T) {
	const row = `{"id": "PATH-ABS"}`
	for _, tc := range []struct{ name, doc, want string }{
		{"clean", `{"schema": "containment-inventory/v1", "profiles": [], "rows": [` + row + `]}`, ""},
		{"unknown field", `{"schema": "containment-inventory/v1", "profiles": [], "rows": [{"id": "PATH-ABS", "statsu": "verified"}]}`, `unknown field "statsu"`},
		{"schema", `{"schema": "containment-inventory/v0", "profiles": [], "rows": [` + row + `]}`, `schema "containment-inventory/v0"`},
		{"duplicate ID", `{"schema": "containment-inventory/v1", "profiles": [], "rows": [` + row + `, ` + row + `]}`, "duplicate row ID PATH-ABS"},
		{"trailing data", `{"schema": "containment-inventory/v1", "profiles": [], "rows": [` + row + `]} {}`, "trailing data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "inv.json")
			if err := os.WriteFile(path, []byte(tc.doc), 0o644); err != nil {
				t.Fatal(err)
			}
			inv, err := Load(path)
			if tc.want == "" {
				if err != nil || len(inv.Rows) != 1 || inv.Rows[0].ID != "PATH-ABS" {
					t.Fatalf("clean inventory: %+v, %v", inv, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
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
	ledger, err := LoadLedger(filepath.Join(root, "docs", "security", "containment-mutations.md"))
	if err != nil {
		t.Fatal(err)
	}
	// containment: META-INVENTORY
	// containment: META-MUTATION
	for _, err := range Lint(inv, root, scenarios, ledger) {
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
