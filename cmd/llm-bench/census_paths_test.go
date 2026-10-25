package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestCensusLeadingDashPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the census requires the POSIX exec runner")
	}
	for _, path := range []string{"-n", "-"} {
		t.Run(path, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			root := t.TempDir()
			dir := filepath.Join(root, "fixture")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			content := []byte("A document about the system prompt.\n")
			if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(content)
			m := censusManifest{Schema: censusManifestSchema, Strata: []censusStratum{{
				Name: "fixture", Source: "synthetic", License: "MIT",
				Files: []censusFile{{Path: path, SHA256: hex.EncodeToString(sum[:])}},
			}}}
			if err := m.validate(); err != nil {
				t.Fatal(err)
			}
			obs, err := runCensus(t.Context(), m, root)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"typoglycemia/weak_phrase/tag/message"}
			for _, carrier := range []string{"run_command", "command_tail", "verifier"} {
				o := findCensusObservation(t, obs, carrier, path)
				if got := censusFindingKeys(o); o.Refused != "" || !slices.Equal(got, want) {
					t.Errorf("runCensus(%q) %s refused/findings = %q/%v, want no refusal and %v", path, carrier, o.Refused, got, want)
				}
			}
		})
	}
}

func TestCensusHandleFindings(t *testing.T) {
	for _, tc := range []struct {
		name, handle string
		want         bool
	}{
		{"clean", "bg-" + strings.Repeat("a", 32), false},
		{"encoded zero-width", "bg-" + hex.EncodeToString([]byte("\u00ad"+strings.Repeat("a", 14))), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := censusHandleHasFindings(t.Context(), tc.handle)
			if err != nil || got != tc.want {
				t.Errorf("censusHandleHasFindings(%q) = %t, %v; want %t, nil", tc.handle, got, err, tc.want)
			}
		})
	}
}
