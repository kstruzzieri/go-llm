package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCheckStopsOnCancel: a canceled context ends the profile run with the
// cancellation instead of failing every remaining package run against the
// dead context. The inventory is a two-package fixture and the context is
// canceled before the loop, so no package run starts and none is recorded.
func TestCheckStopsOnCancel(t *testing.T) {
	t.Setenv("GO_LLM_CI_TREE", "")
	t.Setenv("GO_LLM_CI_COMMIT", "")
	inv := filepath.Join(t.TempDir(), "inventory.json")
	const two = `{"schema":"containment-inventory/v1","profiles":[{"name":"p","timeout":"1s"}],"rows":[
{"id":"A","refs":[{"package":"./a","test":"TestA","file":"a/a_test.go","func":"TestA"}],"profiles":["p"]},
{"id":"B","refs":[{"package":"./b","test":"TestB","file":"b/b_test.go","func":"TestB"}],"profiles":["p"]}]}`
	if err := os.WriteFile(inv, []byte(two), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ev, err := check(ctx, inv, "p")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(ev.Failures) != 0 {
		t.Fatalf("failures = %v, want no package run against a canceled context", ev.Failures)
	}
}
