package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// censusFixtureCopy copies the committed fixture into a temp dir, so the
// verifier carrier's scratch write never touches testdata, and isolates the
// MCP trust pins the loopback connection writes.
func censusFixtureCopy(t *testing.T) (censusManifest, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the census refuses windows (TestCensusRefusesWindows)")
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "census")
	if err := os.CopyFS(root, os.DirFS(filepath.Join("testdata", "census-fixture"))); err != nil {
		t.Fatal(err)
	}
	m, _, err := loadCensusManifest(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m, root
}

func runFixtureCensus(t *testing.T) []censusObservation {
	t.Helper()
	m, root := censusFixtureCopy(t)
	obs, err := runCensus(t.Context(), m, root)
	if err != nil {
		t.Fatal(err)
	}
	return obs
}

func findCensusObservation(t *testing.T, obs []censusObservation, carrier, path string) censusObservation {
	t.Helper()
	var hits []censusObservation
	for _, o := range obs {
		if o.Stratum == "fixture" && o.Carrier == carrier && o.Path == path {
			hits = append(hits, o)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("census observations for %s %s = %d, want 1", carrier, path, len(hits))
	}
	return hits[0]
}

func censusFindingKeys(o censusObservation) []string {
	keys := []string{}
	for _, f := range o.Findings {
		keys = append(keys, f.Interceptor+"/"+f.Rule+"/"+f.Verdict+"/"+f.Target)
	}
	return keys
}

// The fixture has 9 files and the search carrier runs the 8 registered
// queries; MCP is the only foreign carrier in production (mcpclient/adapter.go:49).
func TestCensusCarrierOrigins(t *testing.T) {
	obs := runFixtureCensus(t)
	counts := map[string]int{}
	for _, o := range obs {
		want := "workspace"
		if o.Carrier == "mcp" {
			want = "foreign"
		}
		if o.Origin != want {
			t.Errorf("%s %s origin = %q, want %q", o.Carrier, o.Path, o.Origin, want)
		}
		counts[o.Carrier]++
	}
	want := map[string]int{"read_file": 9, "search": 8, "run_command": 9, "command_tail": 9, "verifier": 9, "mcp": 9, "mixed": 9}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("observations per carrier = %v, want %v", counts, want)
	}
	if len(obs) != 62 {
		t.Errorf("observations = %d, want 62", len(obs))
	}
}

// Expectations are spec facts, emitter names read from the interceptor code
// (Ruling R4): F7 tags strong phrases and zero-width at workspace origin and
// blocks them at foreign origin, tags weak phrases everywhere; F8 counts the
// ZWJ and the BOM as zero-width; Secrets blocks a Luhn-valid run everywhere.
func TestCensusFindingsByCarrier(t *testing.T) {
	obs := runFixtureCensus(t)
	workspace := []string{"read_file", "run_command", "command_tail", "verifier"}
	for _, tc := range []struct {
		path               string
		workspace, foreign []string
	}{
		{"emoji.md", []string{"zero_width/zero_width/tag/message"}, []string{"zero_width/zero_width/block/message"}},
		{"bom.txt", []string{"zero_width/zero_width/tag/message"}, []string{"zero_width/zero_width/block/message"}},
		{"weak.md", []string{"typoglycemia/weak_phrase/tag/message"}, []string{"typoglycemia/weak_phrase/tag/message"}},
		{"strong.md", []string{"typoglycemia/instruction_phrase/tag/message"}, []string{"typoglycemia/instruction_phrase/block/message"}},
		{"constant.go", []string{"secrets/sensitive_payment_card/block/message"}, []string{"secrets/sensitive_payment_card/block/message"}},
		{"clean.go", []string{}, []string{}},
	} {
		for _, carrier := range workspace {
			if got := censusFindingKeys(findCensusObservation(t, obs, carrier, tc.path)); !reflect.DeepEqual(got, tc.workspace) {
				t.Errorf("%s %s findings = %v, want %v", carrier, tc.path, got, tc.workspace)
			}
		}
		if got := censusFindingKeys(findCensusObservation(t, obs, "mcp", tc.path)); !reflect.DeepEqual(got, tc.foreign) {
			t.Errorf("mcp %s findings = %v, want %v", tc.path, got, tc.foreign)
		}
	}
	if got := censusFindingKeys(findCensusObservation(t, obs, "mixed", "clean.go")); len(got) != 0 {
		t.Errorf("mixed clean.go findings = %v, want none", got)
	}
	// The read_file credential-path invariant (a guard, not a detector) blocks
	// the call itself, so nothing is inspected; exec and MCP read the bytes.
	for _, carrier := range []string{"read_file", "mixed"} {
		o := findCensusObservation(t, obs, carrier, ".npmrc")
		if got, want := censusFindingKeys(o), []string{"invariants/credential_path/block/tool_call"}; !reflect.DeepEqual(got, want) || o.Bytes != 0 || o.Refused != "" {
			t.Errorf("%s .npmrc findings/bytes/refused = %v/%d/%q, want %v/0/\"\"", carrier, got, o.Bytes, o.Refused, want)
		}
	}
	for _, carrier := range []string{"run_command", "command_tail", "verifier", "mcp"} {
		if o := findCensusObservation(t, obs, carrier, ".npmrc"); len(o.Findings) != 0 || o.Bytes == 0 {
			t.Errorf("%s .npmrc findings/bytes = %v/%d, want none/the rendered bytes", carrier, censusFindingKeys(o), o.Bytes)
		}
	}
}

// Review Focus 1: a refusal is a non-observation, and the cap is observed.
func TestCensusRecordsRefusals(t *testing.T) {
	obs := runFixtureCensus(t)
	// The binary file's name carries the weak phrase, so the read_file call
	// itself is tagged; a refusal still reports no findings at all.
	for _, carrier := range []string{"read_file", "mixed"} {
		o := findCensusObservation(t, obs, carrier, "system_prompt.bin")
		if o.Refused == "" || len(o.Findings) != 0 || o.Bytes != 0 {
			t.Errorf("%s system_prompt.bin refused/findings/bytes = %q/%v/%d, want a refusal with no findings and no bytes", carrier, o.Refused, censusFindingKeys(o), o.Bytes)
		}
	}
	// run_command renders binary output; only read_file refuses it. The
	// detectors scan tool-call arguments too: the tag is on the call.
	o := findCensusObservation(t, obs, "run_command", "system_prompt.bin")
	if got, want := censusFindingKeys(o), []string{"typoglycemia/weak_phrase/tag/tool_call"}; o.Refused != "" || !reflect.DeepEqual(got, want) {
		t.Errorf("run_command system_prompt.bin refused/findings = %q/%v, want \"\"/%v", o.Refused, got, want)
	}
	o = findCensusObservation(t, obs, "read_file", "large.txt")
	if want := 256*1024 + len("\n[truncated after 262144 bytes]"); o.Bytes != want || o.Refused != "" {
		t.Errorf("read_file large.txt bytes/refused = %d/%q, want %d/\"\"", o.Bytes, o.Refused, want)
	}
}

// A path carrying a Luhn-valid run makes the scripted tool call itself
// secret-shaped: Secrets rejects the model response at output (F9), the tool
// never runs, and the observation holds that block alone. The verifier's
// command is not a model call, but its output echoes the command line. The
// run is reused from constant.go so no second lookalike is committed.
func TestCensusRecordsTerminalCallBlocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the census refuses windows (TestCensusRefusesWindows)")
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	src, err := os.ReadFile(filepath.Join("testdata", "census-fixture", "fixture", "constant.go"))
	if err != nil {
		t.Fatal(err)
	}
	name := "order-" + regexp.MustCompile(`[0-9]{19}`).FindString(string(src)) + ".txt"
	root := t.TempDir()
	content := []byte("plain text\n")
	if err := os.Mkdir(filepath.Join(root, "calls"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "calls", name), content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	m := censusManifest{Schema: censusManifestSchema, Strata: []censusStratum{{Name: "calls", Source: "test", License: "Apache-2.0", Files: []censusFile{{Path: name, SHA256: hex.EncodeToString(sum[:])}}}}}
	obs, err := runCensus(t.Context(), m, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.Carrier == "search" {
			continue
		}
		want := []string{"secrets/sensitive_payment_card/block/output_tool_call"}
		if o.Carrier == "verifier" {
			want = []string{"secrets/sensitive_payment_card/block/message"}
		}
		if got := censusFindingKeys(o); !reflect.DeepEqual(got, want) || o.Refused != "" || (o.Carrier != "verifier" && o.Bytes != 0) {
			t.Errorf("%s findings/bytes/refused = %v/%d/%q, want %v, no refusal, and no bytes unless verifier", o.Carrier, got, o.Bytes, o.Refused, want)
		}
	}
	if len(obs) != 14 {
		t.Errorf("observations = %d, want 6 file carriers and 8 queries", len(obs))
	}
}

// Review Focus 2: the census refuses a tree that drifted from its manifest.
func TestCensusRejectsManifestDrift(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		mutate     func(t *testing.T, p string)
	}{
		{"changed byte", "weak.md", func(t *testing.T, p string) {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			data[0] ^= 1
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing file", "clean.go", func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}},
		{"unlisted file", "extra.txt", func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte("not in the manifest\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, root := censusFixtureCopy(t)
			tc.mutate(t, filepath.Join(root, "fixture", tc.path))
			obs, err := runCensus(t.Context(), m, root)
			if err == nil || !strings.Contains(err.Error(), "fixture: "+tc.path+":") || obs != nil {
				t.Errorf("runCensus(%s) = %d observations, error %v; want an error naming fixture: %s", tc.name, len(obs), err, tc.path)
			}
		})
	}
}

// A finding confined to one alternative of the registered ContextSet is
// reported on that alternative only: weak.md's phrase is on its ninth line,
// so the line thirds put it in alternative 2, and the fallback (alternative
// 0's text) carries nothing.
func TestCensusMixedAlternativeOnly(t *testing.T) {
	obs := runFixtureCensus(t)
	o := findCensusObservation(t, obs, "mixed", "weak.md")
	want := []censusFinding{{Interceptor: "typoglycemia", Rule: "weak_phrase", Verdict: "tag", Target: "alternative", Group: 0, Alt: 2}}
	if !reflect.DeepEqual(o.Findings, want) {
		t.Errorf("mixed weak.md findings = %+v, want %+v", o.Findings, want)
	}
}

func TestCensusRefusesWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the census runs here; it refuses windows only")
	}
	_, err := runCensus(t.Context(), censusManifest{}, t.TempDir())
	if err == nil || err.Error() != "census: unsupported on windows: the run_command, command_tail and verifier carriers need the POSIX exec runner" {
		t.Errorf("runCensus on windows = %v", err)
	}
}

func TestCensusManifestStrict(t *testing.T) {
	const sum = "cf9f4f8e79a2d5269eef04639b7ab66ad1e47e116d9d5456a720b214ae2cc957"
	stratum := func(files string) string {
		return `{"schema":"llm-bench.census-manifest/v1","strata":[{"name":"s","source":"x","commit":"","license":"MIT","files":[` + files + `]}]}`
	}
	file := func(path string) string { return fmt.Sprintf(`{"path":%q,"sha256":%q}`, path, sum) }
	for _, tc := range []struct{ name, body, want string }{
		{"unknown field", strings.Replace(stratum(file("a")), `"license"`, `"extra":1,"license"`, 1), "unknown field"},
		{"wrong schema", strings.Replace(stratum(file("a")), "census-manifest/v1", "census-manifest/v2", 1), "schema"},
		{"trailing data", stratum(file("a")) + "{}", "trailing"},
		{"unsorted paths", stratum(file("b") + "," + file("a")), "ascending"},
		{"duplicate path", stratum(file("a") + "," + file("a")), "ascending"},
		{"escaping path", stratum(file("../a")), "invalid path"},
		{"bad sha256", stratum(`{"path":"a","sha256":"ABC"}`), "sha256"},
		{"unknown class", stratum(`{"path":"a","sha256":"` + sum + `","class":"benign"}`), "class"},
		{"reserved path", stratum(file(".census-verify-scratch")), "reserved"},
		{"no files", stratum(""), "no files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadCensusManifest(p); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("loadCensusManifest(%s) = %v, want an error containing %q", tc.name, err, tc.want)
			}
		})
	}
	m, sha, err := loadCensusManifest(filepath.Join("testdata", "census-fixture", "manifest.json"))
	if err != nil || len(m.Strata) != 1 || len(m.Strata[0].Files) != 9 || len(sha) != 64 {
		t.Errorf("loadCensusManifest(fixture) = %d strata, sha %q, error %v", len(m.Strata), sha, err)
	}
}
