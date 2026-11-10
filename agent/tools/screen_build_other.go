//go:build !linux && !darwin

package tools

import "context"

// buildOutputScreen returns a screen that withholds both streams: this
// platform's walk has no reachability checks to build the screen's go-on walk
// on, so its credential files are not read.
func buildOutputScreen(context.Context, *Workspace) (*outputScreen, error) {
	s := newOutputScreen(nil)
	s.withhold = "no hardened workspace walk on this platform"
	return s, nil
}
