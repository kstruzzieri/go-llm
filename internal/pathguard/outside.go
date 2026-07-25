// Package pathguard provides shared filesystem containment checks.
package pathguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateOutside rejects path when its resolved location is root or below it.
func ValidateOutside(path, root string) error {
	resolvedPath, err := resolveExistingAncestors(path)
	if err != nil {
		return fmt.Errorf("pathguard: resolve data path: %w", err)
	}
	resolvedRoot, err := resolveExistingAncestors(root)
	if err != nil {
		return fmt.Errorf("pathguard: resolve root: %w", err)
	}
	rel, err := filepath.Rel(filepath.Clean(resolvedRoot), resolvedPath)
	if err != nil {
		return fmt.Errorf("pathguard: compare data path to root: %w", err)
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return fmt.Errorf("pathguard: data path %q must be outside root %q", resolvedPath, resolvedRoot)
	}
	// The lexical check misses on-disk aliasing the resolver cannot normalize
	// (case folding, Unicode normalization on case-insensitive filesystems), so
	// also reject when any existing ancestor of the path IS the root directory.
	inside, err := ancestorSameFile(resolvedPath, resolvedRoot)
	if err != nil {
		return fmt.Errorf("pathguard: compare data path identity to root: %w", err)
	}
	if inside {
		return fmt.Errorf("pathguard: data path %q resolves inside root %q", resolvedPath, resolvedRoot)
	}
	return nil
}

// ancestorSameFile reports whether any existing ancestor of path (including
// path itself) is the same on-disk directory as root. A root that does not
// exist yet cannot contain anything, so it reports false.
func ancestorSameFile(path, root string) (bool, error) {
	rootInfo, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	current := path
	for {
		info, err := os.Stat(current)
		if err == nil {
			if os.SameFile(info, rootInfo) {
				return true, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
		current = parent
	}
}

func resolveExistingAncestors(path string) (string, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		if _, err := os.Lstat(current); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %q", path)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		current = filepath.Join(current, suffix[i])
	}
	return filepath.Clean(current), nil
}
