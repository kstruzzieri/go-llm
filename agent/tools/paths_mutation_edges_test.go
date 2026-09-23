//go:build linux || darwin

package tools

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMutationLeafBoundaries(t *testing.T) {
	for _, late := range []bool{false, true} {
		for _, remove := range []bool{false, true} {
			for _, kind := range []string{"symlink", "directory", "fifo", "regular"} {
				t.Run(map[bool]string{false: "observed", true: "late"}[late]+"/"+map[bool]string{false: "write", true: "remove"}[remove]+"/"+kind, func(t *testing.T) {
					root := t.TempDir()
					leaf := filepath.Join(root, "file")
					replacement := filepath.Join(root, "replacement")
					outside := filepath.Join(t.TempDir(), "outside")
					mutationMust(t, os.WriteFile(leaf, []byte("ORIGINAL\n"), 0600))
					mutationMust(t, os.WriteFile(outside, []byte("OUTSIDE\n"), 0600))
					switch kind {
					case "symlink":
						mutationMust(t, os.Symlink(outside, replacement))
					case "directory":
						mutationMust(t, os.Mkdir(replacement, 0700))
					case "fifo":
						mutationMust(t, unix.Mkfifo(replacement, 0600))
					case "regular":
						mutationMust(t, os.WriteFile(replacement, []byte("FOREIGN\n"), 0600))
					}
					ws, err := NewWorkspace(root)
					mutationMust(t, err)
					reached := false
					ws.beforeMutation = func(at mutationPhase, _ string) error {
						wanted := mutationAfterCheck
						if !remove {
							wanted = mutationAfterTemp
						}
						if late {
							wanted = mutationBeforeRename
							if remove {
								wanted = mutationBeforeRemove
							}
						}
						if at == wanted && !reached {
							reached = true
							mutationMust(t, os.Rename(leaf, leaf+"-old"))
							mutationMust(t, os.Rename(replacement, leaf))
						}
						return nil
					}
					if remove {
						err = ws.RemoveFile("file")
					} else {
						err = ws.WriteFileAtomic("file", []byte("APPROVED\n"))
					}
					if !reached {
						t.Fatal("leaf phase not reached")
					}
					if !late || kind == "directory" {
						if err == nil {
							t.Fatal("observed replacement or directory was accepted")
						}
					} else {
						mutationMust(t, err)
					}
					mutationBytes(t, outside, "OUTSIDE\n")
					mutationBytes(t, leaf+"-old", "ORIGINAL\n")
					if kind == "directory" {
						fi, err := os.Lstat(leaf)
						if err != nil || !fi.IsDir() {
							t.Fatalf("directory removed: %v %v", fi, err)
						}
					} else if late && remove {
						if _, err := os.Lstat(leaf); !errors.Is(err, fs.ErrNotExist) {
							t.Fatalf("late basename not removed: %v", err)
						}
					} else if late {
						mutationBytes(t, leaf, "APPROVED\n")
					} else if kind == "regular" {
						mutationBytes(t, leaf, "FOREIGN\n")
					}
					mutationNoTemps(t, root)
				})
			}
		}
	}
}

func TestMutationFailureCleanup(t *testing.T) {
	for _, phase := range []mutationPhase{mutationBeforeWrite, mutationBeforeChmod, mutationBeforeClose, mutationAfterTemp, mutationBeforeRename} {
		for _, blocked := range []bool{false, true} {
			t.Run(string(phase)+map[bool]string{false: "/ordinary", true: "/blocked"}[blocked], func(t *testing.T) {
				if os.Geteuid() == 0 {
					t.Skip("requires unprivileged permissions")
				}
				root := t.TempDir()
				mutationMust(t, os.WriteFile(filepath.Join(root, "file"), []byte("ORIGINAL\n"), 0600))
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				injected := errors.New("injected I/O failure")
				reached := false
				t.Cleanup(func() { _ = os.Chmod(root, 0700) })
				ws.beforeMutation = func(at mutationPhase, _ string) error {
					if at == phase {
						reached = true
						return injected
					}
					if at == mutationBeforeCleanup && blocked {
						mutationMust(t, os.Chmod(root, 0500))
					}
					return nil
				}
				err = ws.WriteFileAtomic("file", []byte("APPROVED\n"))
				if !reached || !errors.Is(err, injected) {
					t.Fatalf("lost failure: reached=%v err=%v", reached, err)
				}
				if blocked && !errors.Is(err, fs.ErrPermission) {
					t.Fatalf("cleanup failure not joined: %v", err)
				}
				mutationMust(t, os.Chmod(root, 0700))
				mutationBytes(t, filepath.Join(root, "file"), "ORIGINAL\n")
				if !blocked {
					mutationNoTemps(t, root)
				}
			})
		}
	}
}

func TestMutationPermissionBoundary(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires unprivileged permissions")
	}
	for _, guarded := range []bool{false, true} {
		for _, conditional := range []bool{false, true} {
			for _, parentMode := range []fs.FileMode{0700, 0300} {
				for _, leafMode := range []fs.FileMode{0600, 0000} {
					for _, remove := range []bool{false, true} {
						label := strings.Join([]string{map[bool]string{false: "unguarded", true: "guarded"}[guarded], map[bool]string{false: "unconditional", true: "conditional"}[conditional], parentMode.String(), leafMode.String(), map[bool]string{false: "write", true: "remove"}[remove]}, "/")
						t.Run(label, func(t *testing.T) {
							root := t.TempDir()
							parent := filepath.Join(root, "parent")
							mutationMust(t, os.Mkdir(parent, 0700))
							leaf := filepath.Join(parent, "file")
							mutationMust(t, os.WriteFile(leaf, []byte("ORIGINAL\n"), 0600))
							mutationMust(t, os.Chmod(leaf, leafMode))
							mutationMust(t, os.Chmod(parent, parentMode))
							t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
							ws, err := NewWorkspace(root)
							mutationMust(t, err)
							if guarded {
								ws.SetScopeGuard(func(string, bool) error { return nil })
							}
							expected := FilePrecondition{Exists: true, Hash: ContentHash([]byte("ORIGINAL\n")), CheckMode: true, Mode: leafMode}
							if conditional {
								if remove {
									err = ws.RemoveFileIfMatch("parent/file", expected)
								} else {
									err = ws.WriteFileAtomicIfMatch("parent/file", []byte("APPROVED\n"), expected)
								}
							} else if remove {
								err = ws.RemoveFile("parent/file")
							} else {
								err = ws.WriteFileAtomic("parent/file", []byte("APPROVED\n"))
							}
							refused := guarded && parentMode == 0300 || conditional && leafMode == 0
							if refused {
								want := error(fs.ErrPermission)
								if guarded && parentMode == 0300 {
									want = errScopeDenied
								}
								if !errors.Is(err, want) {
									t.Fatalf("got %v want %v", err, want)
								}
								mutationMust(t, os.Chmod(leaf, 0600))
								mutationBytes(t, leaf, "ORIGINAL\n")
							} else {
								mutationMust(t, err)
								if !remove {
									fi, err := os.Stat(leaf)
									mutationMust(t, err)
									if fi.Mode().Perm() != leafMode {
										t.Fatalf("mode=%v want=%v", fi.Mode(), leafMode)
									}
									mutationMust(t, os.Chmod(leaf, 0600))
									mutationBytes(t, leaf, "APPROVED\n")
								}
							}
							mutationMust(t, os.Chmod(parent, 0700))
							mutationNoTemps(t, parent)
						})
					}
				}
			}
		}
	}
}

func TestMutationCanonicalPolicy(t *testing.T) {
	for _, pair := range [][2]string{{"Secret", "secret"}, {"caf\u00e9", "cafe\u0301"}} {
		for _, op := range []string{"write", "remove", "canonical", "plan"} {
			t.Run(pair[0]+"/"+op, func(t *testing.T) {
				root := t.TempDir()
				actual := filepath.Join(root, pair[0])
				mutationMust(t, os.WriteFile(actual, []byte("DENIED\n"), 0600))
				if _, err := os.Lstat(filepath.Join(root, pair[1])); errors.Is(err, fs.ErrNotExist) {
					t.Skip("fixture distinguishes these names")
				} else {
					mutationMust(t, err)
				}
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				ws.SetScopeGuard(func(rel string, _ bool) error {
					if rel == pair[0] {
						return fs.ErrPermission
					}
					return nil
				})
				switch op {
				case "write":
					err = ws.WriteFileAtomic(pair[1], []byte("APPROVED\n"))
				case "remove":
					err = ws.RemoveFile(pair[1])
				case "canonical":
					_, err = ws.CanonicalPathForUndo(pair[1])
				case "plan":
					_, _, err = ws.resolveWriteTarget(pair[1])
				}
				if !errors.Is(err, errScopeDenied) {
					t.Fatalf("canonical policy bypass: %v", err)
				}
				mutationBytes(t, actual, "DENIED\n")
			})
		}
	}
}

func TestMutationDescriptorLifetime(t *testing.T) {
	const marker = "GO_LLM_MUTATION_FD_HELPER"
	if os.Getenv(marker) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMutationDescriptorLifetime$", "-test.count=1")
		cmd.Env = append(os.Environ(), marker+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated FD helper: %v\n%s", err, output)
		}
		return
	}
	root := t.TempDir()
	mutationMust(t, os.Mkdir(filepath.Join(root, "parent"), 0700))
	mutationMust(t, os.WriteFile(filepath.Join(root, "parent", "file"), []byte("ORIGINAL\n"), 0600))
	ws, err := NewWorkspace(root)
	mutationMust(t, err)
	injected := errors.New("injected temporary I/O failure")
	phase := mutationBeforeWrite
	ws.beforeMutation = func(at mutationPhase, _ string) error {
		if at == phase {
			return injected
		}
		return nil
	}
	expected := FilePrecondition{Exists: true, Hash: ContentHash([]byte("ORIGINAL\n"))}
	run := func() {
		t.Helper()
		for _, phase = range []mutationPhase{mutationBeforeWrite, mutationBeforeChmod, mutationBeforeClose, mutationAfterTemp, mutationBeforeRename} {
			if err := ws.WriteFileAtomicIfMatch("parent/file", []byte("APPROVED\n"), expected); !errors.Is(err, injected) {
				t.Fatalf("failure path %s: %v", phase, err)
			}
		}
	}
	for range 8 {
		run()
	}
	runtime.GC()
	debug.SetGCPercent(-1) // finalizers must not conceal a missing Close in this helper
	fdDir := "/dev/fd"
	if runtime.GOOS == "linux" {
		fdDir = "/proc/self/fd"
	}
	count := func() int { entries, err := os.ReadDir(fdDir); mutationMust(t, err); return len(entries) }
	before := count()
	for range 64 {
		run()
	}
	after := count()
	if before != after {
		t.Fatalf("descriptors before=%d after=%d", before, after)
	}
	mutationNoTemps(t, filepath.Join(root, "parent"))
}
