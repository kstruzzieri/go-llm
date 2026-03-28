//go:build !darwin && !linux

package fingerprint

import "runtime"

// Detect returns a minimal resource profile for unsupported platforms.
func Detect() (ResourceProfile, error) {
	return ResourceProfile{
		CPUCores: runtime.NumCPU(),
	}, nil
}
