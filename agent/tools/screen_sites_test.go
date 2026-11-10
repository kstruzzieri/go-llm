package tools

import (
	"os"
	"strings"
	"testing"
)

// testScreen is the screen a hand-built test spec carries: no keys, nothing
// withheld, so output passes through as the command wrote it.
func testScreen() *outputScreen { return newOutputScreen(nil) }

// TestOnlyProbesCaptureWithoutWorkspaceScreen pins, over the package's
// non-test source, the only places exec output is captured without a screen
// built from the workspace: the Seatbelt probe's keyless screen, the unix
// runner's two capture buffers behind the screen, the bwrap probe's buffer,
// and the background manager's rings (screened by Task 8).
func TestOnlyProbesCaptureWithoutWorkspaceScreen(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	type site struct {
		call, def string
		want      map[string]int // file -> count; 0 means at least one
	}
	sites := []site{
		{"probeScreen(", "func probeScreen(", map[string]int{"exec_darwin.go": 1}},
		{"&cappedBuffer{", "", map[string]int{"exec_unix.go": 2, "exec_linux.go": 1}},
		{"newTailRing(", "func newTailRing(", map[string]int{"background_manager.go": 0}},
	}
	got := make([]map[string]int, len(sites))
	for i := range got {
		got[i] = map[string]int{}
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		for i, s := range sites {
			n := strings.Count(text, s.call)
			if s.def != "" {
				n -= strings.Count(text, s.def)
			}
			if n > 0 {
				got[i][name] = n
			}
		}
	}
	for i, s := range sites {
		for file, n := range got[i] {
			want, ok := s.want[file]
			if !ok {
				t.Errorf("site %d: %s has %d unexpected call(s)", i, file, n)
			} else if want != 0 && n != want {
				t.Errorf("site %d: %s has %d call(s), want %d", i, file, n, want)
			}
		}
		for file := range s.want {
			if got[i][file] == 0 {
				t.Errorf("site %d: %s has no call", i, file)
			}
		}
	}
}
