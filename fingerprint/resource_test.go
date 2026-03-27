package fingerprint

import (
	"testing"
)

func TestParseMeminfoValue(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    int64
		wantErr bool
	}{
		{"MemTotal", "MemTotal:       32768000 kB", 32000, false},
		{"MemAvailable", "MemAvailable:   16384000 kB", 16000, false},
		{"small value", "MemTotal:       1024 kB", 1, false},
		{"invalid", "MemTotal:", 0, true},
		{"non-numeric", "MemTotal:       abc kB", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMeminfoValue(tt.line)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseMeminfoValue(%q) error = %v, wantErr %v", tt.line, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseMeminfoValue(%q) = %d, want %d", tt.line, got, tt.want)
			}
		})
	}
}

func TestParseNvidiaSmiOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   int64
	}{
		{"single GPU", "24576", 24576},
		{"multi GPU first line", "24576\n16384", 24576},
		{"with whitespace", "  24576  \n", 24576},
		{"empty", "", 0},
		{"non-numeric", "error: not found", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseNvidiaSmiOutput(tt.output)
			if got != tt.want {
				t.Errorf("parseNvidiaSmiOutput(%q) = %d, want %d", tt.output, got, tt.want)
			}
		})
	}
}

func TestDetect_ReturnsNonZeroCPU(t *testing.T) {
	rp, err := Detect()
	if err != nil {
		t.Fatalf("Detect() error: %v", err)
	}
	if rp.CPUCores < 1 {
		t.Errorf("CPUCores = %d, want >= 1", rp.CPUCores)
	}
}

func TestDetect_ReturnsNonZeroMemory(t *testing.T) {
	rp, err := Detect()
	if err != nil {
		t.Fatalf("Detect() error: %v", err)
	}
	// On any real machine, total memory should be at least 1 GB
	if rp.TotalMemoryMB < 1024 {
		t.Errorf("TotalMemoryMB = %d, want >= 1024", rp.TotalMemoryMB)
	}
}
