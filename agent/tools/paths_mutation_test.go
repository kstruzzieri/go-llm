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

func mutationMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func mutationBytes(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("%s: bytes=%q err=%v, want %q", path, got, err, want)
	}
}
func mutationNoTemps(t *testing.T, parent string) {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(parent, ".golem-*.tmp"))
	mutationMust(t, err)
	if len(names) != 0 {
		t.Fatalf("leaked temps: %v", names)
	}
}

// Restoring any ambient create/rename/remove/cleanup must change a canary here.
func TestMutationParentCapability(t *testing.T) {
	for _, victim := range []string{"outside", "denied"} {
		for _, kind := range []string{"directory", "symlink", "moved"} {
			for _, phase := range []mutationPhase{mutationBeforeTemp, mutationAfterTemp, mutationBeforeRename, mutationBeforeRemove, mutationBeforeCleanup} {
				t.Run(victim+"/"+kind+"/"+string(phase), func(t *testing.T) {
					base := t.TempDir()
					root := filepath.Join(base, "root")
					allowed := filepath.Join(root, "allowed")
					outside := filepath.Join(base, "outside")
					denied := filepath.Join(root, "denied")
					for _, dir := range []string{allowed, outside, denied} {
						mutationMust(t, os.MkdirAll(dir, 0700))
					}
					for path, body := range map[string]string{filepath.Join(allowed, "file"): "ORIGINAL\n", filepath.Join(outside, "file"): "OUTSIDE\n", filepath.Join(denied, "file"): "DENIED\n"} {
						mutationMust(t, os.WriteFile(path, []byte(body), 0600))
					}
					ws, err := NewWorkspace(root)
					mutationMust(t, err)
					ws.SetScopeGuard(func(rel string, _ bool) error {
						if rel == "denied" || strings.HasPrefix(rel, "denied/") {
							return fs.ErrPermission
						}
						return nil
					})
					selected := outside
					if victim == "denied" {
						selected = denied
					}
					replacement := filepath.Join(base, "replacement")
					if kind == "symlink" {
						mutationMust(t, os.Symlink(selected, replacement))
					}
					moved := filepath.Join(base, "moved")
					if victim == "denied" {
						moved = filepath.Join(denied, "admitted")
					}
					reached := false
					temp := ""
					injected := errors.New("injected write failure")
					ws.beforeMutation = func(at mutationPhase, name string) error {
						if at == mutationAfterTemp && phase == mutationBeforeTemp {
							for _, dir := range []string{outside, denied} {
								if body, err := os.ReadFile(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
									t.Errorf("SECURITY: temporary content escaped admitted parent: %q, %v", body, err)
								}
							}
						}
						if at == mutationAfterTemp && phase == mutationBeforeCleanup {
							return injected
						}
						if at != phase {
							return nil
						}
						reached = true
						temp = name
						if name != "" {
							for path, body := range map[string]string{filepath.Join(outside, name): "OUTSIDE-TEMP\n", filepath.Join(denied, name): "DENIED-TEMP\n"} {
								mutationMust(t, os.WriteFile(path, []byte(body), 0600))
							}
						}
						mutationMust(t, os.Rename(allowed, moved))
						switch kind {
						case "directory":
							mutationMust(t, os.Rename(selected, allowed))
							if victim == "outside" {
								outside = allowed
							} else {
								denied = allowed
								moved = filepath.Join(allowed, "admitted")
							}
						case "symlink":
							mutationMust(t, os.Rename(replacement, allowed))
						}
						return nil
					}
					if phase == mutationBeforeRemove {
						err = ws.RemoveFile("allowed/file")
					} else {
						err = ws.WriteFileAtomic("allowed/file", []byte("APPROVED\n"))
					}
					if !reached {
						t.Fatal("phase did not execute")
					}
					if phase == mutationBeforeCleanup {
						if !errors.Is(err, injected) {
							t.Fatalf("cleanup error: %v", err)
						}
					} else {
						mutationMust(t, err)
					}
					mutationBytes(t, filepath.Join(outside, "file"), "OUTSIDE\n")
					mutationBytes(t, filepath.Join(denied, "file"), "DENIED\n")
					switch phase {
					case mutationBeforeRemove:
						if _, err := os.Lstat(filepath.Join(moved, "file")); !errors.Is(err, fs.ErrNotExist) {
							t.Fatalf("admitted leaf not deleted: %v", err)
						}
					case mutationBeforeCleanup:
						mutationBytes(t, filepath.Join(moved, "file"), "ORIGINAL\n")
					default:
						mutationBytes(t, filepath.Join(moved, "file"), "APPROVED\n")
					}
					if temp != "" {
						mutationBytes(t, filepath.Join(outside, temp), "OUTSIDE-TEMP\n")
						mutationBytes(t, filepath.Join(denied, temp), "DENIED-TEMP\n")
					}
					mutationNoTemps(t, moved)
				})
			}
		}
	}
}

func TestMutationRootAndComponentIdentity(t *testing.T) {
	for _, point := range []string{"root", "component"} {
		for _, remove := range []bool{false, true} {
			t.Run(point+"/"+map[bool]string{false: "write", true: "remove"}[remove], func(t *testing.T) {
				base := t.TempDir()
				root := filepath.Join(base, "root")
				parent := filepath.Join(root, "parent")
				replacement := filepath.Join(base, "replacement")
				mutationMust(t, os.MkdirAll(parent, 0700))
				mutationMust(t, os.Mkdir(replacement, 0700))
				mutationMust(t, os.WriteFile(filepath.Join(parent, "file"), []byte("ORIGINAL\n"), 0600))
				if point == "root" {
					mutationMust(t, os.Mkdir(filepath.Join(replacement, "parent"), 0700))
					mutationMust(t, os.WriteFile(filepath.Join(replacement, "parent", "file"), []byte("FOREIGN\n"), 0600))
				} else {
					mutationMust(t, os.WriteFile(filepath.Join(replacement, "file"), []byte("FOREIGN\n"), 0600))
				}
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				reached := false
				swap := func() {
					reached = true
					target := parent
					if point == "root" {
						target = root
					}
					mutationMust(t, os.Rename(target, target+"-old"))
					mutationMust(t, os.Rename(replacement, target))
				}
				if point == "root" {
					swap()
				} else {
					ws.beforeMutation = func(at mutationPhase, _ string) error {
						if at == mutationBeforeOpen && !reached {
							swap()
						}
						return nil
					}
				}
				if remove {
					err = ws.RemoveFile("parent/file")
				} else {
					err = ws.WriteFileAtomic("parent/file", []byte("APPROVED\n"))
				}
				if !reached || !errors.Is(err, errFileChanged) {
					t.Fatalf("reached=%v err=%v", reached, err)
				}
				mutationBytes(t, filepath.Join(parent, "file"), "FOREIGN\n")
			})
		}
	}
}

func TestMutationPreconditions(t *testing.T) {
	for _, remove := range []bool{false, true} {
		for _, change := range []string{
			"hash", "absent", "mode", "special-mode", "zero", "invalid-hash", "invalid-absent", "unused-mode",
			"short-hash", "upper-hash", "nonhex-hash", "absent-check-mode", "absent-mode",
		} {
			t.Run(map[bool]string{false: "write", true: "remove"}[remove]+"/"+change, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "file")
				mutationMust(t, os.WriteFile(path, []byte("ORIGINAL\n"), 0600))
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				expected := FilePrecondition{Exists: true, Hash: ContentHash([]byte("ORIGINAL\n"))}
				wantErr := ErrPreconditionMismatch
				switch change {
				case "hash":
					expected.Hash = ContentHash([]byte("OTHER\n"))
				case "absent":
					mutationMust(t, os.Remove(path))
				case "mode":
					expected.CheckMode = true
					expected.Mode = 0700
				case "invalid-hash":
					expected.Hash = ""
					wantErr = fs.ErrInvalid
				case "invalid-absent":
					expected.Exists = false
					wantErr = fs.ErrInvalid
				case "unused-mode":
					expected.Mode = 0600
					wantErr = fs.ErrInvalid
				case "special-mode":
					// Identical bytes and rwx bits; only the setuid bit drifted.
					mutationMust(t, os.Chmod(path, 0600|fs.ModeSetuid))
					if fi, err := os.Lstat(path); err != nil || fi.Mode()&fs.ModeSetuid == 0 {
						t.Fatalf("fixture lacks setuid: %v %v", fi, err)
					}
					expected.CheckMode = true
					expected.Mode = 0600
				case "zero":
					expected = FilePrecondition{} // a create for writes; never valid for removes
					if remove {
						wantErr = fs.ErrInvalid
					}
				case "short-hash":
					expected.Hash = expected.Hash[:10]
					wantErr = fs.ErrInvalid
				case "upper-hash":
					expected.Hash = strings.ToUpper(expected.Hash)
					wantErr = fs.ErrInvalid
				case "nonhex-hash":
					expected.Hash = strings.Repeat("z", 64)
					wantErr = fs.ErrInvalid
				case "absent-check-mode":
					expected = FilePrecondition{CheckMode: true, Mode: 0600}
					wantErr = fs.ErrInvalid
				case "absent-mode":
					expected = FilePrecondition{Mode: 0600}
					wantErr = fs.ErrInvalid
				}
				if remove {
					err = ws.RemoveFileIfMatch("file", expected)
				} else {
					err = ws.WriteFileAtomicIfMatch("file", []byte("APPROVED\n"), expected)
				}
				if !errors.Is(err, wantErr) {
					t.Fatalf("got %v, want %v", err, wantErr)
				}
				if change == "absent" {
					if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("absent leaf changed: %v", err)
					}
				} else {
					mutationBytes(t, path, "ORIGINAL\n")
				}
				mutationNoTemps(t, root)
			})
		}
	}
}

func TestMutationAbsentNoReplace(t *testing.T) {
	for _, conditional := range []bool{false, true} {
		for _, kind := range []string{"regular", "symlink", "directory", "ENOTSUP", "EINVAL"} {
			t.Run(map[bool]string{false: "unconditional", true: "conditional"}[conditional]+"/"+kind, func(t *testing.T) {
				// Darwin reports an unsupported RENAME_EXCL as ENOTSUP; Linux
				// reports an unsupported RENAME_NOREPLACE as EINVAL.
				unsupported := map[string]error{"ENOTSUP": unix.ENOTSUP, "EINVAL": unix.EINVAL}[kind]
				root := t.TempDir()
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				path := filepath.Join(root, "file")
				replacement := filepath.Join(root, "replacement")
				referent := filepath.Join(root, "referent")
				mutationMust(t, os.WriteFile(referent, []byte("REFERENT\n"), 0600))
				switch kind {
				case "symlink":
					mutationMust(t, os.Symlink(referent, replacement))
				case "directory":
					mutationMust(t, os.Mkdir(replacement, 0700))
				default:
					mutationMust(t, os.WriteFile(replacement, []byte("CONCURRENT\n"), 0600))
				}
				reached := false
				if unsupported != nil {
					// Fail the real no-replace call, not an earlier phase, so a
					// plain-rename fallback on its result would create the target.
					ws.noReplaceRename = func(int, string, int, string) error {
						reached = true
						return unsupported
					}
				} else {
					ws.beforeMutation = func(at mutationPhase, _ string) error {
						if at == mutationBeforeRename {
							reached = true
							mutationMust(t, os.Rename(replacement, path))
						}
						return nil
					}
				}
				if conditional {
					err = ws.WriteFileAtomicIfMatch("file", []byte("APPROVED\n"), FilePrecondition{})
				} else {
					err = ws.WriteFileAtomic("file", []byte("APPROVED\n"))
				}
				want := error(fs.ErrExist)
				if conditional {
					want = ErrPreconditionMismatch
				}
				if unsupported != nil {
					want = unsupported
				}
				if !reached || !errors.Is(err, want) {
					t.Fatalf("reached=%v err=%v want=%v", reached, err, want)
				}
				if unsupported != nil && !strings.Contains(err.Error(), "filesystem does not support atomic no-replace create") {
					t.Fatalf("unsupported no-replace hides its cause: %v", err)
				}
				switch kind {
				case "regular":
					mutationBytes(t, path, "CONCURRENT\n")
				case "symlink":
					if _, err := os.Readlink(path); err != nil {
						t.Fatal(err)
					}
				case "directory":
					fi, err := os.Lstat(path)
					if err != nil || !fi.IsDir() {
						t.Fatalf("directory replaced: %v %v", fi, err)
					}
				case "ENOTSUP", "EINVAL":
					if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("unsupported install changed target: %v", err)
					}
				}
				mutationBytes(t, referent, "REFERENT\n")
				mutationNoTemps(t, root)
			})
		}
	}
}

func TestMutationTemporaryIdentity(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		for _, kind := range []string{"regular", "symlink", "directory", "missing"} {
			t.Run(map[bool]string{false: "install", true: "cleanup"}[cleanup]+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "file")
				mutationMust(t, os.WriteFile(path, []byte("ORIGINAL\n"), 0600))
				referent := filepath.Join(root, "referent")
				mutationMust(t, os.WriteFile(referent, []byte("REFERENT\n"), 0600))
				decoy := filepath.Join(root, "decoy")
				switch kind {
				case "regular":
					mutationMust(t, os.WriteFile(decoy, []byte("FOREIGN\n"), 0600))
				case "symlink":
					mutationMust(t, os.Symlink(referent, decoy))
				case "directory":
					mutationMust(t, os.Mkdir(decoy, 0700))
				}
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				temp := ""
				injected := errors.New("stop before install")
				ws.beforeMutation = func(at mutationPhase, name string) error {
					if at != mutationAfterTemp {
						return nil
					}
					temp = filepath.Join(root, name)
					mutationMust(t, os.Rename(temp, filepath.Join(root, "saved")))
					if kind != "missing" {
						mutationMust(t, os.Rename(decoy, temp))
					}
					if cleanup {
						return injected
					}
					return nil
				}
				err = ws.WriteFileAtomic("file", []byte("APPROVED\n"))
				if temp == "" || err == nil {
					t.Fatalf("temp=%q err=%v", temp, err)
				}
				if cleanup && !errors.Is(err, injected) {
					t.Fatalf("lost primary error: %v", err)
				}
				if kind != "missing" {
					if !errors.Is(err, errFileChanged) {
						t.Fatalf("missing identity refusal: %v", err)
					}
					switch kind {
					case "regular":
						mutationBytes(t, temp, "FOREIGN\n")
					case "symlink":
						if _, err := os.Readlink(temp); err != nil {
							t.Fatal(err)
						}
					case "directory":
						fi, err := os.Lstat(temp)
						if err != nil || !fi.IsDir() {
							t.Fatalf("foreign directory removed: %v %v", fi, err)
						}
					}
				}
				mutationBytes(t, path, "ORIGINAL\n")
				mutationBytes(t, referent, "REFERENT\n")
				mutationBytes(t, filepath.Join(root, "saved"), "APPROVED\n")
			})
		}
	}
}
