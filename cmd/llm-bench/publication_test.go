package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishFileSetStageFailurePreservesExistingFiles(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.json")
	if err := os.WriteFile(oldPath, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	_, err := publishFileSet([]filePublication{
		{target: oldPath, data: []byte("new\n"), mode: 0o600},
		{target: filepath.Join(dir, "missing-parent", "new.json"), data: []byte("new\n"), mode: 0o600},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "stage") {
		t.Fatalf("publishFileSet error = %v; want actual staging failure", err)
	}
	assertFileState(t, oldPath, "old\n", 0o640)
	assertNoPublicationDebris(t, dir)
}

func TestPublishFileSetFirstPublishFailureRollsBackReplacementsAndRemoval(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "data.json")
	manifestPath := filepath.Join(dir, "manifest.json")
	stalePath := filepath.Join(dir, "stale.json")
	for _, f := range []struct {
		path string
		data string
		mode os.FileMode
	}{{dataPath, "old data\n", 0o640}, {manifestPath, "old manifest\n", 0o600}, {stalePath, "old stale\n", 0o644}} {
		if err := os.WriteFile(f.path, []byte(f.data), f.mode); err != nil {
			t.Fatal(err)
		}
	}
	oldInfo := make(map[string]os.FileInfo, 3)
	for _, path := range []string{dataPath, manifestPath, stalePath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		oldInfo[path] = info
	}

	_, err := publishFileSetWithRename([]filePublication{
		{target: dataPath, data: []byte("new data\n"), mode: 0o600},
		{target: manifestPath, data: []byte("new manifest\n"), mode: 0o600},
	}, []string{stalePath}, func(oldPath, newPath string) error {
		if newPath == dataPath && strings.Contains(filepath.Base(oldPath), ".publish-") {
			return errors.New("injected first publish failure")
		}
		return os.Rename(oldPath, newPath)
	})
	if err == nil || !strings.Contains(err.Error(), "injected first publish failure") {
		t.Fatalf("publishFileSetWithRename error = %v", err)
	}
	assertFileState(t, dataPath, "old data\n", 0o640)
	assertFileState(t, manifestPath, "old manifest\n", 0o600)
	assertFileState(t, stalePath, "old stale\n", 0o644)
	for path, prior := range oldInfo {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat restored %s: %v", path, err)
			continue
		}
		if !os.SameFile(prior, info) {
			t.Errorf("%s was not restored as its original inode (err=%v)", path, err)
		}
	}
	assertNoPublicationDebris(t, dir)
}

func TestPublishFileSetManifestLastFailureRemovesNewFilesAndRollsBack(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "data.json")
	manifestPath := filepath.Join(dir, "manifest.json")
	stalePath := filepath.Join(dir, "stale.json")
	if err := os.WriteFile(manifestPath, []byte("old manifest\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePath, []byte("old stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var published []string
	_, err := publishFileSetWithRename([]filePublication{
		{target: dataPath, data: []byte("new data\n"), mode: 0o644},
		{target: manifestPath, data: []byte("new manifest\n"), mode: 0o600},
	}, []string{stalePath}, func(oldPath, newPath string) error {
		if strings.Contains(filepath.Base(oldPath), ".publish-") {
			published = append(published, newPath)
			if newPath == manifestPath {
				return errors.New("injected manifest publish failure")
			}
		}
		return os.Rename(oldPath, newPath)
	})
	if err == nil || !strings.Contains(err.Error(), "injected manifest publish failure") {
		t.Fatalf("publishFileSetWithRename error = %v", err)
	}
	if len(published) != 2 || published[0] != dataPath || published[1] != manifestPath {
		t.Fatalf("publish order = %v; want data then manifest", published)
	}
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatalf("new data survived rollback: %v", err)
	}
	assertFileState(t, manifestPath, "old manifest\n", 0o640)
	assertFileState(t, stalePath, "old stale\n", 0o644)
	assertNoPublicationDebris(t, dir)
}

func TestPublishFileSetRestoreFailureNamesRecoveryBackup(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "data.json")
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte("old manifest\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	var recoveryBackup string
	_, err := publishFileSetWithRename([]filePublication{
		{target: dataPath, data: []byte("new data\n"), mode: 0o644},
		{target: manifestPath, data: []byte("new manifest\n"), mode: 0o600},
	}, nil, func(oldPath, newPath string) error {
		if strings.Contains(filepath.Base(oldPath), ".publish-") && newPath == manifestPath {
			return errors.New("injected publish failure")
		}
		if strings.Contains(filepath.Base(oldPath), ".backup-") && newPath == manifestPath {
			recoveryBackup = oldPath
			return errors.New("injected restore failure")
		}
		return os.Rename(oldPath, newPath)
	})
	if err == nil || recoveryBackup == "" || !strings.Contains(err.Error(), recoveryBackup) {
		t.Fatalf("error = %v; want named recovery backup %q", err, recoveryBackup)
	}
	assertFileState(t, recoveryBackup, "old manifest\n", 0o640)
}

func TestPublishFileSetPartialBackupFailureRestoresPriorBackup(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.json")
	second := filepath.Join(dir, "second.json")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("old "+filepath.Base(path)+"\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	firstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}

	_, err = publishFileSetWithOps([]filePublication{
		{target: first, data: []byte("new first\n"), mode: 0o600},
		{target: second, data: []byte("new second\n"), mode: 0o600},
	}, nil, filePublicationOps{
		rename: func(oldPath, newPath string) error {
			if oldPath == second && strings.Contains(filepath.Base(newPath), ".backup-") {
				return errors.New("injected second backup failure")
			}
			return os.Rename(oldPath, newPath)
		},
		remove:  os.Remove,
		inspect: inspectFilePublicationTarget,
	})
	if err == nil || !strings.Contains(err.Error(), "injected second backup failure") {
		t.Fatalf("publish error = %v", err)
	}
	assertFileState(t, first, "old first.json\n", 0o640)
	assertFileState(t, second, "old second.json\n", 0o640)
	restoredInfo, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, restoredInfo) {
		t.Fatal("first backup was not restored as its original inode")
	}
	assertNoPublicationDebris(t, dir)
}

func TestPublishFileSetCleanupFailureReturnsCommittedWarning(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.json")
	if err := os.WriteFile(target, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	var backupPath string
	outcome, err := publishFileSetWithOps([]filePublication{
		{target: target, data: []byte("new\n"), mode: 0o600},
	}, nil, filePublicationOps{
		rename: func(oldPath, newPath string) error {
			if oldPath == target && strings.Contains(filepath.Base(newPath), ".backup-") {
				backupPath = newPath
			}
			return os.Rename(oldPath, newPath)
		},
		remove: func(path string) error {
			if path == backupPath {
				return errors.New("injected cleanup failure")
			}
			return os.Remove(path)
		},
		inspect: inspectFilePublicationTarget,
	})
	if err != nil {
		t.Fatalf("committed publication returned fatal error: %v", err)
	}
	if len(outcome.cleanupWarnings) != 1 || !strings.Contains(outcome.cleanupWarnings[0].Error(), "injected cleanup failure") {
		t.Fatalf("cleanup warnings = %v", outcome.cleanupWarnings)
	}
	assertFileState(t, target, "new\n", 0o600)
	assertFileState(t, backupPath, "old\n", 0o640)
	var warning bytes.Buffer
	writeFilePublicationWarnings(&warning, "test-scope", outcome)
	if !strings.Contains(warning.String(), "test-scope: WARNING evidence set published but backup cleanup failed:") {
		t.Fatalf("warning = %q", warning.String())
	}
}

func TestPublishFileSetInspectsEachTargetOnce(t *testing.T) {
	dir := t.TempDir()
	const targetCount = 128
	replacements := make([]filePublication, 0, targetCount)
	for i := 0; i < targetCount; i++ {
		replacements = append(replacements, filePublication{
			target: filepath.Join(dir, fmt.Sprintf("file-%03d.json", i)),
			data:   []byte("data\n"),
			mode:   0o600,
		})
	}
	inspectCalls := 0
	ops := defaultFilePublicationOps()
	ops.inspect = func(path string) (publicationTarget, error) {
		inspectCalls++
		return inspectFilePublicationTarget(path)
	}
	if _, err := publishFileSetWithOps(replacements, nil, ops); err != nil {
		t.Fatal(err)
	}
	if inspectCalls != targetCount {
		t.Fatalf("target inspections = %d; want exactly %d", inspectCalls, targetCount)
	}
	assertNoPublicationDebris(t, dir)
}

func TestPublishFileSetPublishesInOrderWithRequestedModes(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "data.json")
	manifestPath := filepath.Join(dir, "manifest.json")
	stalePath := filepath.Join(dir, "stale.json")
	if err := os.WriteFile(dataPath, []byte("old data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePath, []byte("old stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var published []string
	_, err := publishFileSetWithRename([]filePublication{
		{target: dataPath, data: []byte("new data\n"), mode: 0o644},
		{target: manifestPath, data: []byte("new manifest\n"), mode: 0o600},
	}, []string{stalePath}, func(oldPath, newPath string) error {
		if strings.Contains(filepath.Base(oldPath), ".publish-") {
			published = append(published, newPath)
		}
		return os.Rename(oldPath, newPath)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 || published[0] != dataPath || published[1] != manifestPath {
		t.Fatalf("publish order = %v; want data then manifest", published)
	}
	assertFileState(t, dataPath, "new data\n", 0o644)
	assertFileState(t, manifestPath, "new manifest\n", 0o600)
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale target survived publication: %v", err)
	}
	assertNoPublicationDebris(t, dir)
}

func TestPublishFileSetPreflightResolvesDotDotAfterSymlinkWithoutMutation(t *testing.T) {
	dir, target, disguised := makeSymlinkDotDotPath(t, "evidence.json")
	if err := os.WriteFile(target, []byte("original\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	prior, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	_, err = publishFileSet([]filePublication{
		{target: target, data: []byte("first\n"), mode: 0o600},
		{target: disguised, data: []byte("second\n"), mode: 0o600},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "aliases target") {
		t.Fatalf("publishFileSet error = %v; want preflight alias refusal", err)
	}
	assertFileState(t, target, "original\n", 0o640)
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(prior, after) {
		t.Fatal("preflight alias refusal replaced the original inode")
	}
	assertNoPublicationDebris(t, dir)
}

func TestPublishFileSetRollbackRestoresTargetReachedThroughSymlinkDotDot(t *testing.T) {
	dir, target, disguised := makeSymlinkDotDotPath(t, "evidence.json")
	if err := os.WriteFile(target, []byte("original\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	prior, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	later := filepath.Join(dir, "later.json")

	_, err = publishFileSetWithRename([]filePublication{
		{target: disguised, data: []byte("replacement\n"), mode: 0o600},
		{target: later, data: []byte("later\n"), mode: 0o600},
	}, nil, func(oldPath, newPath string) error {
		if newPath == later && strings.Contains(filepath.Base(oldPath), ".publish-") {
			return errors.New("injected later publish failure")
		}
		return os.Rename(oldPath, newPath)
	})
	if err == nil || !strings.Contains(err.Error(), "injected later publish failure") {
		t.Fatalf("publishFileSetWithRename error = %v; want injected failure", err)
	}
	assertFileState(t, target, "original\n", 0o640)
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(prior, after) {
		t.Fatal("rollback did not restore the original inode")
	}
	if _, err := os.Stat(later); !os.IsNotExist(err) {
		t.Fatalf("later target survived failed publication: %v", err)
	}
	assertNoPublicationDebris(t, dir)
}

func TestPublishFileSetPreflightRejectsUnsafeOrAliasedTargetsWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir, prior string) ([]filePublication, []string)
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, dir, prior string) ([]filePublication, []string) {
				t.Helper()
				link := filepath.Join(dir, "link.json")
				if err := os.Symlink(prior, link); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return []filePublication{{target: link, data: []byte("new\n"), mode: 0o600}}, nil
			},
		},
		{
			name: "non-regular removal",
			setup: func(t *testing.T, dir, _ string) ([]filePublication, []string) {
				t.Helper()
				directory := filepath.Join(dir, "directory")
				if err := os.Mkdir(directory, 0o755); err != nil {
					t.Fatal(err)
				}
				return nil, []string{directory}
			},
		},
		{
			name: "replacement removal overlap",
			setup: func(_ *testing.T, _ string, prior string) ([]filePublication, []string) {
				return []filePublication{{target: prior, data: []byte("new\n"), mode: 0o600}}, []string{prior}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			prior := filepath.Join(dir, "prior.json")
			if err := os.WriteFile(prior, []byte("prior\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			replacements, removals := tc.setup(t, dir, prior)
			if _, err := publishFileSet(replacements, removals); err == nil {
				t.Fatal("publishFileSet accepted an unsafe target set")
			}
			assertFileState(t, prior, "prior\n", 0o640)
			assertNoPublicationDebris(t, dir)
		})
	}
}

func assertFileState(t *testing.T, path, want string, mode os.FileMode) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if string(raw) != want || info.Mode().Perm() != mode {
		t.Fatalf("%s = %q mode %04o; want %q mode %04o", path, raw, info.Mode().Perm(), want, mode)
	}
}

func assertNoPublicationDebris(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if strings.Contains(name, ".publish-") || strings.Contains(name, ".backup-") {
			t.Errorf("publication debris remains: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
