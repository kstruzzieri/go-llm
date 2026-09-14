//go:build !unix

package consult

import "os"

// The runner is unsupported on non-Unix platforms; Unix mode, ownership and
// sticky-directory checks do not apply to their config-only validation.
func validateCommandPermissions(string, os.FileInfo) error { return nil }
