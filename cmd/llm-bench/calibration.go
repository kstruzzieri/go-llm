package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

// Artifact is one frozen candidate output for a (trace, candidate model)
// pair. Written by -calibrate-capture; consumed by -calibrate and by the
// labeling workflow (humans add expected_answer_quality to corresponding
// Label rows).
type Artifact struct {
	TraceID           string    `json:"trace_id"`
	CandidateModel    string    `json:"candidate_model"`
	ArtifactHash      string    `json:"artifact_hash"`
	Trace             Trace     `json:"trace"`
	ActualFinalAnswer string    `json:"actual_final_answer"`
	ActualToolCalls   []string  `json:"actual_tool_calls"`
	ActualTranscript  []Turn    `json:"actual_transcript"`
	CapturedAt        time.Time `json:"captured_at"`
	// Capture records controlled-capture provenance for assembly-mode traces
	// (#331 slice 3c): nil on every other artifact so their JSON stays
	// byte-identical to the pre-3c shape. Deliberately excluded from
	// artifactHash: provenance describes HOW an output was captured, not
	// WHAT was captured, so it must not change artifact identity.
	Capture *CaptureProvenance `json:"capture,omitempty"`
}

// CaptureProvenance pins the controlled conditions one assembly artifact was
// captured under. Temperature is the registered greedy-decoding setting.
// Seed is always nil today: neither ollama.ModelOptions nor the
// openai-compat request shape exposes a seed field, so temperature-0 is the
// sole registered decoding control on both transports. ModelDigest is the
// ollama /api/show digest when the transport exposes one; openai-compat
// servers have no digest endpoint, so it stays empty there.
type CaptureProvenance struct {
	// OrderIndex is the trace's position in the counterbalanced capture
	// order (the per-target replay sequence); artifacts of the same trace
	// under different targets share it.
	OrderIndex  int      `json:"order_index"`
	Temperature *float64 `json:"temperature,omitempty"`
	Seed        *int     `json:"seed,omitempty"`
	Transport   string   `json:"transport"`
	Model       string   `json:"model"`
	ModelDigest string   `json:"model_digest,omitempty"`
	// KNOWN GAP: a provider that omits usage counts JSON-decodes to 0
	// tokens — a plausible-looking value — so "usage not reported" is
	// indistinguishable from a genuine zero. Closing it needs presence
	// signals (*int) threaded from the transports; deferred because the
	// replay usage plumbing is shared with Score's token fields.
	PromptTokens int `json:"prompt_tokens"`
	GenTokens    int `json:"gen_tokens"`
	// CapturedOrder is "legacy-first" or "mixed-first" for legacy/mixed pair
	// members (which arm of the pair the counterbalanced order ran first);
	// empty for topline artifacts.
	CapturedOrder string `json:"captured_order,omitempty"`
}

// Label is the human-supplied truth for one frozen Artifact. ArtifactHash
// links them: a Label with a hash that doesn't match any current Artifact
// is "stale" and excluded from agreement. Duplicate matched ArtifactHash
// values are rejected so each artifact contributes at most once.
type Label struct {
	TraceID               string    `json:"trace_id"`
	CandidateModel        string    `json:"candidate_model"`
	ArtifactHash          string    `json:"artifact_hash"`
	ActualFinalAnswer     string    `json:"actual_final_answer,omitempty"`
	ActualToolCalls       []string  `json:"actual_tool_calls,omitempty"`
	ActualTranscript      []Turn    `json:"actual_transcript,omitempty"`
	ExpectedAnswerQuality float64   `json:"expected_answer_quality"`
	LabelNotes            string    `json:"label_notes,omitempty"`
	LabeledAt             time.Time `json:"labeled_at"`
	Labeler               string    `json:"labeler"`
}

// artifactHashInput is the canonical struct hashed to produce ArtifactHash.
// Order-sensitive on trace IDs, tool calls, and transcript turns; candidate
// model is normalized to lowercase to avoid spurious mismatch from case.
type artifactHashInput struct {
	TraceID           string   `json:"trace_id"`
	CandidateModel    string   `json:"candidate_model"`
	Trace             Trace    `json:"trace"`
	ActualFinalAnswer string   `json:"actual_final_answer"`
	ActualToolCalls   []string `json:"actual_tool_calls"`
	ActualTranscript  []Turn   `json:"actual_transcript"`
}

// artifactHash returns the canonical sha256 hash for an Artifact. Stable
// under JSON struct-tag ordering; sensitive to every input including
// tool-call sequence order.
func artifactHash(a Artifact) string {
	raw, _ := json.Marshal(artifactHashInput{
		TraceID:           a.TraceID,
		CandidateModel:    normalizeModelSelector(a.CandidateModel),
		Trace:             a.Trace,
		ActualFinalAnswer: a.ActualFinalAnswer,
		ActualToolCalls:   a.ActualToolCalls,
		ActualTranscript:  a.ActualTranscript,
	})
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(sum[:]))
}

// candidateDigestResolver is the minimal ShowModel surface needed to
// resolve candidate model digests (satisfied by *ollama.Client).
type candidateDigestResolver interface {
	ShowModel(ctx context.Context, name string) (*ollama.ModelInfo, error)
}

// digestResolutionTimeout bounds the digest-resolution client only; the
// capture replay client keeps the operator-configured -timeout.
const digestResolutionTimeout = 10 * time.Second

// resolveCaptureModelDigests constructs a short-timeout ollama client and
// resolves candidate digests for capture provenance. Failures degrade to
// missing digests with one stderr line each, never a capture failure.
func resolveCaptureModelDigests(ctx context.Context, ollamaURL string, targets []ModelTarget) map[string]string {
	client, err := newOllamaClient(ollamaURL, ollama.WithTimeout(digestResolutionTimeout))
	if err != nil {
		fmt.Fprintf(os.Stderr, "calibrate-capture: digest resolution skipped: %s\n", redactErrorMessage(err.Error()))
		return nil
	}
	return resolveCandidateDigests(ctx, client, targets)
}

// resolveCandidateDigests resolves the content digest for each ollama
// candidate target via /api/show, keyed by the normalized Display selector.
// Per-target errors degrade to a missing digest (with a stderr note) per
// resolveJudgeDigest precedent — degraded provenance, not a capture
// failure. openai-compat targets are skipped — that transport has no digest
// endpoint, so their provenance ModelDigest stays empty by design.
func resolveCandidateDigests(ctx context.Context, resolver candidateDigestResolver, targets []ModelTarget) map[string]string {
	digests := make(map[string]string, len(targets))
	for _, target := range targets {
		if normalizeModelSelector(target.Provider) != defaultBenchProvider {
			continue
		}
		info, err := resolver.ShowModel(ctx, target.Model)
		if err != nil {
			fmt.Fprintf(os.Stderr, "calibrate-capture: digest resolution skipped for %q: %s\n", target.Display, redactErrorMessage(err.Error()))
			continue
		}
		if info == nil || info.Digest == "" {
			// A successful ShowModel with no digest degrades exactly like the
			// error branch above — noted, never silent.
			fmt.Fprintf(os.Stderr, "calibrate-capture: digest resolution skipped for %q: ShowModel returned no digest\n", target.Display)
			continue
		}
		digest := canonicalSHA256Digest(info.Digest)
		if digest == "" {
			fmt.Fprintf(os.Stderr, "calibrate-capture: digest resolution skipped for %q: invalid digest\n", target.Display)
			continue
		}
		digests[normalizeModelSelector(target.Display)] = digest
	}
	return digests
}

func canonicalSHA256Digest(raw string) string {
	hexDigest := raw
	if len(raw) == len("sha256:")+sha256.Size*2 && strings.EqualFold(raw[:len("sha256:")], "sha256:") {
		hexDigest = raw[len("sha256:"):]
	}
	if len(hexDigest) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return ""
	}
	return "sha256:" + strings.ToLower(hexDigest)
}

// calibrationRunner abstracts Runner.RunAll so calibration tests can
// inject canned Results without needing a live Ollama.
type calibrationRunner interface {
	RunAll(ctx context.Context, targets []ModelTarget, traces []Trace) ([]Result, error)
}

// calibrateCaptureOptions configures runCalibrateCapture. Runner is the
// (typically *Runner) replayer; Targets and Traces are forwarded to it;
// OutputPath receives one JSONL Artifact per non-failed Result. Clock is
// an optional time source for deterministic tests; defaults to time.Now
// in UTC.
type calibrateCaptureOptions struct {
	Runner     calibrationRunner
	Targets    []ModelTarget
	Traces     []Trace
	OutputPath string
	Clock      func() time.Time
	// ModelDigests maps a normalized target Display selector to the model
	// content digest recorded on assembly-artifact capture provenance.
	// Resolved by the caller (see resolveCandidateDigests); missing entries
	// leave CaptureProvenance.ModelDigest empty.
	ModelDigests map[string]string
	// OllamaURL and OpenAICompatBaseURL are the capture endpoints, recorded
	// on the run manifest (#331 W3) when the trace set contains assembly
	// modes.
	OllamaURL           string
	OpenAICompatBaseURL string
	// Stdout receives the manifest_digest line (the pack's committed report
	// embeds it); nil defaults to os.Stdout.
	Stdout io.Writer
}

// Capture run manifest (#331 W3). A -calibrate-capture over traces
// containing assembly modes (legacy/mixed/topline) MUST also write
// <artifacts-out>.manifest.json pinning the run conditions; the registered
// -assembly-report run verifies pairs against it (-capture-manifest).
const (
	// captureManifestSchemaVersion is the LEGACY v1 schema: per_artifact rows
	// for successful captures only, no run ledger. The committed slice-3c
	// manifests are v1 and sealed; the report keeps v1 semantics for v1 files.
	captureManifestSchemaVersion = "mixed-capture-manifest/v1"
	// captureManifestSchemaVersionV2 adds the `expected` run ledger — one row
	// per (trace x model) attempt, failures included — so a failed run leaves
	// auditable evidence instead of vanishing (external PR review P1). New
	// captures write v2.
	captureManifestSchemaVersionV2 = "mixed-capture-manifest/v2"
)

// captureManifestPath is the manifest's sibling path for one artifacts
// output path.
func captureManifestPath(artifactsOut string) string {
	return artifactsOut + ".manifest.json"
}

type captureManifest struct {
	SchemaVersion        string                  `json:"schema_version"`
	CreatedAt            time.Time               `json:"created_at"`
	Endpoint             string                  `json:"endpoint"`
	Transport            string                  `json:"transport"`
	ModelTargets         []captureManifestTarget `json:"model_targets"`
	ServerProbe          captureServerProbe      `json:"server_probe"`
	Decoding             captureDecoding         `json:"decoding"`
	CounterbalanceScheme string                  `json:"counterbalance_scheme"`
	// ArtifactCount counts SUCCESSFUL captures only (one per PerArtifact
	// row) — unchanged from v1.
	ArtifactCount int                  `json:"artifact_count"`
	PerArtifact   []captureManifestRow `json:"per_artifact"`
	// ExpectedCount / Expected are the v2 run ledger: one row per
	// (trace x model) the capture attempted, failures included. Absent on v1
	// manifests; required on v2.
	ExpectedCount int                  `json:"expected_count,omitempty"`
	Expected      []captureExpectedRow `json:"expected,omitempty"`
}

// captureExpectedRow is one v2-ledger entry: a (trace x model) run the
// capture attempted. Status "captured" rows carry the written artifact's
// hash; "failed" rows carry the (path-redacted) error and no hash — they
// produced no artifact and cannot be labeled, but they no longer vanish:
// the report synthesizes their pairs into missing-arm exclusions.
//
// Attempts is 1 or 2: the registered one-retry policy runs IN-RUN — a cell
// that fails (or gets no result) on the first attempt is retried exactly
// once within the same invocation, and the ledger records whether the retry
// happened. Loading rejects any value outside 1..2.
type captureExpectedRow struct {
	TraceID string `json:"trace_id"`
	Model   string `json:"model"`
	// PairID/Arm locate assembly traces: Arm is legacy/mixed/topline (empty
	// for non-assembly traces); PairID is set on legacy/mixed arms only.
	PairID       string `json:"pair_id,omitempty"`
	Arm          string `json:"arm,omitempty"`
	Status       string `json:"status"` // captured | failed
	Attempts     int    `json:"attempts"`
	Error        string `json:"error,omitempty"`
	ArtifactHash string `json:"artifact_hash,omitempty"`
}

type captureManifestTarget struct {
	Selector string `json:"selector"`
	// ResolvedDigest is the ollama ShowModel digest, or empty when the
	// transport exposes none (openai-compat) or resolution degraded.
	ResolvedDigest string `json:"resolved_digest"`
}

// captureServerProbe records safe Ollama model digests resolved before capture.
type captureServerProbe struct {
	OllamaDigests map[string]string `json:"ollama_digests,omitempty"`
}

// captureDecoding pins the registered decoding controls. SeedSupported is
// false today on both transports (see CaptureProvenance.Seed).
type captureDecoding struct {
	Temperature   float64 `json:"temperature"`
	SeedSupported bool    `json:"seed_supported"`
}

// captureManifestRow is one written artifact's manifest entry. UsagePresent
// reports whether the replay carried non-zero prompt AND gen token counts —
// the report's -capture-manifest verification excludes pairs whose arms
// lack it (a provider that omits usage decodes to 0, so zero is treated as
// "not verified").
type captureManifestRow struct {
	TraceID      string `json:"trace_id"`
	ArtifactHash string `json:"artifact_hash"`
	OrderIndex   int    `json:"order_index"`
	UsagePresent bool   `json:"usage_present"`
}

// captureEndpointTransport derives the manifest endpoint/transport pair from
// the target set. Registered runs are single-transport; a mixed target set
// records transport "mixed" with both URLs space-joined, ollama first.
func captureEndpointTransport(targets []ModelTarget, ollamaURL, compatURL string) (endpoint, transport string) {
	hasOllama, hasCompat := false, false
	for _, t := range targets {
		if normalizeModelSelector(t.Provider) == openAICompatTransport {
			hasCompat = true
		} else {
			hasOllama = true
		}
	}
	switch {
	case hasCompat && hasOllama:
		return strings.TrimSpace(ollamaURL) + " " + strings.TrimSpace(compatURL), "mixed"
	case hasCompat:
		return strings.TrimSpace(compatURL), openAICompatTransport
	default:
		return strings.TrimSpace(ollamaURL), defaultBenchProvider
	}
}

// loadCaptureManifestForReport reads a capture run manifest for
// -assembly-report verification, returning the embedded reference (the
// FILE's sha256 digest + artifact count) and the verification set the pair
// verification consults. v1 manifests (the sealed slice-3c run) keep exactly
// the pre-ledger semantics: a usage-present hash set and nothing else. v2
// manifests additionally validate the expected run ledger against the
// per_artifact rows (hash match, both directions) and surface the expected
// legacy-mixed pair keys for missing-pair synthesis.
func loadCaptureManifestForReport(path string) (AssemblyCaptureManifest, *captureVerification, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return AssemblyCaptureManifest{}, nil, fmt.Errorf("capture-manifest: read %q: %w", path, err)
	}
	var m captureManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return AssemblyCaptureManifest{}, nil, fmt.Errorf("capture-manifest: decode %q: %w", path, err)
	}
	if m.ArtifactCount != len(m.PerArtifact) {
		return AssemblyCaptureManifest{}, nil, fmt.Errorf("capture-manifest: %q artifact_count %d does not match its %d per_artifact row(s) (corrupt or hand-edited manifest)", path, m.ArtifactCount, len(m.PerArtifact))
	}
	verify := &captureVerification{usagePresent: make(map[string]bool, len(m.PerArtifact))}
	for _, row := range m.PerArtifact {
		verify.usagePresent[row.ArtifactHash] = row.UsagePresent
	}
	switch m.SchemaVersion {
	case captureManifestSchemaVersion:
		// v1: no ledger may ride along — a hand-added one would silently
		// change pair discovery on a schema that never carried it.
		if m.ExpectedCount != 0 || len(m.Expected) > 0 {
			return AssemblyCaptureManifest{}, nil, fmt.Errorf("capture-manifest: v1 manifest %q carries a v2 expected ledger (%d row(s)); re-run the capture or fix the schema_version", path, len(m.Expected))
		}
		verify.legacyV1ModelIdentity = true
	case captureManifestSchemaVersionV2:
		pairs, err := validateCaptureLedger(m)
		if err != nil {
			return AssemblyCaptureManifest{}, nil, fmt.Errorf("capture-manifest: %q: %w", path, err)
		}
		verify.expectedPairs = pairs
		verify.expectedArtifacts = make(map[string]captureExpectedArtifact, m.ArtifactCount)
		for _, row := range m.Expected {
			if row.Status == "captured" {
				verify.expectedArtifacts[row.ArtifactHash] = captureExpectedArtifact{
					traceID: row.TraceID, model: modelKey(row.Model), pairID: row.PairID, arm: row.Arm,
				}
			}
		}
	default:
		return AssemblyCaptureManifest{}, nil, fmt.Errorf("capture-manifest: %q schema_version %q (want %q or %q)", path, m.SchemaVersion, captureManifestSchemaVersion, captureManifestSchemaVersionV2)
	}
	sum := sha256.Sum256(raw)
	ref := AssemblyCaptureManifest{
		Digest:        "sha256:" + hex.EncodeToString(sum[:]),
		ArtifactCount: m.ArtifactCount,
	}
	return ref, verify, nil
}

func validateCaptureArtifacts(arts []Artifact, verify *captureVerification) error {
	if verify == nil || verify.expectedArtifacts == nil {
		return nil
	}
	actual := make(map[string]*Artifact, len(arts))
	for i := range arts {
		actual[arts[i].ArtifactHash] = &arts[i]
	}
	expectedHashes := make([]string, 0, len(verify.expectedArtifacts))
	for hash := range verify.expectedArtifacts {
		expectedHashes = append(expectedHashes, hash)
	}
	sort.Strings(expectedHashes)
	for _, hash := range expectedHashes {
		if actual[hash] == nil {
			return fmt.Errorf("capture-manifest v2 artifact binding: expected artifact hash %q is missing from artifacts", hash)
		}
	}
	actualHashes := make([]string, 0, len(actual))
	for hash := range actual {
		actualHashes = append(actualHashes, hash)
	}
	sort.Strings(actualHashes)
	for _, hash := range actualHashes {
		if _, ok := verify.expectedArtifacts[hash]; !ok {
			return fmt.Errorf("capture-manifest v2 artifact binding: artifact hash %q is absent from the captured ledger", hash)
		}
	}
	for _, hash := range expectedHashes {
		expected := verify.expectedArtifacts[hash]
		artifact := actual[hash]
		if artifact.TraceID != expected.traceID {
			return fmt.Errorf("capture-manifest v2 artifact binding: artifact hash %q trace_id mismatch", hash)
		}
		if artifact.Trace.ID != expected.traceID {
			return fmt.Errorf("capture-manifest v2 artifact binding: artifact hash %q trace.id mismatch", hash)
		}
		if modelKey(artifact.CandidateModel) != expected.model {
			return fmt.Errorf("capture-manifest v2 artifact binding: artifact hash %q candidate_model mismatch", hash)
		}
		pairID, arm := "", ""
		if prefilledAssemblyMode(artifact.Trace) {
			arm = string(artifact.Trace.AssemblyEval.Mode)
			if artifact.Trace.AssemblyEval.Mode == AssemblyLegacy || artifact.Trace.AssemblyEval.Mode == AssemblyMixed {
				pairID = artifact.Trace.AssemblyEval.PairID
			}
		}
		if pairID != expected.pairID {
			return fmt.Errorf("capture-manifest v2 artifact binding: artifact hash %q pair_id mismatch", hash)
		}
		if arm != expected.arm {
			return fmt.Errorf("capture-manifest v2 artifact binding: artifact hash %q arm mismatch", hash)
		}
	}
	return nil
}

// validateCaptureLedger checks a v2 manifest's expected run ledger against
// its per_artifact rows and returns the deduplicated expected legacy-mixed
// pair keys (file order). Every violation is loud: the ledger is the audit
// trail for failed runs, so a corrupt or hand-edited one must never verify.
func validateCaptureLedger(m captureManifest) ([]assemblyPairKey, error) {
	if m.ExpectedCount != len(m.Expected) {
		return nil, fmt.Errorf("expected_count %d does not match its %d expected row(s)", m.ExpectedCount, len(m.Expected))
	}
	if len(m.Expected) == 0 {
		return nil, fmt.Errorf("v2 manifest has no expected ledger rows")
	}
	perArtifact := make(map[string]string, len(m.PerArtifact))
	for i, row := range m.PerArtifact {
		if strings.TrimSpace(row.TraceID) == "" || strings.TrimSpace(row.ArtifactHash) == "" {
			return nil, fmt.Errorf("per_artifact row %d: trace_id and artifact_hash must be nonblank", i)
		}
		if _, dup := perArtifact[row.ArtifactHash]; dup {
			return nil, fmt.Errorf("per_artifact row %d: duplicate artifact_hash %q", i, row.ArtifactHash)
		}
		perArtifact[row.ArtifactHash] = row.TraceID
	}
	seen := make(map[[2]string]struct{}, len(m.Expected))
	capturedHashes := make(map[string]struct{}, len(m.Expected))
	var pairs []assemblyPairKey
	seenPair := map[assemblyPairKey]struct{}{}
	for i, row := range m.Expected {
		if strings.TrimSpace(row.TraceID) == "" || strings.TrimSpace(row.Model) == "" {
			return nil, fmt.Errorf("expected row %d: trace_id and model must be nonblank", i)
		}
		key := [2]string{row.TraceID, modelKey(row.Model)}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("expected row %d: duplicate (trace_id, model) (%s, %s)", i, row.TraceID, row.Model)
		}
		seen[key] = struct{}{}
		if row.Attempts < 1 || row.Attempts > 2 {
			return nil, fmt.Errorf("expected row %d (%s/%s): attempts %d outside the registered 1..2 range (one initial attempt plus at most one in-run retry)", i, row.TraceID, row.Model, row.Attempts)
		}
		switch row.Arm {
		case "", string(AssemblyTopline):
			if row.PairID != "" {
				return nil, fmt.Errorf("expected row %d (%s/%s): pair_id %q on a non-paired arm %q", i, row.TraceID, row.Model, row.PairID, row.Arm)
			}
		case string(AssemblyLegacy), string(AssemblyMixed):
			if row.PairID == "" {
				return nil, fmt.Errorf("expected row %d (%s/%s): %s arm requires a pair_id", i, row.TraceID, row.Model, row.Arm)
			}
		default:
			return nil, fmt.Errorf("expected row %d (%s/%s): invalid arm %q", i, row.TraceID, row.Model, row.Arm)
		}
		switch row.Status {
		case "captured":
			if row.ArtifactHash == "" {
				return nil, fmt.Errorf("expected row %d (%s/%s): captured row without an artifact_hash", i, row.TraceID, row.Model)
			}
			if row.Error != "" {
				return nil, fmt.Errorf("expected row %d (%s/%s): captured row carries error %q", i, row.TraceID, row.Model, row.Error)
			}
			if _, dup := capturedHashes[row.ArtifactHash]; dup {
				return nil, fmt.Errorf("expected row %d (%s/%s): duplicate captured artifact_hash %q", i, row.TraceID, row.Model, row.ArtifactHash)
			}
			traceID, ok := perArtifact[row.ArtifactHash]
			if !ok {
				return nil, fmt.Errorf("expected row %d (%s/%s): captured hash %q is not in per_artifact", i, row.TraceID, row.Model, row.ArtifactHash)
			}
			if traceID != row.TraceID {
				return nil, fmt.Errorf("expected row %d (%s/%s): captured artifact_hash %q maps to per_artifact trace_id %q", i, row.TraceID, row.Model, row.ArtifactHash, traceID)
			}
			capturedHashes[row.ArtifactHash] = struct{}{}
		case "failed":
			if row.Attempts != 2 {
				return nil, fmt.Errorf("expected row %d (%s/%s): failed row attempts %d (want 2 after the in-run retry)", i, row.TraceID, row.Model, row.Attempts)
			}
			if row.ArtifactHash != "" {
				return nil, fmt.Errorf("expected row %d (%s/%s): failed row carries artifact_hash %q (failed runs produce no artifact)", i, row.TraceID, row.Model, row.ArtifactHash)
			}
			switch row.Error {
			case "<error: no-result>", "<error: timeout>", "<error: network>", "<error: parse>", "<error: other>":
			default:
				return nil, fmt.Errorf("expected row %d (%s/%s): failed row error %q is not a categorized error stub", i, row.TraceID, row.Model, row.Error)
			}
		default:
			return nil, fmt.Errorf("expected row %d (%s/%s): invalid status %q (want captured or failed)", i, row.TraceID, row.Model, row.Status)
		}
		if row.Arm == string(AssemblyLegacy) || row.Arm == string(AssemblyMixed) {
			k := assemblyPairKey{assemblyKindLegacyMixed, row.PairID, modelKey(row.Model)}
			if _, dup := seenPair[k]; !dup {
				seenPair[k] = struct{}{}
				pairs = append(pairs, k)
			}
		}
	}
	// Both directions: every per_artifact row must be vouched for by a
	// captured ledger row too, or the ledger under-reports the run.
	for _, row := range m.PerArtifact {
		if _, ok := capturedHashes[row.ArtifactHash]; !ok {
			return nil, fmt.Errorf("per_artifact hash %q has no captured row in the expected ledger", row.ArtifactHash)
		}
	}
	return pairs, nil
}

// writeCaptureManifest marshals and writes the manifest, then emits the
// manifest_digest line (sha256 of the file bytes) to stdout. Any failure
// here fails the capture loudly — an assembly capture without its manifest
// cannot be verified by the registered report.
func writeCaptureManifest(path string, m captureManifest, stdout io.Writer) error {
	raw, err := marshalCaptureManifest(m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("calibrate-capture: write manifest: %w", err)
	}
	emitCaptureManifestDigest(raw, stdout)
	return nil
}

func marshalCaptureManifest(m captureManifest) ([]byte, error) {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("calibrate-capture: marshal manifest: %w", err)
	}
	return append(raw, '\n'), nil
}

func emitCaptureManifestDigest(raw []byte, stdout io.Writer) {
	sum := sha256.Sum256(raw)
	if stdout == nil {
		stdout = os.Stdout
	}
	_, _ = fmt.Fprintf(stdout, "manifest_digest sha256:%s\n", hex.EncodeToString(sum[:]))
}

// runCalibrateCapture replays each (trace, candidate) cell — retrying every
// failed cell exactly once in-run (the registered one-retry policy) — and
// writes one Artifact per captured cell to OutputPath as JSONL. Artifacts
// have no ExpectedAnswerQuality — labels are a separate file the operator
// hand-edits. The v2 manifest ledger derives from the requested
// (target x trace) universe, so a cell the runner never answered for still
// appears as failed, and an all-failed assembly run still writes its
// manifest before erroring: evidence must persist.
func runCalibrateCapture(ctx context.Context, opts calibrateCaptureOptions) error {
	if opts.Runner == nil {
		return fmt.Errorf("calibrate-capture: nil runner")
	}
	if strings.TrimSpace(opts.OutputPath) == "" {
		return fmt.Errorf("calibrate-capture: empty output path")
	}
	if len(opts.Traces) == 0 {
		return errors.New("calibrate-capture: no traces")
	}
	if len(opts.Targets) == 0 {
		return errors.New("calibrate-capture: no targets")
	}
	seenTargets := make(map[[2]string]struct{}, len(opts.Targets))
	for _, target := range opts.Targets {
		key := canonicalModelTargetKey(target)
		if _, ok := seenTargets[key]; ok {
			return fmt.Errorf("calibrate-capture: duplicate model target %q", target.Display)
		}
		seenTargets[key] = struct{}{}
	}
	modelDigests := make(map[string]string, len(opts.ModelDigests))
	for _, target := range opts.Targets {
		if canonicalModelTargetKey(target)[0] != defaultBenchProvider {
			continue
		}
		selector := normalizeModelSelector(target.Display)
		raw := opts.ModelDigests[selector]
		if raw == "" {
			continue
		}
		digest := canonicalSHA256Digest(raw)
		if digest == "" {
			fmt.Fprintf(os.Stderr, "calibrate-capture: digest resolution skipped for %q: invalid digest\n", target.Display)
			continue
		}
		modelDigests[selector] = digest
	}
	traceByID := make(map[string]Trace, len(opts.Traces))
	for _, trace := range opts.Traces {
		if _, ok := traceByID[trace.ID]; ok {
			return fmt.Errorf("calibrate-capture: duplicate trace ID %q", trace.ID)
		}
		traceByID[trace.ID] = trace
	}
	ordered, capturedOrders := counterbalanceCaptureTraces(opts.Traces)
	orderIndex := make(map[string]int, len(ordered))
	for i, trace := range ordered {
		orderIndex[trace.ID] = i
	}
	// The run ledger derives from the REQUESTED (target x trace) universe,
	// never from whatever results came back (external PR review round 2 P2):
	// a cell the runner silently dropped still appears — as failed — in the
	// manifest.
	type captureCell struct {
		trace    Trace
		res      *Result
		attempts int
	}
	type cellKey struct{ model, trace string }
	cells := make(map[cellKey]*captureCell, len(opts.Targets)*len(ordered))
	cellOrder := make([]cellKey, 0, len(opts.Targets)*len(ordered))
	for _, target := range opts.Targets {
		for _, trace := range ordered {
			k := cellKey{target.Display, trace.ID}
			cells[k] = &captureCell{trace: trace}
			cellOrder = append(cellOrder, k)
		}
	}
	results, err := opts.Runner.RunAll(ctx, opts.Targets, ordered)
	if err != nil {
		return fmt.Errorf("calibrate-capture: run: %s", redactErrorMessage(err.Error()))
	}
	for i := range results {
		r := &results[i]
		c, ok := cells[cellKey{r.Model, r.TraceID}]
		if !ok {
			return fmt.Errorf("calibrate-capture: result %s/%s has no matching trace context", r.TraceID, r.Model)
		}
		if c.res != nil {
			return fmt.Errorf("calibrate-capture: duplicate result for %s/%s", r.TraceID, r.Model)
		}
		c.res = r
		c.attempts = 1
	}
	// One in-run retry per failed cell (the registered one-retry policy,
	// external PR review round 2 P2): each target's failed or unanswered
	// traces are re-run once, in their original counterbalanced order.
	// Attempts records 1 (captured first try) or 2 (retried, either outcome).
	for _, target := range opts.Targets {
		var retryTraces []Trace
		for _, trace := range ordered {
			if c := cells[cellKey{target.Display, trace.ID}]; c.res == nil || c.res.Err != nil {
				retryTraces = append(retryTraces, trace)
			}
		}
		if len(retryTraces) == 0 {
			continue
		}
		retryResults, err := opts.Runner.RunAll(ctx, []ModelTarget{target}, retryTraces)
		if err != nil {
			return fmt.Errorf("calibrate-capture: retry run: %s", redactErrorMessage(err.Error()))
		}
		pending := make(map[cellKey]bool, len(retryTraces))
		for _, trace := range retryTraces {
			k := cellKey{target.Display, trace.ID}
			pending[k] = true
			cells[k].attempts = 2 // the retry was attempted, result or not
		}
		for i := range retryResults {
			r := &retryResults[i]
			k := cellKey{r.Model, r.TraceID}
			// Only the retried cells may be updated (canned test runners can
			// replay unrelated results), and only once each.
			if pending[k] {
				cells[k].res = r
				pending[k] = false
			}
		}
	}
	now := func() time.Time { return time.Now().UTC() }
	if opts.Clock != nil {
		now = opts.Clock
	}
	var artifacts []Artifact
	var manifestRows []captureManifestRow
	expected := make([]captureExpectedRow, 0, len(cellOrder))
	failed := 0
	for _, k := range cellOrder {
		c := cells[k]
		row := captureExpectedRow{TraceID: k.trace, Model: k.model, Status: "captured", Attempts: c.attempts}
		if prefilledAssemblyMode(c.trace) {
			row.Arm = string(c.trace.AssemblyEval.Mode)
			switch c.trace.AssemblyEval.Mode {
			case AssemblyLegacy, AssemblyMixed:
				row.PairID = c.trace.AssemblyEval.PairID
			}
		}
		switch {
		case c.res == nil:
			// The runner returned no Result for this cell on either attempt.
			fmt.Fprintf(os.Stderr, "calibrate-capture: skipped %s/%s: runner returned no result\n", k.trace, k.model)
			failed++
			row.Status = "failed"
			row.Error = "<error: no-result>"
		case c.res.Err != nil:
			// Skip failed runs — they cannot be labeled coherently.
			// Surface the per-pair reason: a silently dropped result makes a
			// partial corpus look complete and hides timeout-vs-refusal.
			fmt.Fprintf(os.Stderr, "calibrate-capture: skipped %s/%s: %s\n", k.trace, k.model, redactErrorMessage(c.res.Err.Error()))
			failed++
			row.Status = "failed"
			// The manifest is committed evidence and an openai-compat error can
			// embed arbitrary server response text (up to 64 KiB, API keys
			// included) — record only the redactErrorMessage category stub,
			// never the raw text.
			row.Error = redactErrorMessage(c.res.Err.Error())
		default:
			r := *c.res
			artifact := Artifact{
				TraceID:           r.TraceID,
				CandidateModel:    r.Model,
				Trace:             c.trace,
				ActualFinalAnswer: lastAssistantContent(r.Transcript),
				ActualToolCalls:   extractToolNames(r.Transcript),
				ActualTranscript:  r.Transcript,
				CapturedAt:        now(),
				Capture:           captureProvenance(c.trace, r, orderIndex, capturedOrders, modelDigests),
			}
			artifact.ArtifactHash = artifactHash(artifact)
			artifacts = append(artifacts, artifact)
			row.ArtifactHash = artifact.ArtifactHash
			manifestRows = append(manifestRows, captureManifestRow{
				TraceID:      r.TraceID,
				ArtifactHash: artifact.ArtifactHash,
				OrderIndex:   orderIndex[r.TraceID],
				// Zero counts mean "usage not reported" (see the KNOWN GAP on
				// CaptureProvenance), so presence requires both counts non-zero.
				UsagePresent: r.Score.PromptEvalTokens > 0 && r.Score.GenTokens > 0,
			})
		}
		expected = append(expected, row)
	}
	var artifactRaw bytes.Buffer
	artifactEncoder := json.NewEncoder(&artifactRaw)
	for _, artifact := range artifacts {
		if err := artifactEncoder.Encode(artifact); err != nil {
			return fmt.Errorf("calibrate-capture: encode artifact %s/%s: %w", artifact.TraceID, artifact.CandidateModel, err)
		}
	}
	// Assembly captures REQUIRE the run manifest (#331 W3): the registered
	// report verifies pairs against it, so a capture that cannot write it
	// fails loudly. Non-assembly captures stay manifest-free.
	var manifestPath string
	var manifestRaw []byte
	if anyPrefilledAssemblyTrace(opts.Traces) {
		endpoint, transport := captureEndpointTransport(opts.Targets, opts.OllamaURL, opts.OpenAICompatBaseURL)
		targets := make([]captureManifestTarget, 0, len(opts.Targets))
		for _, target := range opts.Targets {
			targets = append(targets, captureManifestTarget{
				Selector:       target.Display,
				ResolvedDigest: modelDigests[normalizeModelSelector(target.Display)],
			})
		}
		manifest := captureManifest{
			SchemaVersion:        captureManifestSchemaVersionV2,
			CreatedAt:            now(),
			Endpoint:             endpoint,
			Transport:            transport,
			ModelTargets:         targets,
			ServerProbe:          captureServerProbe{OllamaDigests: modelDigests},
			Decoding:             captureDecoding{Temperature: assemblyCaptureTemperature, SeedSupported: false},
			CounterbalanceScheme: captureCounterbalanceScheme,
			ArtifactCount:        len(manifestRows),
			PerArtifact:          manifestRows,
			ExpectedCount:        len(expected),
			Expected:             expected,
		}
		manifestPath = captureManifestPath(opts.OutputPath)
		var err error
		manifestRaw, err = marshalCaptureManifest(manifest)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(opts.OutputPath), 0o755); err != nil {
		return fmt.Errorf("calibrate-capture: mkdir: %w", err)
	}
	replacements := []filePublication{{target: opts.OutputPath, data: artifactRaw.Bytes(), mode: 0o600}}
	if manifestPath != "" {
		replacements = append(replacements, filePublication{target: manifestPath, data: manifestRaw, mode: 0o600})
	}
	publication, err := publishFileSet(replacements, nil)
	if err != nil {
		return fmt.Errorf("calibrate-capture: publish evidence: %w", err)
	}
	writeFilePublicationWarnings(os.Stderr, "calibrate-capture", publication)
	if manifestPath != "" {
		emitCaptureManifestDigest(manifestRaw, opts.Stdout)
	}
	if failed > 0 && len(artifacts) > 0 {
		// Partial capture is a valid best-effort outcome (the caller can gap-fill
		// the missing pairs), so this is a warning, not an error — but it must be
		// loud: a partial corpus that prints only the success line reads as complete.
		fmt.Fprintf(os.Stderr, "calibrate-capture: WARNING partial capture — wrote %d artifact(s), %d of %d runs failed\n", len(artifacts), failed, len(cellOrder))
	}
	if len(artifacts) == 0 {
		return fmt.Errorf("calibrate-capture: no artifacts written; %d run(s) attempted, %d failed", len(cellOrder), failed)
	}
	return nil
}

// counterbalanceCaptureTraces returns the deterministic capture order for a
// mixed trace set — non-assembly traces (and 3a flat/progressive arms) first
// in their input order, then legacy/mixed pairs in the registered
// counterbalance order (counterbalancePairOrder: FNV-1a-64(PairID) ascending,
// first arm alternating), then topline traces sorted by trace ID — plus the
// pair-ID -> captured-order label map the pair ordering was derived from, so
// the recorded captured_order provenance can never disagree with the actual
// order. A set with no legacy/mixed/topline traces is returned untouched,
// preserving today's capture order byte-for-byte.
func counterbalanceCaptureTraces(traces []Trace) ([]Trace, map[string]string) {
	var plain, topline []Trace
	var pairIDs []string
	pairs := map[string][]Trace{}
	for _, t := range traces {
		if !prefilledAssemblyMode(t) {
			plain = append(plain, t)
			continue
		}
		switch t.AssemblyEval.Mode {
		case AssemblyLegacy, AssemblyMixed:
			id := t.AssemblyEval.PairID
			if _, ok := pairs[id]; !ok {
				pairIDs = append(pairIDs, id)
			}
			pairs[id] = append(pairs[id], t)
		default: // AssemblyTopline
			topline = append(topline, t)
		}
	}
	// Only pairs with BOTH arms present are counterbalanced (and labeled):
	// a single-arm pair has no within-pair order to balance, so it must not
	// carry a captured_order label or consume an alternation slot.
	var completeIDs, incompleteIDs []string
	for _, id := range pairIDs {
		if pairArmsComplete(pairs[id]) {
			completeIDs = append(completeIDs, id)
		} else {
			incompleteIDs = append(incompleteIDs, id)
		}
	}
	pairOrder, labels := counterbalancePairOrder(completeIDs)
	incompleteOrder, _ := counterbalancePairOrder(incompleteIDs) // hash order only; labels discarded
	pairOrder = append(pairOrder, incompleteOrder...)
	if len(pairOrder) == 0 && len(topline) == 0 {
		return traces, labels
	}
	sort.Slice(topline, func(i, j int) bool { return topline[i].ID < topline[j].ID })
	out := make([]Trace, 0, len(traces))
	out = append(out, plain...)
	for _, id := range pairOrder {
		first := AssemblyLegacy
		if labels[id] == "mixed-first" {
			first = AssemblyMixed
		}
		for _, t := range pairs[id] {
			if t.AssemblyEval.Mode == first {
				out = append(out, t)
			}
		}
		for _, t := range pairs[id] {
			if t.AssemblyEval.Mode != first {
				out = append(out, t)
			}
		}
	}
	return append(out, topline...), labels
}

// pairArmsComplete reports whether a pair's collected traces include at
// least one legacy AND one mixed arm.
func pairArmsComplete(ts []Trace) bool {
	var hasLegacy, hasMixed bool
	for _, t := range ts {
		switch t.AssemblyEval.Mode {
		case AssemblyLegacy:
			hasLegacy = true
		case AssemblyMixed:
			hasMixed = true
		}
	}
	return hasLegacy && hasMixed
}

// pairIDHash is the registered counterbalance hash: FNV-1a 64 of the PairID.
func pairIDHash(pairID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(pairID))
	return h.Sum64()
}

// captureCounterbalanceScheme names the registered counterbalance rule for
// the capture manifest. Single source: counterbalancePairOrder implements
// exactly this sentence.
const captureCounterbalanceScheme = "pairs sorted by fnv1a64(pair_id) ascending; first arm alternates by position (even=legacy-first, odd=mixed-first)"

// counterbalancePairOrder is the REGISTERED balanced counterbalance (#331
// W3): pair IDs sorted by FNV-1a-64(PairID) ascending (PairID breaks the
// astronomically unlikely hash tie), then the first arm ALTERNATES by
// position — even position => "legacy-first", odd => "mixed-first". Balanced
// within 1 by construction, unlike the retired per-pair hash parity, which
// could skew arbitrarily far on an unlucky ID set.
func counterbalancePairOrder(pairIDs []string) ([]string, map[string]string) {
	ordered := append([]string(nil), pairIDs...)
	sort.Slice(ordered, func(i, j int) bool {
		hi, hj := pairIDHash(ordered[i]), pairIDHash(ordered[j])
		if hi != hj {
			return hi < hj
		}
		return ordered[i] < ordered[j]
	})
	labels := make(map[string]string, len(ordered))
	for i, id := range ordered {
		if i%2 == 1 {
			labels[id] = "mixed-first"
		} else {
			labels[id] = "legacy-first"
		}
	}
	return ordered, labels
}

// captureProvenance builds the controlled-capture provenance for one
// result, or nil for non-assembly traces so their artifacts stay
// byte-identical to the pre-3c shape. Prompt/gen tokens come from the
// replay usage carried on Result.Score; capturedOrders is the label map the
// actual capture order was derived from (counterbalanceCaptureTraces), the
// single source for CapturedOrder.
func captureProvenance(trace Trace, r Result, orderIndex map[string]int, capturedOrders map[string]string, digests map[string]string) *CaptureProvenance {
	if !prefilledAssemblyMode(trace) {
		return nil
	}
	temp := assemblyCaptureTemperature
	prov := &CaptureProvenance{
		OrderIndex:   orderIndex[trace.ID],
		Temperature:  &temp,
		Transport:    r.CandidateProvider,
		Model:        r.Model,
		ModelDigest:  digests[normalizeModelSelector(r.Model)],
		PromptTokens: r.Score.PromptEvalTokens,
		GenTokens:    r.Score.GenTokens,
	}
	switch trace.AssemblyEval.Mode {
	case AssemblyLegacy, AssemblyMixed:
		prov.CapturedOrder = capturedOrders[trace.AssemblyEval.PairID]
	}
	return prov
}

// matchedLabel pairs a Label with its current Artifact so the calibration
// loop can re-score the frozen output without re-running the candidate.
type matchedLabel struct {
	Label    Label
	Artifact Artifact
}

// loadLabelsMatchedAgainst loads both files and partitions labels into
// (matched, stale). A label is "stale" iff its ArtifactHash is not present
// in artifactsPath. Stale labels are not errors — they're reported and
// excluded from agreement.
func loadLabelsMatchedAgainst(labelsPath, artifactsPath string) ([]matchedLabel, []Label, error) {
	arts, err := loadArtifacts(artifactsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load artifacts: %w", err)
	}
	labels, err := loadLabels(labelsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load labels: %w", err)
	}
	return matchLabels(labels, arts)
}

// matchLabels partitions already-loaded labels into (matched, stale) against
// the artifact set. A label is "matched" when its ArtifactHash is present in
// arts and "stale" otherwise; stale labels are reported, not errors. Duplicate
// matched ArtifactHash values are rejected so each artifact contributes at most
// once. Kept pure (no I/O) so the paired-report path can load artifacts once
// and retain the full set for lineup/gap detection.
func matchLabels(labels []Label, arts []Artifact) ([]matchedLabel, []Label, error) {
	byHash := make(map[string]Artifact, len(arts))
	for _, a := range arts {
		byHash[a.ArtifactHash] = a
	}
	var matched []matchedLabel
	var stale []Label
	seenMatched := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		a, ok := byHash[l.ArtifactHash]
		if !ok {
			stale = append(stale, l)
			continue
		}
		if _, ok := seenMatched[l.ArtifactHash]; ok {
			return nil, nil, fmt.Errorf("duplicate label for artifact_hash %q", l.ArtifactHash)
		}
		seenMatched[l.ArtifactHash] = struct{}{}
		matched = append(matched, matchedLabel{Label: l, Artifact: a})
	}
	return matched, stale, nil
}

// requireJSONDecoderEOF asserts a json.Decoder has consumed its entire input:
// exactly one more Decode must return io.EOF. Decoder.More() cannot do this —
// it reports false at a closing ']' or '}', so a JSONL file (or single-value
// document) with a stray bracket or brace after the final value would pass a
// More()-based end check silently (external PR review P3). Whitespace-only
// tails still pass (Decode returns io.EOF).
func requireJSONDecoderEOF(dec *json.Decoder, kind string) error {
	var trailing json.RawMessage
	switch err := dec.Decode(&trailing); {
	case err == nil:
		return fmt.Errorf("%s: trailing data after the final value", kind)
	case errors.Is(err, io.EOF):
		return nil
	default:
		return fmt.Errorf("%s: trailing data after the final value: %w", kind, err)
	}
}

func loadArtifacts(path string) ([]Artifact, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	var out []Artifact
	seen := make(map[string]struct{})
	dec := json.NewDecoder(f)
	for dec.More() {
		var a Artifact
		if err := dec.Decode(&a); err != nil {
			return nil, fmt.Errorf("decode artifact: %w", err)
		}
		if want := artifactHash(a); a.ArtifactHash != want {
			return nil, fmt.Errorf("artifact %d: artifact_hash mismatch: got %q, want %q",
				len(out)+1, a.ArtifactHash, want)
		}
		if _, ok := seen[a.ArtifactHash]; ok {
			return nil, fmt.Errorf("artifact %d: duplicate artifact_hash %q", len(out)+1, a.ArtifactHash)
		}
		seen[a.ArtifactHash] = struct{}{}
		out = append(out, a)
	}
	if err := requireJSONDecoderEOF(dec, "artifacts"); err != nil {
		return nil, err
	}
	return out, nil
}

func loadLabels(path string) ([]Label, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	var out []Label
	dec := json.NewDecoder(f)
	for dec.More() {
		var l Label
		if err := dec.Decode(&l); err != nil {
			return nil, fmt.Errorf("decode label: %w", err)
		}
		// This is the single domain guard for every downstream mode (manual,
		// paired, calibration, discrimination): an out-of-domain value aborts the
		// load. KNOWN GAP: an omitted expected_answer_quality field JSON-decodes
		// to 0.0 — a valid score — so a forgotten label is indistinguishable from
		// a deliberate 0.0. Closing it needs a presence signal (*float64) on
		// Label; deferred because it touches the shared label schema.
		if !validExpectedAnswerQuality(l.ExpectedAnswerQuality) {
			return nil, fmt.Errorf("invalid expected_answer_quality for label %s/%s: %.2f (want one of 0.0, 0.5, 1.0)",
				l.TraceID, l.CandidateModel, l.ExpectedAnswerQuality)
		}
		out = append(out, l)
	}
	if err := requireJSONDecoderEOF(dec, "labels"); err != nil {
		return nil, err
	}
	return out, nil
}

func validExpectedAnswerQuality(v float64) bool {
	return v == 0 || v == 0.5 || v == 1
}

const (
	calibrationAgreementDelta          = 0.25
	calibrationPassThreshold           = 0.85
	calibrationBorderlinePassThreshold = 0.80
	calibrationDefaultMinLabels        = 50
)

type calibrationAgreementMode string

const (
	calibrationAgreementExact     calibrationAgreementMode = "exact"
	calibrationAgreementTolerance calibrationAgreementMode = "tolerance"
)

func normalizeCalibrationAgreementMode(mode calibrationAgreementMode) (calibrationAgreementMode, error) {
	if mode == "" {
		return calibrationAgreementExact, nil
	}
	switch mode {
	case calibrationAgreementExact, calibrationAgreementTolerance:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid calibrate agreement mode %q (want exact or tolerance)", mode)
	}
}

type calibrationSubsetSummary struct {
	Count         int
	AgreeCount    int
	AgreementRate float64
}

type knownFixtureOutcome struct {
	TraceID  string
	Expected float64
	Judge    float64
	Present  bool
	Pass     bool
}

var knownSubtleBugFixtureIDs = []string{"fa-f03", "fa-c05", "fa-g04"}

// normalizeFixtureTraceID maps a captured trace ID to its bare conversation
// id so known-bug fixtures match regardless of the conversationTraceIDPrefix
// that capture prepends (see conversationToTrace).
func normalizeFixtureTraceID(traceID string) string {
	return strings.TrimPrefix(traceID, conversationTraceIDPrefix)
}

// calibrateOptions configures runCalibrate.
//
// MinLabels defaults to calibrationDefaultMinLabels (50) when zero so a
// sufficient sample backs the agreement verdict. StabilityRuns, when >1,
// runs the judge exactly N times per artifact and reports the max-min agreement
// spread as a diagnostic; it does NOT gate the PASS/FAIL verdict. Clock is
// an optional time source for deterministic report filenames.
type calibrateOptions struct {
	LabelsPath    string
	ArtifactsPath string
	Scorer        Scorer
	JudgeModel    string
	JudgeProvider string // provider instance identity for report provenance (e.g. "ollama", "openai-compat:<endpoint-id>")
	ReportDir     string
	MinLabels     int // defaults to calibrationDefaultMinLabels when zero
	StabilityRuns int // when >1, runs the judge exactly N times per artifact and reports max-min spread as a diagnostic (does NOT gate the PASS/FAIL verdict); uses bypass-cache (Task 23)
	AgreementMode calibrationAgreementMode
	Clock         func() time.Time
}

// CalibrationResult is the in-memory summary returned by runCalibrate. The
// markdown report contains the same data plus per-label rows.
type CalibrationResult struct {
	JudgeModel               string
	JudgeProvider            string
	MatchedCount             int
	StaleCount               int
	SelfJudgedSkipCount      int
	AgreeCount               int
	AgreementRate            float64
	AgreementMode            calibrationAgreementMode
	ToleranceAgreeCount      int
	ToleranceAgreementRate   float64
	Overall                  calibrationSubsetSummary
	R1Anchor                 calibrationSubsetSummary
	Borderline               calibrationSubsetSummary
	ClearOne                 calibrationSubsetSummary
	HarshDisagreementCount   int
	LenientDisagreementCount int
	KnownFixtures            map[string]knownFixtureOutcome
	StratifiedGateFailures   []string
	MinLabels                int
	StabilityRuns            int
	Verdict                  string // PASS / FAIL / INSUFFICIENT_LABELS
	ReportPath               string
	PerLabel                 []perLabelOutcome
}

// perLabelOutcome is one row of the calibration report: a label paired with
// the judge's verdict on the same frozen artifact. StabilitySpread is
// reserved for Task 20 (multi-run judge spread) and zero otherwise.
type perLabelOutcome struct {
	TraceID         string
	CandidateModel  string
	Expected        float64
	Judge           float64
	Delta           float64
	Agree           bool
	ToleranceAgree  bool
	LabelNotes      string
	StabilitySpread float64
}

// runCalibrate loads (labels, artifacts), invokes the scorer once per
// matched (label, artifact) pair, and computes the agreement rate against
// human ExpectedAnswerQuality values. The verdict is INSUFFICIENT_LABELS
// when MatchedCount < MinLabels, otherwise PASS only when overall agreement
// reaches calibrationPassThreshold (0.85) and all stratified gates pass.
//
// The synthesized Trace inside the loop uses a placeholder System and a
// single empty user turn so Scorer.Score's signature is satisfied. The
// frozen ActualTranscript from the Artifact is what the scorer actually
// reads from; live LLMJudgeScorer would need a transcript with an
// assistant final answer, which the captured Artifact provides.
//
// When ReportDir is non-empty the markdown report is written and its path
// stored on the returned result; otherwise no report is emitted.
func runCalibrate(ctx context.Context, opts calibrateOptions) (CalibrationResult, error) {
	if opts.Scorer == nil {
		return CalibrationResult{}, errors.New("calibrate: nil scorer")
	}
	agreementMode, err := normalizeCalibrationAgreementMode(opts.AgreementMode)
	if err != nil {
		return CalibrationResult{}, err
	}
	if opts.MinLabels == 0 {
		opts.MinLabels = calibrationDefaultMinLabels
	}
	matched, stale, err := loadLabelsMatchedAgainst(opts.LabelsPath, opts.ArtifactsPath)
	if err != nil {
		return CalibrationResult{}, err
	}
	res := CalibrationResult{
		JudgeModel:    opts.JudgeModel,
		JudgeProvider: opts.JudgeProvider,
		StaleCount:    len(stale),
		MinLabels:     opts.MinLabels,
		StabilityRuns: opts.StabilityRuns,
		AgreementMode: agreementMode,
		KnownFixtures: make(map[string]knownFixtureOutcome, len(knownSubtleBugFixtureIDs)),
	}
	for _, id := range knownSubtleBugFixtureIDs {
		res.KnownFixtures[id] = knownFixtureOutcome{TraceID: id, Pass: true}
	}
	for _, m := range matched {
		if sameModelSelector(opts.JudgeModel, m.Artifact.CandidateModel) {
			res.SelfJudgedSkipCount++
			continue
		}
		trace, traceErr := calibrationTraceFromArtifact(m.Artifact)
		if traceErr != nil {
			return CalibrationResult{}, traceErr
		}
		actual := Result{
			Model:      m.Artifact.CandidateModel,
			TraceID:    m.Artifact.TraceID,
			Transcript: m.Artifact.ActualTranscript,
		}
		scorer := calibrationScorer(opts.Scorer, opts.StabilityRuns)
		score, scoreErr := scorer.Score(ctx, trace, actual)
		if scoreErr != nil {
			return CalibrationResult{}, fmt.Errorf("calibrate: scorer for %s/%s: %w",
				m.Artifact.TraceID, m.Artifact.CandidateModel, scoreErr)
		}
		delta := score.AnswerQuality - m.Label.ExpectedAnswerQuality
		if delta < 0 {
			delta = -delta
		}
		exactAgree := score.AnswerQuality == m.Label.ExpectedAnswerQuality
		toleranceAgree := delta <= calibrationAgreementDelta
		agree := exactAgree
		if agreementMode == calibrationAgreementTolerance {
			agree = toleranceAgree
		}
		outcome := perLabelOutcome{
			TraceID:        m.Artifact.TraceID,
			CandidateModel: m.Artifact.CandidateModel,
			Expected:       m.Label.ExpectedAnswerQuality,
			Judge:          score.AnswerQuality,
			Delta:          delta,
			Agree:          agree,
			ToleranceAgree: toleranceAgree,
			LabelNotes:     m.Label.LabelNotes,
		}
		if opts.StabilityRuns > 1 {
			scores := []float64{score.AnswerQuality}
			for i := 1; i < opts.StabilityRuns; i++ {
				extra, sErr := scorer.Score(ctx, trace, actual)
				if sErr != nil {
					return CalibrationResult{}, fmt.Errorf("calibrate: stability run %d for %s/%s: %w",
						i+1, m.Artifact.TraceID, m.Artifact.CandidateModel, sErr)
				}
				scores = append(scores, extra.AnswerQuality)
			}
			outcome.StabilitySpread = maxFloat(scores) - minFloat(scores)
		}
		res.PerLabel = append(res.PerLabel, outcome)
		if outcome.Agree {
			res.AgreeCount++
		}
		if outcome.ToleranceAgree {
			res.ToleranceAgreeCount++
		}
		addSubsetOutcome(&res.Overall, outcome.Agree)
		if labelHasToken(outcome.LabelNotes, "r1-anchor") {
			addSubsetOutcome(&res.R1Anchor, outcome.Agree)
		}
		if outcome.Expected == 0 || outcome.Expected == 0.5 {
			addSubsetOutcome(&res.Borderline, outcome.Agree)
		}
		if outcome.Expected == 1 {
			addSubsetOutcome(&res.ClearOne, outcome.Agree)
		}
		if !outcome.Agree {
			if outcome.Judge < outcome.Expected {
				res.HarshDisagreementCount++
			} else if outcome.Judge > outcome.Expected {
				res.LenientDisagreementCount++
			}
		}
		// Known subtle-bug fixtures gate only the bug instances (expected < 1.0)
		// and match on the normalized (prefix-stripped) trace id. A fixture fails
		// if ANY of its bug artifacts is judged a perfect 1.0; record the most
		// concerning (highest-judge) one for the report.
		if outcome.Expected < 1 {
			normID := normalizeFixtureTraceID(outcome.TraceID)
			if fixture, ok := res.KnownFixtures[normID]; ok {
				if !fixture.Present || outcome.Judge > fixture.Judge {
					fixture.Expected = outcome.Expected
					fixture.Judge = outcome.Judge
				}
				fixture.Present = true
				if outcome.Judge == 1 {
					fixture.Pass = false
				}
				res.KnownFixtures[normID] = fixture
			}
		}
		res.MatchedCount++
	}
	if res.MatchedCount > 0 {
		res.AgreementRate = float64(res.AgreeCount) / float64(res.MatchedCount)
		res.ToleranceAgreementRate = float64(res.ToleranceAgreeCount) / float64(res.MatchedCount)
	}
	finalizeSubset(&res.Overall)
	finalizeSubset(&res.R1Anchor)
	finalizeSubset(&res.Borderline)
	finalizeSubset(&res.ClearOne)
	res.StratifiedGateFailures = calibrationStratifiedGateFailures(res)
	switch {
	case res.MatchedCount < opts.MinLabels:
		res.Verdict = "INSUFFICIENT_LABELS"
	case res.AgreementRate < calibrationPassThreshold:
		res.Verdict = "FAIL"
	case len(res.StratifiedGateFailures) > 0:
		res.Verdict = "FAIL"
	default:
		res.Verdict = "PASS"
	}
	if opts.ReportDir != "" {
		path, writeErr := writeCalibrationReport(opts.ReportDir, opts.JudgeModel, res, opts.Clock)
		if writeErr != nil {
			return res, fmt.Errorf("calibrate: write report: %w", writeErr)
		}
		res.ReportPath = path
	}
	return res, nil
}

func labelHasToken(notes, token string) bool {
	for _, field := range strings.FieldsFunc(strings.ToLower(notes), func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	}) {
		if field == token {
			return true
		}
	}
	return false
}

func addSubsetOutcome(s *calibrationSubsetSummary, agree bool) {
	s.Count++
	if agree {
		s.AgreeCount++
	}
}

func finalizeSubset(s *calibrationSubsetSummary) {
	if s.Count > 0 {
		s.AgreementRate = float64(s.AgreeCount) / float64(s.Count)
	}
}

func calibrationStratifiedGateFailures(res CalibrationResult) []string {
	var failures []string
	if res.Borderline.Count > 0 && res.Borderline.AgreementRate < calibrationBorderlinePassThreshold {
		failures = append(failures, fmt.Sprintf("borderline/fail agreement %.0f%% below %.0f%% threshold (%d/%d)",
			res.Borderline.AgreementRate*100, calibrationBorderlinePassThreshold*100, res.Borderline.AgreeCount, res.Borderline.Count))
	}
	for _, id := range knownSubtleBugFixtureIDs {
		fixture := res.KnownFixtures[id]
		if fixture.Present && !fixture.Pass {
			failures = append(failures, fmt.Sprintf("known subtle-bug fixture %s judged 1.0", id))
		}
	}
	return failures
}

func calibrationScorer(s Scorer, stabilityRuns int) Scorer {
	if stabilityRuns <= 1 {
		return s
	}
	if cs, ok := s.(*LLMJudgeScorer); ok {
		clone := *cs
		clone.BypassCache = true
		return &clone
	}
	return s
}

func calibrationTraceFromArtifact(a Artifact) (Trace, error) {
	trace := a.Trace
	if strings.TrimSpace(trace.ID) == "" {
		return Trace{}, fmt.Errorf("calibrate: artifact %s/%s missing original trace context", a.TraceID, a.CandidateModel)
	}
	if trace.ID != a.TraceID {
		return Trace{}, fmt.Errorf("calibrate: artifact %s/%s trace ID mismatch: %q", a.TraceID, a.CandidateModel, trace.ID)
	}
	if strings.TrimSpace(trace.System) == "" {
		return Trace{}, fmt.Errorf("calibrate: artifact %s/%s missing original trace system prompt", a.TraceID, a.CandidateModel)
	}
	if len(trace.Turns) == 0 {
		return Trace{}, fmt.Errorf("calibrate: artifact %s/%s missing original trace turns", a.TraceID, a.CandidateModel)
	}
	if strings.TrimSpace(trace.Golden.FinalAnswerCriteria) == "" && strings.TrimSpace(trace.Golden.FinalAnswerSubstring) == "" {
		return Trace{}, fmt.Errorf("calibrate: artifact %s/%s missing original trace golden rubric", a.TraceID, a.CandidateModel)
	}
	return trace, nil
}

// slugifyModel makes a path-safe slug from a model selector by replacing
// '/' and ':' with '_'. Example: "ollama/gemma4:31b" -> "ollama_gemma4_31b".
func slugifyModel(s string) string {
	out := strings.ReplaceAll(s, "/", "_")
	return strings.ReplaceAll(out, ":", "_")
}

// writeCalibrationReport emits a markdown calibration report to dir with a
// slugified, timestamped filename and returns the report path. Clock is the
// time source used to stamp the filename and report header; defaults to
// time.Now().UTC() when nil.
func writeCalibrationReport(dir, judgeModel string, res CalibrationResult, clock func() time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", dir, err)
	}
	now := time.Now().UTC()
	if clock != nil {
		now = clock().UTC()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Judge calibration — %s — %s\n\n", judgeModel, now.Format("2006-01-02"))
	if provider := strings.TrimSpace(res.JudgeProvider); provider != "" {
		fmt.Fprintf(&b, "Judge provider: %s\n", provider)
	}
	fmt.Fprintf(&b, "Matched labels: %d / %d required → %s\n", res.MatchedCount, res.MinLabels, sufficiencyLabel(res))
	fmt.Fprintf(&b, "Stale labels: %d\n", res.StaleCount)
	fmt.Fprintf(&b, "Self-judged labels skipped: %d\n", res.SelfJudgedSkipCount)
	fmt.Fprintf(&b, "Agreement: %d / %d (%.0f%%) → **%s**\n\n",
		res.AgreeCount, res.MatchedCount, res.AgreementRate*100, res.Verdict)
	fmt.Fprintf(&b, "Agreement mode: %s\n", res.AgreementMode)
	fmt.Fprintf(&b, "Old tolerance diagnostic: %d / %d (%.0f%%) would have agreed under |judge - expected| <= %.2f\n\n",
		res.ToleranceAgreeCount, res.MatchedCount, res.ToleranceAgreementRate*100, calibrationAgreementDelta)
	fmt.Fprintln(&b, "| subset | agreement |")
	fmt.Fprintln(&b, "|---|---|")
	fmt.Fprintf(&b, "| overall | %s |\n", formatSubsetAgreement(res.Overall))
	fmt.Fprintf(&b, "| R1-60 anchor | %s |\n", formatSubsetAgreement(res.R1Anchor))
	fmt.Fprintf(&b, "| Borderline/fail subset | %s |\n", formatSubsetAgreement(res.Borderline))
	fmt.Fprintf(&b, "| clear-1.0 | %s |\n", formatSubsetAgreement(res.ClearOne))
	fmt.Fprintf(&b, "\nHarsh disagreements: %d\n", res.HarshDisagreementCount)
	fmt.Fprintf(&b, "Lenient disagreements: %d\n\n", res.LenientDisagreementCount)
	if len(res.StratifiedGateFailures) == 0 {
		fmt.Fprintln(&b, "Stratified gate failures: none")
		fmt.Fprintln(&b)
	} else {
		fmt.Fprintln(&b, "Stratified gate failures:")
		for _, failure := range res.StratifiedGateFailures {
			fmt.Fprintf(&b, "- %s\n", failure)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, "## Known subtle-bug fixtures")
	fmt.Fprintln(&b, "| trace_id | expected | judge | roll-call |")
	fmt.Fprintln(&b, "|---|---|---|---|")
	for _, id := range knownSubtleBugFixtureIDs {
		f := res.KnownFixtures[id]
		status := "MISSING"
		if f.Present && f.Pass {
			status = "PASS"
		} else if f.Present {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "| %s | %.2f | %.2f | %s |\n", id, f.Expected, f.Judge, status)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| trace_id | candidate | expected | judge | Δ | agree | stability |")
	fmt.Fprintln(&b, "|---|---|---|---|---|---|---|")
	for _, p := range res.PerLabel {
		agree := "✗"
		if p.Agree {
			agree = "✓"
		}
		stability := "n/a"
		if res.StabilityRuns > 1 {
			stability = fmt.Sprintf("spread=%.2f", p.StabilitySpread)
		}
		fmt.Fprintf(&b, "| %s | %s | %.2f | %.2f | %.2f | %s | %s |\n",
			p.TraceID, p.CandidateModel, p.Expected, p.Judge, p.Delta, agree, stability)
	}
	f, path, err := createCalibrationReportFile(dir, judgeModel, now)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close %q: %w", path, err)
	}
	return path, nil
}

func formatSubsetAgreement(s calibrationSubsetSummary) string {
	if s.Count == 0 {
		return "0 / 0 (n/a)"
	}
	return fmt.Sprintf("%d / %d (%.0f%%)", s.AgreeCount, s.Count, s.AgreementRate*100)
}

func createCalibrationReportFile(dir, judgeModel string, now time.Time) (*os.File, string, error) {
	for attempt := 0; ; attempt++ {
		path := calibrationReportPath(dir, judgeModel, now, attempt)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, path, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return nil, "", fmt.Errorf("create %q: %w", path, err)
	}
}

func calibrationReportPath(dir, judgeModel string, now time.Time, attempt int) string {
	stem := fmt.Sprintf("%s-%s", now.Format("2006-01-02T150405Z"), slugifyModel(judgeModel))
	if attempt > 0 {
		stem = fmt.Sprintf("%s-%d", stem, attempt+1)
	}
	return filepath.Join(dir, stem+".md")
}

// sufficiencyLabel renders the matched-vs-required label-count state as a
// short string for the report header.
func sufficiencyLabel(res CalibrationResult) string {
	if res.MatchedCount >= res.MinLabels {
		return "SUFFICIENT"
	}
	return "INSUFFICIENT"
}

func maxFloat(vs []float64) float64 {
	m := vs[0]
	for _, v := range vs[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func minFloat(vs []float64) float64 {
	m := vs[0]
	for _, v := range vs[1:] {
		if v < m {
			m = v
		}
	}
	return m
}
