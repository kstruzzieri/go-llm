package mcpclient

import "testing"

func TestNormalizeSchema(t *testing.T) {
	t.Run("nil -> empty object", func(t *testing.T) {
		got, err := normalizeSchema(nil)
		if err != nil || string(got) != `{"type":"object"}` {
			t.Fatalf("got (%s,%v)", got, err)
		}
	})
	t.Run("typed-null -> empty object", func(t *testing.T) {
		var p *int // marshals to "null"
		got, err := normalizeSchema(p)
		if err != nil || string(got) != `{"type":"object"}` {
			t.Fatalf("got (%s,%v)", got, err)
		}
	})
	t.Run("object passes through", func(t *testing.T) {
		in := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
		got, err := normalizeSchema(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) == 0 || got[0] != '{' {
			t.Fatalf("expected JSON object, got %s", got)
		}
	})
	t.Run("non-object rejected", func(t *testing.T) {
		if _, err := normalizeSchema([]any{1, 2}); err == nil {
			t.Fatal("expected error for array schema")
		}
	})
}
