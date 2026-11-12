package main

import (
	"strings"
	"testing"
	"time"
)

func TestCredentialScreenBudgetFlag(t *testing.T) {
	for _, tc := range []struct {
		label   string
		args    []string
		want    time.Duration // checked when non-zero
		wantErr bool
	}{
		{"default", nil, 5 * time.Second, false},
		{"raised", []string{"-credential-screen-budget", "45s"}, 45 * time.Second, false},
		{"too small", []string{"-credential-screen-budget", "1s"}, 0, true},
		{"too large", []string{"-credential-screen-budget", "6m"}, 0, true},
		{"zero", []string{"-credential-screen-budget", "0s"}, 0, true},
		{"unused", []string{"-credential-screen-budget", "30s"}, 30 * time.Second, false},
		{"unused one-shot", []string{"-credential-screen-budget", "30s", "-p", "x"}, 30 * time.Second, false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			f, err := parseFlags(tc.args)
			if err != nil {
				t.Fatalf("%s: parse failed", tc.label)
			}
			if tc.want != 0 && f.screenBudget != tc.want {
				t.Fatalf("%s: screenBudget = %s, want %s", tc.label, f.screenBudget, tc.want)
			}
			err = validateFlags(f)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("%s: unexpected error %q", tc.label, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "-credential-screen-budget") || !strings.Contains(err.Error(), "5s to 5m0s") {
				t.Fatalf("%s: error = %v, want the flag name and the range", tc.label, err)
			}
		})
	}
	t.Run("hand-built", func(t *testing.T) {
		var f flags
		if err := validateFlags(f); err != nil {
			t.Fatalf("hand-built: unexpected error %q", err)
		}
		if f.screenBudgetSet {
			t.Fatal("hand-built: screenBudgetSet is true")
		}
	})
}
