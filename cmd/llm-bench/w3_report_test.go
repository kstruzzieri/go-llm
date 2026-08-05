package main

// W3 round-2 consult adoptions (#331 slice 3c): sealed forced-choice side
// map, arm-guess blinding audit, capture-manifest report verification, and
// worksheet model filters.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var testFCSidemapDigest = "sha256:" + strings.Repeat("a", 64)

func testFCSidemapWithDigest(m fcSidemapFile, digest string) fcSidemapFile {
	m.verifiedDigest = digest
	return m
}

// parityFCSidemap builds a sidemap whose assignments EQUAL the legacy parity
// rule for the given (pair, model) keys, so fixtures built around
// fcSideIsLegacyA (fcRowFor, fcPairArtifacts) stay bindable while the code
// under test resolves strictly through the map. W5: entries carry the arm
// hashes in A/B order, derived from arts (the sidemap is the sidecar's only
// hash carrier). An empty pairIDs list covers every complete pair in arts.
func parityFCSidemap(arts []Artifact, model string, pairIDs ...string) fcSidemapFile {
	want := map[string]bool{}
	for _, p := range pairIDs {
		want[p] = true
	}
	pairs, err := collectFCPairs(arts)
	if err != nil {
		panic("parityFCSidemap: " + err.Error())
	}
	m := fcSidemapFile{SchemaVersion: fcSidemapSchemaVersion, Pairs: map[string]fcSidemapEntry{}}
	for _, p := range pairs {
		if p.model != model || (len(pairIDs) > 0 && !want[p.pairID]) {
			continue
		}
		legacyIsA := fcSideIsLegacyA(p.pairID, p.model)
		hashA, hashB := p.legacy.ArtifactHash, p.mixed.ArtifactHash
		if !legacyIsA {
			hashA, hashB = hashB, hashA
		}
		m.Pairs[fcSidemapKey(p.pairID, p.model)] = fcSidemapEntry{LegacyIsA: legacyIsA, HashA: hashA, HashB: hashB}
	}
	return testFCSidemapWithDigest(m, testFCSidemapDigest)
}

func TestFCSidemapGenerateAndLoad(t *testing.T) {
	arts := fcPairArtifacts()
	m, err := generateFCSidemap(arts)
	if err != nil {
		t.Fatalf("generateFCSidemap: %v", err)
	}
	if m.SchemaVersion != "fc-sidemap/v2" {
		t.Errorf("schema_version = %q; want fc-sidemap/v2", m.SchemaVersion)
	}
	if m.SealedDigest != "" {
		t.Errorf("sealed_digest = %q; want empty (the seal is the printed file digest)", m.SealedDigest)
	}
	for _, key := range []string{"pair-alpha|c", "pair-gamma|c"} {
		if _, ok := m.Pairs[key]; !ok {
			t.Errorf("sidemap missing key %q; got %v", key, m.Pairs)
		}
	}
	if len(m.Pairs) != 2 {
		t.Errorf("sidemap pairs = %d; want exactly the 2 complete pairs", len(m.Pairs))
	}
	// W5: entries carry the arm hashes in A/B order — the assignment boolean
	// and the hash order must agree, whichever way the coin landed.
	armHashes := map[string][2]string{ // pair key -> {legacy, mixed}
		"pair-alpha|c": {"sha256:al", "sha256:am"},
		"pair-gamma|c": {"sha256:gl", "sha256:gm"},
	}
	for key, hashes := range armHashes {
		e := m.Pairs[key]
		wantA, wantB := hashes[0], hashes[1]
		if !e.LegacyIsA {
			wantA, wantB = wantB, wantA
		}
		if e.HashA != wantA || e.HashB != wantB {
			t.Errorf("entry %q hashes = %q/%q; want %q/%q under legacy_is_a=%t", key, e.HashA, e.HashB, wantA, wantB, e.LegacyIsA)
		}
	}

	path := filepath.Join(t.TempDir(), "sidemap.json")
	digest, err := writeFCSidemap(path, m)
	if err != nil {
		t.Fatalf("writeFCSidemap: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if digest != hex.EncodeToString(sum[:]) {
		t.Errorf("printed digest %q does not match the file bytes %q", digest, hex.EncodeToString(sum[:]))
	}

	loaded, resolver, loadedDigest, err := loadFCSidemap(path)
	if err != nil {
		t.Fatalf("loadFCSidemap: %v", err)
	}
	if loadedDigest != digest {
		t.Errorf("load digest %q != write digest %q", loadedDigest, digest)
	}
	for key, want := range m.Pairs {
		if loaded.Pairs[key] != want {
			t.Errorf("round-trip entry %q = %v; want %v", key, loaded.Pairs[key], want)
		}
	}
	if _, err := resolver("pair-alpha", "c"); err != nil {
		t.Errorf("resolver on a present pair: %v", err)
	}
	if _, err := resolver("pair-unknown", "c"); err == nil || !strings.Contains(err.Error(), "pair-unknown") {
		t.Errorf("resolver on a missing pair = %v; want a loud error naming it", err)
	}

	t.Run("no complete pairs is a loud error", func(t *testing.T) {
		if _, err := generateFCSidemap(nil); err == nil {
			t.Error("generateFCSidemap(nil) accepted; want error")
		}
	})
	t.Run("wrong schema version rejected at load", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(bad, []byte(`{"schema_version":"fc-sidemap/v0","pairs":{"p|c":{"legacy_is_a":true}},"sealed_digest":""}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := loadFCSidemap(bad); err == nil || !strings.Contains(err.Error(), "schema_version") {
			t.Errorf("wrong schema version = %v; want a schema error", err)
		}
	})
	t.Run("hash-free entry rejected at load", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "hashless.json")
		body := `{"schema_version":"fc-sidemap/v2","pairs":{"p|c":{"legacy_is_a":true}},"sealed_digest":""}`
		if err := os.WriteFile(bad, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := loadFCSidemap(bad); err == nil || !strings.Contains(err.Error(), "hash_a") {
			t.Errorf("hash-free v2 entry = %v; want a missing-hash error", err)
		}
	})
}

func TestFCSidemapDigestVerification(t *testing.T) {
	digest := strings.Repeat("a", 64)
	if err := verifyFCSidemapDigest(digest, digest); err != nil {
		t.Errorf("matching digest rejected: %v", err)
	}
	if err := verifyFCSidemapDigest(digest, "sha256:"+strings.ToUpper(digest)); err != nil {
		t.Errorf("case/prefix-tolerant match rejected: %v", err)
	}
	if err := verifyFCSidemapDigest(digest, strings.Repeat("b", 64)); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("mismatched digest = %v; want a hard mismatch error", err)
	}
	secret := "sk-FAKE-SIDEMAP-SECRET-0000"
	if err := verifyFCSidemapDigest(digest, secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("invalid committed digest error = %v; want a non-echoing error", err)
	}
}

// TestFCSidemapDigestVerificationEndToEnd drives -assembly-report's digest
// gate through runAssemblyReport in both directions.
func TestFCSidemapDigestVerificationEndToEnd(t *testing.T) {
	dir := t.TempDir()
	arts, labels := fcReportFixture()
	artsPath := filepath.Join(dir, "artifacts.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")
	if err := writeJSONLRows(artsPath, "artifacts", arts); err != nil {
		t.Fatal(err)
	}
	if err := writeLabelsJSONL(labelsPath, labels); err != nil {
		t.Fatal(err)
	}
	sidemapPath := filepath.Join(dir, "sidemap.json")
	digest, err := writeFCSidemap(sidemapPath, parityFCSidemap(arts, "c"))
	if err != nil {
		t.Fatal(err)
	}

	opts := assemblyReportOptions{
		LabelsPath: labelsPath, ArtifactsPath: artsPath,
		FCSidemapPath: sidemapPath, FCSidemapDigest: digest,
	}
	if _, err := runAssemblyReport(opts); err != nil {
		t.Fatalf("matching committed digest rejected: %v", err)
	}
	opts.FCSidemapDigest = ""
	if _, err := runAssemblyReport(opts); err == nil || !strings.Contains(err.Error(), "requires -fc-sidemap-digest") {
		t.Fatalf("sidemap without committed digest = %v; want a loud requirement error", err)
	}
	opts.FCSidemapDigest = strings.Repeat("0", 64)
	if _, err := runAssemblyReport(opts); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("wrong committed digest = %v; want a hard error", err)
	}

	// A committed digest with no sidemap to verify is operator confusion, not
	// a silent no-op.
	opts.FCSidemapPath = ""
	opts.FCSidemapDigest = digest
	if _, err := runAssemblyReport(opts); err == nil || !strings.Contains(err.Error(), "without -fc-sidemap") {
		t.Fatalf("digest without sidemap = %v; want a loud error", err)
	}

	// Historical reports that provide no sidemap retain the registered parity
	// resolver. A sidecar produced in parity order must still report cleanly.
	prefsPath := filepath.Join(dir, "preferences.jsonl")
	if err := writeJSONLRows(prefsPath, "fc-preferences", []FCPreference{fcRowFor(arts, "pair-alpha", "a")}); err != nil {
		t.Fatal(err)
	}
	opts = assemblyReportOptions{LabelsPath: labelsPath, ArtifactsPath: artsPath, FCPrefsPath: prefsPath}
	if _, err := runAssemblyReport(opts); err != nil {
		t.Fatalf("historical no-sidemap parity report: %v", err)
	}

	// The same row cannot be reinterpreted under a different, independently
	// valid map: its existing A/B artifact hashes bind it to the original map.
	flipped := parityFCSidemap(arts, "c")
	e := flipped.Pairs["pair-alpha|c"]
	e.LegacyIsA = !e.LegacyIsA
	e.HashA, e.HashB = e.HashB, e.HashA
	flipped.Pairs["pair-alpha|c"] = e
	flippedPath := filepath.Join(dir, "flipped-sidemap.json")
	flippedDigest, err := writeFCSidemap(flippedPath, flipped)
	if err != nil {
		t.Fatal(err)
	}
	opts.FCSidemapPath, opts.FCSidemapDigest = flippedPath, flippedDigest
	if _, err := runAssemblyReport(opts); err == nil || !strings.Contains(err.Error(), "registered side assignment") {
		t.Fatalf("preference row accepted under a different sidemap = %v; want hash-order rejection", err)
	}
}

func TestForcedChoiceRequiresVerifiedDigestAndWorksheetBinding(t *testing.T) {
	arts := fcPairArtifacts()
	verified := parityFCSidemap(arts, "c")
	unverified := fcSidemapFile{
		SchemaVersion: verified.SchemaVersion,
		Pairs:         verified.Pairs,
		SealedDigest:  verified.SealedDigest,
	}

	t.Run("render function rejects an unverified map", func(t *testing.T) {
		if _, err := renderForcedChoiceWorksheet(arts, unverified); err == nil || !strings.Contains(err.Error(), "fc-sidemap-digest") {
			t.Fatalf("render with no committed digest = %v; want a loud requirement error", err)
		}
	})

	worksheet, err := renderForcedChoiceWorksheet(arts, verified)
	if err != nil {
		t.Fatalf("render verified worksheet: %v", err)
	}
	metadata := "# fc-sidemap-digest: " + testFCSidemapDigest
	if got := strings.Count(worksheet, metadata+"\n"); got != 1 {
		t.Fatalf("worksheet digest metadata count = %d; want exactly one\n%s", got, worksheet)
	}
	filled := fillWorksheetField(t, worksheet, "=== PAIR pair-alpha c ===", "prefer", "A")

	t.Run("ingest function rejects an unverified map", func(t *testing.T) {
		if _, _, err := ingestForcedChoiceWorksheet(filled, arts, "t", unverified, false); err == nil || !strings.Contains(err.Error(), "fc-sidemap-digest") {
			t.Fatalf("ingest with no committed digest = %v; want a loud requirement error", err)
		}
	})

	t.Run("worksheet metadata is mandatory and singular", func(t *testing.T) {
		missing := strings.Replace(worksheet, metadata+"\n", "", 1)
		if _, _, err := ingestForcedChoiceWorksheet(missing, arts, "t", verified, false); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("worksheet without digest metadata = %v; want a loud missing-metadata error", err)
		}
		duplicate := metadata + "\n" + worksheet
		if _, _, err := ingestForcedChoiceWorksheet(duplicate, arts, "t", verified, false); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("worksheet with duplicate digest metadata = %v; want a loud duplicate error", err)
		}
		forged := strings.Replace(worksheet, metadata, "# fc-sidemap-digest: sha256:"+strings.Repeat("c", 64), 1)
		if _, _, err := ingestForcedChoiceWorksheet(forged, arts, "t", verified, false); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("worksheet with forged digest metadata = %v; want a loud mismatch", err)
		}
		secret := "sk-FAKE-WORKSHEET-SECRET-0000"
		invalid := strings.Replace(worksheet, metadata, "# fc-sidemap-digest: "+secret, 1)
		if _, _, err := ingestForcedChoiceWorksheet(invalid, arts, "t", verified, false); err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("invalid worksheet digest error = %v; want a non-echoing error", err)
		}
		forgedBody := strings.Replace(worksheet, metadata+"\n", "", 1)
		forgedBody = strings.Replace(forgedBody, "[answer A]\n", "[answer A]\n"+metadata+"\n", 1)
		if _, _, err := ingestForcedChoiceWorksheet(forgedBody, arts, "t", verified, false); err == nil || !strings.Contains(err.Error(), "before the first PAIR") {
			t.Fatalf("answer-body digest sentinel accepted as worksheet metadata = %v", err)
		}
	})

	t.Run("worksheet rendered under map A is rejected under map B", func(t *testing.T) {
		other := parityFCSidemap(arts, "c")
		other = testFCSidemapWithDigest(other, "sha256:"+strings.Repeat("b", 64))
		e := other.Pairs["pair-alpha|c"]
		e.LegacyIsA = !e.LegacyIsA
		e.HashA, e.HashB = e.HashB, e.HashA
		other.Pairs["pair-alpha|c"] = e
		if _, _, err := ingestForcedChoiceWorksheet(filled, arts, "t", other, false); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("map-A worksheet ingested under map B = %v; want digest mismatch", err)
		}
	})
}

func TestFCRenderIngestRequireSidemapResolver(t *testing.T) {
	arts := fcPairArtifacts()
	if _, err := renderForcedChoiceWorksheet(arts, fcSidemapFile{}); err == nil || !strings.Contains(err.Error(), "fc-sidemap") {
		t.Errorf("render without a sidemap = %v; want the sidemap requirement", err)
	}
	if _, _, err := ingestForcedChoiceWorksheet("", arts, "t", fcSidemapFile{}, false); err == nil || !strings.Contains(err.Error(), "fc-sidemap") {
		t.Errorf("ingest without a sidemap = %v; want the sidemap requirement", err)
	}

	// A sidemap missing one of the complete pairs aborts the render loudly.
	partial := parityFCSidemap(arts, "c", "pair-alpha")
	if _, err := renderForcedChoiceWorksheet(arts, partial); err == nil || !strings.Contains(err.Error(), "pair-gamma") {
		t.Errorf("render with a partial sidemap = %v; want a loud error naming pair-gamma", err)
	}

	// The map — not parity — decides sides: flip pair-alpha relative to
	// parity (parity says legacy is A; hashes swap with the assignment) and
	// the mixed answer must render as A.
	flipped := parityFCSidemap(arts, "c", "pair-alpha", "pair-gamma")
	entry := flipped.Pairs["pair-alpha|c"]
	entry.LegacyIsA = !entry.LegacyIsA
	entry.HashA, entry.HashB = entry.HashB, entry.HashA
	flipped.Pairs["pair-alpha|c"] = entry
	out, err := renderForcedChoiceWorksheet(arts, flipped)
	if err != nil {
		t.Fatalf("renderForcedChoiceWorksheet: %v", err)
	}
	alphaBlock := out[strings.Index(out, "=== PAIR pair-alpha c ==="):]
	alphaBlock = alphaBlock[:strings.Index(alphaBlock, blindEndMarker)]
	if !strings.Contains(alphaBlock, "[answer A]\nmixed alpha answer") {
		t.Errorf("flipped sidemap did not flip pair-alpha sides:\n%s", out)
	}
	// Ingest under the SAME flipped map accepts and binds the flipped order...
	ws := fillWorksheetField(t, out, "=== PAIR pair-alpha c ===", "prefer", "A")
	rows, _, err := ingestForcedChoiceWorksheet(ws, arts, "t", flipped, false)
	if err != nil {
		t.Errorf("ingest under the rendering sidemap: %v", err)
	} else if rows[0].ArtifactHashA != "sha256:am" || rows[0].ArtifactHashB != "sha256:al" {
		t.Errorf("row hashes = %q/%q; want the flipped map's am/al order", rows[0].ArtifactHashA, rows[0].ArtifactHashB)
	}
	// ...while a map flipped WITHOUT swapping its hashes is internally
	// inconsistent and rejected. The worksheet itself carries no hashes (W5),
	// so ingest under a DIFFERENT self-consistent map cannot be detected
	// structurally — the committed -fc-sidemap-digest is the registered guard
	// that render and ingest used the same sealed map.
	inconsistent := parityFCSidemap(arts, "c", "pair-alpha", "pair-gamma")
	e2 := inconsistent.Pairs["pair-alpha|c"]
	e2.LegacyIsA = !e2.LegacyIsA
	inconsistent.Pairs["pair-alpha|c"] = e2
	if _, _, err := ingestForcedChoiceWorksheet(ws, arts, "t", inconsistent, false); err == nil || !strings.Contains(err.Error(), "side assignment") {
		t.Errorf("ingest under an inconsistent map = %v; want the side-assignment rejection", err)
	}
	if _, err := renderForcedChoiceWorksheet(arts, inconsistent); err == nil || !strings.Contains(err.Error(), "side assignment") {
		t.Errorf("render under an inconsistent map = %v; want the side-assignment rejection", err)
	}
}

func TestFCIngestRequireCompleteAndArmGuess(t *testing.T) {
	arts := fcPairArtifacts()
	sidemap := parityFCSidemap(arts, "c", "pair-alpha", "pair-gamma")
	out, err := renderForcedChoiceWorksheet(arts, sidemap)
	if err != nil {
		t.Fatalf("renderForcedChoiceWorksheet: %v", err)
	}
	alphaHeader := "=== PAIR pair-alpha c ==="
	gammaHeader := "=== PAIR pair-gamma c ==="
	removeBlock := func(t *testing.T, worksheet, header string) string {
		t.Helper()
		start := strings.Index(worksheet, header+"\n")
		if start < 0 {
			t.Fatalf("worksheet missing block %q", header)
		}
		end := strings.Index(worksheet[start:], blindEndMarker+"\n")
		if end < 0 {
			t.Fatalf("worksheet block %q missing end marker", header)
		}
		end += start + len(blindEndMarker) + 1
		return worksheet[:start] + worksheet[end:]
	}

	t.Run("require-complete lists blank pairs", func(t *testing.T) {
		ws := fillWorksheetField(t, out, alphaHeader, "prefer", "A")
		_, _, err := ingestForcedChoiceWorksheet(ws, arts, "t", sidemap, true)
		if err == nil || !strings.Contains(err.Error(), "pair-gamma/c") || !strings.Contains(err.Error(), "left blank") {
			t.Errorf("require-complete on a half-filled worksheet = %v; want an error listing pair-gamma/c", err)
		}
		// Without the flag the same worksheet skips the blank block.
		rows, skipped, err := ingestForcedChoiceWorksheet(ws, arts, "t", sidemap, false)
		if err != nil || len(rows) != 1 || skipped != 1 {
			t.Errorf("rows=%d skipped=%d err=%v; want 1/1/nil without the flag", len(rows), skipped, err)
		}
	})

	t.Run("require-complete rejects deleted blocks", func(t *testing.T) {
		ws := removeBlock(t, out, gammaHeader)
		ws = fillWorksheetField(t, ws, alphaHeader, "prefer", "A")
		_, _, err := ingestForcedChoiceWorksheet(ws, arts, "t", sidemap, true)
		if err == nil || !strings.Contains(err.Error(), "pair-gamma/c") || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("require-complete with pair-gamma block deleted = %v; want a missing-block error naming pair-gamma/c", err)
		}

		rows, skipped, err := ingestForcedChoiceWorksheet(ws, arts, "t", sidemap, false)
		if err != nil || len(rows) != 1 || skipped != 0 {
			t.Fatalf("partial ingest with pair-gamma block deleted: rows=%d skipped=%d err=%v; want 1/0/nil", len(rows), skipped, err)
		}
	})

	t.Run("require-complete rejects all blocks deleted in renderer order", func(t *testing.T) {
		ws := removeBlock(t, removeBlock(t, out, alphaHeader), gammaHeader)
		_, _, err := ingestForcedChoiceWorksheet(ws, arts, "t", sidemap, true)
		if err == nil || !strings.Contains(err.Error(), "pair-alpha/c") || !strings.Contains(err.Error(), "pair-gamma/c") {
			t.Fatalf("require-complete with all blocks deleted = %v; want both missing pairs", err)
		}
		if strings.Index(err.Error(), "pair-alpha/c") > strings.Index(err.Error(), "pair-gamma/c") {
			t.Fatalf("missing pairs not reported in renderer order: %v", err)
		}
	})

	t.Run("stale extra sidemap entry is not an expected block", func(t *testing.T) {
		stale := parityFCSidemap(arts, "c")
		stale.Pairs["pair-stale|c"] = fcSidemapEntry{LegacyIsA: true, HashA: "sha256:stale-a", HashB: "sha256:stale-b"}
		ws := fillWorksheetField(t, out, alphaHeader, "prefer", "A")
		ws = fillWorksheetField(t, ws, gammaHeader, "prefer", "B")
		if _, _, err := ingestForcedChoiceWorksheet(ws, arts, "t", stale, true); err != nil {
			t.Fatalf("complete worksheet rejected for stale extra sidemap entry: %v", err)
		}
	})

	t.Run("missing sidemap entry for deleted eligible block is loud", func(t *testing.T) {
		partial := parityFCSidemap(arts, "c", "pair-alpha")
		ws := removeBlock(t, out, gammaHeader)
		ws = fillWorksheetField(t, ws, alphaHeader, "prefer", "A")
		_, _, err := ingestForcedChoiceWorksheet(ws, arts, "t", partial, true)
		if err == nil || !strings.Contains(err.Error(), "fc-sidemap") || !strings.Contains(err.Error(), "pair-gamma") {
			t.Fatalf("missing sidemap entry hidden by deleted block = %v; want a loud pair-gamma sidemap error", err)
		}
	})

	t.Run("stale sidemap hashes for deleted eligible block are loud", func(t *testing.T) {
		stale := parityFCSidemap(arts, "c")
		entry := stale.Pairs["pair-gamma|c"]
		entry.HashA = "sha256:stale"
		stale.Pairs["pair-gamma|c"] = entry
		ws := removeBlock(t, out, gammaHeader)
		ws = fillWorksheetField(t, ws, alphaHeader, "prefer", "A")
		_, _, err := ingestForcedChoiceWorksheet(ws, arts, "t", stale, true)
		if err == nil || !strings.Contains(err.Error(), "pair-gamma") || !strings.Contains(err.Error(), "sidemap hashes") {
			t.Fatalf("stale sidemap hashes hidden by deleted block = %v; want a loud pair-gamma hash-binding error", err)
		}
	})

	t.Run("arm_guess carried onto rows", func(t *testing.T) {
		ws := fillWorksheetField(t, out, alphaHeader, "prefer", "A")
		ws = fillWorksheetField(t, ws, alphaHeader, "arm_guess", "B")
		ws = fillWorksheetField(t, ws, gammaHeader, "prefer", "tie")
		rows, _, err := ingestForcedChoiceWorksheet(ws, arts, "t", sidemap, true)
		if err != nil {
			t.Fatalf("ingestForcedChoiceWorksheet: %v", err)
		}
		if len(rows) != 2 || rows[0].ArmGuess != "b" || rows[1].ArmGuess != "" {
			t.Fatalf("rows = %+v; want arm_guess b on pair-alpha, empty on pair-gamma", rows)
		}
	})

	t.Run("invalid arm_guess is a loud error", func(t *testing.T) {
		ws := fillWorksheetField(t, out, alphaHeader, "prefer", "A")
		ws = fillWorksheetField(t, ws, alphaHeader, "arm_guess", "legacy")
		if _, _, err := ingestForcedChoiceWorksheet(ws, arts, "t", sidemap, false); err == nil || !strings.Contains(err.Error(), "arm_guess") {
			t.Errorf("arm_guess legacy accepted = %v; want a loud domain error", err)
		}
	})
}

func TestAssemblyReportArmGuessAccuracy(t *testing.T) {
	arts, labels := fcReportFixture()
	sidemap := parityFCSidemap(arts, "c")
	// pair-alpha: legacy is A => a mixed-arm guess is "b"; pair-gamma: mixed
	// is A => a mixed-arm guess is "a". One correct, one wrong, one blank.
	alphaRow := fcRowFor(arts, "pair-alpha", "a")
	alphaRow.ArmGuess = "b" // correct: b is the mixed side
	gammaRow := fcRowFor(arts, "pair-gamma", "tie")
	gammaRow.ArmGuess = "b"                       // wrong: a is the mixed side
	deltaRow := fcRowFor(arts, "pair-delta", "b") // no guess
	prefs := []FCPreference{alphaRow, gammaRow, deltaRow}

	rep, err := computeAssemblyReport(arts, labels, 1, 200, prefs,
		assemblyReportExtras{sideResolver: sidemap.resolver(), armGuess: true})
	if err != nil {
		t.Fatalf("computeAssemblyReport: %v", err)
	}
	fc := rep.LegacyMixedModels[0].ForcedChoice
	if fc.ArmGuessAccuracy == nil {
		t.Fatal("arm_guess_accuracy missing with a sidemap-backed report")
	}
	if fc.ArmGuessAccuracy.NGuessed != 2 || fc.ArmGuessAccuracy.NCorrect != 1 {
		t.Fatalf("arm_guess_accuracy = %+v; want n_guessed 2, n_correct 1", fc.ArmGuessAccuracy)
	}

	t.Run("absent without a sidemap", func(t *testing.T) {
		rep, err := computeAssemblyReport(arts, labels, 1, 200, prefs, assemblyReportExtras{})
		if err != nil {
			t.Fatalf("computeAssemblyReport: %v", err)
		}
		if rep.LegacyMixedModels[0].ForcedChoice.ArmGuessAccuracy != nil {
			t.Error("arm_guess_accuracy present without a sidemap; must stay omitted")
		}
	})

	// Decision independence: neither the arm guesses nor the sidemap swap of
	// the resolver may move ANY decision-adjacent number. Compare against a
	// no-FC report and against the same rows with every guess changed.
	t.Run("never consulted by any decision", func(t *testing.T) {
		base, err := computeAssemblyReport(arts, labels, 1, 200, nil, assemblyReportExtras{})
		if err != nil {
			t.Fatal(err)
		}
		reguessed := make([]FCPreference, len(prefs))
		copy(reguessed, prefs)
		reguessed[0].ArmGuess = "a"
		reguessed[1].ArmGuess = ""
		reguessed[2].ArmGuess = "a"
		alt, err := computeAssemblyReport(arts, labels, 1, 200, reguessed,
			assemblyReportExtras{sideResolver: sidemap.resolver(), armGuess: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, other := range []*AssemblyReport{alt, rep} {
			for i := range base.LegacyMixedModels {
				b, o := base.LegacyMixedModels[i], other.LegacyMixedModels[i]
				if b.Decision != o.Decision || b.MeanDelta != o.MeanDelta ||
					b.DeltaCILow != o.DeltaCILow || b.DeltaCIHigh != o.DeltaCIHigh || b.Pairs != o.Pairs {
					t.Fatalf("decision-adjacent numbers moved with FC/arm-guess input:\nbase %+v\ngot  %+v", b, o)
				}
			}
		}
		// The guesses DID change the audit tally (proof this test could see a
		// difference if one leaked into scope): alpha "a" is wrong (b is the
		// mixed side there); delta "a" is correct iff mixed renders as A on
		// pair-delta under the fixture assignment.
		deltaCorrect := 0
		if !fcSideIsLegacyA("pair-delta", "c") {
			deltaCorrect = 1
		}
		got := alt.LegacyMixedModels[0].ForcedChoice.ArmGuessAccuracy
		if got.NGuessed != 2 || got.NCorrect != deltaCorrect {
			t.Fatalf("reguessed audit = %+v; want n_guessed 2, n_correct %d", got, deltaCorrect)
		}
	})
}

// TestAssemblyReportSidemapReplacesParity proves the report's side
// resolution really flows through the sidemap: with pair-alpha FLIPPED
// relative to parity, a preference row carrying the flipped a/b hashes binds
// and resolves ("a" becomes a MIXED win), while the same rows under the
// parity default are a loud hash-order error. A resolver that silently fell
// back to parity would fail both arms of this test.
func TestAssemblyReportSidemapReplacesParity(t *testing.T) {
	arts, labels := fcReportFixture()
	sidemap := parityFCSidemap(arts, "c", "pair-alpha")
	entry := sidemap.Pairs["pair-alpha|c"]
	entry.LegacyIsA = !entry.LegacyIsA // parity says legacy is A; flip it
	entry.HashA, entry.HashB = entry.HashB, entry.HashA
	sidemap.Pairs["pair-alpha|c"] = entry

	row := fcRowFor(arts, "pair-alpha", "a")
	row.ArtifactHashA, row.ArtifactHashB = row.ArtifactHashB, row.ArtifactHashA

	rep, err := computeAssemblyReport(arts, labels, 1, 200, []FCPreference{row},
		assemblyReportExtras{sideResolver: sidemap.resolver(), armGuess: true})
	if err != nil {
		t.Fatalf("computeAssemblyReport under the flipped sidemap: %v", err)
	}
	fc := rep.LegacyMixedModels[0].ForcedChoice
	// Parity would have called "a" a LEGACY win on pair-alpha; the flipped
	// map makes it a MIXED win.
	if fc.MixedWins != 1 || fc.LegacyWins != 0 {
		t.Fatalf("fc = %+v; want the flipped map to score a as a mixed win", fc)
	}
	if _, err := computeAssemblyReport(arts, labels, 1, 200, []FCPreference{row}, assemblyReportExtras{}); err == nil {
		t.Fatal("flipped-hash row accepted under the parity default; the resolver seam is dead")
	}
}

// withCaptureTemp stamps minimal capture provenance (temperature temp)
// onto an artifact. ArtifactHash deliberately ignores Capture, so this stays
// hash-stable.
func withCaptureTemp(a Artifact, temp float64) Artifact {
	a.Capture = &CaptureProvenance{Temperature: &temp, Transport: "ollama", Model: a.CandidateModel}
	return a
}

func TestAssemblyReportCaptureManifestVerification(t *testing.T) {
	// Five complete labeled pairs: pair-ok verified; pair-miss absent from
	// the manifest; pair-cold listed but with usage_present false; pair-temp
	// with mismatched capture temperatures; pair-warm consistently captured
	// at 0.7 on BOTH arms — pair-internal agreement is not enough, the
	// registered temperature (0) is the requirement.
	var arts []Artifact
	var labels []Label
	for _, pair := range []string{"pair-ok", "pair-miss", "pair-cold", "pair-temp", "pair-warm"} {
		temp := 0.0
		if pair == "pair-warm" {
			temp = 0.7
		}
		l := withCaptureTemp(mixedArtifact(pair, AssemblyLegacy, "c", nil), temp)
		m := withCaptureTemp(mixedArtifact(pair, AssemblyMixed, "c", nil), temp)
		if pair == "pair-temp" {
			m = withCaptureTemp(m, 0.7)
		}
		arts = append(arts, l, m)
		labels = append(labels, labelFor(l, 0), labelFor(m, 1))
	}
	usage := map[string]bool{}
	for _, a := range arts {
		switch a.Trace.AssemblyEval.PairID {
		case "pair-miss":
			// absent from the manifest entirely
		case "pair-cold":
			usage[a.ArtifactHash] = false
		default:
			usage[a.ArtifactHash] = true
		}
	}
	rep, err := computeAssemblyReport(arts, labels, 1, 200, nil,
		assemblyReportExtras{
			capture:    &captureVerification{usagePresent: usage},
			captureRef: &AssemblyCaptureManifest{Digest: "sha256:feed", ArtifactCount: len(arts)},
		})
	if err != nil {
		t.Fatalf("computeAssemblyReport: %v", err)
	}
	m := rep.LegacyMixedModels[0]
	if m.Pairs != 1 {
		t.Errorf("verified pairs = %d; want only pair-ok", m.Pairs)
	}
	reasons := map[string]string{}
	for _, ex := range m.Exclusions {
		reasons[ex.PairID] = ex.Reason
	}
	if reasons["pair-miss"] != "unverified-capture" {
		t.Errorf("pair-miss reason = %q; want unverified-capture (absent from manifest)", reasons["pair-miss"])
	}
	if reasons["pair-cold"] != "unverified-capture" {
		t.Errorf("pair-cold reason = %q; want unverified-capture (usage_present false)", reasons["pair-cold"])
	}
	if reasons["pair-temp"] != "temperature-mismatch" {
		t.Errorf("pair-temp reason = %q; want temperature-mismatch", reasons["pair-temp"])
	}
	if reasons["pair-warm"] != "temperature-mismatch" {
		t.Errorf("pair-warm reason = %q; want temperature-mismatch (0.7 on both arms is not the registered temperature)", reasons["pair-warm"])
	}
	if rep.CaptureManifest == nil || rep.CaptureManifest.Digest != "sha256:feed" || rep.CaptureManifest.ArtifactCount != len(arts) {
		t.Errorf("capture_manifest = %+v; want the embedded reference", rep.CaptureManifest)
	}

	t.Run("end to end through runAssemblyReport", func(t *testing.T) {
		dir := t.TempDir()
		artsPath := filepath.Join(dir, "artifacts.jsonl")
		labelsPath := filepath.Join(dir, "labels.jsonl")
		if err := writeJSONLRows(artsPath, "artifacts", arts); err != nil {
			t.Fatal(err)
		}
		if err := writeLabelsJSONL(labelsPath, labels); err != nil {
			t.Fatal(err)
		}
		rows := make([]captureManifestRow, 0, len(arts))
		for i, a := range arts {
			rows = append(rows, captureManifestRow{
				TraceID: a.TraceID, ArtifactHash: a.ArtifactHash, OrderIndex: i,
				UsagePresent: usage[a.ArtifactHash],
			})
		}
		manifestRaw, err := json.Marshal(captureManifest{
			SchemaVersion: captureManifestSchemaVersion, ArtifactCount: len(rows), PerArtifact: rows,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(dir, "artifacts.jsonl.manifest.json")
		if err := os.WriteFile(manifestPath, manifestRaw, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := runAssemblyReport(assemblyReportOptions{
			LabelsPath: labelsPath, ArtifactsPath: artsPath, CaptureManifestPath: manifestPath,
		})
		if err != nil {
			t.Fatalf("runAssemblyReport: %v", err)
		}
		var got AssemblyReport
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode report: %v", err)
		}
		sum := sha256.Sum256(manifestRaw)
		if got.CaptureManifest == nil || got.CaptureManifest.Digest != "sha256:"+hex.EncodeToString(sum[:]) ||
			got.CaptureManifest.ArtifactCount != len(rows) {
			t.Errorf("capture_manifest = %+v; want the manifest file digest and count %d", got.CaptureManifest, len(rows))
		}
		found := false
		for _, ex := range got.LegacyMixedModels[0].Exclusions {
			if ex.PairID == "pair-miss" && ex.Reason == "unverified-capture" {
				found = true
			}
		}
		if !found {
			t.Errorf("exclusions = %+v; want pair-miss excluded unverified-capture", got.LegacyMixedModels[0].Exclusions)
		}
	})

	t.Run("no manifest keeps every pair and omits the key", func(t *testing.T) {
		rep, err := computeAssemblyReport(arts, labels, 1, 200, nil, assemblyReportExtras{})
		if err != nil {
			t.Fatal(err)
		}
		if rep.LegacyMixedModels[0].Pairs != 5 || len(rep.LegacyMixedModels[0].Exclusions) != 0 {
			t.Errorf("without a manifest pairs=%d exclusions=%d; want 5/0",
				rep.LegacyMixedModels[0].Pairs, len(rep.LegacyMixedModels[0].Exclusions))
		}
		raw, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "capture_manifest") {
			t.Errorf("capture_manifest key present without -capture-manifest:\n%s", raw)
		}
	})
}

func TestLoadCaptureManifestForReport(t *testing.T) {
	dir := t.TempDir()
	manifest := captureManifest{
		SchemaVersion: captureManifestSchemaVersion,
		ArtifactCount: 2,
		PerArtifact: []captureManifestRow{
			{TraceID: "t1", ArtifactHash: "sha256:aa", OrderIndex: 0, UsagePresent: true},
			{TraceID: "t2", ArtifactHash: "sha256:bb", OrderIndex: 1, UsagePresent: false},
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "artifacts.jsonl.manifest.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ref, verify, err := loadCaptureManifestForReport(path)
	if err != nil {
		t.Fatalf("loadCaptureManifestForReport: %v", err)
	}
	sum := sha256.Sum256(raw)
	if ref.Digest != "sha256:"+hex.EncodeToString(sum[:]) || ref.ArtifactCount != 2 {
		t.Errorf("ref = %+v; want the file digest and count 2", ref)
	}
	if !verify.usagePresent["sha256:aa"] || verify.usagePresent["sha256:bb"] {
		t.Errorf("usagePresent = %v; want aa true, bb false", verify.usagePresent)
	}

	t.Run("wrong schema version rejected", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(bad, []byte(`{"schema_version":"nope/v9"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadCaptureManifestForReport(bad); err == nil || !strings.Contains(err.Error(), "schema_version") {
			t.Errorf("wrong schema version = %v; want a schema error", err)
		}
	})

	t.Run("artifact_count row mismatch rejected", func(t *testing.T) {
		manifest.ArtifactCount = 3 // 2 per_artifact rows
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		bad := filepath.Join(dir, "count-mismatch.json")
		if err := os.WriteFile(bad, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadCaptureManifestForReport(bad); err == nil || !strings.Contains(err.Error(), "artifact_count") {
			t.Errorf("count/row mismatch = %v; want an artifact_count consistency error", err)
		}
	})
}

// TestMainFCSidemapDigestGate drives the -fc-sidemap-digest verification
// through the -fc-render and -fc-ingest CLI paths: a wrong committed digest
// is fatal before any artifacts load; the right digest passes the gate (the
// runs then fail later, on the unreadable artifacts input — proof the gate
// was traversed, not skipped).
func TestMainFCSidemapDigestGate(t *testing.T) {
	dir := t.TempDir()
	sidemapPath := filepath.Join(dir, "sidemap.json")
	digest, err := writeFCSidemap(sidemapPath, parityFCSidemap(fcPairArtifacts(), "c", "pair-alpha"))
	if err != nil {
		t.Fatal(err)
	}
	wsPath := filepath.Join(dir, "worksheet.txt")
	if err := os.WriteFile(wsPath, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingArts := filepath.Join(dir, "no-such-artifacts.jsonl")
	wrong := strings.Repeat("0", 64)
	cases := []struct {
		name, wantErr string
		args          []string
	}{
		{"fc-render missing digest", "requires -fc-sidemap-digest",
			[]string{"-fc-render", "-artifacts", missingArts, "-fc-sidemap", sidemapPath}},
		{"fc-ingest missing digest", "requires -fc-sidemap-digest",
			[]string{"-fc-ingest", "-worksheet", wsPath, "-artifacts", missingArts, "-fc-out", filepath.Join(dir, "out-missing.jsonl"), "-fc-sidemap", sidemapPath}},
		{"fc-render wrong digest", "mismatch",
			[]string{"-fc-render", "-artifacts", missingArts, "-fc-sidemap", sidemapPath, "-fc-sidemap-digest", wrong}},
		{"fc-ingest wrong digest", "mismatch",
			[]string{"-fc-ingest", "-worksheet", wsPath, "-artifacts", missingArts, "-fc-out", filepath.Join(dir, "out.jsonl"), "-fc-sidemap", sidemapPath, "-fc-sidemap-digest", wrong}},
		{"fc-render right digest passes the gate", "no-such-artifacts",
			[]string{"-fc-render", "-artifacts", missingArts, "-fc-sidemap", sidemapPath, "-fc-sidemap-digest", digest}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], tc.args...)
			cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("run succeeded; want failure:\n%s", out)
			}
			if !strings.Contains(string(out), tc.wantErr) {
				t.Fatalf("output missing %q:\n%s", tc.wantErr, out)
			}
			if tc.wantErr != "mismatch" && strings.Contains(string(out), "mismatch") {
				t.Fatalf("right digest reported a mismatch:\n%s", out)
			}
		})
	}
}

func TestMainFCIngestHonorsModelFilter(t *testing.T) {
	base := fcPairArtifacts()[:2]
	other := append([]Artifact(nil), base...)
	for i := range other {
		other[i].TraceID += "-d"
		other[i].Trace.ID += "-d"
		other[i].CandidateModel = "ollama/d"
	}
	arts := append(base, other...)
	for i := range arts {
		arts[i].ArtifactHash = artifactHash(arts[i])
	}

	dir := t.TempDir()
	artsPath := filepath.Join(dir, "artifacts.jsonl")
	writeArtifactsJSONL(t, artsPath, arts)
	sidemap, err := generateFCSidemap(arts)
	if err != nil {
		t.Fatal(err)
	}
	sidemapPath := filepath.Join(dir, "sidemap.json")
	digest, err := writeFCSidemap(sidemapPath, sidemap)
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
		return cmd.CombinedOutput()
	}

	worksheetPath := filepath.Join(dir, "worksheet-c.txt")
	if out, err := run("-fc-render", "-artifacts", artsPath, "-model", "c", "-fc-sidemap", sidemapPath, "-fc-sidemap-digest", digest, "-report", worksheetPath); err != nil {
		t.Fatalf("render model c: %v\n%s", err, out)
	}
	worksheet, err := os.ReadFile(worksheetPath)
	if err != nil {
		t.Fatal(err)
	}
	filled := fillWorksheetField(t, string(worksheet), "=== PAIR pair-alpha c ===", "prefer", "A")
	if err := os.WriteFile(worksheetPath, []byte(filled), 0o600); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(dir, "preferences-c.jsonl")
	if out, err := run("-fc-ingest", "-worksheet", worksheetPath, "-artifacts", artsPath, "-model", "c", "-fc-sidemap", sidemapPath, "-fc-sidemap-digest", digest, "-fc-require-complete", "-fc-out", outPath); err != nil {
		t.Fatalf("ingest model c worksheet with model c: %v\n%s", err, out)
	}
	rows, err := loadFCPreferences(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CandidateModel != "c" {
		t.Fatalf("filtered preferences = %+v; want one model-c row", rows)
	}

	failures := []struct {
		name, model, want string
	}{
		{name: "model flag omitted", want: "pair-alpha/d"},
		{name: "wrong present model", model: "d", want: "not in -artifacts"},
		{name: "absent model", model: "absent", want: "matches no artifacts"},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			failedOut := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".jsonl")
			args := []string{"-fc-ingest", "-worksheet", worksheetPath, "-artifacts", artsPath, "-fc-sidemap", sidemapPath, "-fc-sidemap-digest", digest, "-fc-require-complete", "-fc-out", failedOut}
			if tc.model != "" {
				args = append(args, "-model", tc.model)
			}
			out, err := run(args...)
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("ingest model %q: err=%v, want output containing %q\n%s", tc.model, err, tc.want, out)
			}
			if _, err := os.Stat(failedOut); !os.IsNotExist(err) {
				t.Fatalf("failed ingest wrote %s: %v", failedOut, err)
			}
		})
	}

	allWorksheetPath := filepath.Join(dir, "worksheet-all.txt")
	if out, err := run("-fc-render", "-artifacts", artsPath, "-fc-sidemap", sidemapPath, "-fc-sidemap-digest", digest, "-report", allWorksheetPath); err != nil {
		t.Fatalf("render all models: %v\n%s", err, out)
	}
	allWorksheet, err := os.ReadFile(allWorksheetPath)
	if err != nil {
		t.Fatal(err)
	}
	allFilled := fillWorksheetField(t, string(allWorksheet), "=== PAIR pair-alpha c ===", "prefer", "A")
	allFilled = fillWorksheetField(t, allFilled, "=== PAIR pair-alpha d ===", "prefer", "B")
	if err := os.WriteFile(allWorksheetPath, []byte(allFilled), 0o600); err != nil {
		t.Fatal(err)
	}
	allOut := filepath.Join(dir, "preferences-all.jsonl")
	if out, err := run("-fc-ingest", "-worksheet", allWorksheetPath, "-artifacts", artsPath, "-fc-sidemap", sidemapPath, "-fc-sidemap-digest", digest, "-fc-require-complete", "-fc-out", allOut); err != nil {
		t.Fatalf("ingest all-model worksheet without -model: %v\n%s", err, out)
	}
	allRows, err := loadFCPreferences(allOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(allRows) != 2 {
		t.Fatalf("all-model preferences = %+v; want two rows", allRows)
	}
}

func TestFilterArtifactsByModel(t *testing.T) {
	arts := []Artifact{
		{CandidateModel: "ollama/qwen3:8b"},
		{CandidateModel: "gemma4:31b"},
		{CandidateModel: "qwen3:8b"},
	}
	got, err := filterArtifactsByModel(arts, "qwen3:8b")
	if err != nil {
		t.Fatalf("filterArtifactsByModel: %v", err)
	}
	// modelKey strips the bench provider prefix, so both qwen spellings match.
	if len(got) != 2 || got[0].CandidateModel != "ollama/qwen3:8b" || got[1].CandidateModel != "qwen3:8b" {
		t.Fatalf("filtered = %+v; want both qwen artifacts in order", got)
	}
	if all, err := filterArtifactsByModel(arts, ""); err != nil || len(all) != 3 {
		t.Fatalf("empty selector = %d artifacts, err %v; want all 3", len(all), err)
	}
	if _, err := filterArtifactsByModel(arts, "missing:1b"); err == nil || !strings.Contains(err.Error(), "matches no artifacts") {
		t.Fatalf("zero-match selector = %v; want a loud error", err)
	}
}
