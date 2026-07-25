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
	return nil
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
