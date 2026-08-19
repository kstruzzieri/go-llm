package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrDurabilityUncertain marks a save whose bytes ARE published (rename or
// link succeeded) but whose directory-entry durability could not be confirmed
// (parent fsync failed). Callers must treat the on-disk state as current.
var ErrDurabilityUncertain = errors.New("config: published but durability uncertain")

// syncDir fsyncs a directory so a just-published rename/link is durable.
// Injectable for durability-failure tests.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// writeSiblingTemp writes data to a unique 0600 sibling temp file, fsyncs and
// closes it. Pre-publication phase: any error leaves the target untouched and
// the temp removed.
func writeSiblingTemp(path string, data []byte) (tmp string, err error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("config: create temp for %q: %w", path, err)
	}
	tmp = f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return "", fmt.Errorf("config: chmod %q: %w", tmp, err)
	}
	if _, err = f.Write(data); err != nil {
		return "", fmt.Errorf("config: write %q: %w", tmp, err)
	}
	if err = f.Sync(); err != nil {
		return "", fmt.Errorf("config: sync %q: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return "", fmt.Errorf("config: close %q: %w", tmp, err)
	}
	return tmp, nil
}

// publishReplace atomically replaces path with data (temp + rename + parent
// sync). Pre-rename failure → target untouched. Post-rename sync failure →
// bytes are live; returns ErrDurabilityUncertain (wrapped).
func publishReplace(path string, data []byte) error {
	tmp, err := writeSiblingTemp(path, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: rename %q: %w", path, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: %s: dir sync: %v", ErrDurabilityUncertain, path, err)
	}
	return nil
}

// publishNew creates path with data only if it does not exist: the synced
// temp is hard-linked to the target (link fails with ErrExist on a race — no
// stat-then-act window), then the temp name is dropped.
func publishNew(path string, data []byte) error {
	tmp, err := writeSiblingTemp(path, data)
	if err != nil {
		return err
	}
	if err := os.Link(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: create %q: %w", path, err)
	}
	_ = os.Remove(tmp)
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: %s: dir sync: %v", ErrDurabilityUncertain, path, err)
	}
	return nil
}
