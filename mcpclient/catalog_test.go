package mcpclient

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewToolCatalogCanonicalFixtures(t *testing.T) {
	tests := []struct {
		name    string
		entries []catalogEntry
		want    string
		digest  string
	}{
		{
			name:   "empty",
			want:   `[]`,
			digest: "sha256:4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
		},
		{
			name: "single",
			entries: []catalogEntry{{
				Name: "mcp__fs__read", Description: "Read file", InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
			want:   `[{"description":"Read file","inputSchema":{"type":"object"},"name":"mcp__fs__read"}]`,
			digest: "sha256:d076c0d77d90e89d7022d158c501320cb52ff0acfb18157506f397b195feb99e",
		},
		{
			name: "changed description",
			entries: []catalogEntry{{
				Name: "mcp__fs__read", Description: "Read files", InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
			want:   `[{"description":"Read files","inputSchema":{"type":"object"},"name":"mcp__fs__read"}]`,
			digest: "sha256:5ed6cdea197afcbc274e95c7b9eb7fa76263b49fb300dd5a43409054e2ae9bf3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog, err := newToolCatalog(tt.entries)
			if err != nil {
				t.Fatalf("newToolCatalog(%v): %v", tt.entries, err)
			}
			if got := string(catalog.canonicalBytes()); got != tt.want {
				t.Errorf("newToolCatalog(%v).canonicalBytes() = %s, want %s", tt.entries, got, tt.want)
			}
			if got := catalog.digest(); got != tt.digest {
				t.Errorf("newToolCatalog(%v).digest() = %s, want %s", tt.entries, got, tt.digest)
			}
			if got := catalog.version(); got != 1 {
				t.Errorf("newToolCatalog(%v).version() = %d, want 1", tt.entries, got)
			}
		})
	}
}

func TestNewToolCatalogSortsPrivateCopy(t *testing.T) {
	entries := []catalogEntry{
		{Name: "mcp__fs__write", Description: "Write", InputSchema: json.RawMessage(`{"z":1,"type":"object"}`)},
		{Name: "mcp__fs__read", Description: "Read", InputSchema: json.RawMessage(`{"type":"object","a":1}`)},
	}
	reversed, err := newToolCatalog(entries)
	if err != nil {
		t.Fatal(err)
	}
	forward, err := newToolCatalog([]catalogEntry{entries[1], entries[0]})
	if err != nil {
		t.Fatal(err)
	}
	if reversed.digest() != forward.digest() {
		t.Errorf("newToolCatalog(reverse).digest() = %s, want %s", reversed.digest(), forward.digest())
	}
	const wantDigest = "sha256:7a71cbcb93241353363a9c835a6b791d6a24e37ccca46df31b923d8505b4c5fc"
	if reversed.digest() != wantDigest {
		t.Errorf("newToolCatalog(reverse).digest() = %s, want %s", reversed.digest(), wantDigest)
	}
	if entries[0].Name != "mcp__fs__write" {
		t.Errorf("newToolCatalog mutated caller order: first = %q, want mcp__fs__write", entries[0].Name)
	}
	want := `[{"description":"Read","inputSchema":{"a":1,"type":"object"},"name":"mcp__fs__read"},{"description":"Write","inputSchema":{"type":"object","z":1},"name":"mcp__fs__write"}]`
	if got := string(reversed.canonicalBytes()); got != want {
		t.Errorf("newToolCatalog(reverse).canonicalBytes() = %s, want %s", got, want)
	}
}

func TestNewToolCatalogPinsEveryModelFacingSchemaField(t *testing.T) {
	base := map[string]any{
		"type":        "object",
		"title":       "Base",
		"properties":  map[string]any{"item": map[string]any{"type": "string", "description": "Base description"}},
		"$defs":       map[string]any{"item": map[string]any{"type": "string"}},
		"x-extension": "one",
		"const":       "a",
		"default":     "a",
		"examples":    []any{"a", "b"},
		"x-number":    1.25,
	}
	baseDigest := digestForSchema(t, "Description", base)
	changes := map[string]func(map[string]any){
		"nested description": func(schema map[string]any) {
			schema["properties"].(map[string]any)["item"].(map[string]any)["description"] = "Changed"
		},
		"title": func(schema map[string]any) { schema["title"] = "Changed" },
		"$defs": func(schema map[string]any) {
			schema["$defs"].(map[string]any)["item"].(map[string]any)["type"] = "number"
		},
		"type":              func(schema map[string]any) { schema["type"] = "array" },
		"unknown extension": func(schema map[string]any) { schema["x-extension"] = "two" },
		"const":             func(schema map[string]any) { schema["const"] = "b" },
		"default":           func(schema map[string]any) { schema["default"] = "b" },
		"array order":       func(schema map[string]any) { schema["examples"] = []any{"b", "a"} },
		"numeric value":     func(schema map[string]any) { schema["x-number"] = 1.5 },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			schema := cloneSchemaMap(t, base)
			change(schema)
			if got := digestForSchema(t, "Description", schema); got == baseDigest {
				t.Errorf("digestForSchema(%s) = %s, want digest different from base", name, got)
			}
		})
	}
	if got := digestForSchema(t, "Changed", base); got == baseDigest {
		t.Errorf("digestForSchema(changed description) = %s, want digest different from base", got)
	}

	left := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}, "b": map[string]any{"type": "number"}}}
	right := map[string]any{"properties": map[string]any{"b": map[string]any{"type": "number"}, "a": map[string]any{"type": "string"}}, "type": "object"}
	if got, want := digestForSchema(t, "Description", left), digestForSchema(t, "Description", right); got != want {
		t.Errorf("schema key order digests differ: got %s, want %s", got, want)
	}
}

func cloneSchemaMap(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestToolCatalogOwnsInputAndReturnedBytes(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","x":"original"}`)
	catalog, err := newToolCatalog([]catalogEntry{{Name: "mcp__fs__read", Description: "Read", InputSchema: raw}})
	if err != nil {
		t.Fatal(err)
	}
	want := string(catalog.canonicalBytes())
	copy(raw, strings.Repeat("x", len(raw)))
	entries := catalog.entriesCopy()
	entries[0].Name = "mutated"
	copy(entries[0].InputSchema, strings.Repeat("x", len(entries[0].InputSchema)))
	canonical := catalog.canonicalBytes()
	copy(canonical, strings.Repeat("x", len(canonical)))
	if got := string(catalog.canonicalBytes()); got != want {
		t.Errorf("catalog bytes after caller mutations = %s, want %s", got, want)
	}
	if got := catalog.entriesCopy()[0].Name; got != "mcp__fs__read" {
		t.Errorf("catalog entry after returned mutation = %q, want mcp__fs__read", got)
	}
	if got := string(catalog.entriesCopy()[0].InputSchema); got != `{"type":"object","x":"original"}` {
		t.Errorf("catalog schema after returned mutation = %s, want original schema", got)
	}
}

func digestForSchema(t *testing.T, description string, schema any) string {
	t.Helper()
	raw, err := normalizeSchema(schema)
	if err != nil {
		t.Fatalf("normalizeSchema(%v): %v", schema, err)
	}
	catalog, err := newToolCatalog([]catalogEntry{{Name: "mcp__fs__read", Description: description, InputSchema: raw}})
	if err != nil {
		t.Fatalf("newToolCatalog(%v): %v", schema, err)
	}
	return catalog.digest()
}
