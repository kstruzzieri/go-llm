//go:build darwin

package fingerprint

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"syscall"
)

// Detect returns the resource profile for macOS.
func Detect() (ResourceProfile, error) {
	rp := ResourceProfile{
		CPUCores: runtime.NumCPU(),
	}

	// Total memory via sysctl hw.memsize.
	// hw.memsize returns a binary-encoded uint64. syscall.Sysctl strips
	// trailing NUL bytes, so we pad back to 8 bytes before decoding.
	memstr, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return rp, fmt.Errorf("fingerprint: sysctl hw.memsize: %w", err)
	}
	raw := make([]byte, 8)
	copy(raw, []byte(memstr))
	rp.TotalMemoryMB = int64(binary.LittleEndian.Uint64(raw)) / (1024 * 1024)

	// GPU detection based on architecture
	if runtime.GOARCH == "arm64" {
		// Apple Silicon: unified memory, GPU always available
		rp.GPUAvailable = true
		rp.GPUMemoryMB = rp.TotalMemoryMB
	}
	// Intel Macs: GPUAvailable stays false (discrete AMD GPUs not usable by Ollama)

	return rp, nil
}
