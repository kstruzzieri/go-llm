package interceptor

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"runtime"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// guardCall builds a tool-call inspection for a named tool with raw
// arguments. ID c9 and step 2 are arbitrary and pinned by the finding test.
func guardCall(name, args string) agent.ToolCallInspection {
	return agent.ToolCallInspection{Step: 2, Call: provider.ToolCall{ID: "c9", Type: "function",
		Function: provider.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}}
}

// inspect runs the default table over one call and returns its findings.
func inspect(t *testing.T, tool, args string) []agent.Finding {
	t.Helper()
	iv := mustInvariants(DefaultInvariants())
	found, err := iv.InspectToolCall(context.Background(), guardCall(tool, args))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// expectOne asserts exactly one finding with the given rule and detail.
func expectOne(t *testing.T, found []agent.Finding, rule, detail string) {
	t.Helper()
	if len(found) != 1 || found[0].Rule != rule || found[0].Detail != detail || found[0].Verdict != agent.VerdictBlock {
		t.Fatalf("findings = %+v, want one %s block: %s", found, rule, detail)
	}
}

func expectNone(t *testing.T, found []agent.Finding) {
	t.Helper()
	if len(found) != 0 {
		t.Fatalf("findings = %+v, want none", found)
	}
}

func TestNewInvariantsRejectsMalformedTables(t *testing.T) {
	deny := PathDeny{Pattern: regexp.MustCompile(`x`)}
	cases := []struct {
		name  string
		table []Invariant
		want  string
	}{
		{"no tool", []Invariant{{Name: "n", Field: "f", Check: deny}}, "interceptor: invariant 0 has no tool"},
		{"bad name", []Invariant{{Tool: "t", Name: "has space", Field: "f", Check: deny}}, `interceptor: invariant 0 (t) has invalid name "has space"`},
		{"empty name", []Invariant{{Tool: "t", Field: "f", Check: deny}}, `interceptor: invariant 0 (t) has invalid name ""`},
		{"reserved name", []Invariant{{Tool: "t", Name: "ambiguous_argument", Field: "f", Check: deny}}, `interceptor: invariant 0 (t) has invalid name "ambiguous_argument"`},
		{"no field", []Invariant{{Tool: "t", Name: "n", Check: deny}}, "interceptor: invariant t/n has no field"},
		{"no check", []Invariant{{Tool: "t", Name: "n", Field: "f"}}, "interceptor: invariant t/n has no check"},
		{"nil pattern", []Invariant{{Tool: "t", Name: "n", Field: "f", Check: PathDeny{}}}, "interceptor: invariant t/n has a PathDeny with no pattern"},
		{"pointer check", []Invariant{{Tool: "t", Name: "n", Field: "f", Check: &deny}}, "interceptor: invariant t/n has unsupported check kind *interceptor.PathDeny"},
		{"typed nil check", []Invariant{{Tool: "t", Name: "n", Field: "f", Check: (*PathDeny)(nil)}}, "interceptor: invariant t/n has unsupported check kind *interceptor.PathDeny"},
		{"duplicate", []Invariant{{Tool: "t", Name: "n", Field: "f", Check: deny}, {Tool: "t", Name: "n", Field: "g", Check: deny}}, "interceptor: duplicate invariant t/n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewInvariants(tc.table)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := NewInvariants(DefaultInvariants()); err != nil {
		t.Fatalf("default table must validate: %v", err)
	}
}

// TestNewInvariantsOwnsItsTable: mutating the caller's table after
// construction changes nothing the guard enforces.
func TestNewInvariantsOwnsItsTable(t *testing.T) {
	table := []Invariant{{Tool: "write_file", Name: "protected_path", Field: "path", Check: PathDeny{Pattern: regexp.MustCompile(`(^|/)\.git(/|$)`)}}}
	iv, err := NewInvariants(table)
	if err != nil {
		t.Fatal(err)
	}
	table[0].Field = "other"
	table[0].Tool = "other_tool"
	table[0] = Invariant{}
	found, err := iv.InspectToolCall(context.Background(), guardCall("write_file", `{"path":".git/config"}`))
	if err != nil {
		t.Fatal(err)
	}
	expectOne(t, found, "protected_path", `path ".git/config" matches protected pattern`)
}

// TestNewInvariantsOwnsPatternData: the guard holds its own compiled
// pattern, so the caller flipping Longest on its handle while inspections
// run neither races (the race detector would flag a shared Regexp) nor
// changes what is blocked.
func TestNewInvariantsOwnsPatternData(t *testing.T) {
	re := regexp.MustCompile(`(^|/)\.git(/|$)`)
	iv, err := NewInvariants([]Invariant{{Tool: "write_file", Name: "protected_path", Field: "path", Check: PathDeny{Pattern: re}}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				found, err := iv.InspectToolCall(context.Background(), guardCall("write_file", `{"path":".git/config"}`))
				if err != nil || len(found) != 1 {
					t.Errorf("findings = %+v, %v", found, err)
					return
				}
			}
		})
	}
	for range 200 {
		re.Longest()
	}
	wg.Wait()
	expectOne(t, inspectWith(t, iv, "write_file", `{"path":"sub/.git/config"}`), "protected_path", `path "sub/.git/config" matches protected pattern`)
}

// inspectWith runs one call through a specific guard.
func inspectWith(t *testing.T, iv Invariants, tool, args string) []agent.Finding {
	t.Helper()
	found, err := iv.InspectToolCall(context.Background(), guardCall(tool, args))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestPathDenyComponentMatrix covers every protected component on every write
// tool, at the root and nested, and every credential component plus exact
// .env on read_file.
func TestPathDenyComponentMatrix(t *testing.T) {
	writeTools := map[string]string{
		"write_file":       `{"path":%s,"content":"x"}`,
		"edit_file":        `{"path":%s,"old_string":"a","new_string":"b"}`,
		"promote_artifact": `{"id":"1","path":%s}`,
	}
	for tool, tmpl := range writeTools {
		for _, comp := range []string{".git", ".ssh", ".gnupg", ".aws", ".kube"} {
			for _, p := range []string{comp, comp + "/x", "sub/" + comp + "/x", "a/b/" + comp} {
				t.Run(tool+"/"+p, func(t *testing.T) {
					found := inspect(t, tool, sprintfJSON(tmpl, p))
					expectOne(t, found, "protected_path", `path "`+p+`" matches protected pattern`)
				})
			}
		}
		t.Run(tool+"/env is writable", func(t *testing.T) {
			expectNone(t, inspect(t, tool, sprintfJSON(tmpl, ".env")))
		})
	}
	for _, comp := range []string{".ssh", ".gnupg", ".aws", ".kube"} {
		for _, p := range []string{comp, comp + "/x", "sub/" + comp + "/x"} {
			t.Run("read_file/"+p, func(t *testing.T) {
				expectOne(t, inspect(t, "read_file", sprintfJSON(`{"path":%s}`, p)), "credential_path", `path "`+p+`" matches protected pattern`)
			})
		}
	}
	for _, p := range []string{".env", "sub/.env", "./.env"} {
		t.Run("read_file/"+p, func(t *testing.T) {
			clean := p
			if p == "./.env" {
				clean = ".env"
			}
			expectOne(t, inspect(t, "read_file", sprintfJSON(`{"path":%s}`, p)), "credential_path", `path "`+clean+`" matches protected pattern`)
		})
	}
	for _, p := range []string{".git/config", ".env.example", ".env.local", "env", "sub/.envrc", ".environment"} {
		t.Run("read_file allows "+p, func(t *testing.T) {
			expectNone(t, inspect(t, "read_file", sprintfJSON(`{"path":%s}`, p)))
		})
	}
}

// TestPathDenyNormalization pins case folding, native parent traversal, the
// documented non-matching neighbors, and non-string values.
func TestPathDenyNormalization(t *testing.T) {
	cases := []struct {
		name   string
		args   string
		detail string // "" means no finding
	}{
		{"case folded", `{"path":"sub/.Git/config","content":"x"}`, `path "sub/.git/config" matches protected pattern`},
		{"upper component", `{"path":".SSH/id_rsa","content":"x"}`, `path ".ssh/id_rsa" matches protected pattern`},
		{"dot-dot cleaned", `{"path":"foo/../.ssh/id_rsa","content":"x"}`, `path ".ssh/id_rsa" matches protected pattern`},
		{"dot-slash cleaned", `{"path":"./.git/hooks/pre-commit","content":"x"}`, `path ".git/hooks/pre-commit" matches protected pattern`},
		{"absolute kube", `{"path":"/abs/.kube/config","content":"x"}`, `path "/abs/.kube/config" matches protected pattern`},
		{"parent escape kept", `{"path":"../.git/config","content":"x"}`, `path "../.git/config" matches protected pattern`},
		{"gitignore is not .git", `{"path":".gitignore","content":"x"}`, ""},
		{"github dir is not .git", `{"path":".github/workflows/ci.yml","content":"x"}`, ""},
		{"plain git dir", `{"path":"notes/git/x","content":"x"}`, ""},
		{"prefix only", `{"path":".gitmodules","content":"x"}`, ""},
		{"ordinary file", `{"path":"README.md","content":"x"}`, ""},
		{"empty path", `{"path":"","content":"x"}`, ""},
		{"numeric path is the tool's problem", `{"path":3,"content":"x"}`, ""},
		{"null path is the tool's problem", `{"path":null,"content":"x"}`, ""},
		{"array path is the tool's problem", `{"path":[".git"],"content":"x"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found := inspect(t, "write_file", tc.args)
			if tc.detail == "" {
				expectNone(t, found)
				return
			}
			expectOne(t, found, "protected_path", tc.detail)
		})
	}
	if runtime.GOOS != "windows" {
		// On POSIX a backslash is a filename character, not a separator: the
		// workspace would create one oddly named file, not a path under .aws.
		t.Run("posix backslash is a filename character", func(t *testing.T) {
			expectNone(t, inspect(t, "write_file", `{"path":"sub\\.aws\\credentials","content":"x"}`))
		})
	}
}

// TestPathDenyDecoderParity: the guard reads the argument the way the tool's
// struct decoder does. Case-equivalent spellings are guarded; two equivalent
// members are ambiguous and blocked as such, whatever their values; unrelated
// members, invalid unrelated members, and non-object arguments do not disturb
// the guarded check.
func TestPathDenyDecoderParity(t *testing.T) {
	cases := []struct {
		name   string
		args   string
		rule   string // "" means no finding
		detail string
	}{
		{"capitalized key", `{"Path":".git/hooks/pre-commit","content":"x"}`, "protected_path", `path ".git/hooks/pre-commit" matches protected pattern`},
		{"upper key", `{"PATH":".ssh/id_rsa","content":"x"}`, "protected_path", `path ".ssh/id_rsa" matches protected pattern`},
		{"two spellings, bad last", `{"path":"README.md","PATH":".git/hooks/pre-commit","content":"x"}`, "ambiguous_argument", `argument "path" appears 2 times under equivalent spellings`},
		{"two spellings, bad first", `{"PATH":".git/hooks/pre-commit","path":"README.md","content":"x"}`, "ambiguous_argument", `argument "path" appears 2 times under equivalent spellings`},
		{"exact duplicate, both benign", `{"path":"a.txt","path":"b.txt","content":"x"}`, "ambiguous_argument", `argument "path" appears 2 times under equivalent spellings`},
		{"three spellings", `{"path":"a","Path":"b","PATH":"c"}`, "ambiguous_argument", `argument "path" appears 3 times under equivalent spellings`},
		{"unrelated duplicate is fine", `{"content":"a","content":"b","path":".git/x"}`, "protected_path", `path ".git/x" matches protected pattern`},
		{"unrelated invalid type is fine", `{"content":7,"path":".git/x"}`, "protected_path", `path ".git/x" matches protected pattern`},
		{"unrelated nested object is skipped", `{"meta":{"path":".git/x"},"path":"ok.txt"}`, "", ""},
		{"missing field", `{"content":"x"}`, "", ""},
		{"empty object", `{}`, "", ""},
		{"array arguments", `[".git"]`, "", ""},
		{"string arguments", `".git"`, "", ""},
		{"null arguments", `null`, "", ""},
		{"unlisted tool", `{"path":".git"}`, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := "write_file"
			if tc.name == "unlisted tool" {
				tool = "list"
			}
			found := inspect(t, tool, tc.args)
			if tc.rule == "" {
				expectNone(t, found)
				return
			}
			expectOne(t, found, tc.rule, tc.detail)
		})
	}
}

// TestInvariantFindingLiteral pins the whole finding a block produces.
func TestInvariantFindingLiteral(t *testing.T) {
	found := inspect(t, "write_file", `{"path":".git/hooks/pre-commit","content":"x"}`)
	want := []agent.Finding{{
		Rule: "protected_path", Verdict: agent.VerdictBlock, Risk: 30, Origin: agent.OriginModel,
		Target: agent.TargetToolCall, StateIndex: -1, Group: -1, Alternative: -1, ToolCallID: "c9",
		Detail: `path ".git/hooks/pre-commit" matches protected pattern`,
	}}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("finding = %+v, want %+v", found, want)
	}
}

func TestInvariantsOtherHooksAndName(t *testing.T) {
	iv := mustInvariants(DefaultInvariants())
	if got, err := iv.InspectInput(context.Background(), agent.InputInspection{System: ".git/hooks"}); err != nil || got != nil {
		t.Fatalf("input = %+v, %v", got, err)
	}
	if got, err := iv.InspectOutput(context.Background(), agent.OutputInspection{Content: ".git/hooks"}); err != nil || got != nil {
		t.Fatalf("output = %+v, %v", got, err)
	}
	if iv.Name() != "invariants" {
		t.Fatalf("name = %q", iv.Name())
	}
}

// TestDefaultTableRows pins the shipped (tool, name, field) rows so a dropped
// or renamed row is a failing test, not a silent policy change.
func TestDefaultTableRows(t *testing.T) {
	var got []string
	for _, inv := range DefaultInvariants() {
		got = append(got, inv.Tool+"/"+inv.Name+"/"+inv.Field)
	}
	want := []string{
		"write_file/protected_path/path",
		"edit_file/protected_path/path",
		"promote_artifact/protected_path/path",
		"read_file/credential_path/path",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	// Fresh storage on every call: a caller's mutation of one table cannot
	// change the next.
	a, b := DefaultInvariants(), DefaultInvariants()
	a[0].Tool = "mutated"
	if b[0].Tool != "write_file" {
		t.Fatalf("DefaultInvariants shares storage across calls")
	}
}

// sprintfJSON substitutes one JSON-encoded string operand for %s.
func sprintfJSON(tmpl, p string) string {
	q, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(tmpl, q)
}
