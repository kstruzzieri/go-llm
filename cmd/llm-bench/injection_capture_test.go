package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

type scriptedCaptureRunner struct {
	rounds [][]Result
	i      int
}

func (s *scriptedCaptureRunner) RunAll(_ context.Context, _ []ModelTarget, _ []Trace) ([]Result, error) {
	r := s.rounds[s.i]
	s.i++
	return r, nil
}

func TestInjectionCaptureWritesV3AndKeepsEveryCell(t *testing.T) {
	tr := validInjectionTrace()
	dir := t.TempDir()
	out := filepath.Join(dir, "art.jsonl")
	runner := &scriptedCaptureRunner{rounds: [][]Result{
		{{Model: "m", TraceID: tr.ID, Err: errors.New("dial tcp: timeout")}},
		{{Model: "m", TraceID: tr.ID, Err: errors.New("dial tcp: timeout")}},
	}}
	err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: runner, Targets: []ModelTarget{{Display: "m", Provider: openAICompatTransport, Model: "m"}},
		Traces: []Trace{tr}, OutputPath: out, Clock: func() time.Time { return time.Unix(0, 0).UTC() },
		OpenAICompatBaseURL: "http://127.0.0.1:8090", RegistrationDigest: "sha256:abc",
	})
	if err == nil {
		t.Fatal("an all-failed capture must still return an error after publishing the ledger")
	}
	m, err := readCaptureManifestV3(captureManifestPath(out))
	if err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != captureManifestSchemaVersionV3 || m.RegistrationDigest != "sha256:abc" {
		t.Fatalf("manifest header %+v", m)
	}
	if len(m.Expected) != 1 || m.Expected[0].Status != "failed" || len(m.Expected[0].AttemptErrors) != 2 {
		t.Fatalf("failed cell must keep both attempts: %+v", m.Expected)
	}
}

func TestArtifactHashStableForNonInjection(t *testing.T) {
	a := Artifact{TraceID: "t", CandidateModel: "m", Trace: Trace{ID: "t"}}
	before := artifactHash(a)
	a.ActualRawToolCalls = nil
	if artifactHash(a) != before {
		t.Fatal("nil raw calls changed the hash of existing artifacts")
	}
	a.ActualRawToolCalls = []injectionRawCall{{Name: "x", Arguments: "{}", DecodeOK: true}}
	if artifactHash(a) == before {
		t.Fatal("raw calls are not covered by the artifact hash")
	}
}

func TestRefuseJudgeOnInjection(t *testing.T) {
	if err := refuseJudgeOnInjection("llm-judge", []Trace{validInjectionTrace()}); err == nil {
		t.Fatal("llm-judge accepted an injection trace")
	}
	if err := refuseJudgeOnInjection("exact-match", []Trace{validInjectionTrace()}); err != nil {
		t.Fatal(err)
	}
}

// RunAll may return partial results together with a cancellation. The
// completed cell must survive; the never-run cell is "unattempted", not failed.
type cancelingRunner struct{ done Result }

func (c cancelingRunner) RunAll(context.Context, []ModelTarget, []Trace) ([]Result, error) {
	return []Result{c.done}, context.Canceled
}

func TestInjectionCapturePublishesPartialOnCancel(t *testing.T) {
	a := validInjectionTrace()
	b := validInjectionTrace()
	b.ID = "inj-src-exfil-plain-helpful"
	out := filepath.Join(t.TempDir(), "art.jsonl")
	runner := cancelingRunner{done: Result{Model: "m", TraceID: a.ID, Transcript: []Turn{{Role: "assistant", Content: "answer"}}}}
	err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: runner, Targets: []ModelTarget{{Display: "m", Provider: openAICompatTransport, Model: "m"}},
		Traces: []Trace{a, b}, OutputPath: out, Clock: func() time.Time { return time.Unix(0, 0).UTC() },
		OpenAICompatBaseURL: "http://127.0.0.1:8090", RegistrationDigest: "sha256:abc",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must surface after publishing: %v", err)
	}
	m, rerr := readCaptureManifestV3(captureManifestPath(out))
	if rerr != nil {
		t.Fatal(rerr)
	}
	got := map[string]string{}
	for _, r := range m.Expected {
		got[r.TraceID] = fmt.Sprintf("%s/%d", r.Status, r.Attempts)
	}
	if got[a.ID] != "captured/1" || got[b.ID] != "unattempted/0" {
		t.Fatalf("ledger = %v, want captured/1 and unattempted/0", got)
	}
	arts, aerr := loadArtifacts(out)
	if aerr != nil || len(arts) != 1 || arts[0].TraceID != a.ID {
		t.Fatalf("completed artifact lost: %v %+v", aerr, arts)
	}
}

// The registered decoding reaches the wire on the real capture path.
func TestInjectionCaptureSendsRegisteredDecoding(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	sel := "openai-compat/fixture-model"
	temp := 0.0
	runner := &Runner{OpenAICompatBaseURL: srv.URL, Timeout: 10 * time.Second, Scorer: &CaptureScorer{},
		InjectionDecoding: map[string]injectionDecoding{sel: {Options: provider.ModelOptions{Temperature: &temp, NumPredict: 4096}}}}
	target, err := parseModelTarget(sel) // target.go:37; Display is the full selector
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.RunAll(context.Background(), []ModelTarget{target}, []Trace{validInjectionTrace()})
	if err != nil || len(res) != 1 || res[0].Err != nil {
		t.Fatalf("run: %v %+v", err, res)
	}
	// Independently expected wire fields: openaicompat sends NumPredict as
	// max_tokens and Temperature as temperature (confirm the key names in
	// provider/openaicompat/openaicompat.go near :740 before running).
	if body["temperature"] != 0.0 || body["max_tokens"] != 4096.0 {
		t.Fatalf("registered decoding not on the wire: temperature=%v max_tokens=%v", body["temperature"], body["max_tokens"])
	}
}

// The registered think_mode, not the transport default, decides whether the
// capture splits inline <think> out of the content (matches Golem's parser).
func TestInjectionCaptureRespectsRegisteredThinkMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"<think>reason</think>answer"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	yes := true
	temp := 0.0
	arms := []struct {
		name         string
		dec          injectionDecoding
		wantThinking string
		wantContent  string
	}{
		{"none passes tags through", injectionDecoding{Options: provider.ModelOptions{Temperature: &temp, NumPredict: 64}, ThinkMode: provider.ThinkNone}, "", "<think>reason</think>answer"},
		{"toggle enabled extracts", injectionDecoding{Options: provider.ModelOptions{Temperature: &temp, NumPredict: 64, Think: &yes}, ThinkMode: provider.ThinkToggle}, "reason", "answer"},
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			sel := "openai-compat/fixture-model"
			runner := &Runner{OpenAICompatBaseURL: srv.URL, Timeout: 10 * time.Second, Scorer: &CaptureScorer{},
				InjectionDecoding: map[string]injectionDecoding{sel: arm.dec}}
			target, err := parseModelTarget(sel)
			if err != nil {
				t.Fatal(err)
			}
			res, err := runner.RunAll(context.Background(), []ModelTarget{target}, []Trace{validInjectionTrace()})
			if err != nil || len(res) != 1 || res[0].Err != nil || len(res[0].Transcript) != 1 {
				t.Fatalf("run: %v %+v", err, res)
			}
			turn := res[0].Transcript[0]
			if turn.Thinking != arm.wantThinking || turn.Content != arm.wantContent {
				t.Fatalf("transcript turn = (%q,%q), want (%q,%q)", turn.Thinking, turn.Content, arm.wantThinking, arm.wantContent)
			}
		})
	}
}

func TestInjectionRunFailsWithoutRegisteredDecoding(t *testing.T) {
	runner := &Runner{OpenAICompatBaseURL: "http://127.0.0.1:1", Scorer: &CaptureScorer{}}
	target, _ := parseModelTarget("openai-compat/unregistered")
	res, _ := runner.RunAll(context.Background(), []ModelTarget{target}, []Trace{validInjectionTrace()})
	if len(res) != 1 || res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "no registered decoding") {
		t.Fatalf("injection trace ran without registered decoding: %+v", res)
	}
}

func hostedFixture() map[string]any {
	return map[string]any{"endpoint": "https://example.invalid/v1", "capture_window": "w", "think_fields": "reasoning_content",
		"parser_policy": "p", "tool_call_envelope": "openai", "time_limit": "30m", "session_header_policy": "none",
		"unavailable_controls": []string{}}
}

func validRegistrationJSON(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	reg := map[string]any{
		"schema": injectionRegistrationSchema, "canary_guid": injectionCanaryGUID,
		"cases_sha256": "abc", "builder_revision": "b1", "scorer_revision": "s1",
		"census": []map[string]string{
			{"id": "t1", "sha256": "h1", "population": "corpus"},
			{"id": "t2", "sha256": "h2", "population": "control"},
			{"id": "t3", "sha256": "h3", "population": "corpus"},
		},
		"models": []string{"openai-compat/a", "openai-compat/kimi"}, "baseline": "openai-compat/a",
		"decoding": map[string]any{
			"openai-compat/a":    map[string]any{"temperature": 0.0, "num_predict": 4096, "serving_stack": "llama.cpp", "think_mode": "none"},
			"openai-compat/kimi": map[string]any{"temperature": 0.0, "num_predict": 4096, "serving_stack": "hosted:opencode-zen", "think_mode": "none", "hosted": hostedFixture()},
		},
		"decoder_policy": map[string]any{"encodings": []string{"literal", "base64", "hex"},
			"max_value_bytes": decodeMaxValueBytes, "max_total_bytes": decodeMaxTotalBytes, "max_depth": decodeMaxDepth, "min_token": decodeMinToken},
		"estimand": "e", "incomplete_policy": "i", "retry_policy": "r", "deferred_trigger": 0.2, "claim_gates": []string{"g"},
	}
	if mutate != nil {
		mutate(reg)
	}
	raw, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "reg.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadInjectionRegistration(t *testing.T) {
	p := validRegistrationJSON(t, nil)
	reg, digest, err := loadInjectionRegistration(p)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	sum := sha256.Sum256(raw)
	if want := "sha256:" + hex.EncodeToString(sum[:]); digest != want {
		t.Fatalf("digest %q, want %q", digest, want)
	}
	if got := strings.Join(reg.corpusIDs(), ","); got != "t1,t3" {
		t.Fatalf("corpusIDs = %q, want t1,t3", got)
	}
	po := reg.Decoding["openai-compat/a"].providerOptions()
	if po.Temperature == nil || *po.Temperature != 0 || po.NumPredict != 4096 {
		t.Fatalf("providerOptions = %+v", po)
	}
}

func TestLoadInjectionRegistrationRejects(t *testing.T) {
	cases := map[string]func(map[string]any){
		"wrong canary":    func(r map[string]any) { r["canary_guid"] = "x" },
		"unknown field":   func(r map[string]any) { r["surprise"] = 1 },
		"baseline absent": func(r map[string]any) { r["baseline"] = "openai-compat/zzz" },
		"model no decoding": func(r map[string]any) {
			delete(r["decoding"].(map[string]any), "openai-compat/a")
		},
		"hosted stack without hosted block": func(r map[string]any) {
			delete(r["decoding"].(map[string]any)["openai-compat/kimi"].(map[string]any), "hosted")
		},
		"nonzero temperature": func(r map[string]any) {
			r["decoding"].(map[string]any)["openai-compat/a"].(map[string]any)["temperature"] = 0.7
		},
		"blank hosted endpoint": func(r map[string]any) {
			r["decoding"].(map[string]any)["openai-compat/kimi"].(map[string]any)["hosted"].(map[string]any)["endpoint"] = "  "
		},
		"missing hosted unavailable_controls": func(r map[string]any) {
			delete(r["decoding"].(map[string]any)["openai-compat/kimi"].(map[string]any)["hosted"].(map[string]any), "unavailable_controls")
		},
		"unknown think_mode": func(r map[string]any) {
			r["decoding"].(map[string]any)["openai-compat/a"].(map[string]any)["think_mode"] = "sometimes"
		},
		"missing think_mode": func(r map[string]any) {
			delete(r["decoding"].(map[string]any)["openai-compat/a"].(map[string]any), "think_mode")
		},
		"none with think set": func(r map[string]any) {
			r["decoding"].(map[string]any)["openai-compat/a"].(map[string]any)["think"] = true
		},
		"toggle with think nil": func(r map[string]any) {
			r["decoding"].(map[string]any)["openai-compat/a"].(map[string]any)["think_mode"] = "toggle"
		},
		"decoder policy drift": func(r map[string]any) {
			r["decoder_policy"].(map[string]any)["max_depth"] = decodeMaxDepth + 1
		},
		"duplicate census id": func(r map[string]any) {
			r["census"] = []map[string]string{{"id": "t1", "sha256": "h", "population": "corpus"}, {"id": "t1", "sha256": "h", "population": "control"}}
		},
		"bad population": func(r map[string]any) {
			r["census"] = []map[string]string{{"id": "t1", "sha256": "h", "population": "other"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := loadInjectionRegistration(validRegistrationJSON(t, mutate)); err == nil {
				t.Fatal("registration accepted")
			}
		})
	}
}

func TestValidateCaptureLedgerV3AttemptRules(t *testing.T) {
	tr := validInjectionTrace()
	base := func() captureManifest {
		return captureManifest{
			SchemaVersion: captureManifestSchemaVersionV3, RegistrationDigest: "sha256:abc",
			ModelTargets:    []captureManifestTarget{{Selector: "m"}},
			RequestedTraces: []captureRequestedTrace{{TraceID: tr.ID}},
			ExpectedCount:   1,
		}
	}
	cases := map[string]struct {
		row  captureExpectedRow
		want bool
	}{
		"failed, two attempts, two errors": {captureExpectedRow{TraceID: tr.ID, Model: "m", Status: "failed", Attempts: 2, AttemptErrors: []string{"<error: timeout>", "<error: network>"}, Error: "<error: network>"}, true},
		"failed, one attempt, one error":   {captureExpectedRow{TraceID: tr.ID, Model: "m", Status: "failed", Attempts: 1, AttemptErrors: []string{"<error: timeout>"}, Error: "<error: timeout>"}, true},
		"failed, error count mismatch":     {captureExpectedRow{TraceID: tr.ID, Model: "m", Status: "failed", Attempts: 2, AttemptErrors: []string{"<error: timeout>"}, Error: "<error: timeout>"}, false},
		"unattempted ok":                   {captureExpectedRow{TraceID: tr.ID, Model: "m", Status: "unattempted"}, true},
		"unattempted with attempts":        {captureExpectedRow{TraceID: tr.ID, Model: "m", Status: "unattempted", Attempts: 1}, false},
		"unattempted with errors":          {captureExpectedRow{TraceID: tr.ID, Model: "m", Status: "unattempted", AttemptErrors: []string{"<error: other>"}}, false},
		"unattempted with hash":            {captureExpectedRow{TraceID: tr.ID, Model: "m", Status: "unattempted", ArtifactHash: "sha256:x"}, false},
		"uncategorized attempt error":      {captureExpectedRow{TraceID: tr.ID, Model: "m", Status: "failed", Attempts: 1, AttemptErrors: []string{"dial tcp 10.0.0.1: boom"}, Error: "<error: network>"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := base()
			m.Expected = []captureExpectedRow{tc.row}
			err := validateCaptureLedgerV3(m)
			if (err == nil) != tc.want {
				t.Fatalf("validateCaptureLedgerV3 err=%v, want ok=%v", err, tc.want)
			}
		})
	}
	// captured: attempts-1 errors
	m := base()
	m.PerArtifact = []captureManifestRow{{TraceID: tr.ID, ArtifactHash: "sha256:h", ProvenanceHash: "sha256:" + strings.Repeat("a", 64)}}
	m.ArtifactCount = 1
	m.Expected = []captureExpectedRow{{TraceID: tr.ID, Model: "m", Status: "captured", Attempts: 2, AttemptErrors: []string{"<error: timeout>"}, ArtifactHash: "sha256:h"}}
	if err := validateCaptureLedgerV3(m); err != nil {
		t.Fatalf("captured after one failed attempt rejected: %v", err)
	}
	m.Expected[0].AttemptErrors = nil
	if err := validateCaptureLedgerV3(m); err == nil {
		t.Fatal("captured row with attempts=2 and no attempt error accepted")
	}
}

func TestInjectionAndAssemblyTracesCannotShareCapture(t *testing.T) {
	asm := Trace{ID: "asm", AssemblyEval: &AssemblyEval{Mode: AssemblyTopline}}
	err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: &scriptedCaptureRunner{}, Targets: []ModelTarget{{Display: "m", Provider: openAICompatTransport, Model: "m"}},
		Traces: []Trace{validInjectionTrace(), asm}, OutputPath: filepath.Join(t.TempDir(), "a.jsonl"), RegistrationDigest: "sha256:abc",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot share one capture") {
		t.Fatalf("mixed capture err = %v", err)
	}
}

func TestInjectionCaptureRequiresRegistrationDigest(t *testing.T) {
	err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: &scriptedCaptureRunner{}, Targets: []ModelTarget{{Display: "m", Provider: openAICompatTransport, Model: "m"}},
		Traces: []Trace{validInjectionTrace()}, OutputPath: filepath.Join(t.TempDir(), "a.jsonl"),
	})
	if err == nil || !strings.Contains(err.Error(), "registration") {
		t.Fatalf("capture without digest err = %v", err)
	}
}

func TestRegistrationBindCaptureFailsClosed(t *testing.T) {
	tr := validInjectionTrace()
	reg := injectionRegistration{
		Census:   []registeredTrace{{ID: tr.ID, SHA256: canonicalTraceHash(tr), Population: "corpus"}},
		Decoding: map[string]registeredDecoding{"openai-compat/a": {NumPredict: 128, ThinkMode: "none"}},
	}
	targets := []ModelTarget{{Display: "openai-compat/a"}}
	dec, err := reg.bindCapture([]Trace{tr}, targets)
	if err != nil || dec["openai-compat/a"].Options.NumPredict != 128 {
		t.Fatalf("bind: %v %+v", err, dec)
	}
	tampered := tr
	tampered.System = "changed"
	if _, err := reg.bindCapture([]Trace{tampered}, targets); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	other := tr
	other.ID = "not-in-census"
	if _, err := reg.bindCapture([]Trace{other}, targets); err == nil {
		t.Fatal("unknown trace accepted")
	}
	if _, err := reg.bindCapture([]Trace{tr}, []ModelTarget{{Display: "openai-compat/zzz"}}); err == nil {
		t.Fatal("unregistered model accepted")
	}
}
