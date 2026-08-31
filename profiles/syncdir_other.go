//go:build !windows

package profiles

import "os"

// syncDir fsyncs a directory so a just-created entry is durable. Mirrors
// config's syncDirectory pair — kept in lockstep.
func syncDir(dir string) error {
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
