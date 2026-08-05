package main

// Decoder.More-as-EOF hardening (#331 slice 3c, external PR review P3):
// json.Decoder.More() returns false at a closing ']' or '}', so using it as
// an end-of-input check silently accepts trailing garbage after the final
// value. Every single-value parser and JSONL loader in the package must
// instead require io.EOF from one more Decode.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trailingCases are the shared suffix variants: a bare ']' or '}' (invisible
// to More), a second value (single-value parsers only), and whitespace
// (which MUST still pass).
func TestParseMixedFixtureRejectsTrailingData(t *testing.T) {
	cases := []struct {
		name, raw string
		wantErr   bool
	}{
		{"clean object", `{}`, false},
		{"trailing whitespace", "{}\n  \n\t", false},
		{"trailing bracket", "{}]", true},
		{"trailing brace", "{}}", true},
		{"second object", "{} {}", true},
		{"trailing garbage", "{}garbage", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMixedFixture([]byte(tc.raw))
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "trailing data")) {
				t.Errorf("err = %v; want a trailing-data error", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("err = %v; want nil", err)
			}
		})
	}
}

func TestJSONLLoadersRejectTrailingData(t *testing.T) {
	art := testCalibrationArtifact("eof-t1", "ans")
	artRaw, err := json.Marshal(art)
	if err != nil {
		t.Fatal(err)
	}
	label := Label{TraceID: "eof-t1", CandidateModel: "m", ArtifactHash: art.ArtifactHash, ExpectedAnswerQuality: 1}
	labelRaw, err := json.Marshal(label)
	if err != nil {
		t.Fatal(err)
	}
	prefRaw := []byte(`{"pair_id":"p","candidate_model":"c","artifact_hash_a":"sha256:a","artifact_hash_b":"sha256:b","preference":"a"}`)
	manifestRaw := []byte(`{"trace_id":"t1","partition":"natural","category":"cat"}`)

	loaders := []struct {
		name string
		row  []byte
		load func(path string) error
	}{
		{"artifacts", artRaw, func(p string) error { _, err := loadArtifacts(p); return err }},
		{"labels", labelRaw, func(p string) error { _, err := loadLabels(p); return err }},
		{"fc-preferences", prefRaw, func(p string) error { _, err := loadFCPreferences(p); return err }},
		{"corpus-manifest", manifestRaw, func(p string) error { _, err := loadManifest(p); return err }},
	}
	suffixes := []struct {
		name, suffix string
		wantErr      bool
	}{
		{"clean row", "", false},
		{"trailing whitespace", "\n   \n", false},
		{"trailing bracket", "]", true},
		{"trailing brace", "}", true},
		{"trailing garbage", "garbage", true},
	}
	dir := t.TempDir()
	for _, ld := range loaders {
		for _, sfx := range suffixes {
			t.Run(ld.name+"/"+sfx.name, func(t *testing.T) {
				path := filepath.Join(dir, ld.name+"-"+strings.ReplaceAll(sfx.name, " ", "-")+".jsonl")
				if err := os.WriteFile(path, append(append([]byte{}, ld.row...), []byte("\n"+sfx.suffix)...), 0o600); err != nil {
					t.Fatal(err)
				}
				// A bare ']' / '}' must produce the trailing-data error; other
				// garbage may fail either as a decode-row error (More() sees it)
				// or as trailing data — loud either way.
				err := ld.load(path)
				if sfx.wantErr && err == nil {
					t.Errorf("trailing %q accepted; want a loud error", sfx.suffix)
				}
				if sfx.wantErr && err != nil && (sfx.suffix == "]" || sfx.suffix == "}") && !strings.Contains(err.Error(), "trailing data") {
					t.Errorf("err = %v; want a trailing-data error for a bare %q", err, sfx.suffix)
				}
				if !sfx.wantErr && err != nil {
					t.Errorf("err = %v; want nil", err)
				}
			})
		}
		t.Run(ld.name+"/second row still parses", func(t *testing.T) {
			if ld.name == "artifacts" || ld.name == "corpus-manifest" {
				t.Skip("duplicate-id loaders reject a repeated row by design; multi-row JSONL is covered by the other loaders")
			}
			path := filepath.Join(dir, ld.name+"-two-rows.jsonl")
			two := append(append([]byte{}, ld.row...), '\n')
			two = append(two, ld.row...)
			two = append(two, '\n')
			if err := os.WriteFile(path, two, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := ld.load(path); err != nil {
				t.Errorf("two clean rows rejected: %v", err)
			}
		})
	}
}
