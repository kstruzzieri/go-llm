package containmentinv

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
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

func TestInventoryRefMustReachMarkedFunction(t *testing.T) {
	// containment: META-INVENTORY
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "unrelated test", body: "", wantErr: true},
		{name: "shadowed helper", body: "marked := func(*testing.T) {}; marked(t)", wantErr: true},
		{name: "unrelated field key", body: "_ = struct{ marked bool }{marked: true}", wantErr: true},
		{name: "direct helper", body: "marked(t)"},
		{name: "transitive helper", body: "bridge(t)"},
		{name: "registered callback", body: `t.Run("proof", marked)`},
		{name: "callback in selector receiver", body: "struct{ run func(*testing.T) }{run: marked}.run(t)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeGo(t, root, "pkg/a_test.go", `package pkg
import "testing"
func TestCited(t *testing.T) { `+tc.body+` }
func bridge(t *testing.T) { marked(t) }
`)
			writeGo(t, root, "pkg/proof_test.go", `package pkg
import "testing"
func marked(t *testing.T) {
	// containment: PATH-ABS
	if true { t.Fatal("proof ran") }
}
`)
			inv := boundInventory()
			inv.Rows[0].Refs = []Ref{{Package: "./pkg", Test: "TestCited", File: "pkg/proof_test.go", Func: "marked"}}
			errs := Lint(inv, root, nil, fixtureLedger)
			if tc.wantErr {
				if !containsErr(errs, "not reachable from test TestCited") {
					t.Fatalf("Lint(unrelated marked function) = %v, want unreachable-function error", errs)
				}
			} else if len(errs) > 0 {
				t.Fatalf("Lint(reachable marked function) = %v, want no errors", errs)
			}
		})
	}
}

func TestInventoryRefRejectsUnrelatedTopLevelTest(t *testing.T) {
	// containment: META-INVENTORY
	root := t.TempDir()
	writeGo(t, root, "pkg/a_test.go", boundTree+"\nfunc TestOther(t *testing.T) {}\n")
	inv := boundInventory()
	inv.Rows[0].Refs[0].Test = "TestOther"
	if errs := Lint(inv, root, nil, fixtureLedger); !containsErr(errs, "not reachable from test TestOther") {
		t.Fatalf("Lint(misbound top-level test) = %v, want unreachable-function error", errs)
	}
}

func TestInventoryRefExternalTestBridge(t *testing.T) {
	// containment: META-INVENTORY
	for _, importPath := range []string{"example.test/repo/pkg", "example.test/foreign/pkg"} {
		t.Run(importPath, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeGo(t, root, "go.mod", "module example.test/repo\n")
			writeGo(t, root, "pkg/aggregate_test.go", `package pkg_test
import (
	"testing"
	inside "`+importPath+`"
)
func TestCited(t *testing.T) { inside.RunProofs(t) }
`)
			writeGo(t, root, "pkg/bridge_test.go", `package pkg
import "testing"
func RunProofs(t *testing.T) {
	for _, proof := range []struct {name string; run func(*testing.T)}{{"proof", TestProof}} {
		t.Run(proof.name, proof.run)
	}
}
`)
			writeGo(t, root, "pkg/proof_test.go", strings.ReplaceAll(boundTree, "TestCited", "TestProof"))
			inv := boundInventory()
			inv.Rows[0].Refs = []Ref{{Package: "./pkg", Test: "TestCited/proof", File: "pkg/proof_test.go", Func: "TestProof"}}
			errs := Lint(inv, root, nil, fixtureLedger)
			if importPath == "example.test/repo/pkg" {
				if len(errs) != 0 {
					t.Fatalf("Lint(local external-test bridge) = %v, want no errors", errs)
				}
			} else if !containsErr(errs, "not reachable from test TestCited") {
				t.Fatalf("Lint(foreign import) = %v, want unreachable-function error", errs)
			}
		})
	}
}

// boundInventory is a lint-clean inventory over the fixture written by
// boundTree: one verified row whose two refs share their marker. Its profile
// is windows because linux-host may allow no skip and the lint cases give the
// profile one, for TestCited/sub, so TestCited keeps the row mandatory.
func boundInventory() Inventory {
	return Inventory{
		Profiles: []Profile{{Name: "windows", GOOS: "windows"}},
		Rows: []Row{{ID: "PATH-ABS", Source: "issue-452:52", Requirement: "absolute paths rejected",
			Status: "verified", Layer: "focused", Profiles: []string{"windows"}, Mutations: []string{"M-001"},
			Refs: []Ref{{Package: "./pkg", Test: "TestCited", File: "pkg/a_test.go", Func: "TestCited"},
				{Package: "./pkg", Test: "TestCited/sub", File: "pkg/a_test.go", Func: "TestCited"}}}},
	}
}

// fixtureLedger is the mutation ledger the bound fixture cites;
// scenarioLedger is the scenario fixture's.
var (
	fixtureLedger  = []LedgerEntry{{ID: "M-001", Rows: []string{"PATH-ABS"}}}
	scenarioLedger = []LedgerEntry{{ID: "M-001", Rows: []string{"FP-SCRIPT-CONTENT"}}}
)

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

// TestInventoryScanSkipsForeignTrees: markers under an underscore-prefixed
// directory (CI's _agentflow checkout) or a nested module are not this
// module's and are never scanned.
func TestInventoryScanSkipsForeignTrees(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "pkg/a_test.go", boundTree)
	stray := strings.Replace(boundTree, "PATH-ABS", "PATH-GHOST", 1)
	writeGo(t, root, "_agentflow/x/a_test.go", stray)
	writeGo(t, root, "nested/go.mod", "module nested\n")
	writeGo(t, root, "nested/a_test.go", stray)
	if errs := Lint(boundInventory(), root, nil, fixtureLedger); len(errs) != 0 {
		t.Fatalf("foreign trees scanned: %v", errs)
	}
	writeGo(t, root, "inmodule/a_test.go", stray)
	if errs := Lint(boundInventory(), root, nil, fixtureLedger); !containsErr(errs, "PATH-GHOST") {
		t.Fatalf("a marker in this module was not scanned: %v", errs)
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
// and the marker sits in x3, which registers X3, before its observation.
const scenarioTree = `package pkg
import "testing"
type observation struct{ ControlEffect, AttackEffect bool }
type scenario struct {
	ID  string
	run func() observation
}
func TestContainment(t *testing.T) {}
func executableScenarios() []scenario { return []scenario{x3()} }
func x3() scenario {
	return scenario{ID: "X3", run: func() observation {
		var o observation
		// containment: FP-SCRIPT-CONTENT
		o.AttackEffect = true
		return o
	}}
}
`

// testLiteral is a test's own scenario literal (no run:), as in
// TestContainmentDeclaredMismatchMessage; unreachableLiteral sets run: but no
// registry root calls it. Neither is a registry entry.
const testLiteral = `func TestMismatch(t *testing.T) {
	_ = scenario{ID: "X3"}
	// containment: FP-SCRIPT-CONTENT
	_ = observation{}
}
`

// noRunLiteral is reached from the registry but its literal sets no run:.
const noRunLiteral = `func noRun() scenario {
	_ = scenario{ID: "X3"}
	// containment: FP-SCRIPT-CONTENT
	_ = observation{}
	return scenario{}
}
`

const unreachableLiteral = `func stray() scenario {
	return scenario{ID: "X3", run: func() observation {
		// containment: FP-SCRIPT-CONTENT
		return observation{}
	}}
}
`

// probeFunc marks FP-SCRIPT-CONTENT in a function that registers no
// scenario; helperTree registers X4 through a wScenario call.
const probeFunc = `func probe() observation {
	// containment: FP-SCRIPT-CONTENT
	return observation{AttackEffect: true}
}
`

const helperTree = `func wScenario(id string) scenario {
	return scenario{ID: id, run: func() observation {
		// containment: FP-SCRIPT-CONTENT
		return observation{AttackEffect: true}
	}}
}
func pendingScenarios() []scenario { return []scenario{wScenario("X4")} }
`

func scenarioFixture() (Inventory, []ScenarioDecl) {
	inv := Inventory{
		Profiles: []Profile{{Name: "linux-host", GOOS: "linux"}},
		Rows: []Row{{ID: "FP-SCRIPT-CONTENT", Source: "issue-452:56", Requirement: "script change needs approval",
			Layer: "scenario", Scenarios: []string{"X3"}, Profiles: []string{"linux-host"}, Mutations: []string{"M-001"},
			Refs: []Ref{{Package: "./cmd/golem", Test: "TestContainment/X3", File: scenarioFile, Func: "x3"}}}},
	}
	return inv, []ScenarioDecl{{ID: "X3", Reqs: []string{"FP-SCRIPT-CONTENT"}, Declared: "uncontained", Owner: "#420", Caps: []string{"bash", "exec"}}}
}

// scenarioSkip adds a windows profile providing caps (comma-separated) that
// allows skip; X3's row stays mandatory only on linux-host.
func scenarioSkip(caps, skip string) func(*Inventory, *[]ScenarioDecl) {
	return func(inv *Inventory, _ *[]ScenarioDecl) {
		inv.Profiles = append(inv.Profiles, Profile{Name: "windows", GOOS: "windows", Caps: strings.FieldsFunc(caps, func(r rune) bool { return r == ',' }), AllowedSkips: []string{skip}})
	}
}

// TestInventoryLintRules breaks one rule per case, starting from a fixture
// that lints clean, and requires the rule's own message.
func TestInventoryLintRules(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scenario bool   // start from scenarioFixture instead of boundInventory
		tree     string // replaces the fixture's test file when set
		edit     func(inv *Inventory, sc *[]ScenarioDecl)
		ledger   []LedgerEntry // replaces fixtureLedger when set
		extra    string        // written to cmd/golem/containment_test.go when set
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
			inv.Profiles[0].AllowedSkips = []string{"TestCited/sub=capability:bash", "TestOther=capability:bash"}
		}, want: "allowed skip for TestOther, which no row cites"},
		{name: "skip-allowed row lists the profile", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows[0].Refs = inv.Rows[0].Refs[1:]
		}, want: "PATH-ABS: lists profile windows, but every ref is an allowed skip there"},
		{name: "skip-allowed row on its other profile", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows[0].Refs, inv.Rows[0].Profiles = inv.Rows[0].Refs[1:], []string{"linux-host"}
			inv.Profiles = append(inv.Profiles, Profile{Name: "linux-host", GOOS: "linux"})
		}},
		{name: "scenario skip names the first missing cap", scenario: true, edit: scenarioSkip("bash", "TestContainment/X3=capability:exec")},
		{name: "scenario skip for a cap the scenario lacks", scenario: true, edit: scenarioSkip("bash", "TestContainment/X3=capability:symlink"),
			want: "names capability symlink, which the scenario does not require (caps bash, exec)"},
		{name: "scenario skip for a cap the profile provides", scenario: true, edit: scenarioSkip("bash", "TestContainment/X3=capability:bash"),
			want: "names capability bash, which the profile provides"},
		{name: "scenario skip for a later missing cap", scenario: true, edit: scenarioSkip("", "TestContainment/X3=capability:exec"),
			want: "names capability exec, but the scenario reports the first cap the profile lacks, bash"},
		{name: "scenario skip for an undeclared scenario", scenario: true, edit: scenarioSkip("", "TestContainment/X9=capability:bash"),
			want: "allowed skip for scenario X9, which is not declared"},
		{name: "ref to a function not bound to the registration", scenario: true, tree: scenarioTree + probeFunc, edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows[0].Refs[0].Func = "probe"
		}, want: "probe is not bound to scenario X3's registration (x3)"},
		{name: "test literal without run is no registration", scenario: true, tree: scenarioTree + testLiteral, edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows[0].Refs[0].Func = "TestMismatch"
		}, want: "TestMismatch is not bound to scenario X3's registration (x3)"},
		{name: "reachable literal without run is no registration", scenario: true,
			tree: strings.Replace(scenarioTree, "return []scenario{x3()}", "return []scenario{x3(), noRun()}", 1) + noRunLiteral,
			edit: func(inv *Inventory, _ *[]ScenarioDecl) { inv.Rows[0].Refs[0].Func = "noRun" },
			want: "noRun is not bound to scenario X3's registration (x3)"},
		{name: "unreachable literal with run is no registration", scenario: true, tree: scenarioTree + unreachableLiteral, edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows[0].Refs[0].Func = "stray"
		}, want: "stray is not bound to scenario X3's registration (x3)"},
		{name: "scenario ref outside the registry file", scenario: true, extra: testLiteral, edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows[0].Refs[0].File, inv.Rows[0].Refs[0].Func = "cmd/golem/containment_test.go", "TestMismatch"
		}, want: "file cmd/golem/containment_test.go, want " + scenarioFile},
		{name: "ref to an unregistered scenario", scenario: true, edit: func(inv *Inventory, sc *[]ScenarioDecl) {
			inv.Rows[0].Scenarios = []string{"X3", "X5"}
			inv.Rows[0].Refs = append(inv.Rows[0].Refs, Ref{Package: "./cmd/golem", Test: "TestContainment/X5", File: scenarioFile, Func: "x3"})
			*sc = append(*sc, ScenarioDecl{ID: "X5", Reqs: []string{"FP-SCRIPT-CONTENT"}, Declared: "uncontained", Owner: "#420"})
		}, want: "no function in " + scenarioFile + " registers scenario X5"},
		{name: "ref to a helper-registered scenario", scenario: true, tree: scenarioTree + helperTree, edit: func(inv *Inventory, sc *[]ScenarioDecl) {
			inv.Rows[0].Scenarios = []string{"X3", "X4"}
			inv.Rows[0].Refs = append(inv.Rows[0].Refs, Ref{Package: "./cmd/golem", Test: "TestContainment/X4", File: scenarioFile, Func: "wScenario"})
			*sc = append(*sc, ScenarioDecl{ID: "X4", Reqs: []string{"FP-SCRIPT-CONTENT"}, Declared: "uncontained", Owner: "#420"})
		}},
		{name: "marker after the last observation", scenario: true,
			tree: strings.Replace(scenarioTree, "\t\t// containment: FP-SCRIPT-CONTENT\n\t\to.AttackEffect = true\n\t\treturn o\n",
				"\t\to.AttackEffect = true\n\t\treturn o\n\t\t// containment: FP-SCRIPT-CONTENT\n", 1),
			edit: func(*Inventory, *[]ScenarioDecl) {}, want: "no `// containment: FP-SCRIPT-CONTENT` marker in x3 precedes an AttackEffect assignment"},
		{name: "control write after the marker is not an AttackEffect assignment", scenario: true,
			tree: strings.Replace(scenarioTree, "\t\to.AttackEffect = true\n", "\t\to.ControlEffect = true\n", 1),
			edit: func(*Inventory, *[]ScenarioDecl) {}, want: "no `// containment: FP-SCRIPT-CONTENT` marker in x3 precedes an AttackEffect assignment"},
		{name: "AttackEffect read after the marker is not an assignment", scenario: true,
			tree: strings.Replace(scenarioTree, "\t\t// containment: FP-SCRIPT-CONTENT\n\t\to.AttackEffect = true\n",
				"\t\to.AttackEffect = true\n\t\t// containment: FP-SCRIPT-CONTENT\n\t\t_ = o.AttackEffect\n", 1),
			edit: func(*Inventory, *[]ScenarioDecl) {}, want: "no `// containment: FP-SCRIPT-CONTENT` marker in x3 precedes an AttackEffect assignment"},
		{name: "AttackEffect literal key after the marker is an assignment", scenario: true,
			tree: strings.Replace(scenarioTree, "\t\tvar o observation\n\t\t// containment: FP-SCRIPT-CONTENT\n\t\to.AttackEffect = true\n\t\treturn o\n",
				"\t\t// containment: FP-SCRIPT-CONTENT\n\t\treturn observation{AttackEffect: true}\n", 1),
			edit: func(*Inventory, *[]ScenarioDecl) {}},
		{name: "ledger entry not listing a citing row", edit: func(inv *Inventory, _ *[]ScenarioDecl) {
			inv.Rows[0].Mutations = []string{"M-001", "M-002"}
		}, ledger: []LedgerEntry{fixtureLedger[0], {ID: "M-002", Rows: []string{"PATH-NUL"}}},
			want: "PATH-ABS: cites M-002, whose Affected refs do not list this row"},
		{name: "ledger entry listing a row that does not cite it", ledger: []LedgerEntry{fixtureLedger[0], {ID: "M-002", Rows: []string{"PATH-ABS"}}},
			edit: func(*Inventory, *[]ScenarioDecl) {}, want: "PATH-ABS: ledger M-002 lists this row, which does not cite it"},
		{name: "ledger entry listing no row", ledger: []LedgerEntry{fixtureLedger[0], {ID: "M-002"}},
			edit: func(*Inventory, *[]ScenarioDecl) {}, want: "ledger M-002 lists no inventory row"},
		{name: "ledger entry listing an unknown row", ledger: []LedgerEntry{{ID: "M-001", Rows: []string{"PATH-ABS", "PATH-GHOST"}}},
			edit: func(*Inventory, *[]ScenarioDecl) {}, want: "ledger M-001 lists PATH-GHOST, which has no inventory row"},
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
			inv.Rows[0].Refs = append(inv.Rows[0].Refs, Ref{Package: "./cmd/golem", Test: "TestContainment/X4", File: scenarioFile, Func: "x3"})
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
			inv, sc, tree, ledger := boundInventory(), []ScenarioDecl(nil), boundTree, fixtureLedger
			inv.Profiles[0].AllowedSkips = []string{"TestCited/sub=capability:bash"}
			if tc.scenario {
				inv, sc = scenarioFixture()
				tree, ledger = scenarioTree, scenarioLedger
			}
			if tc.tree != "" {
				tree = tc.tree
			}
			root := t.TempDir()
			file := "pkg/a_test.go"
			if tc.scenario {
				file = scenarioFile
			}
			writeGo(t, root, file, tree)
			if tc.extra != "" {
				writeGo(t, root, "cmd/golem/containment_test.go", "package pkg\nimport \"testing\"\n"+tc.extra)
			}
			tc.edit(&inv, &sc)
			if tc.ledger != nil {
				ledger = tc.ledger
			}
			errs := Lint(inv, root, sc, ledger)
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
	writeGo(t, dir, "ledger.md", "# Ledger\n\n## M-001 — one\n"+
		"- Affected refs: PATH-ABS: `TestX/ONE-TWO`, `TestY`; FP-SCRIPT-CONTENT and CMP-EDIT-EXEC (A, B): `TestContainment/X3`; under A also SECRETS (`D3`)\n"+
		"- Affected refs: PATH-LATER: a second refs line is not read\n"+
		"text ## M-009 — inline\n### M-008 — deeper\n## M-002 — two\n")
	want := []LedgerEntry{{ID: "M-001", Rows: []string{"PATH-ABS", "FP-SCRIPT-CONTENT", "CMP-EDIT-EXEC", "SECRETS"}}, {ID: "M-002"}}
	if entries, err := LoadLedger(path); err != nil || !reflect.DeepEqual(entries, want) {
		t.Fatalf("LoadLedger = %+v, %v, want %+v", entries, err, want)
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
// committed Markdown is the rendering of the JSON.
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
	for _, r := range inv.Rows {
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
