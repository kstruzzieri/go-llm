package main

import "testing"

func TestVSGateDecision(t *testing.T) {
	chain := []string{"ollama/nomic", "ollama/backup"}
	cases := []struct {
		name       string
		known      []string
		hasUnknown bool
		wantReg    bool
		wantKind   vsKind
	}{
		{"match", []string{"ollama/nomic"}, false, true, vsOK},
		{"match-fallback", []string{"ollama/backup"}, false, true, vsOK},
		{"mismatch", []string{"ollama/other"}, false, false, vsMismatch},
		{"mixed-known", []string{"ollama/nomic", "ollama/other"}, false, false, vsInconsistent},
		{"known-plus-legacy", []string{"ollama/nomic"}, true, false, vsInconsistent},
		{"all-legacy", nil, true, true, vsLegacy},
		{"empty", nil, false, true, vsOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := vsGateDecision(tc.known, tc.hasUnknown, chain)
			if got.register != tc.wantReg {
				t.Errorf("register = %v, want %v", got.register, tc.wantReg)
			}
			if got.kind != tc.wantKind {
				t.Errorf("kind = %v, want %v", got.kind, tc.wantKind)
			}
		})
	}
}

func TestVSGateDecision_RecordsStored(t *testing.T) {
	got := vsGateDecision([]string{"ollama/other"}, false, []string{"ollama/nomic"})
	if got.stored != "ollama/other" {
		t.Errorf("stored = %q, want ollama/other", got.stored)
	}
}
