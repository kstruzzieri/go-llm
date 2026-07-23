package fingerprint

import (
	"fmt"
	"strconv"
	"strings"
)

// ResourceProfile describes the system resources available for model inference.
type ResourceProfile struct {
	TotalMemoryMB     int64
	AvailableMemoryMB int64
	CPUCores          int
	GPUAvailable      bool
	GPUMemoryMB       int64
}

// parseMeminfoValue parses a value like "MemTotal:       32768000 kB" and
// returns the value in megabytes. Used by the Linux implementation.
func parseMeminfoValue(line string) (int64, error) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0, fmt.Errorf("fingerprint: invalid meminfo line: %q", line)
	}
	kb, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("fingerprint: parse meminfo value: %w", err)
	}
	return kb / 1024, nil
}

// parseNvidiaSmiOutput parses the output of nvidia-smi --query-gpu=memory.total
// and returns GPU memory in MB. Returns 0 for empty or unparseable output.
func parseNvidiaSmiOutput(output string) int64 {
	output = strings.TrimSpace(output)
	if output == "" {
		return 0
	}
	// nvidia-smi outputs one line per GPU; take the first.
	line := strings.SplitN(output, "\n", 2)[0]
	line = strings.TrimSpace(line)
	// Output is in MiB by default
	mb, err := strconv.ParseInt(line, 10, 64)
	if err != nil {
		return 0
	}
	return mb
}
