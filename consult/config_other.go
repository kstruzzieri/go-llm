//go:build !unix

package consult

import "os"

// The runner is unsupported on non-Unix platforms; Unix ownership and sticky
// directory semantics do not apply to their config-only validation.
func validateCommandParents(string, os.FileInfo) error { return nil }
