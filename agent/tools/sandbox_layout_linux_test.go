//go:build linux

package tools

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Exercise named-user and named-group ACL grants and denials as a real non-owner. The parent
// passes descriptors so the child need not traverse the test runner's tempdir.
func TestLayoutMarkerPOSIXACL(t *testing.T) {
	const childFlag = "GOLEM_TEST_MARKER_ACL_CHILD"
	if os.Getenv(childFlag) == "1" {
		layout := sandboxGitLayout{root: "/proc/self/fd/3/."}
		for name, want := range map[string]bool{"user-granted": true, "user-denied": false, "group-granted": true, "group-denied": false} {
			ok, err := layout.searchableLayoutMarker(3, name)
			if err != nil || ok != want {
				t.Errorf("%s search = %t, %v; want %t", name, ok, err, want)
			}
			fd, err := layout.openLayoutDir("", name)
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			if (err == nil) != want || (!want && !errors.Is(err, unix.EACCES)) {
				t.Errorf("%s traversal: %v; want searchable %t", name, err, want)
			}
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires changing to a non-owner UID to exercise a named-user ACL")
	}
	const uid = 65534
	root := t.TempDir()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		group        bool
		named, other uint16
	}{{"user-granted", false, 1, 0}, {"user-denied", false, 0, 1},
		{"group-granted", true, 1, 0}, {"group-denied", true, 0, 1}} {
		p := filepath.Join(root, tc.name)
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		// Linux posix_acl_xattr_header and posix_acl_xattr_entry (little-endian).
		acl := binary.LittleEndian.AppendUint32(nil, 2)
		const noID = ^uint32(0)
		entry := func(tag, perm uint16, id uint32) {
			acl = binary.LittleEndian.AppendUint16(acl, tag)
			acl = binary.LittleEndian.AppendUint16(acl, perm)
			acl = binary.LittleEndian.AppendUint32(acl, id)
		}
		entry(1, 7, noID) // owner
		if !tc.group {
			entry(2, tc.named, uid) // named user
		}
		entry(4, 0, noID) // owning group
		if tc.group {
			entry(8, tc.named, uid) // named group, matching the child's gid
		}
		entry(16, 1, noID)        // mask
		entry(32, tc.other, noID) // other
		if err := unix.Setxattr(p, "system.posix_acl_access", acl, 0); err != nil {
			if errors.Is(err, unix.EOPNOTSUPP) {
				t.Skip("filesystem lacks POSIX ACL support")
			}
			t.Fatal(err)
		}
	}
	dir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	program, err := os.Open("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = program.Close() }()
	cmd := exec.Command("/proc/self/fd/4", "-test.run=^TestLayoutMarkerPOSIXACL$")
	cmd.Env = []string{childFlag + "=1"}
	cmd.ExtraFiles = []*os.File{dir, program}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid}}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("non-owner ACL check: %v: %s", err, out)
	}
}

// Compare with Git's ordinary discovery while the fixture is still in its
// original mode. The after-walk must preserve that decision during its grant.
func TestCommonDirTraversalPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary owner permissions")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := canonGitTempDir(t)
	runIn(t, root, "mkdir -p work common/objects common/refs locked/inner/objects locked/inner/refs && "+
		validHEADBody+" > work/HEAD && chmod 0400 locked")
	restoreModes(t, root)
	pin, err := pinWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pin.Close() }()
	work := filepath.Join(root, "work")
	for _, tc := range []struct {
		target string
		valid  bool
	}{{"../locked/../common", true}, {"../locked/inner", false}} {
		if err := os.WriteFile(filepath.Join(work, "commondir"), []byte(tc.target), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("git", "-C", work, "rev-parse", "--git-dir")
		cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CEILING_DIRECTORIES=" + root}
		out, err := cmd.CombinedOutput()
		if (err == nil) != tc.valid {
			t.Fatalf("Git oracle for %q returned %q (%v), expected valid %t", tc.target, out, err, tc.valid)
		}
		func() {
			access := newOwnerAccess(pin)
			defer access.close()
			defer access.restore()
			for _, grant := range []bool{false, true} {
				if grant {
					if _, err := access.openUpPath("locked"); err != nil {
						t.Fatal(err)
					}
					if err := unix.Access(filepath.Join(root, "locked"), unix.X_OK); err != nil {
						t.Fatal("fixture did not acquire temporary search access", err)
					}
					layout := sandboxGitLayout{root: root, access: access}
					if ok, err := layout.objectDirectorySearch(filepath.Join(root, "locked", "inner", "objects")); err != nil || ok {
						t.Fatalf("object override traversed the temporary grant: searchable %t, err %v", ok, err)
					}
				}
				layout := sandboxGitLayout{root: root, access: access}
				layout.noteLayout(work)
				unknown := layout.unknown[filepath.Join(work, "HEAD")] != ""
				if (len(layout.layoutHEADs) > 0) != tc.valid || unknown == tc.valid {
					t.Fatalf("target %q, grant %t: layouts %v, unknown %v", tc.target, grant, layout.layoutHEADs, layout.unknown)
				}
			}
		}()
	}
}
