package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishFileSetStageFailurePreservesExistingFiles(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.json")
	blockedParent := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(oldPath, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockedParent, []byte("block\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := publishFileSet([]filePublication{
		{target: oldPath, data: []byte("new\n"), mode: 0o600},
		{target: filepath.Join(blockedParent, "new.json"), data: []byte("new\n"), mode: 0o600},
	}, nil)
	if err == nil {
		t.Fatal("publishFileSet succeeded despite an unusable staging directory")
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

	err := publishFileSetWithRename([]filePublication{
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
	err := publishFileSetWithRename([]filePublication{
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
	err := publishFileSetWithRename([]filePublication{
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
	err := publishFileSetWithRename([]filePublication{
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
			if err := publishFileSet(replacements, removals); err == nil {
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
