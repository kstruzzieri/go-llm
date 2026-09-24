//go:build linux || darwin

package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMutationConditionalPreservesCheckedMode(t *testing.T) {
	for _, checkMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "hash-only", true: "hash-and-mode"}[checkMode], func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "file")
			mutationMust(t, os.WriteFile(path, []byte("ORIGINAL\n"), 0644))
			mutationMust(t, os.Chmod(path, 0644))
			ws, err := NewWorkspace(root)
			mutationMust(t, err)
			reached := false
			ws.SetScopeGuard(func(_ string, write bool) error {
				if write {
					reached = true
					return os.Chmod(path, 0600)
				}
				return nil
			})
			expected := FilePrecondition{Exists: true, Hash: ContentHash([]byte("ORIGINAL\n")), CheckMode: checkMode}
			if checkMode {
				expected.Mode = 0600
			}
			changedAfterCheck := false
			ws.beforeMutation = func(at mutationPhase, _ string) error {
				if at == mutationBeforeChmod {
					// Conditional writes preserve the checked mode, not a later change.
					changedAfterCheck = true
					return os.Chmod(path, 0644)
				}
				return nil
			}
			mutationMust(t, ws.WriteFileAtomicIfMatch("file", []byte("APPROVED\n"), expected))
			info, err := os.Stat(path)
			mutationMust(t, err)
			if !reached || !changedAfterCheck || info.Mode().Perm() != 0600 {
				t.Fatalf("restored stale permissions: reached=%v changedAfterCheck=%v mode=%v", reached, changedAfterCheck, info.Mode())
			}
			mutationBytes(t, path, "APPROVED\n")
			mutationNoTemps(t, root)
		})
	}
}

func TestMutationUnconditionalPreservesCurrentMode(t *testing.T) {
	for _, phase := range []string{"guard", "before-chmod"} {
		for _, mode := range []fs.FileMode{0600, 0000} {
			t.Run(phase+"/"+mode.String(), func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "file")
				mutationMust(t, os.WriteFile(path, []byte("ORIGINAL\n"), 0644))
				mutationMust(t, os.Chmod(path, 0644))
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				reached := false
				changeMode := func() error {
					reached = true
					return os.Chmod(path, mode)
				}
				ws.SetScopeGuard(func(_ string, write bool) error {
					if !write {
						return fs.ErrPermission // unconditional writes must not require read policy
					}
					if phase == "guard" {
						return changeMode()
					}
					return nil
				})
				ws.beforeMutation = func(at mutationPhase, _ string) error {
					if phase == "before-chmod" && at == mutationBeforeChmod {
						return changeMode()
					}
					return nil
				}
				mutationMust(t, ws.WriteFileAtomic("file", []byte("APPROVED\n")))
				info, err := os.Stat(path)
				mutationMust(t, err)
				if !reached || info.Mode().Perm() != mode {
					t.Fatalf("WriteFileAtomic restored stale permissions: reached=%v mode=%v, want %v", reached, info.Mode(), mode)
				}
				mutationMust(t, os.Chmod(path, 0600))
				mutationBytes(t, path, "APPROVED\n")
				mutationNoTemps(t, root)
			})
		}
	}
}

func TestMutationConditionalMissingLeaf(t *testing.T) {
	for _, remove := range []bool{false, true} {
		for _, phase := range []mutationPhase{mutationBeforeCheck, mutationAfterCheck, mutationBeforeRemove} {
			if !remove && phase == mutationBeforeRemove {
				continue
			}
			t.Run(map[bool]string{false: "write", true: "remove"}[remove]+"/"+string(phase), func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "file")
				mutationMust(t, os.WriteFile(path, []byte("ORIGINAL\n"), 0600))
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				reached := false
				cleanupErr := errors.New("cleanup blocked")
				ws.beforeMutation = func(at mutationPhase, _ string) error {
					if at == phase {
						reached = true
						mutationMust(t, os.Remove(path))
					}
					if at == mutationBeforeCleanup {
						return cleanupErr
					}
					return nil
				}
				expected := FilePrecondition{Exists: true, Hash: ContentHash([]byte("ORIGINAL\n"))}
				if remove {
					err = ws.RemoveFileIfMatch("file", expected)
				} else {
					err = ws.WriteFileAtomicIfMatch("file", []byte("APPROVED\n"), expected)
				}
				if !reached || !errors.Is(err, ErrPreconditionMismatch) {
					t.Fatalf("missing leaf not classified as mismatch: reached=%v err=%v", reached, err)
				}
				if !remove && phase == mutationAfterCheck && !errors.Is(err, cleanupErr) {
					t.Fatalf("lost independent cleanup failure: %v", err)
				}
				if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("missing target recreated: %v", err)
				}
			})
		}
	}
}

// An entry appearing after its name was admitted absent is never adopted:
// conditional creates report a mismatch, unconditional creates fs.ErrExist,
// non-regular entries their type, and an absent remove never unlinks it.
func TestMutationLateAbsentEntry(t *testing.T) {
	for _, op := range []string{"conditional", "unconditional", "remove"} {
		for _, kind := range []string{"regular", "symlink", "directory"} {
			t.Run(op+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "file")
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				phase := mutationAfterTemp // after the temp exists, before the final recheck
				if op == "remove" {
					phase = mutationBeforeCheck
				}
				reached := false
				ws.beforeMutation = func(at mutationPhase, _ string) error {
					if at != phase {
						return nil
					}
					reached = true
					switch kind {
					case "symlink":
						mutationMust(t, os.Symlink("elsewhere", path))
					case "directory":
						mutationMust(t, os.Mkdir(path, 0700))
					default:
						mutationMust(t, os.WriteFile(path, []byte("LATE\n"), 0600))
					}
					return nil
				}
				var want error
				switch op {
				case "conditional":
					err = ws.WriteFileAtomicIfMatch("file", []byte("APPROVED\n"), FilePrecondition{})
					want = ErrPreconditionMismatch
				case "unconditional":
					err = ws.WriteFileAtomic("file", []byte("APPROVED\n"))
					want = fs.ErrExist
				default:
					err = ws.RemoveFile("file")
					want = fs.ErrNotExist
				}
				if op != "remove" && kind == "symlink" {
					want = errSymlink
				} else if op != "remove" && kind == "directory" {
					want = errNotRegular
				}
				if !reached || !errors.Is(err, want) {
					t.Fatalf("reached=%v err=%v want=%v", reached, err, want)
				}
				switch kind {
				case "symlink":
					if link, err := os.Readlink(path); err != nil || link != "elsewhere" {
						t.Fatalf("late symlink changed: %q %v", link, err)
					}
				case "directory":
					if fi, err := os.Lstat(path); err != nil || !fi.IsDir() {
						t.Fatalf("late directory changed: %v %v", fi, err)
					}
				default:
					mutationBytes(t, path, "LATE\n")
				}
				mutationNoTemps(t, root)
			})
		}
	}
}

// A conditional create reads no content, so read policy must not block it.
func TestMutationConditionalCreateSkipsReadPolicy(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	mutationMust(t, err)
	ws.SetScopeGuard(func(_ string, write bool) error {
		if !write {
			return errors.New("reads denied")
		}
		return nil
	})
	mutationMust(t, ws.WriteFileAtomicIfMatch("file", []byte("APPROVED\n"), FilePrecondition{}))
	mutationBytes(t, filepath.Join(root, "file"), "APPROVED\n")
}

func TestMutationConditionalCapability(t *testing.T) {
	for _, op := range []string{"create", "overwrite", "remove"} {
		for _, victim := range []string{"outside", "denied"} {
			t.Run(op+"/"+victim, func(t *testing.T) {
				base := t.TempDir()
				root := filepath.Join(base, "root")
				parent := filepath.Join(root, "parent")
				outside := filepath.Join(base, "outside")
				denied := filepath.Join(root, "denied")
				for _, dir := range []string{parent, outside, denied} {
					mutationMust(t, os.MkdirAll(dir, 0700))
				}
				mutationMust(t, os.WriteFile(filepath.Join(outside, "file"), []byte("OUTSIDE\n"), 0600))
				mutationMust(t, os.WriteFile(filepath.Join(denied, "file"), []byte("DENIED\n"), 0600))
				expected := FilePrecondition{}
				if op != "create" {
					mutationMust(t, os.WriteFile(filepath.Join(parent, "file"), []byte("ORIGINAL\n"), 0600))
					expected = FilePrecondition{Exists: true, Hash: ContentHash([]byte("ORIGINAL\n"))}
				}
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				ws.SetScopeGuard(func(rel string, _ bool) error {
					if rel == "denied" || strings.HasPrefix(rel, "denied/") {
						return fs.ErrPermission
					}
					return nil
				})
				reached := false
				ws.beforeMutation = func(at mutationPhase, _ string) error {
					if at == mutationAfterCheck {
						reached = true
						mutationMust(t, os.Rename(parent, parent+"-old"))
						if victim == "outside" {
							mutationMust(t, os.Rename(outside, parent))
							outside = parent
						} else {
							mutationMust(t, os.Rename(denied, parent))
							denied = parent
						}
					}
					return nil
				}
				if op == "remove" {
					err = ws.RemoveFileIfMatch("parent/file", expected)
				} else {
					err = ws.WriteFileAtomicIfMatch("parent/file", []byte("APPROVED\n"), expected)
				}
				if !reached {
					t.Fatal("conditional check seam not reached")
				}
				mutationMust(t, err)
				mutationBytes(t, filepath.Join(outside, "file"), "OUTSIDE\n")
				mutationBytes(t, filepath.Join(denied, "file"), "DENIED\n")
				if op == "remove" {
					if _, err := os.Lstat(filepath.Join(parent+"-old", "file")); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("checked file not deleted: %v", err)
					}
				} else {
					mutationBytes(t, filepath.Join(parent+"-old", "file"), "APPROVED\n")
				}
			})
		}
	}
}

func TestMutationConditionalReadPolicy(t *testing.T) {
	for _, denyWrite := range []bool{false, true} {
		for _, remove := range []bool{false, true} {
			t.Run(map[bool]string{false: "read", true: "write"}[denyWrite]+map[bool]string{false: "/replace", true: "/delete"}[remove], func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "file")
				mutationMust(t, os.WriteFile(path, []byte("ORIGINAL\n"), 0600))
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				denied := errors.New("host-only denial")
				ws.SetScopeGuard(func(_ string, write bool) error {
					if write == denyWrite {
						return denied
					}
					return nil
				})
				expected := FilePrecondition{Exists: true, Hash: ContentHash([]byte("ORIGINAL\n"))}
				if remove {
					err = ws.RemoveFileIfMatch("file", expected)
				} else {
					err = ws.WriteFileAtomicIfMatch("file", []byte("APPROVED\n"), expected)
				}
				if !errors.Is(err, denied) || !errors.Is(err, errScopeDenied) {
					t.Fatalf("policy not enforced: %v", err)
				}
				mutationBytes(t, path, "ORIGINAL\n")
				mutationNoTemps(t, root)
			})
		}
	}
}

func TestMutationNoFollowAdmission(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "remove"}[remove], func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "parent")
			mutationMust(t, os.Mkdir(parent, 0700))
			mutationMust(t, os.WriteFile(filepath.Join(parent, "file"), []byte("ORIGINAL\n"), 0600))
			link := filepath.Join(root, "link")
			mutationMust(t, os.Symlink(parent+"-old", link))
			ws, err := NewWorkspace(root)
			mutationMust(t, err)
			reached := false
			ws.beforeMutation = func(at mutationPhase, _ string) error {
				if at == mutationBeforeOpen && !reached {
					reached = true
					mutationMust(t, os.Rename(parent, parent+"-old"))
					mutationMust(t, os.Rename(link, parent))
				}
				return nil
			}
			if remove {
				err = ws.RemoveFile("parent/file")
			} else {
				err = ws.WriteFileAtomic("parent/file", []byte("APPROVED\n"))
			}
			if !reached || err == nil {
				t.Fatalf("symlink to same admitted inode followed: %v", err)
			}
			mutationBytes(t, filepath.Join(parent+"-old", "file"), "ORIGINAL\n")
		})
	}
}

func TestMutationRejectFIFO(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "remove"}[remove], func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "pipe")
			mutationMust(t, unix.Mkfifo(path, 0600))
			ws, err := NewWorkspace(root)
			mutationMust(t, err)
			if remove {
				err = ws.RemoveFile("pipe")
			} else {
				err = ws.WriteFileAtomic("pipe", []byte("APPROVED\n"))
			}
			if !errors.Is(err, errNotRegular) {
				t.Fatalf("FIFO admitted: %v", err)
			}
			fi, err := os.Lstat(path)
			mutationMust(t, err)
			if fi.Mode()&fs.ModeNamedPipe == 0 {
				t.Fatalf("FIFO mutated: %v", fi.Mode())
			}
		})
	}
}

func TestMutationAbsentAliases(t *testing.T) {
	for _, pair := range [][2]string{{"Secret", "secret"}, {"caf\u00e9", "cafe\u0301"}} {
		for _, scenario := range []string{"concurrent", "byte-exact-limit", "equivalence-guard"} {
			t.Run(pair[0]+"/"+scenario, func(t *testing.T) {
				root := t.TempDir()
				actual := filepath.Join(root, pair[0])
				alias := filepath.Join(root, pair[1])
				mutationMust(t, os.WriteFile(actual, []byte("PROBE\n"), 0600))
				if _, err := os.Lstat(alias); errors.Is(err, fs.ErrNotExist) {
					t.Skip("fixture distinguishes these names")
				} else {
					mutationMust(t, err)
				}
				mutationMust(t, os.Remove(actual))
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				ws.SetScopeGuard(func(rel string, _ bool) error {
					if rel == pair[0] || scenario == "equivalence-guard" && rel == pair[1] {
						return fs.ErrPermission
					}
					return nil
				})
				reached := false
				if scenario == "concurrent" {
					replacement := filepath.Join(root, "replacement")
					mutationMust(t, os.WriteFile(replacement, []byte("CONCURRENT\n"), 0600))
					ws.beforeMutation = func(at mutationPhase, _ string) error {
						if at == mutationBeforeRename {
							reached = true
							mutationMust(t, os.Rename(replacement, actual))
						}
						return nil
					}
				}
				err = ws.WriteFileAtomicIfMatch(pair[1], []byte("APPROVED\n"), FilePrecondition{})
				switch scenario {
				case "concurrent":
					if !reached || !errors.Is(err, ErrPreconditionMismatch) {
						t.Fatalf("alias create overwritten: %v", err)
					}
					mutationBytes(t, actual, "CONCURRENT\n")
				case "byte-exact-limit":
					mutationMust(t, err)
					mutationBytes(t, actual, "APPROVED\n")
				case "equivalence-guard":
					if !errors.Is(err, errScopeDenied) {
						t.Fatalf("equivalence policy bypass: %v", err)
					}
					if _, err := os.Lstat(alias); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("denied alias created: %v", err)
					}
				}
				mutationNoTemps(t, root)
			})
		}
	}
}
