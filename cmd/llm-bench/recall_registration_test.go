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
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// recallRegModels is the registered lineup (primary first).
var recallRegModels = []string{
	"openai-compat/gemma4:31b", "openai-compat/qwen3-coder-next:latest",
	"openai-compat/qwen3.6:35b-a3b", "openai-compat/qwen3.8:27b",
}

// recallRegistrationFile writes a valid registration over traces (mutate may
// change the JSON map first) and returns its path.
func recallRegistrationFile(t *testing.T, traces []Trace, mutate func(map[string]any)) string {
	t.Helper()
	rows := []map[string]string{}
	for _, tr := range traces {
		rows = append(rows, map[string]string{"id": tr.ID, "sha256": canonicalTraceHash(tr)})
	}
	dec := map[string]any{}
	for _, m := range recallRegModels {
		dec[m] = map[string]any{"temperature": 0.0, "num_predict": 4096, "think": false, "think_mode": "toggle",
			"serving_stack": "llama.cpp via llama-swap 127.0.0.1:8090"}
	}
	reg := map[string]any{
		"schema": recallRegistrationSchema, "split": "pilot", "cases_sha256": "sha256:" + strings.Repeat("ab", 32),
		"scorer_revision": recallScorerRevision, "rule_text": recallDecisionRuleText, "primary": recallPrimaryModel,
		"models": recallRegModels, "decoding": dec, "traces": rows,
	}
	if mutate != nil {
		mutate(reg)
	}
	raw, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "recall-reg.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadRecallRegistration(t *testing.T) {
	tr := validRecallTrace()
	dec := func(r map[string]any) map[string]any { return r["decoding"].(map[string]any) }
	setDec := func(r map[string]any, k, v any) {
		dec(r)["openai-compat/qwen3.8:27b"].(map[string]any)[k.(string)] = v
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string // empty = valid
	}{
		{"valid", nil, ""},
		{"rule text differs", func(r map[string]any) { r["rule_text"] = recallDecisionRuleText + " " }, "rule_text"},
		{"scorer revision differs", func(r map[string]any) { r["scorer_revision"] = "recall-scorer/v0" }, "scorer_revision"},
		{"primary differs", func(r map[string]any) { r["primary"] = "openai-compat/qwen3.8:27b" }, "primary"},
		{"primary not in models", func(r map[string]any) {
			r["models"] = recallRegModels[1:]
			delete(dec(r), recallPrimaryModel)
		}, "is not in models"},
		{"model missing from decoding", func(r map[string]any) { delete(dec(r), "openai-compat/qwen3.8:27b") }, "has no decoding"},
		{"repeated model", func(r map[string]any) {
			r["models"] = []string{recallRegModels[0], recallRegModels[0], recallRegModels[2], recallRegModels[3]}
		}, "listed twice"},
		{"decoding key not in models", func(r map[string]any) {
			dec(r)["openai-compat/extra"] = dec(r)[recallPrimaryModel]
		}, "not in models"},
		{"decoding fails validate", func(r map[string]any) { setDec(r, "think_mode", "bogus") }, "think_mode"},
		{"temperature not 0", func(r map[string]any) { setDec(r, "temperature", 0.7) }, "temperature must be 0"},
		{"hosted model", func(r map[string]any) {
			setDec(r, "serving_stack", "hosted:x")
			setDec(r, "hosted", hostedFixture())
		}, "hosted serving stacks are out of scope"},
		{"thinking on", func(r map[string]any) { setDec(r, "think", true) }, "think must be false"},
		{"thinking unset", func(r map[string]any) {
			setDec(r, "think_mode", "auto")
			delete(dec(r)["openai-compat/qwen3.8:27b"].(map[string]any), "think")
		}, "think must be false"},
		{"num_predict not 4096", func(r map[string]any) { setDec(r, "num_predict", 64) }, "num_predict must be 4096"},
		{"wrong schema", func(r map[string]any) { r["schema"] = "x" }, "schema"},
		{"bad split", func(r map[string]any) { r["split"] = "dev" }, "split"},
		{"cases_sha256 empty", func(r map[string]any) { r["cases_sha256"] = "" }, "cases_sha256"},
		{"cases_sha256 not a digest", func(r map[string]any) { r["cases_sha256"] = "abc" }, "cases_sha256"},
		{"cases_sha256 uppercase", func(r map[string]any) { r["cases_sha256"] = "sha256:" + strings.Repeat("AB", 32) }, "cases_sha256"},
		{"unknown field", func(r map[string]any) { r["extra"] = 1 }, "unknown field"},
		{"empty traces", func(r map[string]any) { r["traces"] = []map[string]string{} }, "traces is empty"},
		{"trace row without id", func(r map[string]any) { r["traces"] = []map[string]string{{"id": "", "sha256": "h"}} }, "id and sha256 are required"},
		{"trace row without sha256", func(r map[string]any) { r["traces"] = []map[string]string{{"id": "x", "sha256": " "}} }, "id and sha256 are required"},
		{"trace row with population", func(r map[string]any) {
			r["traces"] = []map[string]string{{"id": "x", "sha256": "h", "population": "recall"}}
		}, "population"},
		{"duplicate trace id", func(r map[string]any) {
			rows := r["traces"].([]map[string]string)
			r["traces"] = append(rows, rows[0])
		}, "duplicate id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, digest, err := loadRecallRegistration(recallRegistrationFile(t, []Trace{tr}, tc.mutate))
			if tc.want == "" {
				if err != nil || !strings.HasPrefix(digest, "sha256:") || reg.Primary != recallPrimaryModel {
					t.Fatalf("valid registration: %v %q %+v", err, digest, reg)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "recall registration") {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	t.Run("trailing data", func(t *testing.T) {
		p := recallRegistrationFile(t, []Trace{tr}, nil)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(raw, []byte(" {}")...), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadRecallRegistration(p); err == nil || !strings.Contains(err.Error(), "trailing data") {
			t.Fatalf("err = %v, want trailing data", err)
		}
	})
	t.Run("digest is over the file bytes", func(t *testing.T) {
		p := recallRegistrationFile(t, []Trace{tr}, nil)
		raw, _ := os.ReadFile(p)
		sum := sha256.Sum256(raw)
		if _, d, err := loadRecallRegistration(p); err != nil || d != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Fatalf("digest %q err %v", d, err)
		}
	})
}

func TestRecallRegistrationBindCaptureFailsClosed(t *testing.T) {
	tr := validRecallTrace()
	reg, _, err := loadRecallRegistration(recallRegistrationFile(t, []Trace{tr}, nil))
	if err != nil {
		t.Fatal(err)
	}
	target := ModelTarget{Display: recallPrimaryModel, Provider: openAICompatTransport, Model: "gemma4:31b"}
	got, err := reg.bindCapture([]Trace{tr}, []ModelTarget{target})
	if err != nil || got[recallPrimaryModel].Options.Temperature == nil || *got[recallPrimaryModel].Options.Temperature != 0 {
		t.Fatalf("bind: %v %+v", err, got)
	}
	other := validRecallTrace()
	other.ID = "recall-other"
	plain := Trace{ID: tr.ID, System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}}
	benign := validInjectionTrace()
	benign.ID = tr.ID
	confirm := validRecallTrace()
	confirm.Recall.Split = "confirmatory"
	edited := validRecallTrace()
	edited.System = "edited"
	// A registered sha written with the sha256: prefix still binds, and a subset
	// of the registered traces and models binds (as the injection bindCapture).
	prefixed, _, err := loadRecallRegistration(recallRegistrationFile(t, []Trace{tr}, func(r map[string]any) {
		rows := r["traces"].([]map[string]string)
		rows[0]["sha256"] = "sha256:" + rows[0]["sha256"]
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prefixed.bindCapture([]Trace{tr}, []ModelTarget{target}); err != nil {
		t.Fatalf("prefixed sha: %v", err)
	}
	if _, err := reg.bindCapture(nil, nil); err != nil {
		t.Fatalf("empty subset: %v", err)
	}
	for name, tc := range map[string]struct {
		traces  []Trace
		targets []ModelTarget
		want    string
	}{
		"unregistered trace": {[]Trace{other}, []ModelTarget{target}, "not registered"},
		"edited trace":       {[]Trace{edited}, []ModelTarget{target}, "does not match"},
		"plain trace":        {[]Trace{plain}, []ModelTarget{target}, "not a recall trace"},
		"injection trace":    {[]Trace{benign}, []ModelTarget{target}, "not a recall trace"},
		"other-split trace":  {[]Trace{confirm}, []ModelTarget{target}, "registration is"},
		"unregistered model": {[]Trace{tr}, []ModelTarget{{Display: "openai-compat/nope", Provider: openAICompatTransport, Model: "nope"}}, "is not registered"},
	} {
		if _, err := reg.bindCapture(tc.traces, tc.targets); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// recallFamilyTraces returns a confirmatory family's ten cells in build order,
// derived from the one cell table.
func recallFamilyTraces(fam string) []Trace {
	var out []Trace
	for _, c := range recallCells("confirmatory") {
		out = append(out, Trace{ID: "recall-" + fam + "-" + c.Suffix, Recall: &RecallEval{FamilyID: fam, Trace: c.Trace, Arm: c.Arm, Cases: c.Cases, Repeat: c.Repeat}})
	}
	return out
}

// traceIDs lists the IDs of ts in order.
func traceIDs(ts []Trace) []string {
	var ids []string
	for _, t := range ts {
		ids = append(ids, t.ID)
	}
	return ids
}

func TestRecallCaptureOrder(t *testing.T) {
	fams := []string{"alpha", "beta", "gamma"}
	var in []Trace
	for _, f := range fams {
		in = append(in, recallFamilyTraces(f)...)
	}
	// Family order is the registered hash order.
	sorted := slices.Clone(fams)
	sort.Slice(sorted, func(i, j int) bool { return pairIDHash(sorted[i]) < pairIDHash(sorted[j]) })
	base := []string{"A-0", "A-1", "A-2", "A-2-s7", "B-s1-1", "B-s1-2", "B-s2-1", "B-s2-2", "B-s2-P"}
	var want []string
	for pos, f := range sorted {
		for _, k := range append(slices.Clone(base[pos:]), base[:pos]...) {
			want = append(want, "recall-"+f+"-"+k)
		}
	}
	for _, f := range sorted {
		want = append(want, "recall-"+f+"-B-s2-1-repeat")
	}
	got := traceIDs(recallCaptureOrder(in))
	if !slices.Equal(got, want) {
		t.Fatalf("order\n got %v\nwant %v", got, want)
	}
	// The second family starts at its second base element; repeats are last.
	if got[9] != "recall-"+sorted[1]+"-A-1" || !strings.HasSuffix(got[len(got)-1], "-repeat") || !strings.HasSuffix(got[len(got)-3], "-repeat") {
		t.Fatalf("rotation or repeat placement wrong: %v", got)
	}
	// The input order does not matter; counterbalanceCaptureTraces routes recall here.
	rev := slices.Clone(in)
	slices.Reverse(rev)
	if g := traceIDs(recallCaptureOrder(rev)); !slices.Equal(g, want) {
		t.Fatalf("order depends on input order: %v", g)
	}
	cb, _ := counterbalanceCaptureTraces(in)
	if !slices.Equal(traceIDs(cb), want) {
		t.Fatalf("counterbalanceCaptureTraces did not use the recall order: %v", traceIDs(cb))
	}
	// A pilot family (no A-2, A-2-s7, B-s1-2, B-s2-2 or repeat cell) skips the absent cells;
	// this one is cut further to three cells to pin the skip.
	pilot := []Trace{}
	for _, tr := range recallFamilyTraces("solo") {
		if strings.HasSuffix(tr.ID, "-A-0") || strings.HasSuffix(tr.ID, "-A-1") || strings.HasSuffix(tr.ID, "-B-s1-1") {
			pilot = append(pilot, tr)
		}
	}
	if g := traceIDs(recallCaptureOrder(pilot)); !slices.Equal(g, []string{"recall-solo-A-0", "recall-solo-A-1", "recall-solo-B-s1-1"}) {
		t.Fatalf("pilot order %v", g)
	}
}

// pilotFamilyTraces returns a pilot family's five cells in input order.
func pilotFamilyTraces(fam string) []Trace {
	var out []Trace
	for _, tr := range recallFamilyTraces(fam) {
		for _, suffix := range []string{"-A-0", "-A-1", "-B-s1-1", "-B-s2-1", "-B-s2-P"} {
			if strings.HasSuffix(tr.ID, suffix) {
				out = append(out, tr)
			}
		}
	}
	return out
}

// With more families than cells the rotation wraps (pos % n) and every cell
// leads about equally often.
func TestRecallCaptureOrderWrapsAndBalances(t *testing.T) {
	const families, n = 12, 5
	var in []Trace
	var ids []string
	for i := 0; i < families; i++ {
		id := fmt.Sprintf("fam%02d", i)
		ids = append(ids, id)
		in = append(in, pilotFamilyTraces(id)...)
	}
	sort.Slice(ids, func(i, j int) bool { return pairIDHash(ids[i]) < pairIDHash(ids[j]) })
	base := []string{"A-0", "A-1", "B-s1-1", "B-s2-1", "B-s2-P"}
	got := traceIDs(recallCaptureOrder(in))
	if len(got) != families*n {
		t.Fatalf("got %d traces, want %d", len(got), families*n)
	}
	leads := map[string]int{}
	for pos, id := range ids {
		first := got[pos*n]
		want := "recall-" + id + "-" + base[pos%n]
		if first != want {
			t.Fatalf("family %d (%s) leads with %s, want %s", pos, id, first, want)
		}
		leads[base[pos%n]]++
	}
	for _, k := range base {
		if leads[k] < families/n || leads[k] > families/n+1 {
			t.Fatalf("cell %s leads %d of %d families: %v", k, leads[k], families, leads)
		}
	}
}

// recallBaseOrder is derived from the cell table and the manifest scheme text
// names it.
func TestRecallBaseOrderFollowsCellTable(t *testing.T) {
	want := []string{"A-0", "A-1", "A-2", "A-2-s7", "B-s1-1", "B-s1-2", "B-s2-1", "B-s2-2", "B-s2-P"}
	if !slices.Equal(recallBaseOrder, want) {
		t.Fatalf("recallBaseOrder = %v, want %v", recallBaseOrder, want)
	}
	if !strings.Contains(recallCaptureCounterbalanceScheme, strings.Join(recallBaseOrder, ", ")) {
		t.Fatalf("the scheme text does not state the base order: %s", recallCaptureCounterbalanceScheme)
	}
	if strings.Contains(recallCaptureCounterbalanceScheme, "without a recall block") {
		t.Fatalf("the scheme still describes a non-recall branch: %s", recallCaptureCounterbalanceScheme)
	}
}

// Non-recall traces keep today's order byte-for-byte (golden).
func TestCounterbalanceCaptureTracesNonRecallUnchanged(t *testing.T) {
	in := []Trace{{ID: "z"}, {ID: "a"}, {ID: "m"}}
	got, labels := counterbalanceCaptureTraces(in)
	if !slices.Equal(traceIDs(got), []string{"z", "a", "m"}) || len(labels) != 0 {
		t.Fatalf("non-recall order changed: %v %v", traceIDs(got), labels)
	}
	mixed := Trace{ID: "x", AssemblyEval: &AssemblyEval{PairID: "p", Mode: AssemblyTopline}}
	got, _ = counterbalanceCaptureTraces([]Trace{{ID: "z"}, mixed, {ID: "a"}})
	if !slices.Equal(traceIDs(got), []string{"z", "a", "x"}) {
		t.Fatalf("assembly order changed: %v", traceIDs(got))
	}
}

// A recall set holding any non-recall trace, even a plain one, is refused by
// runCalibrateCapture itself.
func TestRunCalibrateCaptureRefusesRecallMixedWithPlain(t *testing.T) {
	plain := Trace{ID: "z", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}}
	err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: &scriptedCaptureRunner{}, Targets: []ModelTarget{{Display: "m", Provider: openAICompatTransport, Model: "m"}},
		Traces: []Trace{validRecallTrace(), plain}, OutputPath: filepath.Join(t.TempDir(), "a.jsonl"), RegistrationDigest: "sha256:abc",
		InjectionTemperatures: map[string]float64{"m": 0},
	})
	if err == nil || !strings.Contains(err.Error(), "recall traces cannot share one capture with non-recall") {
		t.Fatalf("plain mix: %v", err)
	}
}

func TestCalibrateCaptureRecallUsesRegisteredDecoding(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","object":"chat.completion","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"retry.go\"}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	tr := validRecallTrace()
	reg, digest, err := loadRecallRegistration(recallRegistrationFile(t, []Trace{tr}, nil))
	if err != nil {
		t.Fatal(err)
	}
	target, err := parseModelTarget(recallPrimaryModel)
	if err != nil {
		t.Fatal(err)
	}
	decoding, err := reg.bindCapture([]Trace{tr}, []ModelTarget{target})
	if err != nil {
		t.Fatal(err)
	}
	temps := map[string]float64{}
	for sel, d := range decoding {
		temps[sel] = *d.Options.Temperature
	}
	out := filepath.Join(t.TempDir(), "art.jsonl")
	runner := &Runner{OpenAICompatBaseURL: srv.URL, Timeout: 10 * time.Second, Scorer: &CaptureScorer{}, InjectionDecoding: decoding}
	err = runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: runner, Targets: []ModelTarget{target}, Traces: []Trace{tr}, OutputPath: out,
		Clock: func() time.Time { return time.Unix(0, 0).UTC() }, OpenAICompatBaseURL: srv.URL,
		RegistrationDigest: digest, InjectionTemperatures: temps, Stdout: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["temperature"] != 0.0 || body["max_tokens"] != 4096.0 {
		t.Fatalf("registered decoding not on the wire: temperature=%v max_tokens=%v", body["temperature"], body["max_tokens"])
	}
	arts, err := loadArtifacts(out)
	if err != nil || len(arts) != 1 {
		t.Fatalf("artifacts: %v %+v", err, arts)
	}
	a := arts[0]
	if a.Capture == nil || a.Capture.Temperature == nil || *a.Capture.Temperature != 0 {
		t.Fatalf("artifact capture temperature: %+v", a.Capture)
	}
	if len(a.ActualRawToolCalls) != 1 || a.ActualRawToolCalls[0].Name != "read_file" {
		t.Fatalf("ActualRawToolCalls = %+v", a.ActualRawToolCalls)
	}
	m, err := readCaptureManifestV3(captureManifestPath(out))
	if err != nil {
		t.Fatal(err)
	}
	if m.CounterbalanceScheme != recallCaptureCounterbalanceScheme {
		t.Fatalf("manifest counterbalance_scheme = %q, want the recall scheme", m.CounterbalanceScheme)
	}
	if m.Decoding.Temperature != 0 || m.RegistrationDigest != digest {
		t.Fatalf("manifest decoding %+v digest %q, want temperature 0 and %q", m.Decoding, m.RegistrationDigest, digest)
	}
}

// The registered temperature, not the assembly default (also 0), reaches the
// artifact provenance and the manifest, and a target without one is refused.
func TestCalibrateCaptureRecallRecordsRegisteredTemperature(t *testing.T) {
	tr := validRecallTrace()
	sel := recallPrimaryModel
	target := ModelTarget{Display: sel, Provider: openAICompatTransport, Model: "gemma4:31b"}
	opts := func(out string, temps map[string]float64) calibrateCaptureOptions {
		return calibrateCaptureOptions{
			Runner:  &scriptedCaptureRunner{rounds: [][]Result{{{Model: sel, TraceID: tr.ID, Transcript: []Turn{{Role: "assistant", Content: "answer"}}}}}},
			Targets: []ModelTarget{target}, Traces: []Trace{tr}, OutputPath: out, Clock: func() time.Time { return time.Unix(0, 0).UTC() },
			OpenAICompatBaseURL: "http://127.0.0.1:8090", RegistrationDigest: "sha256:abc", Stdout: io.Discard,
			InjectionTemperatures: temps,
		}
	}
	out := filepath.Join(t.TempDir(), "art.jsonl")
	if err := runCalibrateCapture(context.Background(), opts(out, map[string]float64{sel: 0.25})); err != nil {
		t.Fatal(err)
	}
	arts, err := loadArtifacts(out)
	if err != nil || len(arts) != 1 || arts[0].Capture == nil || arts[0].Capture.Temperature == nil || *arts[0].Capture.Temperature != 0.25 {
		t.Fatalf("artifact capture temperature: %v %+v", err, arts)
	}
	m, err := readCaptureManifestV3(captureManifestPath(out))
	if err != nil || m.Decoding.Temperature != 0.25 || m.CounterbalanceScheme != recallCaptureCounterbalanceScheme {
		t.Fatalf("manifest: %v %+v", err, m)
	}
	out2 := filepath.Join(t.TempDir(), "art.jsonl")
	err = runCalibrateCapture(context.Background(), opts(out2, nil))
	if err == nil || !strings.Contains(err.Error(), "has no registered temperature") {
		t.Fatalf("err = %v, want the missing-temperature refusal", err)
	}
	if _, statErr := os.Stat(out2); !os.IsNotExist(statErr) {
		t.Fatalf("refused capture wrote evidence: %v", statErr)
	}
	// Recall wording for the digest and temperature refusals.
	o := opts(out2, map[string]float64{sel: 0})
	o.RegistrationDigest = ""
	if err := runCalibrateCapture(context.Background(), o); err == nil || err.Error() != "calibrate-capture: a recall capture requires a registration digest (-registration)" {
		t.Fatalf("digest err = %v", err)
	}
	o = opts(out2, map[string]float64{sel: 0, "openai-compat/b": 1})
	o.Targets = append(o.Targets, ModelTarget{Display: "openai-compat/b", Provider: openAICompatTransport, Model: "b"})
	if err := runCalibrateCapture(context.Background(), o); err == nil || err.Error() != "calibrate-capture: recall capture mixes registered temperatures; run one capture per temperature" {
		t.Fatalf("mixed-temperature err = %v", err)
	}
}
