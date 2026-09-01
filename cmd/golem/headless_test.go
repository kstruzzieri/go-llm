package main

import (
	"strings"
	"testing"
)

func TestParseOutputFormat(t *testing.T) {
	cases := []struct {
		in      string
		want    outputFormat
		wantErr bool
	}{
		{"", outputText, false},
		{"text", outputText, false},
		{"json", outputJSON, false},
		{"stream-json", outputStreamJSON, false},
		{"TEXT", 0, true}, // exact match only
		{"jsonl", 0, true},
		{"stream_json", 0, true}, // underscore is not the spelling
		{" json", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseOutputFormat(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseOutputFormat(%q) = %v, want error", tc.in, got)
				}
				for _, want := range []string{"text", "json", "stream-json"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q must list the accepted value %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOutputFormat(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseOutputFormat(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestOutputFormatMachineReportsOnlyJSONModes(t *testing.T) {
	if outputText.machine() {
		t.Error("text is not a machine format")
	}
	if !outputJSON.machine() || !outputStreamJSON.machine() {
		t.Error("json and stream-json are machine formats")
	}
}

func TestValidateFlagsOutputFormatRequiresOneShot(t *testing.T) {
	err := validateFlags(flags{outputFormat: "json"})
	if err == nil {
		t.Fatal("-output-format without -p must be rejected")
	}
	if exitCodeFor(err) != 1 {
		t.Errorf("the requires-p rejection is a mode error, not a headless usage error: exit %d, want 1", exitCodeFor(err))
	}
	if err := validateFlags(flags{outputFormat: "json", promptSet: true, prompt: "hi"}); err != nil {
		t.Fatalf("-output-format with -p must be accepted: %v", err)
	}
	if err := validateFlags(flags{outputFormat: "bogus", promptSet: true, prompt: "hi"}); err == nil {
		t.Fatal("unknown -output-format must be rejected")
	}
	// The default (unset) must stay valid in every mode, including the REPL.
	if err := validateFlags(flags{}); err != nil {
		t.Fatalf("default flags must stay valid: %v", err)
	}
	if err := validateFlags(flags{outputFormat: "text"}); err != nil {
		t.Fatalf("an explicit text format is valid without -p (it is the default behavior): %v", err)
	}
}
