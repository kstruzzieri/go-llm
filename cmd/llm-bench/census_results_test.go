package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/secretscan"
)

var updateCensusGolden = flag.Bool("update-census-golden", false, "rewrite testdata/census-fixture/results.golden.json from a fresh fixture census")

// aggregateFixture is a hand-built census: stratum a has a refused binary, an
// adversarial fixture, an S block over two D tags, a call blocked by S at
// output, a call tag and a search; stratum b has one zero-width tag.
func aggregateFixture() ([]censusObservation, censusManifest) {
	const sum = "cf9f4f8e79a2d5269eef04639b7ab66ad1e47e116d9d5456a720b214ae2cc957"
	m := censusManifest{Schema: censusManifestSchema, Strata: []censusStratum{
		{Name: "a", Source: "test", License: "MIT", Files: []censusFile{
			{Path: "bin.dat", SHA256: sum}, {Path: "card.go", SHA256: sum}, {Path: "clean.go", SHA256: sum},
			{Path: "fix.md", SHA256: sum, Class: censusAdversarial}, {Path: "tag.md", SHA256: sum},
		}},
		{Name: "b", Source: "test", License: "MIT", Files: []censusFile{{Path: "x.go", SHA256: sum}}},
	}}
	weak := censusFindingOf("typoglycemia", "weak_phrase", "tag", "message")
	zw := censusFindingOf("zero_width", "zero_width", "tag", "message")
	obs := func(stratum, carrier, path string, findings ...censusFinding) censusObservation {
		return censusObservation{Stratum: stratum, Path: path, Carrier: carrier, Origin: "workspace", Bytes: 10, Findings: append([]censusFinding{}, findings...)}
	}
	refused := obs("a", "read_file", "bin.dat")
	refused.Bytes, refused.Refused = 0, "binary file"
	mcp := obs("a", "mcp", "tag.md", weak)
	mcp.Origin = "foreign"
	return []censusObservation{
		refused,
		obs("a", "read_file", "card.go", zw, weak, censusFindingOf("secrets", "sensitive_payment_card", "block", "message")),
		obs("a", "read_file", "clean.go"),
		obs("a", "read_file", "fix.md", censusFindingOf("typoglycemia", "instruction_phrase", "tag", "message")),
		obs("a", "read_file", "tag.md", weak),
		obs("a", "run_command", "card.go", censusFindingOf("secrets", "sensitive_payment_card", "block", "output_tool_call")),
		obs("a", "run_command", "tag.md", censusFindingOf("typoglycemia", "weak_phrase", "tag", "tool_call")),
		mcp,
		obs("a", "search", "TODO"),
		obs("b", "read_file", "x.go", zw),
	}, m
}

func marshalResults(t *testing.T, r censusResults) []byte {
	t.Helper()
	b, err := marshalCensusResults(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Review Focus 4, plus the aggregate semantics as hand-computed literals:
// every non-refused, non-fixture observation lands in exactly one outcome or
// in Unobserved; Suppressed counts observations, not tags.
func TestAggregateDeterministic(t *testing.T) {
	obs, m := aggregateFixture()
	r := aggregateCensus(obs, m)
	type c = censusAggregate
	const (
		G, GD, GDS, GS = "golem-default-guards", "guards+detectors", "guards+detectors+secrets", "guards+secrets"
		w, z           = weakTrailerBytes, zwTrailerBytes
	)
	want := []censusAggregate{
		c{Stratum: "a", Carrier: "mcp", Arm: G, Observations: 1, Files: 1},
		c{Stratum: "a", Carrier: "mcp", Arm: GD, Observations: 1, Files: 1, Trailer: 1, TrailerBytes: w},
		c{Stratum: "a", Carrier: "mcp", Arm: GDS, Observations: 1, Files: 1, Trailer: 1, TrailerBytes: w},
		c{Stratum: "a", Carrier: "mcp", Arm: GS, Observations: 1, Files: 1},
		c{Stratum: "a", Carrier: "read_file", Arm: G, Observations: 3, Files: 3, AdversarialFixture: 1, Refused: 1},
		c{Stratum: "a", Carrier: "read_file", Arm: GD, Observations: 3, Files: 3, Trailer: 2, TrailerBytes: w + (z + w), AdversarialFixture: 1, Refused: 1},
		c{Stratum: "a", Carrier: "read_file", Arm: GDS, Observations: 3, Files: 3, Trailer: 1, Blocked: 1, Suppressed: 1, TrailerBytes: w, AdversarialFixture: 1, Refused: 1},
		c{Stratum: "a", Carrier: "read_file", Arm: GS, Observations: 3, Files: 3, Blocked: 1, AdversarialFixture: 1, Refused: 1},
		c{Stratum: "a", Carrier: "run_command", Arm: G, Observations: 1, Files: 1, Unobserved: 1},
		c{Stratum: "a", Carrier: "run_command", Arm: GD, Observations: 1, Files: 1, Unobserved: 1},
		c{Stratum: "a", Carrier: "run_command", Arm: GDS, Observations: 2, Files: 2, CallBlocked: 1},
		c{Stratum: "a", Carrier: "run_command", Arm: GS, Observations: 2, Files: 2, CallBlocked: 1},
		c{Stratum: "a", Carrier: "search", Arm: G, Observations: 1},
		c{Stratum: "a", Carrier: "search", Arm: GD, Observations: 1},
		c{Stratum: "a", Carrier: "search", Arm: GDS, Observations: 1},
		c{Stratum: "a", Carrier: "search", Arm: GS, Observations: 1},
		c{Stratum: "b", Carrier: "read_file", Arm: G, Observations: 1, Files: 1},
		c{Stratum: "b", Carrier: "read_file", Arm: GD, Observations: 1, Files: 1, Trailer: 1, TrailerBytes: z},
		c{Stratum: "b", Carrier: "read_file", Arm: GDS, Observations: 1, Files: 1, Trailer: 1, TrailerBytes: z},
		c{Stratum: "b", Carrier: "read_file", Arm: GS, Observations: 1, Files: 1},
	}
	if !reflect.DeepEqual(r.Aggregates, want) {
		t.Errorf("aggregates:\n got %+v\nwant %+v", r.Aggregates, want)
	}
	var fired []string
	for _, o := range r.Fired {
		fired = append(fired, o.Stratum+"/"+o.Carrier+"/"+o.Path)
	}
	if want := []string{"a/mcp/tag.md", "a/read_file/card.go", "a/read_file/fix.md", "a/read_file/tag.md", "a/run_command/card.go", "a/run_command/tag.md", "b/read_file/x.go"}; !reflect.DeepEqual(fired, want) {
		t.Errorf("fired = %v, want %v", fired, want)
	}
	if r.Schema != "llm-bench.census-results/v1" {
		t.Errorf("schema = %q", r.Schema)
	}

	first := marshalResults(t, r)
	if again := marshalResults(t, aggregateCensus(obs, m)); !bytes.Equal(first, again) {
		t.Error("two aggregations of the same input differ")
	}
	for _, perm := range [][]censusObservation{reversed(obs), append(slices.Clone(obs[4:]), obs[:4]...)} {
		if got := marshalResults(t, aggregateCensus(perm, m)); !bytes.Equal(first, got) {
			t.Errorf("a permuted input aggregates differently:\n%s\nwant\n%s", got, first)
		}
	}
}

func reversed(obs []censusObservation) []censusObservation {
	out := slices.Clone(obs)
	slices.Reverse(out)
	return out
}

// fixtureCensusResults runs the census over the committed fixture and
// aggregates it. The isolated workspace makes every field root-independent.
func fixtureCensusResults(t *testing.T) censusResults {
	t.Helper()
	m, root := censusFixtureCopy(t)
	obs, err := runCensus(t.Context(), m, root)
	if err != nil {
		t.Fatal(err)
	}
	return aggregateCensus(obs, m)
}

// Review Focus 3: the committed results carry findings, never content.
func TestCensusResultsCarryNoContent(t *testing.T) {
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, at string) {
		switch typ.Kind() {
		case reflect.Slice, reflect.Pointer:
			walk(typ.Elem(), at)
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				if f.Name == "Content" {
					t.Errorf("%s.%s holds observation text", at, f.Name)
				}
				walk(f.Type, at+"."+f.Name)
			}
		}
	}
	walk(reflect.TypeFor[censusResults](), "censusResults")

	if got := secretscan.Scan(string(marshalResults(t, fixtureCensusResults(t)))); len(got) != 0 {
		t.Errorf("fixture census results carry secret-shaped bytes: %+v", got)
	}
	// R19: the digests are identifiers that can hold a Luhn-valid digit run;
	// they are blanked before the committed file is scanned.
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "llm", "census", "census-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var committed censusResults
	if err := json.Unmarshal(raw, &committed); err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, d := range []string{committed.ManifestSHA256, committed.FullSHA256} {
		if len(d) != 64 {
			t.Fatalf("committed digest %q is not a sha256", d)
		}
		text = strings.ReplaceAll(text, d, "")
	}
	if got := secretscan.Scan(text); len(got) != 0 {
		t.Errorf("docs/llm/census/census-results.json carries secret-shaped bytes: %+v", got)
	}
	// Preserve the v1 committed format's zero verifier byte counts.
	for _, o := range committed.Fired {
		if o.Carrier == "verifier" && o.Bytes != 0 {
			t.Errorf("docs/llm/census/census-results.json: verifier row %s %s has bytes %d, want 0; rerun the census", o.Stratum, o.Path, o.Bytes)
		}
	}
}

// Review Focus 4: two fixture regenerations are byte-identical and equal the
// committed golden.
func TestCensusResultsRegen(t *testing.T) {
	got := marshalResults(t, fixtureCensusResults(t))
	if again := marshalResults(t, fixtureCensusResults(t)); !bytes.Equal(got, again) {
		t.Fatal("two fixture census regenerations differ")
	}
	golden := filepath.Join("testdata", "census-fixture", "results.golden.json")
	if *updateCensusGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("results.golden.json: rebuilt bytes differ from the committed file; regenerate with go test ./cmd/llm-bench -run TestCensusResultsRegen -update-census-golden")
	}
}

// The mode writes the results and the full per-observation output, and binds
// both digests: the manifest's bytes and the JSONL's bytes.
func TestCensusModeWritesOutputs(t *testing.T) {
	_, root := censusFixtureCopy(t)
	out := filepath.Join(t.TempDir(), "census")
	var log bytes.Buffer
	if err := runCensusMode(t.Context(), filepath.Join(root, "manifest.json"), root, out, &log); err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(filepath.Join(out, "full", "observations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "census-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r censusResults
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	digest := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	if r.ManifestSHA256 != digest(manifest) || r.FullSHA256 != digest(full) {
		t.Errorf("digests manifest/full = %s/%s, want %s/%s", r.ManifestSHA256, r.FullSHA256, digest(manifest), digest(full))
	}
	lines := strings.Split(strings.TrimSuffix(string(full), "\n"), "\n")
	if len(lines) != 62 || !strings.HasSuffix(string(full), "\n") {
		t.Fatalf("observations.jsonl lines = %d, want 62, newline-terminated", len(lines))
	}
	for i, l := range lines {
		var o censusObservation
		if err := json.Unmarshal([]byte(l), &o); err != nil || o.Stratum != "fixture" {
			t.Errorf("line %d = %q: %v", i+1, l, err)
		}
	}
	if len(r.Aggregates) != 7*4 || len(r.Fired) == 0 {
		t.Errorf("results hold %d aggregates (want 7 carriers x 4 arms) and %d fired rows", len(r.Aggregates), len(r.Fired))
	}
	if !strings.Contains(log.String(), "62 observation(s)") {
		t.Errorf("summary = %q", log.String())
	}
	// The committed rows carry no root-dependent bytes: a verifier row's
	// bytes, which the full output keeps, are 0 in fired; every other fired
	// row keeps its bytes.
	fullBytes := map[[2]string]int{}
	for _, l := range lines {
		var o censusObservation
		if err := json.Unmarshal([]byte(l), &o); err == nil {
			fullBytes[[2]string{o.Carrier, o.Path}] = o.Bytes
		}
	}
	verifiers := 0
	for _, o := range r.Fired {
		full := fullBytes[[2]string{o.Carrier, o.Path}]
		switch {
		case o.Carrier == "verifier":
			verifiers++
			if o.Bytes != 0 || full == 0 {
				t.Errorf("verifier %s: fired bytes %d, full bytes %d; want 0 in fired and the rendering's length in full", o.Path, o.Bytes, full)
			}
		case o.Bytes != full:
			t.Errorf("%s %s: fired bytes %d, want the full output's %d", o.Carrier, o.Path, o.Bytes, full)
		}
	}
	if verifiers == 0 {
		t.Error("no fired verifier row: the fixture no longer exercises the zeroing")
	}
}

// The -census-* flags without -interceptor-census are refused, as the
// -attempt-* flags are without -replay-attempt, instead of being ignored. The
// end-to-end refusal is TestStrayCensusFlagsRefusedByMain (census_flags_main_test.go).
func TestStrayCensusFlags(t *testing.T) {
	got := strayCensusFlags(map[string]bool{"census-out": true, "models": true, "census-root": true, "census-manifest": true})
	if want := []string{"-census-manifest", "-census-root", "-census-out"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("strayCensusFlags = %v, want %v", got, want)
	}
	if got := strayCensusFlags(map[string]bool{"interceptor-census": true, "models": true}); len(got) != 0 {
		t.Fatalf("strayCensusFlags = %v, want none", got)
	}
}
