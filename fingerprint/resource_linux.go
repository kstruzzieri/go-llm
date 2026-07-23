//go:build linux

package fingerprint

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Detect returns the resource profile for Linux.
func Detect() (ResourceProfile, error) {
	rp := ResourceProfile{
		CPUCores: runtime.NumCPU(),
	}

	// Parse /proc/meminfo
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return rp, fmt.Errorf("fingerprint: open /proc/meminfo: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			if v, err := parseMeminfoValue(line); err == nil {
				rp.TotalMemoryMB = v
			}
		} else if strings.HasPrefix(line, "MemAvailable:") {
			if v, err := parseMeminfoValue(line); err == nil {
				rp.AvailableMemoryMB = v
			}
		}
	}

	// GPU detection via nvidia-smi
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader,nounits").Output()
	if err == nil {
		gpuMem := parseNvidiaSmiOutput(string(out))
		if gpuMem > 0 {
			rp.GPUAvailable = true
			rp.GPUMemoryMB = gpuMem
		}
	}

	return rp, nil
}
