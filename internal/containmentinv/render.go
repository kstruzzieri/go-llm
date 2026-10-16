package containmentinv

import (
	"fmt"
	"slices"
	"strings"
)

// families is the rendering order. A pattern ending in "-" matches an ID
// prefix; any other pattern matches one ID exactly.
var families = []struct {
	name    string
	members []string
}{
	{"Paths", []string{"PATH-"}},
	{"Mutation substitution", []string{"MUT-"}},
	{"Environment", []string{"ENV-"}},
	{"Terminal", []string{"TERM-", "PREVIEW-PROFILE"}},
	{"Fingerprint", []string{"FP-"}},
	{"Grants", []string{"GRANT-"}},
	{"Ingress", []string{"FENCE-", "MCP-", "GUIDANCE-"}},
	{"Interception", []string{"DETECT-", "SECRETS", "INVARIANTS", "EGRESS", "PIPELINE-ORDER", "RISK-SNAPSHOT", "CANARY"}},
	{"Compound", []string{"CMP-"}},
	{"Strict", []string{"STRICT-"}},
	{"Exposure", []string{"EXPOSURE-"}},
	{"Pending boundaries", []string{"QUARANTINE", "RETRIEVAL-SCREEN"}},
	{"Model", []string{"MODEL-"}},
	{"Meta", []string{"META-"}},
}

func familyOf(id string) string {
	for _, f := range families {
		for _, m := range f.members {
			if id == m || strings.HasSuffix(m, "-") && strings.HasPrefix(id, m) {
				return f.name
			}
		}
	}
	return ""
}

// RenderMarkdown renders the declaration-only inventory: one table per family
// in a fixed order, scenario-backed statuses derived from the registry, then
// the profile table. It never shows observed results.
func RenderMarkdown(inv Inventory, scenarios []ScenarioDecl) []byte {
	decls := make(map[string]ScenarioDecl, len(scenarios))
	for _, s := range scenarios {
		decls[s.ID] = s
	}
	var b strings.Builder
	b.WriteString("# Containment requirement inventory (#452)\n\n")
	b.WriteString("<!-- Generated from docs/security/containment-inventory.json and docs/security/containment-scenarios.json: go test ./internal/containmentinv -run TestCommittedInventory -update-inventory. Do not edit. -->\n\n")
	b.WriteString("This inventory is a declaration. Each row names a #452 requirement, its status, the layer that evidences it, the exact test leaves and assertion sites or the artifacts behind it, and the profiles on which its refs are mandatory. An AST lint binds every ref to a `// containment: <ID>` marker inside the cited function (for a scenario leaf, the function bound to the scenario's registration, ahead of its AttackEffect assignment). The inventory never shows observed results: each checker run publishes those separately as evidence JSON, stamped with the forwarded tree and commit and, in a git checkout, measured for cleanliness against them; evidence is not cross-verified across profiles, and the Windows checkout's source digest differs from the other platforms' under core.autocrlf. Every mutation proof in docs/security/containment-mutations.md ran on darwin unless its entry names another platform.\n\n")
	b.WriteString("A row backed by containment scenarios takes its status from the scenario registry's declared outcome (docs/security/containment.md); the registry is the only owner of scenario outcomes. Statuses: `verified`, `uncontained(<owner>)`, `pending(<owner>)`, `unobserved(<profile>)`, `out-of-scope(<reason>)`.\n\n")
	for _, f := range families {
		var rows []Row
		for _, r := range inv.Rows {
			if familyOf(r.ID) == f.name {
				rows = append(rows, r)
			}
		}
		if len(rows) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(&b, "## %s\n\n", f.name)
		b.WriteString("| ID | Requirement | Status | Layer | Refs / Scenarios | Profiles |\n|---|---|---|---|---|---|\n")
		for _, r := range rows {
			status, err := rowStatus(r, decls)
			if err != nil {
				status = "invalid (see lint)"
			}
			row(&b, r.ID, r.Requirement, status, r.Layer, strings.Join(evidence(r), "<br>"), strings.Join(r.Profiles, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString("## Profiles\n\n")
	b.WriteString("A row is mandatory on the profiles it lists: its refs run there, at least one of them may not skip, and a ref with an allowed skip there may skip for exactly that reason. A row whose every ref may skip on a profile (an allowed skip, the first capability the scenario needs that the profile lacks) is skip-allowed there and does not list it; the checker still runs its leaves on that profile, and a skip must carry the allowed reason. Caps are the capabilities the profile's runner provides.\n\n")
	b.WriteString("| Profile | GOOS | Tags | Race | Caps | Timeout | Mandatory rows | Skip-allowed leaves |\n|---|---|---|---|---|---|---|---|\n")
	for _, p := range inv.Profiles {
		race := "no"
		if p.Race {
			race = "yes"
		}
		mandatory := 0
		for _, r := range inv.Rows {
			if slices.Contains(r.Profiles, p.Name) {
				mandatory++
			}
		}
		row(&b, p.Name, p.GOOS, strings.Join(p.Tags, ", "), race, strings.Join(p.Caps, ", "), p.Timeout,
			fmt.Sprint(mandatory), strings.Join(p.AllowedSkips, "<br>"))
	}
	return []byte(b.String())
}

func evidence(r Row) []string {
	var parts []string
	if len(r.Scenarios) > 0 {
		parts = append(parts, "scenarios: "+strings.Join(r.Scenarios, ", "))
	}
	for _, ref := range r.Refs {
		parts = append(parts, fmt.Sprintf("`%s` (%s: `%s`)", ref.Test, ref.File, ref.Func))
	}
	for _, a := range r.Artifacts {
		parts = append(parts, "artifact: "+a)
	}
	if len(r.Mutations) > 0 {
		parts = append(parts, "mutations: "+strings.Join(r.Mutations, ", "))
	}
	if r.Exempt != "" {
		parts = append(parts, "mutation exempt: "+r.Exempt)
	}
	return parts
}

// row writes one table row; empty cells render as "-" and pipes are escaped.
func row(b *strings.Builder, cells ...string) {
	b.WriteString("|")
	for _, c := range cells {
		if c == "" {
			c = "-"
		}
		_, _ = fmt.Fprintf(b, " %s |", strings.ReplaceAll(strings.ReplaceAll(c, "\n", " "), "|", `\|`))
	}
	b.WriteString("\n")
}
