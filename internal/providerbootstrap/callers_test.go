package providerbootstrap

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// I18/M19: every production caller of providerbootstrap.New makes an
// INTENTIONAL destination-policy choice. This test enumerates the call sites
// from source, so a seventh caller cannot appear and quietly inherit
// nil-means-ungated behavior: it fails here until it either wires
// DestinationGate or is added to the reviewed exemption list below with a
// reason.
func TestEveryBootstrapCallerChoosesAnAdmissionPosture(t *testing.T) {
	root := moduleRoot(t)

	// Files allowed to call New WITHOUT wiring DestinationGate in the same
	// file. Keep this list empty: the boundary has no exempt callers today,
	// and adding one is a reviewed decision, not a default.
	exempt := map[string]string{}

	var callers []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			// Dot-directories include .worktrees/.claude checkouts of OTHER
			// branches; scanning them would report the past, not this tree.
			if strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			if name == "testdata" || name == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if strings.HasPrefix(rel, filepath.Join("internal", "providerbootstrap")) {
			return nil // the package itself
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), "providerbootstrap.New(") {
			callers = append(callers, rel)
			if _, ok := exempt[rel]; ok {
				return nil
			}
			if !strings.Contains(string(data), "DestinationGate") {
				t.Errorf("%s calls providerbootstrap.New but never references DestinationGate: a new bootstrap caller must make an explicit admission choice (#477 I18)", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The six known production callers, pinned so a silent DELETION of one
	// (moving bootstrap somewhere unscanned) is as loud as an addition.
	want := []string{
		filepath.Join("cmd", "golem", "index.go"),
		filepath.Join("cmd", "golem", "main.go"),
		filepath.Join("cmd", "golem", "models.go"),
		filepath.Join("cmd", "golem", "source_cmd.go"),
		filepath.Join("golem", "bootstrap.go"),
		filepath.Join("mcp", "server.go"),
	}
	for _, w := range want {
		if !slices.Contains(callers, w) {
			t.Errorf("expected bootstrap caller %s not found; if bootstrap moved, update this census deliberately", w)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
