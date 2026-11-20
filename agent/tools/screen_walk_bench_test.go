//go:build linux || darwin

package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// genTree writes about `entries` entries under root: empty files, plus .env
// files holding one generated surrogate TOKEN= line each.
//
//	wide:     50 files per directory, directories three levels deep
//	dirheavy: 12 files per leaf directory, 24 "worktrees" under
//	          .claude/worktrees, about one entry in ten a directory,
//	          directories up to ten levels below the root
//
// It returns the files and the leaf directories it made. The shapes are the
// ones the #789 design measured; keep them unchanged.
func genTree(root, shape string, entries int) (files, dirs int, err error) {
	mk := func(dir string, n int) error {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		dirs++
		for i := 0; i < n; i++ {
			f, err := os.Create(filepath.Join(dir, fmt.Sprintf("f%03d.go", i)))
			if err != nil {
				return err
			}
			_ = f.Close()
			files++
		}
		return nil
	}
	switch shape {
	case "wide":
		nd := entries / 51
		for d := 0; d < nd; d++ {
			dir := filepath.Join(root, fmt.Sprintf("a%03d", d/1000), fmt.Sprintf("b%02d", (d/30)%34), fmt.Sprintf("c%02d", d%30))
			if err := mk(dir, 50); err != nil {
				return 0, 0, err
			}
		}
	case "dirheavy":
		per := entries / 24
		for wt := 0; wt < 24; wt++ {
			base := filepath.Join(root, ".claude", "worktrees", fmt.Sprintf("wt-%02d", wt))
			if err := os.MkdirAll(base, 0o755); err != nil {
				return 0, 0, err
			}
			if err := os.WriteFile(filepath.Join(base, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
				return 0, 0, err
			}
			if err := os.WriteFile(filepath.Join(base, ".env"), []byte(fmt.Sprintf("TOKEN=surrogate-%04d-abcdef\n", wt)), 0o600); err != nil {
				return 0, 0, err
			}
			nd := per * 20 / 267 // 12 files per leaf directory plus intermediate directories: about one entry in ten is a directory
			for d := 0; d < nd; d++ {
				dir := filepath.Join(base, "src", fmt.Sprintf("m%02d", d/400), "pkg", fmt.Sprintf("p%02d", (d/20)%20), "internal", fmt.Sprintf("q%02d", d%20), "impl")
				if d%4 != 0 {
					dir = filepath.Join(base, "src", fmt.Sprintf("m%02d", d/400), "pkg", fmt.Sprintf("p%02d", (d/20)%20), fmt.Sprintf("r%02d", d%20))
				}
				if err := mk(dir, 12); err != nil {
					return 0, 0, err
				}
			}
		}
	default:
		return 0, 0, fmt.Errorf("unknown shape %q", shape)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=surrogate-root-abcdef\n"), 0o600); err != nil {
		return 0, 0, err
	}
	return files, dirs, nil
}

// screenShape is one benchmark tree: genTree's arguments, the entries a walk
// visits (the root excluded) and the keys a complete screen holds. The two
// counts were measured by the #789 design (Table 2), not derived here.
type screenShape struct {
	name    string
	shape   string
	entries int
	visits  int
	keys    int
}

var screenShapes = []screenShape{
	{"wide-100k", "wide", 100000, 100030, 2},
	{"wide-400k", "wide", 400000, 400269, 2},
	{"wide-1m", "wide", 1000000, 1000645, 2},
	{"dirheavy-355k", "dirheavy", 355460, 355611, 50},
	{"dirheavy-1m", "dirheavy", 1000000, 1000515, 50},
}

// screenTree returns the root of sh's tree. Under GO_LLM_BENCH_PARENT the tree
// is kept at a fixed name and reused by later processes once its marker, a
// sibling written last, exists; a tree without one is rebuilt from scratch.
// Otherwise the tree goes under tb.TempDir(); only TestBuildOutputScreenLargeTree
// reaches that path, since the benchmarks skip without GO_LLM_BENCH_PARENT
// rather than build every tree afresh, about 5.7 million files in all.
func screenTree(tb testing.TB, sh screenShape) string {
	tb.Helper()
	parent := os.Getenv("GO_LLM_BENCH_PARENT")
	if parent == "" {
		root := tb.TempDir()
		if _, _, err := genTree(root, sh.shape, sh.entries); err != nil {
			failNoPath(tb, sh.name+": generate", err)
		}
		return root
	}
	root := filepath.Join(parent, "screen-789-"+sh.name)
	marker := root + ".done" // beside the tree, so the walk never sees it
	if _, err := os.Stat(marker); err == nil {
		return root
	}
	if err := os.RemoveAll(root); err != nil {
		failNoPath(tb, sh.name+": clear", err)
	}
	if _, _, err := genTree(root, sh.shape, sh.entries); err != nil {
		failNoPath(tb, sh.name+": generate", err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		failNoPath(tb, sh.name+": marker", err)
	}
	return root
}

// BenchmarkScreenWalk times the screen's walk with callbacks that only count.
// Every iteration must visit the shape's measured number of entries and
// report no skip.
func BenchmarkScreenWalk(b *testing.B) {
	if os.Getenv("GO_LLM_BENCH_PARENT") == "" {
		b.Skip("set GO_LLM_BENCH_PARENT to a directory for the generated trees: these benchmarks build up to 5.7 million files")
	}
	for _, sh := range screenShapes {
		b.Run(sh.name, func(b *testing.B) {
			ws, err := NewWorkspace(screenTree(b, sh))
			if err != nil {
				failNoPath(b, sh.name+": workspace", err)
			}
			var visits, skips int
			fn := func(string, fs.DirEntry) error { visits++; return nil }
			ignored := func(string, fs.DirEntry) error { return nil }
			skip := func(walkSkip) { skips++ }
			for b.Loop() {
				visits, skips = 0, 0
				if err := benchWalk(context.Background(), ws, fn, ignored, skip); err != nil {
					failNoPath(b, sh.name+": walk", err)
				}
				if visits != sh.visits || skips != 0 {
					b.Fatalf("%s: visits %d, skips %d; want %d, 0", sh.name, visits, skips, sh.visits)
				}
			}
		})
	}
}

// checkScreen fails unless s is a complete screen with want keys.
func checkScreen(tb testing.TB, label string, s *outputScreen, err error, want int) {
	tb.Helper()
	if err != nil {
		failNoPath(tb, label+": build", err)
	}
	if s.withhold != "" || s.changed != 0 || len(s.keys) != want {
		tb.Fatalf("%s: withheld %t, changed %d, keys %d; want a complete screen of %d keys", label, s.withhold != "", s.changed, len(s.keys), want)
	}
}

// BenchmarkBuildOutputScreen times the whole screen build with a budget no
// variant reaches, so every iteration measures a complete screen; one that
// is withheld, changed or short of keys fails the benchmark.
func BenchmarkBuildOutputScreen(b *testing.B) {
	if os.Getenv("GO_LLM_BENCH_PARENT") == "" {
		b.Skip("set GO_LLM_BENCH_PARENT to a directory for the generated trees: these benchmarks build up to 5.7 million files")
	}
	for _, sh := range screenShapes {
		b.Run(sh.name, func(b *testing.B) {
			ws, err := NewWorkspace(screenTree(b, sh))
			if err != nil {
				failNoPath(b, sh.name+": workspace", err)
			}
			ws.screenLimits.budget = 10 * time.Minute
			for b.Loop() {
				s, err := buildOutputScreen(context.Background(), ws)
				checkScreen(b, sh.name, s, err, sh.keys)
			}
		})
	}
}

// TestBuildOutputScreenLargeTree checks Goal 1 of #789 under the default
// budget: the directory-heavy 355k tree builds a complete screen five times,
// with a median under 2.5 s. One untimed warm-up build comes first: the
// design's Mac figures are medians after one warm-up, and a run started
// straight after a million-entry benchmark meets an emptied kernel file
// cache. It builds a large tree, so it runs only when GO_LLM_SCREEN_LARGE=1.
func TestBuildOutputScreenLargeTree(t *testing.T) {
	if os.Getenv("GO_LLM_SCREEN_LARGE") != "1" {
		t.Skip("set GO_LLM_SCREEN_LARGE=1 to run")
	}
	sh := screenShapes[3]
	if sh.name != "dirheavy-355k" {
		t.Fatalf("shape table changed: got %s", sh.name)
	}
	ws, err := NewWorkspace(screenTree(t, sh))
	if err != nil {
		failNoPath(t, sh.name+": workspace", err)
	}
	if _, err := buildOutputScreen(context.Background(), ws); err != nil {
		failNoPath(t, sh.name+" warm-up: build", err)
	}
	var ds []time.Duration
	for i := 0; i < 5; i++ {
		t0 := time.Now()
		s, err := buildOutputScreen(context.Background(), ws)
		d := time.Since(t0)
		checkScreen(t, fmt.Sprintf("%s run %d", sh.name, i+1), s, err, sh.keys)
		t.Logf("run %d: %s", i+1, d)
		ds = append(ds, d)
	}
	slices.Sort(ds)
	t.Logf("median %s", ds[2])
	if ds[2] >= 2500*time.Millisecond {
		t.Fatalf("%s: median %s, want under 2.5s", sh.name, ds[2])
	}
}
