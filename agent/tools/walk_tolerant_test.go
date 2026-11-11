//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// tolerantFixture writes files under a fresh workspace root and sibFiles under
// a sibling directory beside it. It returns the workspace, its canonical root,
// and the sibling's path, which swapRoot renames into the root's place.
func tolerantFixture(t *testing.T, files, sibFiles map[string]string) (*Workspace, string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, files)
	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	sib := ws.root + "-sib"
	if err := os.Mkdir(sib, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTree(t, sib, sibFiles)
	return ws, ws.root, sib
}

// swapRoot renames root away and renames sib into its place. Rename, never
// remove and recreate: a recreated directory can get the old inode number
// and pass the identity check by accident.
func swapRoot(t *testing.T, root, sib string) {
	t.Helper()
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sib, root); err != nil {
		t.Fatal(err)
	}
}

// answer returns a seam that answers err for want and nil otherwise.
func answer(want string, err error) func(string) error {
	return func(rel string) error {
		if rel == want {
			return err
		}
		return nil
	}
}

var tolerantTree = map[string]string{
	".git/config": "x\n",
	"a/bad":       "x\n",
	"a/gone":      "x\n",
	"a/ok":        "x\n",
	"data/f":      "x\n",
	"sub/g":       "x\n",
	"vendor/v":    "x\n",
	"z/x":         "x\n",
}

type wantSkip struct {
	rel string
	err error
}

func TestWalkTolerant(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	errBoom := errors.New("boom")
	all := []string{"a", "a/bad", "a/gone", "a/ok", "data", "data/f", "sub", "sub/g", "z", "z/x"}
	rows := []struct {
		label string
		// setup installs seams on ws and returns the ignored callback, if any.
		setup       func(t *testing.T, ws *Workspace, root, sib string) func(string) error
		wantVisits  []string
		wantIgnored []string
		wantSkips   []wantSkip
		wantErr     error  // nil: the walk returns nil
		fnErr       string // fn returns errBoom for this path
	}{
		{
			label:       "clean",
			setup:       func(*testing.T, *Workspace, string, string) func(string) error { return nil },
			wantVisits:  all,
			wantIgnored: []string{".git", "vendor"},
		},
		{
			label: "stat failures",
			setup: func(_ *testing.T, ws *Workspace, _, _ string) func(string) error {
				ws.screenSeams = &screenSeams{statErr: func(rel string) error {
					switch rel {
					case "a/gone":
						return fs.ErrNotExist
					case "a/bad":
						return syscall.EIO
					}
					return nil
				}}
				return nil
			},
			wantVisits:  []string{"a", "a/ok", "data", "data/f", "sub", "sub/g", "z", "z/x"},
			wantIgnored: []string{".git", "vendor"},
			wantSkips:   []wantSkip{{"a/bad", syscall.EIO}, {"a/gone", fs.ErrNotExist}},
		},
		{
			label: "subdir open denied",
			setup: func(_ *testing.T, ws *Workspace, _, _ string) func(string) error {
				ws.screenSeams = &screenSeams{openErr: answer("data", fs.ErrPermission)}
				return nil
			},
			wantVisits:  []string{"a", "a/bad", "a/gone", "a/ok", "data", "sub", "sub/g", "z", "z/x"},
			wantIgnored: []string{".git", "vendor"},
			wantSkips:   []wantSkip{{"data", fs.ErrPermission}},
		},
		{
			label: "subdir listing fails",
			setup: func(_ *testing.T, ws *Workspace, _, _ string) func(string) error {
				ws.screenSeams = &screenSeams{listErr: answer("sub", syscall.EIO)}
				return nil
			},
			wantVisits:  []string{"a", "a/bad", "a/gone", "a/ok", "data", "data/f", "sub", "z", "z/x"},
			wantIgnored: []string{".git", "vendor"},
			wantSkips:   []wantSkip{{"sub", syscall.EIO}},
		},
		{
			label: "root listing fails",
			setup: func(_ *testing.T, ws *Workspace, _, _ string) func(string) error {
				ws.screenSeams = &screenSeams{listErr: answer("", syscall.EIO)}
				return nil
			},
			wantErr: errAnchorUnreachable,
		},
		{
			label: "root replaced before last subdir open",
			setup: func(t *testing.T, ws *Workspace, root, sib string) func(string) error {
				ws.screenSeams = &screenSeams{openErr: func(rel string) error {
					if rel != "z" {
						return nil
					}
					swapRoot(t, root, sib)
					return fs.ErrNotExist
				}}
				return nil
			},
			wantVisits:  all[:len(all)-1],
			wantIgnored: []string{".git", "vendor"},
			wantErr:     ErrRootReplaced,
		},
		{
			// A directory no longer reachable once listed is skipped as
			// changed, and nothing from its listing is used, skips included.
			label: "subdir moved during its listing",
			setup: func(t *testing.T, ws *Workspace, root, _ string) func(string) error {
				ws.screenSeams = &screenSeams{statErr: answer("sub/g", syscall.EIO)}
				ws.beforeReadDir = func(rel string) {
					if rel == "sub" {
						if err := os.Rename(filepath.Join(root, "sub"), root+"-moved-sub"); err != nil {
							t.Fatal(err)
						}
					}
				}
				return nil
			},
			wantVisits:  []string{"a", "a/bad", "a/gone", "a/ok", "data", "data/f", "sub", "z", "z/x"},
			wantIgnored: []string{".git", "vendor"},
			wantSkips:   []wantSkip{{"sub", errFileChanged}},
		},
		{
			// An anchor-level failure at the end of a listing ends the walk
			// even though an entry of that listing was already skipped.
			label: "root replaced during its listing",
			setup: func(t *testing.T, ws *Workspace, root, sib string) func(string) error {
				ws.screenSeams = &screenSeams{statErr: answer("a", syscall.EIO)}
				ws.beforeReadDir = func(rel string) {
					if rel == "" {
						swapRoot(t, root, sib)
					}
				}
				return nil
			},
			wantErr: ErrRootReplaced,
		},
		{
			label: "root gone before walk",
			setup: func(t *testing.T, _ *Workspace, root, _ string) func(string) error {
				if err := os.Rename(root, root+"-old"); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			wantErr: errAnchorUnreachable,
		},
		{
			label: "root replaced before walk",
			setup: func(t *testing.T, _ *Workspace, root, sib string) func(string) error {
				swapRoot(t, root, sib)
				return nil
			},
			wantErr: ErrRootReplaced,
		},
		{
			label: "subdir replaced before open",
			setup: func(t *testing.T, ws *Workspace, root, _ string) func(string) error {
				other := root + "-other-data"
				writeTree(t, other, map[string]string{"f": "x\n"})
				ws.screenSeams = &screenSeams{openErr: func(rel string) error {
					if rel == "data" {
						if err := os.Rename(filepath.Join(root, "data"), root+"-old-data"); err != nil {
							t.Fatal(err)
						}
						if err := os.Rename(other, filepath.Join(root, "data")); err != nil {
							t.Fatal(err)
						}
					}
					return nil
				}}
				return nil
			},
			wantVisits:  []string{"a", "a/bad", "a/gone", "a/ok", "data", "sub", "sub/g", "z", "z/x"},
			wantIgnored: []string{".git", "vendor"},
			wantSkips:   []wantSkip{{"data", errFileChanged}},
		},
		{
			// An error fn returns inside a subdirectory ends the walk; it is
			// not that directory's failure.
			label:       "fn fails below a subdir",
			setup:       func(*testing.T, *Workspace, string, string) func(string) error { return nil },
			fnErr:       "data/f",
			wantVisits:  []string{"a", "a/bad", "a/gone", "a/ok", "data", "data/f"},
			wantIgnored: []string{".git"},
			wantErr:     errBoom,
		},
		{
			label: "ignored fails",
			setup: func(*testing.T, *Workspace, string, string) func(string) error {
				return func(rel string) error {
					if rel == ".git" {
						return errBoom
					}
					return nil
				}
			},
			wantIgnored: []string{".git"},
			wantErr:     errBoom,
		},
		{
			// A fatal error from ignored is returned before the recheck,
			// whose own (non-fatal) error would otherwise turn it into a skip.
			label: "ignored fails fatally below a moved subdir",
			setup: func(t *testing.T, _ *Workspace, root, _ string) func(string) error {
				if err := os.Mkdir(filepath.Join(root, "sub", ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
				return func(rel string) error {
					if rel != "sub/.git" {
						return nil
					}
					if err := os.Rename(filepath.Join(root, "sub"), root+"-moved-sub"); err != nil {
						t.Fatal(err)
					}
					return ErrRootReplaced
				}
			},
			wantVisits:  []string{"a", "a/bad", "a/gone", "a/ok", "data", "data/f", "sub"},
			wantIgnored: []string{".git", "sub/.git"},
			wantErr:     ErrRootReplaced,
		},
		{
			label: "root replaced during ignored",
			setup: func(t *testing.T, _ *Workspace, root, sib string) func(string) error {
				return func(rel string) error {
					if rel == ".git" {
						swapRoot(t, root, sib)
					}
					return nil
				}
			},
			wantIgnored: []string{".git"},
			wantErr:     ErrRootReplaced,
		},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			ws, root, sib := tolerantFixture(t, tolerantTree, map[string]string{"other": "x\n"})
			onIgnored := r.setup(t, ws, root, sib)
			var visits, ignored []string
			var skips []walkSkip
			err := ws.walkTolerant(t.Context(), func(rel string, _ fs.DirEntry) error {
				visits = append(visits, rel)
				if rel == r.fnErr {
					return errBoom
				}
				return nil
			}, func(rel string, _ fs.DirEntry) error {
				ignored = append(ignored, rel)
				if onIgnored != nil {
					return onIgnored(rel)
				}
				return nil
			}, func(s walkSkip) { skips = append(skips, s) })
			if r.wantErr == nil && err != nil || r.wantErr != nil && !errors.Is(err, r.wantErr) {
				t.Fatalf("%s: walk error mismatch (got nil: %t)", r.label, err == nil)
			}
			if !slices.Equal(visits, r.wantVisits) {
				t.Fatalf("%s: visits differ (%d, want %d)", r.label, len(visits), len(r.wantVisits))
			}
			if !slices.Equal(ignored, r.wantIgnored) {
				t.Fatalf("%s: ignored calls differ (%d, want %d)", r.label, len(ignored), len(r.wantIgnored))
			}
			if len(skips) != len(r.wantSkips) {
				t.Fatalf("%s: %d skips, want %d", r.label, len(skips), len(r.wantSkips))
			}
			for i, s := range skips {
				if s.rel != r.wantSkips[i].rel || !errors.Is(s.err, r.wantSkips[i].err) {
					t.Fatalf("%s: skip %d differs (path matches: %t, error matches: %t)", r.label, i, s.rel == r.wantSkips[i].rel, errors.Is(s.err, r.wantSkips[i].err))
				}
			}
		})
	}

	// A context error ends the walk.
	ws, _, _ := tolerantFixture(t, tolerantTree, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	nop := func(string, fs.DirEntry) error { return nil }
	if err := ws.walkTolerant(ctx, func(string, fs.DirEntry) error { return nil }, nop, func(walkSkip) {}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: error is not context.Canceled (nil: %t)", err == nil)
	}

	// The clean tree's literal visits are walk's visits too.
	ws, _, _ = tolerantFixture(t, tolerantTree, nil)
	var visits []string
	if err := ws.walk(t.Context(), func(rel string, _ fs.DirEntry) error {
		visits = append(visits, rel)
		return nil
	}); err != nil || !slices.Equal(visits, all) {
		t.Fatalf("walk: visits differ (%d, want %d; error: %t)", len(visits), len(all), err != nil)
	}
}

// The strict walk, and search built on it, still end on a subdirectory that
// changed after its parent was listed.
func TestWalkUnchangedByTolerantMode(t *testing.T) {
	setup := func(t *testing.T) *Workspace {
		ws, root, _ := tolerantFixture(t, map[string]string{"a/f": "needle\n", "b/f": "needle\n"}, nil)
		ws.beforeReadDir = func(rel string) {
			if rel == "a" {
				if err := os.RemoveAll(filepath.Join(root, "b")); err != nil {
					t.Fatal(err)
				}
			}
		}
		return ws
	}
	ws := setup(t)
	err := ws.walk(t.Context(), func(string, fs.DirEntry) error { return nil })
	if !errors.Is(err, errFileChanged) {
		t.Fatalf("walk: error is not errFileChanged (nil: %t)", err == nil)
	}
	res, err := NewSearch(setup(t)).Invoke(t.Context(), json.RawMessage(`{"pattern":"needle"}`))
	if err != nil || !res.IsError || res.Content != "path changed during access" {
		t.Fatalf("search: want the changed-path tool error (is error: %t, invoke error: %t)", res.IsError, err != nil)
	}
}
