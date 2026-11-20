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

	"golang.org/x/sys/unix"

	"github.com/kstruzzieri/go-llm/agent/interceptor"
)

// tolerantFixture writes files under a fresh workspace root and sibFiles under
// a sibling directory beside it. It returns the workspace, its canonical root,
// and the sibling's path, which swapRoot renames into the root's place.
// Its tests assume the temporary directory's filesystem reports entry types in
// directory listings, and fail rather than skip on one that does not.
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
		// examine is the walk's matcher; nil examines nothing.
		examine func(string) bool
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
			examine:     func(rel string) bool { return rel == "a/gone" || rel == "a/bad" },
			wantVisits:  []string{"a", "a/ok", "data", "data/f", "sub", "sub/g", "z", "z/x"},
			wantIgnored: []string{".git", "vendor"},
			wantSkips:   []wantSkip{{"a/bad", syscall.EIO}, {"a/gone", fs.ErrNotExist}},
		},
		{
			// A plain file the matcher does not accept is never stat'ed.
			label: "plain stat never asked",
			setup: func(_ *testing.T, ws *Workspace, _, _ string) func(string) error {
				ws.screenSeams = &screenSeams{statErr: answer("a/ok", syscall.EIO)}
				return nil
			},
			wantVisits:  all,
			wantIgnored: []string{".git", "vendor"},
		},
		{
			// A directory is always stat'ed, whatever the matcher says.
			label: "dir stat fails",
			setup: func(_ *testing.T, ws *Workspace, _, _ string) func(string) error {
				ws.screenSeams = &screenSeams{statErr: answer("data", syscall.EIO)}
				return nil
			},
			wantVisits:  []string{"a", "a/bad", "a/gone", "a/ok", "sub", "sub/g", "z", "z/x"},
			wantIgnored: []string{".git", "vendor"},
			wantSkips:   []wantSkip{{"data", syscall.EIO}},
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
			examine:     func(rel string) bool { return rel == "sub/g" },
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
			examine := r.examine
			if examine == nil {
				examine = func(string) bool { return false }
			}
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
			}, func(s walkSkip) { skips = append(skips, s) }, examine)
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
	if err := ws.walkTolerant(ctx, func(string, fs.DirEntry) error { return nil }, nop, func(walkSkip) {}, func(string) bool { return false }); !errors.Is(err, context.Canceled) {
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

// walkRecord walks ws tolerantly with examine, ignoring nothing, and returns
// the paths fn visited and the skips.
func walkRecord(t *testing.T, label string, ws *Workspace, examine func(string) bool) ([]string, []walkSkip) {
	t.Helper()
	var visits []string
	var skips []walkSkip
	err := ws.walkTolerant(t.Context(), func(rel string, _ fs.DirEntry) error {
		visits = append(visits, rel)
		return nil
	}, func(string, fs.DirEntry) error { return nil }, func(s walkSkip) { skips = append(skips, s) }, examine)
	if err != nil {
		failNoPath(t, label, err)
	}
	return visits, skips
}

// recordStats returns a statErr seam that appends each path it is asked about
// to got and answers fail(rel); a nil fail answers nil.
func recordStats(got *[]string, fail func(string) error) func(string) error {
	return func(rel string) error {
		*got = append(*got, rel)
		if fail != nil {
			return fail(rel)
		}
		return nil
	}
}

// The listing stats directories, symbolic links, other non-regular entries
// and every path the matcher accepts; a plain regular file is never stat'ed.
func TestWalkTolerantExamines(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	const label = "examines"
	ws, root, _ := tolerantFixture(t, map[string]string{
		"plain.txt":      "x\n",
		"a/b/c/deep.txt": "x\n",
		"a/.env":         "TOKEN=examine-0001-abcdef\n",
		"a/b/.npmrc":     "_authToken=examine-0002-abcdef\n",
		".ssh/id":        "KEY=examine-0003-abcdef\n",
		"Sub/.Env.Local": "TOKEN=examine-0004-abcdef\n",
		"node_modules/x": "x\n",
		".git/config":    "[core]\n",
	}, nil)
	if err := os.Symlink("plain.txt", filepath.Join(root, "link")); err != nil {
		failNoPath(t, label, err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		failNoPath(t, label, err)
	}
	var stats []string
	ws.screenSeams = &screenSeams{statErr: recordStats(&stats, nil)}
	visits, skips := walkRecord(t, label, ws, interceptor.IsCredentialPath)
	slices.Sort(stats)
	want := []string{".git", ".ssh", ".ssh/id", "Sub", "Sub/.Env.Local", "a", "a/.env", "a/b", "a/b/.npmrc", "a/b/c", "link", "node_modules", "pipe"}
	if !slices.Equal(stats, want) {
		t.Fatalf("%s: stat'ed paths differ (%d, want %d)", label, len(stats), len(want))
	}
	if !slices.Contains(visits, "plain.txt") || !slices.Contains(visits, "a/b/c/deep.txt") {
		t.Fatalf("%s: a plain file was not visited", label)
	}
	if len(skips) != 0 {
		t.Fatalf("%s: %d skips, want 0", label, len(skips))
	}
}

// An entry whose type the directory read did not report is stat'ed with the
// statErr seam and then fstatat, and a failed stat is a skip. Every unknown
// path is plain-named, so the matcher never forces the stat.
func TestWalkTolerantUnknownType(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	rows := []struct {
		label     string
		unknown   string // the path the read reports as DT_UNKNOWN
		statErr   error  // the stat seam's answer for unknown
		wantStat  bool   // unknown is among the stat'ed paths
		wantVisit string // "": no visit is checked
		wantSkip  error  // nil: no skip
	}{
		{label: "plain file", unknown: "plain.txt", wantStat: true, wantVisit: "plain.txt"},
		// The name is plain, so only its unknown type forces the stat.
		{label: "directory", unknown: "plain-dir", wantStat: true, wantVisit: "plain-dir/.env"},
		{label: "unknown gone", unknown: "a/plain.txt", statErr: fs.ErrNotExist, wantStat: true, wantSkip: fs.ErrNotExist},
		{label: "unknown eio", unknown: "a/plain.txt", statErr: syscall.EIO, wantStat: true, wantSkip: syscall.EIO},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			ws, _, _ := tolerantFixture(t, map[string]string{
				"plain.txt":      "x\n",
				"plain-dir/.env": "TOKEN=unknown-0001-abcdef\n",
				"a/.env":         "TOKEN=unknown-0002-abcdef\n",
				"a/plain.txt":    "x\n",
			}, nil)
			var stats []string
			ws.screenSeams = &screenSeams{
				statErr: recordStats(&stats, answer(r.unknown, r.statErr)),
				entryType: func(rel string, typ uint8) uint8 {
					if rel == r.unknown {
						return unix.DT_UNKNOWN
					}
					return typ
				},
			}
			visits, skips := walkRecord(t, r.label, ws, interceptor.IsCredentialPath)
			if r.wantStat && !slices.Contains(stats, r.unknown) {
				t.Fatalf("%s: the untyped entry was not stat'ed", r.label)
			}
			if r.wantVisit != "" && !slices.Contains(visits, r.wantVisit) {
				t.Fatalf("%s: the expected visit is missing", r.label)
			}
			if r.wantSkip == nil {
				if len(skips) != 0 {
					t.Fatalf("%s: %d skips, want 0", r.label, len(skips))
				}
				return
			}
			if len(skips) != 1 || skips[0].rel != r.unknown || !errors.Is(skips[0].err, r.wantSkip) {
				t.Fatalf("%s: want one skip for the untyped entry with the seam's error (%d skips)", r.label, len(skips))
			}
			if slices.Contains(visits, r.unknown) {
				t.Fatalf("%s: the skipped entry was visited", r.label)
			}
		})
	}
}

// On a filesystem that reports no types, every entry is stat'ed and the walk
// visits every entry, as the walk did before the listing took types from the
// directory read.
func TestWalkTolerantAllUnknown(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	const label = "all unknown"
	ws, _, _ := tolerantFixture(t, tolerantTree, nil)
	var stats []string
	ws.screenSeams = &screenSeams{
		statErr:   recordStats(&stats, nil),
		entryType: func(string, uint8) uint8 { return unix.DT_UNKNOWN },
	}
	visits, _ := walkRecord(t, label, ws, func(string) bool { return false })
	slices.Sort(stats)
	wantStats := []string{".git", "a", "a/bad", "a/gone", "a/ok", "data", "data/f", "sub", "sub/g", "vendor", "z", "z/x"}
	if !slices.Equal(stats, wantStats) {
		t.Fatalf("%s: stat'ed paths differ (%d, want %d)", label, len(stats), len(wantStats))
	}
	wantVisits := []string{"a", "a/bad", "a/gone", "a/ok", "data", "data/f", "sub", "sub/g", "z", "z/x"}
	if !slices.Equal(visits, wantVisits) {
		t.Fatalf("%s: visits differ (%d, want %d)", label, len(visits), len(wantVisits))
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

// openHeld opens dir and closes it when the test ends.
func openHeld(t *testing.T, label, dir string) *os.File {
	t.Helper()
	f, err := os.Open(dir)
	if err != nil {
		failNoPath(t, label, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// examinedEntry builds an entry for name the way the listing does: a real
// Fstatat that does not follow symbolic links.
func examinedEntry(t *testing.T, label string, dir *os.File, name string) workspaceEntry {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		failNoPath(t, label, err)
	}
	return workspaceEntry{name: name, stat: st, dir: dir}
}

func TestWorkspaceEntryUnexamined(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	const label = "unexamined entry"
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".env":      "TOKEN=surrogate-0001-abcdef\n",
		"plain.txt": "PLAIN=surrogate-0002-ghijkl\n",
	})
	dir := openHeld(t, label, root)

	e := workspaceEntry{name: ".env", dir: dir, unexamined: true}
	f, err := e.openRegular()
	if !errors.Is(err, errWalkEntryUnexamined) || f != nil {
		t.Fatalf("%s: openRegular = (%v, %v), want (nil, sentinel)", label, f, err)
	}
	info, err := e.Info()
	if !errors.Is(err, errWalkEntryUnexamined) || info != nil {
		t.Fatalf("%s: Info = (%v, %v), want (nil, sentinel)", label, info, err)
	}
	if e.IsDir() {
		t.Fatalf("%s: IsDir is true", label)
	}
	if e.Type() != 0 {
		t.Fatalf("%s: Type = %v, want 0", label, e.Type())
	}

	ok, err := examinedEntry(t, label, dir, "plain.txt").openRegular()
	if err != nil {
		failNoPath(t, "examined entry", err)
	}
	defer func() { _ = ok.Close() }()
	body := make([]byte, 64)
	n, _ := ok.Read(body)
	if string(body[:n]) != "PLAIN=surrogate-0002-ghijkl\n" {
		t.Fatalf("examined entry: content differs from the fixture")
	}
}

func TestOpenScreenedUnexamined(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	const env = "TOKEN=surrogate-0003-mnopqr\n"
	rows := []struct {
		label      string
		seam       func(t *testing.T, root, sib string) func(string) error
		unexamined bool
		wantErr    error // errors.Is target; nil: success
		notErr     error // errors.Is must be false
	}{
		{label: "refused", unexamined: true, wantErr: errWalkEntryUnexamined},
		{
			label: "seam error ignored", unexamined: true,
			seam: func(t *testing.T, root, sib string) func(string) error {
				return answer("sub/.env", fs.ErrPermission)
			},
			wantErr: errWalkEntryUnexamined, notErr: fs.ErrPermission,
		},
		{
			label: "parent moved", unexamined: true,
			seam: func(t *testing.T, root, sib string) func(string) error {
				return func(rel string) error {
					if rel == "sub/.env" {
						if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "sub-away")); err != nil {
							failNoPath(t, "parent moved", err)
						}
					}
					return nil
				}
			},
			wantErr: errWalkEntryUnexamined, notErr: errFileChanged,
		},
		{
			label: "root swapped", unexamined: true,
			seam: func(t *testing.T, root, sib string) func(string) error {
				return func(rel string) error {
					if rel == "sub/.env" {
						swapRoot(t, root, sib)
					}
					return nil
				}
			},
			wantErr: ErrRootReplaced,
		},
		{
			label: "examined seam error",
			seam: func(t *testing.T, root, sib string) func(string) error {
				return answer("sub/.env", fs.ErrPermission)
			},
			wantErr: fs.ErrPermission,
		},
		{label: "examined"},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			ws, root, sib := tolerantFixture(t, map[string]string{"sub/.env": env}, map[string]string{"sub/.env": env})
			dir := openHeld(t, r.label, filepath.Join(root, "sub"))
			e := workspaceEntry{name: ".env", dir: dir, unexamined: true}
			if !r.unexamined {
				e = examinedEntry(t, r.label, dir, ".env")
			}
			if r.seam != nil {
				ws.screenSeams = &screenSeams{openErr: r.seam(t, root, sib)}
			}
			f, err := ws.openScreened("sub/.env", e)
			if f != nil {
				defer func() { _ = f.Close() }()
			}
			if r.wantErr == nil {
				if err != nil || f == nil {
					t.Fatalf("%s: open failed (%T)", r.label, err)
				}
				body := make([]byte, 64)
				n, _ := f.Read(body)
				if string(body[:n]) != env {
					t.Fatalf("%s: content differs from the fixture", r.label)
				}
				return
			}
			if !errors.Is(err, r.wantErr) || f != nil {
				t.Fatalf("%s: got (file=%v, %T), want the expected error and no file", r.label, f != nil, err)
			}
			if r.notErr != nil && errors.Is(err, r.notErr) {
				t.Fatalf("%s: error is the wrong kind (%T)", r.label, err)
			}
		})
	}
}
