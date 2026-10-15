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
	"strings"
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
	data, err := os.ReadFile(path)
	if err != nil {
		return Inventory{}, fmt.Errorf("containmentinv: %w", err)
	}
	return Parse(data, path)
}

// Parse is Load over an inventory's bytes; name labels errors.
func Parse(data []byte, name string) (Inventory, error) {
	var inv Inventory
	if err := decodeStrict(data, name, &inv); err != nil {
		return Inventory{}, err
	}
	if inv.Schema != Schema {
		return Inventory{}, fmt.Errorf("containmentinv: %s: schema %q, want %q", name, inv.Schema, Schema)
	}
	seen := map[string]bool{}
	for _, r := range inv.Rows {
		if seen[r.ID] {
			return Inventory{}, fmt.Errorf("containmentinv: %s: duplicate row ID %s", name, r.ID)
		}
		seen[r.ID] = true
	}
	return inv, nil
}

// LoadScenarios reads the registry's scenario declarations.
func LoadScenarios(path string) ([]ScenarioDecl, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("containmentinv: %w", err)
	}
	var decls []ScenarioDecl
	if err := decodeStrict(data, path, &decls); err != nil {
		return nil, err
	}
	return decls, nil
}

// ledgerHeading is one mutation ledger entry heading: `## M-NNN — <property>`.
var ledgerHeading = regexp.MustCompile(`^## (M-[0-9]{3}) `)

// ledgerRowID is an inventory row ID in an Affected refs line once its
// backtick-quoted test names are removed: upper-case words joined by hyphens
// (PATH-ABS) or one upper-case word of three or more characters (SECRETS),
// so the (A, B) variant labels are not row IDs.
var ledgerRowID = regexp.MustCompile(`(?:^|[^\w-])([A-Z][A-Z0-9]*(?:-[A-Z0-9]+)+|[A-Z][A-Z0-9]{2,})(?:$|[^\w-])`)

var backticked = regexp.MustCompile("`[^`]*`")

// LedgerEntry is one mutation ledger entry: its ID and the inventory rows its
// `- Affected refs:` line names, in first-seen order.
type LedgerEntry struct {
	ID   string
	Rows []string
}

// LoadLedger returns the entries of a mutation ledger
// (docs/security/containment-mutations.md) in file order, rejecting a
// duplicate ID so a row's citation is never ambiguous. An entry's rows come
// from the first `- Affected refs:` line under its heading.
func LoadLedger(path string) ([]LedgerEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("containmentinv: %w", err)
	}
	var entries []LedgerEntry
	refsSeen := false
	for _, line := range strings.Split(string(data), "\n") {
		if m := ledgerHeading.FindStringSubmatch(line); m != nil {
			if slices.ContainsFunc(entries, func(e LedgerEntry) bool { return e.ID == m[1] }) {
				return nil, fmt.Errorf("containmentinv: %s: duplicate ledger entry %s", path, m[1])
			}
			entries, refsSeen = append(entries, LedgerEntry{ID: m[1]}), false
			continue
		}
		refs, ok := strings.CutPrefix(line, "- Affected refs:")
		if !ok || len(entries) == 0 || refsSeen {
			continue
		}
		refsSeen = true
		e := &entries[len(entries)-1]
		// Each match consumes one delimiter, so scan word by word.
		for _, word := range strings.Fields(backticked.ReplaceAllString(refs, " ")) {
			if m := ledgerRowID.FindStringSubmatch(word); m != nil && !slices.Contains(e.Rows, m[1]) {
				e.Rows = append(e.Rows, m[1])
			}
		}
	}
	return entries, nil
}

func decodeStrict(data []byte, name string, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("containmentinv: %s: %w", name, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("containmentinv: %s: trailing data after the JSON value", name)
	}
	return nil
}
