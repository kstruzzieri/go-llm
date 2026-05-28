package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestStaticToolSchemaSourceReturnsConfiguredTools(t *testing.T) {
	want := []json.RawMessage{
		json.RawMessage(`{"name":"search_code","description":"search","inputSchema":{"type":"object"}}`),
		json.RawMessage(`{"name":"read_file","description":"read","inputSchema":{"type":"object","required":["path"]}}`),
	}
	src := staticToolSchemaSource{tools: want}
	got, err := src.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot = %v, want %v", got, want)
	}
}

func TestStaticToolSchemaSourceNilTreatedAsEmpty(t *testing.T) {
	src := staticToolSchemaSource{}
	got, err := src.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Snapshot len = %d, want 0", len(got))
	}
}
