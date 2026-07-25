// Package datadir resolves Golem's per-user data directory paths. The CLI and
// the embeddable runtime both persist to the same session database; this is
// the single resolver so the two can never drift onto different files.
package datadir

import (
	"fmt"
	"path/filepath"
)

// Base resolves the per-user data dir base ($XDG_DATA_HOME if absolute, else
// $HOME/.local/share). A relative XDG_DATA_HOME is ignored; a relative or
// missing HOME with no usable XDG is an error.
func Base(getenv func(string) string) (string, error) {
	dir := getenv("XDG_DATA_HOME")
	relativeXDG := dir != "" && !filepath.IsAbs(dir)
	if relativeXDG {
		dir = ""
	}
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			if relativeXDG {
				return "", fmt.Errorf("golem: cannot locate session data dir (XDG_DATA_HOME is relative and HOME unset)")
			}
			return "", fmt.Errorf("golem: cannot locate session data dir (HOME and XDG_DATA_HOME unset)")
		}
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("golem: cannot locate session data dir (HOME is relative)")
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return dir, nil
}

// SessionDBPath locates the session DB under the per-user data dir
// ($XDG_DATA_HOME/golem/sessions.db, else ~/.local/share/golem/sessions.db).
func SessionDBPath(getenv func(string) string) (string, error) {
	base, err := Base(getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "golem", "sessions.db"), nil
}
