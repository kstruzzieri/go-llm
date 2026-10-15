package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var updateContainment = flag.Bool("update-containment", false, "rewrite docs/security/containment.{md,json} from the registry")

type containmentOutcome string

const (
	outcomeContained   containmentOutcome = "contained"
	outcomeUncontained containmentOutcome = "uncontained"
	outcomePending     containmentOutcome = "pending"
	outcomeOutOfScope  containmentOutcome = "out-of-scope"
)

type controlKind string

const (
	controlAuthorization controlKind = "authorization"
	controlStructural    controlKind = "structural"
)

type capability string

const (
	capBash    capability = "bash"
	capSymlink capability = "symlink"
	capExec    capability = "exec"
	capUnix    capability = "unix"
)

// scenario is one registered containment case (spec §5.2). run is nil for
// pending and out-of-scope rows, which never execute.
type scenario struct {
	ID          string
	Reqs        []string // inventory requirement IDs (spec §6.1)
	Protections string
	Attack      string
	Probe       string
	Declared    containmentOutcome
	Owner       string // issue, "unassigned", or "" for contained
	Reason      string // out-of-scope reason
	Control     controlKind
	ControlDiff string // structural controls: the declared fixture difference
	Caps        []capability
	run         func(t *testing.T) observation
}

type observation struct {
	ValidityErr   string // non-empty: the attack did not reach the intended boundary
	ControlEffect bool   // the matched control produced its effect
	AttackEffect  bool   // the forbidden effect happened
}

func judgeObservation(s scenario, o observation) error {
	if o.ValidityErr != "" {
		return fmt.Errorf("%s: validity: %s", s.ID, o.ValidityErr)
	}
	if !o.ControlEffect {
		return fmt.Errorf("%s: control produced no effect; the probe cannot distinguish blocked from never-ran", s.ID)
	}
	observed := outcomeContained
	if o.AttackEffect {
		observed = outcomeUncontained
	}
	if observed != s.Declared {
		return fmt.Errorf("%s: observed %s, declared %s (owner %s); if a fix landed, update the declaration and regenerate docs with -update-containment", s.ID, observed, s.Declared, s.Owner)
	}
	return nil
}

func missingCapabilityReason(caps []capability, has func(capability) bool) string {
	for _, c := range caps {
		if !has(c) {
			return "capability:" + string(c)
		}
	}
	return ""
}

func TestContainment(t *testing.T) {
	var contained, uncontained int
	var executed []string
	for _, s := range containmentScenarios() {
		if s.run == nil {
			continue // pending / out-of-scope: listed in docs, never a passing leaf
		}
		t.Run(s.ID, func(t *testing.T) {
			if r := missingCapabilityReason(s.Caps, hostHasCapability); r != "" {
				t.Skip(r)
			}
			if err := judgeObservation(s, s.run(t)); err != nil {
				t.Fatal(err)
			}
			executed = append(executed, s.ID)
			if s.Declared == outcomeContained {
				contained++
			} else {
				uncontained++
			}
		})
	}
	sort.Strings(executed)
	t.Logf("containment executed: %s", strings.Join(executed, ","))
	if n := contained + uncontained; n > 0 {
		t.Logf("containment observed rate: %d/%d", contained, n)
	} else {
		t.Logf("containment observed rate: N/A")
	}
}

func TestContainmentDeclarationsUpToDate(t *testing.T) {
	sc := containmentScenarios()
	files := map[string][]byte{
		"containment.md":             renderContainmentMarkdown(sc),
		"containment-scenarios.json": renderContainmentJSON(sc),
	}
	dir := filepath.Join("..", "..", "docs", "security")
	for name, want := range files {
		path := filepath.Join(dir, name)
		if *updateContainment {
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (run go test ./cmd/golem -run TestContainmentDeclarationsUpToDate -update-containment)", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s is stale; regenerate with -update-containment", path)
		}
	}
}

// containmentRow is the wire shape of one registry entry in
// docs/security/containment-scenarios.json. Slices are never nil so empty
// lists render as [] rather than null.
type containmentRow struct {
	ID          string   `json:"id"`
	Reqs        []string `json:"reqs"`
	Protections string   `json:"protections"`
	Attack      string   `json:"attack"`
	Probe       string   `json:"probe"`
	Declared    string   `json:"declared"`
	Owner       string   `json:"owner"`
	Reason      string   `json:"reason"`
	Control     string   `json:"control"`
	ControlDiff string   `json:"control_diff"`
	Caps        []string `json:"caps"`
}

func renderContainmentJSON(sc []scenario) []byte {
	rows := make([]containmentRow, 0, len(sc))
	for _, s := range sc {
		r := containmentRow{
			ID: s.ID, Reqs: append([]string{}, s.Reqs...), Protections: s.Protections,
			Attack: s.Attack, Probe: s.Probe, Declared: string(s.Declared), Owner: s.Owner,
			Reason: s.Reason, Control: string(s.Control), ControlDiff: s.ControlDiff,
			Caps: []string{},
		}
		for _, c := range s.Caps {
			r.Caps = append(r.Caps, string(c))
		}
		rows = append(rows, r)
	}
	// An Encoder, not MarshalIndent: Attack/Probe text carries &, < and > that
	// must not be HTML-escaped. Encode appends the single trailing newline.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		panic(err) // fixed string/slice shapes cannot fail to marshal
	}
	return buf.Bytes()
}

func mdCell(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

func renderContainmentMarkdown(sc []scenario) []byte {
	var b strings.Builder
	b.WriteString("# Golem runtime containment\n\n")
	b.WriteString("<!-- Generated from the cmd/golem containment registry: go test ./cmd/golem -run TestContainmentDeclarationsUpToDate -update-containment. Do not edit. -->\n\n")
	b.WriteString("Containment is decided only by effect probes: bytes, side-effect files, sink contents or exact environment maps. A probe counts only after every scripted step was consumed and the expected boundary was reached (validity), and after the matched control produced its effect. Prompts, records, badges, fencing and detector findings never decide containment. An unexpected failure is a test failure, never containment. Pending and out-of-scope rows never execute and never become passing leaves.\n\n")
	b.WriteString("The outside-root sink in D1-D2 is a local surrogate destination; it establishes neither network export nor an allowed-service policy.\n\n")

	var contained, uncontained int
	b.WriteString("## Executable scenarios\n\n")
	b.WriteString("| ID | Protections | Attack | Probe | Control | Declared | Owner |\n|---|---|---|---|---|---|---|\n")
	for _, s := range sc {
		switch s.Declared {
		case outcomeContained:
			contained++
		case outcomeUncontained:
			uncontained++
		default:
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", mdCell(s.ID), mdCell(s.Protections), mdCell(s.Attack), mdCell(s.Probe), mdCell(string(s.Control)), mdCell(string(s.Declared)), mdCell(s.Owner))
	}

	b.WriteString("\n## Pending and out-of-scope\n\n")
	b.WriteString("| ID | Protections | Attack | Declared | Owner | Reason |\n|---|---|---|---|---|---|\n")
	for _, s := range sc {
		if s.Declared == outcomeContained || s.Declared == outcomeUncontained {
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", mdCell(s.ID), mdCell(s.Protections), mdCell(s.Attack), mdCell(string(s.Declared)), mdCell(s.Owner), mdCell(s.Reason))
	}

	if n := contained + uncontained; n > 0 {
		fmt.Fprintf(&b, "\nDeclared linux-host ContainmentRate: %d / %d\n", contained, n)
	} else {
		b.WriteString("\nDeclared linux-host ContainmentRate: N/A\n")
	}
	return []byte(b.String())
}

func TestContainmentValidityRejectsWrongBoundary(t *testing.T) {
	s := scenario{ID: "Z1", Declared: outcomeContained}
	err := judgeObservation(s, observation{ValidityErr: "call rejected by invariant remote_script_execution before approval", ControlEffect: true})
	if err == nil || !strings.Contains(err.Error(), "validity") {
		t.Fatalf("invalid fixture reported as containment: %v", err)
	}
}

func TestContainmentControlMustSucceed(t *testing.T) {
	err := judgeObservation(scenario{ID: "Z1", Declared: outcomeContained}, observation{ControlEffect: false})
	if err == nil || !strings.Contains(err.Error(), "control") {
		t.Fatalf("a failed control passed: %v", err)
	}
}

func TestContainmentDeclaredMismatchMessage(t *testing.T) {
	s := scenario{ID: "X3", Declared: outcomeUncontained, Owner: "#420"}
	err := judgeObservation(s, observation{ControlEffect: true, AttackEffect: false})
	if err == nil || !strings.Contains(err.Error(), "X3") || !strings.Contains(err.Error(), "update the declaration") {
		t.Fatalf("mismatch message unhelpful: %v", err)
	}
}

func TestContainmentCapabilitySkipNamesCapability(t *testing.T) {
	if got := missingCapabilityReason([]capability{capBash}, func(capability) bool { return false }); got != "capability:bash" {
		t.Fatalf("skip reason = %q", got)
	}
}

func TestContainmentRegistryIntegrity(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range containmentScenarios() {
		if seen[s.ID] {
			t.Fatalf("duplicate scenario %s", s.ID)
		}
		seen[s.ID] = true
		switch s.Declared {
		case outcomeContained, outcomeUncontained, outcomePending, outcomeOutOfScope:
		default:
			t.Fatalf("%s: declared %q is not one of the four outcomes", s.ID, s.Declared)
		}
		switch s.Control {
		case "", controlAuthorization, controlStructural:
		default:
			t.Fatalf("%s: control %q is not authorization or structural", s.ID, s.Control)
		}
		if len(s.Reqs) == 0 {
			t.Fatalf("%s: no inventory requirement IDs (Reqs)", s.ID)
		}
		if s.Declared == outcomeOutOfScope && s.Reason == "" {
			t.Fatalf("%s: out-of-scope row without a Reason", s.ID)
		}
		if s.Control == controlStructural && s.ControlDiff == "" {
			t.Fatalf("%s: structural control without a declared ControlDiff", s.ID)
		}
		executable := s.Declared == outcomeContained || s.Declared == outcomeUncontained
		if executable != (s.run != nil) {
			t.Fatalf("%s: run present=%v but declared %s", s.ID, s.run != nil, s.Declared)
		}
		if executable && s.Control == "" {
			t.Fatalf("%s: executable scenario without a declared control kind", s.ID)
		}
		if (s.Declared == outcomeUncontained || s.Declared == outcomePending) && s.Owner == "" {
			t.Fatalf("%s: %s needs an owner (use \"unassigned\" explicitly)", s.ID, s.Declared)
		}
	}
}

// TestContainmentDeclaredRate pins the declared outcome counts as literals
// (not derived from the registry) so a row cannot be added, dropped or flipped
// without the spec 5.3 totals being revisited in review.
func TestContainmentDeclaredRate(t *testing.T) {
	var c, u, p, o int
	for _, s := range containmentScenarios() {
		switch s.Declared {
		case outcomeContained:
			c++
		case outcomeUncontained:
			u++
		case outcomePending:
			p++
		case outcomeOutOfScope:
			o++
		}
	}
	if c != 17 || u != 6 || p != 17 || o != 1 {
		t.Fatalf("declared %d/%d/%d/%d, want 17 contained, 6 uncontained, 17 pending, 1 out-of-scope (spec §5.3)", c, u, p, o)
	}
}
