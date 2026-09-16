package mcpclient

import (
	"errors"
	"testing"

	"github.com/kstruzzieri/go-llm/signing"
)

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
		in := map[string]any{"z": 1, "type": "object", "properties": map[string]any{"q": map[string]any{"description": "<<<x\ny>>>", "type": "string"}}}
		got, err := normalizeSchema(in)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := `{"properties":{"q":{"description":"<<<x\ny>>>","type":"string"}},"type":"object","z":1}`
		if string(got) != want {
			t.Fatalf("normalizeSchema(%v) = %s, want %s", in, got, want)
		}
	})
	t.Run("non-object rejected", func(t *testing.T) {
		if _, err := normalizeSchema([]any{1, 2}); err == nil {
			t.Fatal("expected error for array schema")
		}
	})
	t.Run("invalid Go string rejected before JSON replacement", func(t *testing.T) {
		_, err := normalizeSchema(map[string]any{"type": "object", "x": string([]byte{0xff})})
		if !errors.Is(err, signing.ErrInvalidUTF8) {
			t.Fatalf("normalizeSchema(invalid UTF-8) error = %v, want ErrInvalidUTF8", err)
		}
	})
}
