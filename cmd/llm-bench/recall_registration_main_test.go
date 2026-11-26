//go:build !windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCalibrateCaptureRecallNeedsRegistration(t *testing.T) {
	tr := validRecallTrace()
	code, log := runLLMBenchMain(t, nil, "-calibrate-capture", "-traces", writeTraceFiles(t, tr), "-models", "openai-compat/gemma4:31b",
		"-candidate-base-url", "http://127.0.0.1:1", "-labels-out", filepath.Join(t.TempDir(), "a.jsonl"))
	if code != 1 || !strings.Contains(log, "recall traces requires -registration") {
		t.Fatalf("exit %d output %q", code, log)
	}
}

// Selecting a registered filename for a gap fill must never send another
// cell. A matching filename and an arbitrary alias both proceed to the
// independent destination check, which these cases intentionally fail.
func TestCalibrateCaptureRecallRegisteredFilenames(t *testing.T) {
	const id = "recall-p01-B-s2-1"
	raw, err := os.ReadFile(filepath.Join(recallFixtureDir, "pilot", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, want string }{
		{"recall-p01-B-s1-1.json", `selects trace "recall-p01-B-s1-1" but contains "recall-p01-B-s2-1"`},
		{"RECALL-P01-B-S1-1.JSON", `selects trace "recall-p01-B-s1-1" but contains "recall-p01-B-s2-1"`},
		{id + ".json", "differs from the registered loopback endpoint"},
		{strings.ToUpper(id + ".json"), "differs from the registered loopback endpoint"},
		{"alias.json", "differs from the registered loopback endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			code, log := runLLMBenchMain(t, nil, "-calibrate-capture", "-traces", path,
				"-registration", filepath.Join(recallFixtureDir, "registration-pilot.json"), "-models", recallPrimaryModel,
				"-candidate-base-url", "http://127.0.0.1:1", "-labels-out", filepath.Join(dir, "artifacts.jsonl"))
			if code != 1 || !strings.Contains(log, tc.want) {
				t.Fatalf("exit %d, output %q, want %q", code, log, tc.want)
			}
		})
	}
}

// writeTraceFiles writes each trace to its own file in a fresh directory and
// returns the -traces glob that matches them.
func writeTraceFiles(t *testing.T, traces ...Trace) string {
	t.Helper()
	dir := t.TempDir()
	for i, tr := range traces {
		raw, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("t%d.json", i)), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "*.json")
}

// Mixed corpora are refused by main before any network call. Every trace is
// registered and the ollama target has a registered decoding, so main's mix
// refusal and bindCapture's (which would also refuse the non-recall trace) both
// precede the /api/show round trip; main's runs first, and the assertions below
// pin its message.
func TestCalibrateCaptureRecallMixRefusedBeforeNetwork(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); http.Error(w, "no", 500) }))
	t.Cleanup(srv.Close)
	rec := validRecallTrace()
	assembly := Trace{ID: "asm", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}, AssemblyEval: &AssemblyEval{PairID: "p", Mode: AssemblyTopline}}
	inj := validInjectionTrace()
	ollama := "ollama/qwen3:8b"
	for name, other := range map[string]Trace{"injection": inj, "assembly": assembly} {
		reg := recallRegistrationFile(t, []Trace{rec, other}, func(r map[string]any) {
			r["models"] = append(recallRegModels[:len(recallRegModels):len(recallRegModels)], ollama)
			r["decoding"].(map[string]any)[ollama] = r["decoding"].(map[string]any)[recallPrimaryModel]
		})
		code, log := runLLMBenchMain(t, nil, "-calibrate-capture", "-traces", writeTraceFiles(t, rec, other), "-models", ollama, "-ollama-url", srv.URL,
			"-registration", reg, "-labels-out", filepath.Join(t.TempDir(), "a.jsonl"))
		if code != 1 || !strings.Contains(log, "recall traces cannot share one capture") {
			t.Errorf("%s mix: exit %d output %q", name, code, log)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("server saw %d request(s) before the refusal", n)
	}
	// runCalibrateCapture refuses the same mixes on its own.
	for name, other := range map[string]Trace{"injection": inj, "assembly": assembly} {
		err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
			Runner: &scriptedCaptureRunner{}, Targets: []ModelTarget{{Display: "m", Provider: openAICompatTransport, Model: "m"}},
			Traces: []Trace{rec, other}, OutputPath: filepath.Join(t.TempDir(), "a.jsonl"), RegistrationDigest: "sha256:abc",
			InjectionTemperatures: map[string]float64{"m": 0},
		})
		if err == nil || !strings.Contains(err.Error(), "recall traces cannot share one capture") {
			t.Errorf("%s mix in runCalibrateCapture: %v", name, err)
		}
	}
}

func TestCalibrateCaptureRegistrationRefusedWithoutFrozenTraces(t *testing.T) {
	code, log := runLLMBenchMain(t, nil, "-calibrate-capture", "-traces", writeTraceFiles(t, Trace{ID: "z", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}}),
		"-models", "ollama/qwen3:8b", "-registration", "x.json", "-labels-out", filepath.Join(t.TempDir(), "a.jsonl"))
	if code != 1 || !strings.Contains(log, "-registration applies only to injection or recall traces") {
		t.Fatalf("exit %d output %q", code, log)
	}
}

// The real CLI path: main loads the recall registration (not the injection
// one), binds it, and the registered decoding reaches the wire and the manifest.
func TestCalibrateCaptureRecallByMain(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		body.Store(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","object":"chat.completion","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"retry.go\"}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	tr := validRecallTrace()
	regPath := recallRegistrationFile(t, []Trace{tr}, recallTestEndpoint(srv.URL))
	out := filepath.Join(t.TempDir(), "art.jsonl")
	code, log := runLLMBenchMain(t, nil, "-calibrate-capture", "-traces", writeTraceFiles(t, tr), "-models", recallPrimaryModel,
		"-candidate-base-url", srv.URL, "-registration", regPath, "-labels-out", out)
	if code != 0 {
		t.Fatalf("exit %d output %q", code, log)
	}
	b, _ := body.Load().(map[string]any)
	if b["temperature"] != 0.0 || b["max_tokens"] != 4096.0 {
		t.Fatalf("registered decoding not on the wire: %v", b)
	}
	raw, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	m, err := readCaptureManifestV3(captureManifestPath(out))
	if err != nil || m.RegistrationDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("manifest digest: %v %+v", err, m)
	}
	arts, err := loadArtifacts(out)
	if err != nil || len(arts) != 1 || len(arts[0].ActualRawToolCalls) != 1 {
		t.Fatalf("artifacts: %v %+v", err, arts)
	}
}

func TestRecallCaptureRefusesUnregisteredEndpoint(t *testing.T) {
	tr := validRecallTrace()
	corpus := writeTraceFiles(t, tr)
	reg := recallRegistrationFile(t, []Trace{tr}, nil)
	for _, endpoint := range []string{"https://example.invalid", "http://127.0.0.1:1"} {
		out := filepath.Join(t.TempDir(), "art.jsonl")
		code, log := runLLMBenchMain(t, nil, "-calibrate-capture", "-traces", corpus,
			"-models", recallPrimaryModel, "-candidate-base-url", endpoint, "-registration", reg, "-labels-out", out)
		if code != 1 || !strings.Contains(log, "differs from the registered loopback endpoint") {
			t.Fatalf("endpoint %q: exit %d, output %q", endpoint, code, log)
		}
		for _, p := range []string{out, captureManifestPath(out)} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatalf("refused capture left %q: %v", p, err)
			}
		}
	}
}
