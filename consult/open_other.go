//go:build !unix

package consult

import "os"

// openConfigFile opens the consultants file. Platforms without O_NONBLOCK rely
// on the regular-file and identity checks alone.
func openConfigFile(path string) (*os.File, error) {
	return os.Open(path)
}
