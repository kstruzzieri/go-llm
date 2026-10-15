// Package containmentinv is the #452 requirement inventory: the schema of
// docs/security/containment-inventory.json, the AST lint that binds every row
// to `// containment: <ID>` markers at its assertion sites, status derivation
// for rows backed by containment scenarios, and the declaration-only Markdown
// renderer.
package containmentinv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
)

// Schema is the only inventory schema Load accepts.
const Schema = "containment-inventory/v1"

// Inventory is the committed requirement inventory.
type Inventory struct {
	Schema   string    `json:"schema"`
	Profiles []Profile `json:"profiles"`
	Rows     []Row     `json:"rows"`
}

// Row is one requirement. A row backed by Scenarios stores no status of its
// own (or exactly the derived one): the cmd/golem registry owns the outcome.
// Mutations names the row's entries in the mutation ledger
// (docs/security/containment-mutations.md); Exempt is a non-empty reason that
// releases an administrative, pending, unobserved or out-of-scope row from
// that requirement (spec §6.4).
type Row struct {
	ID          string   `json:"id"`
	Source      string   `json:"source"`
	Requirement string   `json:"requirement"`
	Status      string   `json:"status"`
	Owner       string   `json:"owner"`
	Layer       string   `json:"layer"`
	Exempt      string   `json:"exempt"`
	Scenarios   []string `json:"scenarios"`
	Refs        []Ref    `json:"refs"`
	Profiles    []string `json:"profiles"`
	Mutations   []string `json:"mutations"`
	Artifacts   []string `json:"artifacts"`
}

// Ref binds a row to one executed test leaf and its assertion site. Test is the
// emitted `go test -json` name with `/`-separated components; File and Func
// name the top-level function holding the row's marker.
type Ref struct {
	Package string `json:"package"`
	Test    string `json:"test"`
	File    string `json:"file"`
	Func    string `json:"func"`
}

// Profile is one execution environment the checker runs mandatory refs in.
// AllowedSkips entries have the form `<leaf>=<skip reason>`.
type Profile struct {
	Name         string   `json:"name"`
	GOOS         string   `json:"goos"`
	Tags         []string `json:"tags"`
	Race         bool     `json:"race"`
	Caps         []string `json:"caps"`
	Timeout      string   `json:"timeout"`
	AllowedSkips []string `json:"allowed_skips"`
}

// ScenarioDecl mirrors one row of docs/security/containment-scenarios.json,
// the machine-readable declarations rendered by the cmd/golem registry.
type ScenarioDecl struct {
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

// Load reads an inventory, rejecting unknown fields, trailing data, an
// unknown schema and duplicate row IDs.
func Load(path string) (Inventory, error) {
	var inv Inventory
	if err := decodeStrict(path, &inv); err != nil {
		return Inventory{}, err
	}
	if inv.Schema != Schema {
		return Inventory{}, fmt.Errorf("containmentinv: %s: schema %q, want %q", path, inv.Schema, Schema)
	}
	seen := map[string]bool{}
	for _, r := range inv.Rows {
		if seen[r.ID] {
			return Inventory{}, fmt.Errorf("containmentinv: %s: duplicate row ID %s", path, r.ID)
		}
		seen[r.ID] = true
	}
	return inv, nil
}

// LoadScenarios reads the registry's scenario declarations.
func LoadScenarios(path string) ([]ScenarioDecl, error) {
	var decls []ScenarioDecl
	if err := decodeStrict(path, &decls); err != nil {
		return nil, err
	}
	return decls, nil
}

// ledgerHeading is one mutation ledger entry heading: `## M-NNN — <property>`.
var ledgerHeading = regexp.MustCompile(`(?m)^## (M-[0-9]{3}) `)

// LoadLedger returns the entry IDs of a mutation ledger
// (docs/security/containment-mutations.md) in file order, rejecting a
// duplicate ID so a row's citation is never ambiguous.
func LoadLedger(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("containmentinv: %w", err)
	}
	var ids []string
	for _, m := range ledgerHeading.FindAllSubmatch(data, -1) {
		id := string(m[1])
		if slices.Contains(ids, id) {
			return nil, fmt.Errorf("containmentinv: %s: duplicate ledger entry %s", path, id)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func decodeStrict(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("containmentinv: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("containmentinv: %s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("containmentinv: %s: trailing data after the JSON value", path)
	}
	return nil
}
