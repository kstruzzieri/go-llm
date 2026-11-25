package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// recallE2EMode selects how the e2e server treats a labeled record.
type recallE2EMode struct {
	// honorLabel answers a Trace A request whose relevant record carries the
	// label with a read_file call; off, the server ignores the label.
	honorLabel bool
	// declineLabeledS2 is how many S2 Trace B requests per model with the
	// label on a record of the first tool message (arm 2) the server declines:
	// each is a slip.
	declineLabeledS2 int
}

// recallE2EServer is an openai-compat server for the recall e2e. It keys only
// on request content and the model (the server never sees trace IDs): a
// request with two tool messages is a Trace B reply, which declines when its
// read_file result shows a lesson value (S2), a record in its first tool
// message carries the label, and the model's mode.declineLabeledS2 budget
// lasts, and otherwise states the value the read_file result shows; a Trace A
// request whose relevant record's line carries the label (mode.honorLabel) is
// answered with a read_file call on the named file; every other Trace A
// request states the lesson value.
type recallE2EServer struct {
	mu         sync.Mutex
	bodies     []map[string]any
	labeledOff int         // requests whose labeled line is not the relevant record's
	labeledRel int         // Trace A requests whose labeled line is the relevant record's
	readFile   int         // read_file replies sent
	declined   map[any]int // labeled S2 declines sent, per model
}

// recallE2EValue finds the value on the right of the first "=" in a file.
var recallE2EValue = regexp.MustCompile(`=\s*(\w+)`)

// newRecallE2EServer starts the e2e server for fams; it fails the test on a
// request that is not a decodable POST to /v1/chat/completions.
func newRecallE2EServer(t *testing.T, fams []recallFamily, mode recallE2EMode) (*httptest.Server, *recallE2EServer) {
	t.Helper()
	rec := &recallE2EServer{declined: map[any]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected request %s %s, want POST /v1/chat/completions", r.Method, r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		var generic map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode request messages: %v", err)
		}
		if err := json.Unmarshal(raw, &generic); err != nil {
			t.Errorf("decode request: %v", err)
		}
		var tools []string
		for _, m := range body.Messages {
			if m.Role == "tool" {
				tools = append(tools, m.Content)
			}
		}
		// Report a prompt size that tracks the request bytes (4 per token), so
		// the label's few bytes show up as the few tokens the G5 check measures.
		pt := strconv.Itoa(len(raw) / 4)
		reply := func(msg string) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"r","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":`+strconv.Quote(msg)+`}}],"usage":{"prompt_tokens":`+pt+`,"completion_tokens":1,"total_tokens":2}}`)
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.bodies = append(rec.bodies, generic)
		if len(tools) == 0 {
			reply("no tool results")
			return
		}
		if len(tools) >= 2 {
			m := recallE2EValue.FindStringSubmatch(tools[1])
			if m == nil {
				reply("no value found")
				return
			}
			s2 := false
			for _, f := range fams {
				s2 = s2 || f.Value.Lesson == m[1]
			}
			if s2 && strings.Contains(tools[0], recallLabelSegment) && rec.declined[generic["model"]] < mode.declineLabeledS2 {
				rec.declined[generic["model"]]++
				reply("I could not determine the value.")
				return
			}
			reply("The current value is " + m[1] + ".")
			return
		}
		for _, f := range fams {
			if !strings.Contains(tools[0], f.Lesson) {
				continue
			}
			relevantLabeled, anyLabeled := false, false
			for _, l := range strings.Split(tools[0], "\n") {
				if strings.Contains(l, recallLabelSegment) {
					anyLabeled = true
					relevantLabeled = relevantLabeled || strings.Contains(l, f.Lesson)
				}
			}
			if anyLabeled && !relevantLabeled {
				rec.labeledOff++
			}
			if relevantLabeled {
				rec.labeledRel++
			}
			if mode.honorLabel && relevantLabeled {
				rec.readFile++
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"r","object":"chat.completion","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":`+strconv.Quote(fmt.Sprintf(`{"path":%q}`, f.NamedFile))+`}}]}}],"usage":{"prompt_tokens":`+pt+`,"completion_tokens":1,"total_tokens":2}}`)
				return
			}
			reply("The value is " + f.Value.Lesson + ".")
			return
		}
		reply("no matching family")
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// recallE2EG1 matches a G1 line of the report.
var recallE2EG1 = regexp.MustCompile(`G1 vs (arm [01]): B (\d+), C (\d+)(?: \(\d+ missing\))?, neither (\d+)`)

// recallE2ELabelTokens matches a G5 line of the report.
var recallE2ELabelTokens = regexp.MustCompile(`G5 (\S+): label tokens (\d+) \(limit`)

// recallE2ESection returns the report text of one model's "### model" metrics
// section, up to the next "###" heading.
func recallE2ESection(t *testing.T, text, model string) string {
	t.Helper()
	_, rest, ok := strings.Cut(text, "\n### "+model+"\n")
	if !ok {
		t.Fatalf("report has no metrics section for %s:\n%s", model, text)
	}
	section, _, _ := strings.Cut(rest, "\n### ")
	return section
}

// recallE2ERegistration registers exactly the built traces for the four
// registered models, pinned to the httptest server at endpoint (an explicit
// loopback endpoint with a non-legacy serving stack), and returns the
// registration's path.
func recallE2ERegistration(t *testing.T, traces []Trace, casesDigest, endpoint string) string {
	t.Helper()
	return recallExactRegistrationFile(t, traces, "confirmatory", func(reg map[string]any) {
		reg["cases_sha256"] = casesDigest
		recallTestEndpoint(endpoint)(reg)
	})
}

// recallE2ECapture captures traces for models (the four registered models
// when none is given) against srvURL through the capture glue main runs
// (prepareRecallCapture, runCalibrateCapture) and returns the artifacts path
// and the artifacts.
func recallE2ECapture(t *testing.T, srvURL, regPath string, traces []Trace, models ...string) (string, []Artifact) {
	t.Helper()
	if len(models) == 0 {
		models = recallRegModels
	}
	targets, err := parseModelTargets(strings.Join(models, ","))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "art.jsonl")
	runner := &Runner{OpenAICompatBaseURL: srvURL, Timeout: 10 * time.Second, Scorer: &CaptureScorer{}}
	regDigest, endpoint, temps, err := prepareRecallCapture(runner, regPath, traces, targets)
	if err != nil {
		t.Fatal(err)
	}
	err = runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: runner, Targets: targets, Traces: traces, OutputPath: out,
		Clock: func() time.Time { return time.Unix(0, 0).UTC() }, OpenAICompatBaseURL: srvURL,
		RegistrationDigest: regDigest, RecallEndpoint: endpoint, InjectionTemperatures: temps, Stdout: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	arts, err := loadArtifacts(out)
	if err != nil {
		t.Fatal(err)
	}
	return out, arts
}

// recallE2EFailCell copies the capture at out (artifacts and manifest) into a
// fresh directory and, when traceID is set, rewrites the copy as if model's
// cell had failed on its attempt and its in-run retry: the artifact is
// dropped and its ledger row becomes a failed row with two network errors,
// as runCalibrateCapture records one. The kept artifact lines stay byte for
// byte, so their hashes still verify. It returns the copy's artifacts path.
func recallE2EFailCell(t *testing.T, out, model, traceID string) string {
	t.Helper()
	m, err := readCaptureManifestV3(captureManifestPath(out))
	if err != nil {
		t.Fatal(err)
	}
	var drop string
	for i, row := range m.Expected {
		if traceID == "" || row.TraceID != traceID || modelKey(row.Model) != modelKey(model) {
			continue
		}
		if row.Status != "captured" {
			t.Fatalf("%s/%s is %s in the capture, want captured", model, traceID, row.Status)
		}
		drop = row.ArtifactHash
		const netErr = "<error: network>"
		m.Expected[i] = captureExpectedRow{TraceID: row.TraceID, Model: row.Model, Status: "failed", Attempts: 2,
			Error: netErr, AttemptErrors: []string{netErr, netErr}}
	}
	if traceID != "" && drop == "" {
		t.Fatalf("capture has no row for %s/%s", model, traceID)
	}
	var keep []captureManifestRow
	for _, row := range m.PerArtifact {
		if row.ArtifactHash != drop {
			keep = append(keep, row)
		}
	}
	m.PerArtifact, m.ArtifactCount = keep, len(keep)
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.SplitAfter(string(raw), "\n") {
		var a struct {
			ArtifactHash string `json:"artifact_hash"`
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatal(err)
		}
		if drop == "" || a.ArtifactHash != drop {
			lines = append(lines, line)
		}
	}
	copyOut := filepath.Join(t.TempDir(), "art.jsonl")
	if err := os.WriteFile(copyOut, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := marshalCaptureManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(captureManifestPath(copyOut), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	return copyOut
}

// recallE2ECases writes the registered corpus shape, 40 confirmatory families
// over 4 templates (recallScaledCases), with value.alternatives set on the
// identifier families so the round trip carries every RecallValue field. It
// returns the file's path and the cases.
func recallE2ECases(t *testing.T) (string, recallCases) {
	t.Helper()
	c := recallScaledCases(t, recallFamilyCount, 4, "confirmatory")
	for i := range c.Families {
		if c.Families[i].Value.Type == "identifier" {
			c.Families[i].Value.Alternatives = []string{"evictFIFO"}
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "test-cases.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, c
}

// TestRecallEndToEnd wires the real builder, registration, capture and report
// together over httptest openai-compat servers, at the registered shape: 40
// families and the 4 registered models, so the report reaches a verdict. The
// corpus is built once; each server mode's capture runs in its own parallel
// subtest with its own server, registration (pinned to that server's
// loopback endpoint, as the capture's destination binding requires) and
// output, and the failed-repeat variant is derived from the label-reading
// capture instead of recaptured.
// The declining Trace B and failed-repeat variants run in the non-race CI
// benchmark step. Race builds cover the label-reading and label-ignoring
// runs and exercise slips, limits, and missing repeats in the unit tests:
// TestRecallReportMetricsValues, TestRecallReportMissingArtifact,
// TestEvaluateRecallG2, and TestRecallReportDeterminism.
func TestRecallEndToEnd(t *testing.T) {
	casesPath, cases := recallE2ECases(t)
	builtDir := t.TempDir()
	digest, err := buildRecallCorpusWith(context.Background(), casesPath, builtDir, "confirmatory", recallBuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, err := recallCasesDigest(cases, "confirmatory")
	if err != nil || digest != wantDigest {
		t.Fatalf("builder digest %q, want %q (%v)", digest, wantDigest, err)
	}
	paths, err := filepath.Glob(filepath.Join(builtDir, "recall-*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("built traces: %v %v", paths, err)
	}
	sort.Strings(paths)
	traces, err := loadTraces(paths)
	if err != nil {
		t.Fatal(err)
	}
	built := map[string]Trace{}
	for _, tr := range traces {
		built[tr.ID] = tr
	}
	models := recallRegModels
	nFam := len(cases.Families)
	wantRequests := len(models) * len(traces)

	g1 := func(text string) map[string][3]int {
		out := map[string][3]int{}
		for _, m := range recallE2EG1.FindAllStringSubmatch(text, -1) {
			b, _ := strconv.Atoi(m[2])
			c, _ := strconv.Atoi(m[3])
			n, _ := strconv.Atoi(m[4])
			out[m[1]] = [3]int{b, c, n}
		}
		return out
	}
	// run registers the corpus for srvURL, captures against it and reports;
	// a report error fails the test.
	run := func(t *testing.T, srvURL string) (regPath, out string, arts []Artifact, text string, verdict recallVerdict) {
		t.Helper()
		regPath = recallE2ERegistration(t, traces, digest, srvURL)
		out, arts = recallE2ECapture(t, srvURL, regPath, traces)
		text, verdict, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{out}, Run: 1})
		if err != nil {
			t.Fatal(err)
		}
		return regPath, out, arts, text, verdict
	}

	t.Run("label-reading server", func(t *testing.T) {
		t.Parallel()
		srv, rec := newRecallE2EServer(t, cases.Families, recallE2EMode{honorLabel: true})
		regPath, out, arts, text, verdict := run(t, srv.URL)
		if len(arts) != wantRequests || len(rec.bodies) != wantRequests {
			t.Fatalf("artifacts %d, requests %d, want %d", len(arts), len(rec.bodies), wantRequests)
		}
		onWire := map[any]int{}
		for i, b := range rec.bodies {
			onWire[b["model"]]++
			kw, _ := json.Marshal(b["chat_template_kwargs"])
			if _, effort := b["reasoning_effort"]; b["temperature"] != 0.0 || b["max_tokens"] != 4096.0 || string(kw) != `{"enable_thinking":false}` || effort {
				t.Fatalf("request %d decoding not on the wire: temperature=%v max_tokens=%v chat_template_kwargs=%s reasoning_effort present=%v", i, b["temperature"], b["max_tokens"], kw, effort)
			}
		}
		for _, m := range models {
			if name := strings.TrimPrefix(m, "openai-compat/"); onWire[name] != len(traces) {
				t.Fatalf("requests for model %v = %d, want %d (all: %v)", name, onWire[name], len(traces), onWire)
			}
		}
		// Every family's arms 0 and 1 rely on the lesson and its arm 2 re-reads
		// the file: B is every family, past the net of 8 and the sign test.
		got := g1(text)
		for _, cmp := range []string{recallCompareArm1, recallCompareArm0} {
			if got[cmp] != [3]int{nFam, 0, 0} {
				t.Fatalf("G1 vs %s B/C/neither = %v, want B %d, C 0, neither 0\n%s", cmp, got[cmp], nFam, text)
			}
		}
		if verdict != recallPass {
			t.Fatalf("verdict %q, want %q:\n%s", verdict, recallPass, text)
		}
		// S7: the label sits on a distractor, so it must not trigger the read.
		if want := len(models) * nFam; rec.labeledOff != want || rec.readFile != want {
			t.Fatalf("requests with the label on a distractor = %d, read_file replies = %d, want %d each", rec.labeledOff, rec.readFile, want)
		}
		fams := map[string]recallFamily{}
		for _, f := range cases.Families {
			fams[f.ID] = f
		}
		for _, a := range arts {
			tr := built[a.TraceID]
			if !reflect.DeepEqual(a.Trace.Recall, tr.Recall) {
				t.Fatalf("%s: RecallEval did not survive the artifact round trip:\n got %+v\nwant %+v", a.TraceID, a.Trace.Recall, tr.Recall)
			}
			_, cell, err := parseRecallTraceID(a.TraceID)
			if err != nil {
				t.Fatal(err)
			}
			read := len(a.ActualRawToolCalls) == 1 && a.ActualRawToolCalls[0].Name == "read_file"
			if want := cell == "A-2"; read != want {
				t.Fatalf("%s: read_file reply = %v, want %v (raw calls %+v)", a.TraceID, read, want, a.ActualRawToolCalls)
			}
			if read && !strings.Contains(a.ActualRawToolCalls[0].Arguments, fams[tr.Recall.FamilyID].NamedFile) {
				t.Fatalf("%s: read_file arguments %q do not name %s", a.TraceID, a.ActualRawToolCalls[0].Arguments, fams[tr.Recall.FamilyID].NamedFile)
			}
		}
		// The round trip must be able to fail for every field: against an
		// independently written expectation, and no field left at its zero
		// value in every artifact. fam-3 is the identifier/task template's
		// first family.
		wantEval := RecallEval{
			FamilyID: "fam-3", Template: "tpl-3", TaskStyle: "task", Split: "confirmatory",
			Trace: "A", Arm: "2", Cases: []string{"S1", "S2"}, NamedFile: "cache/policy.go", TaskPaths: []string{"cache/policy.go"},
			Value: RecallValue{Type: "identifier", Identifier: "evictionMode", Lesson: "evictLRU", Changed: "evictTTL", Alternatives: []string{"evictFIFO"}},
		}
		var gotA2 *RecallEval
		nonZero := map[string]bool{}
		for _, a := range arts {
			if a.TraceID == "recall-fam-3-A-2" {
				gotA2 = a.Trace.Recall
			}
			for _, v := range []reflect.Value{reflect.ValueOf(*a.Trace.Recall), reflect.ValueOf(a.Trace.Recall.Value)} {
				for i := 0; i < v.NumField(); i++ {
					if !v.Field(i).IsZero() {
						nonZero[v.Type().Name()+"."+v.Type().Field(i).Name] = true
					}
				}
			}
		}
		if gotA2 == nil || !reflect.DeepEqual(*gotA2, wantEval) {
			t.Fatalf("recall-fam-3-A-2 artifact RecallEval = %+v, want %+v", gotA2, wantEval)
		}
		for _, rt := range []reflect.Type{reflect.TypeOf(RecallEval{}), reflect.TypeOf(RecallValue{})} {
			for i := 0; i < rt.NumField(); i++ {
				if name := rt.Name() + "." + rt.Field(i).Name; !nonZero[name] {
					t.Errorf("%s is zero in every artifact, so the round trip cannot fail for it", name)
				}
			}
		}
		// Trace B: the read_file result reaches the model (S1's file value
		// differs from the lesson), so every pair answers in both arms.
		for _, m := range models {
			if want := fmt.Sprintf("%s: pairs %d, arm 1 answers %d, slips 0 (0 missing), result pass", m, nFam, nFam); !strings.Contains(text, want) {
				t.Fatalf("report lacks G2' line %q:\n%s", want, text)
			}
			if want := fmt.Sprintf("%s: 0 change(s) among %d repeats", m, nFam); !strings.Contains(text, want) {
				t.Fatalf("report lacks determinism line %q (repeat cells compared):\n%s", want, text)
			}
			sec := recallE2ESection(t, text, m)
			for _, want := range []string{
				fmt.Sprintf("- S1 Trace B answers, arm 2 vs arm 1: a %d, b 0, c 0, d 0, missing 0", nFam),
				fmt.Sprintf("- S2 Trace B answers, arm 2 vs arm 1: a %d, b 0, c 0, d 0, missing 0", nFam),
				fmt.Sprintf("- arm 1: current value %d, lesson value 0, both 0, neither 0, missing 0", nFam),
				fmt.Sprintf("- arm 2: current value %d, lesson value 0, both 0, neither 0, missing 0", nFam),
			} {
				if !strings.Contains(sec, want) {
					t.Fatalf("%s section lacks %q:\n%s", m, want, sec)
				}
			}
			// The report breaks the gated counts down by template: each
			// template's line holds its own families' pairs.
			perTemplate := map[string]int{}
			for _, f := range cases.Families {
				perTemplate[f.Template]++
			}
			for tpl, n := range perTemplate {
				want := fmt.Sprintf("template %s: G1 vs arm 1 B %d C 0 (0 missing); vs arm 0 B %d C 0 (0 missing); G2' arm 1 answers %d, slips 0 (0 missing)", tpl, n, n, n)
				if !strings.Contains(sec, want) {
					t.Fatalf("%s section lacks %q:\n%s", m, want, sec)
				}
			}
		}
		// G5: the 14-byte label at 4 bytes per token is 3 to 4 tokens.
		tokens := recallE2ELabelTokens.FindAllStringSubmatch(text, -1)
		if len(tokens) != len(models) {
			t.Fatalf("G5 lines = %d, want %d:\n%s", len(tokens), len(models), text)
		}
		for _, m := range tokens {
			if n, _ := strconv.Atoi(m[2]); n < 3 || n > 4 {
				t.Fatalf("G5 %s label tokens = %d, want 3 to 4", m[1], n)
			}
		}

		// A failed determinism repeat, derived from this capture (which just
		// decided pass): the primary's first repeat failed on its attempt and
		// its retry. The cell was attempted, so completeness holds, but the
		// repeat is missing: a harness error naming it, not a verdict. The
		// exact error shows the edited copy passed every capture check first.
		t.Run("a failed determinism repeat", func(t *testing.T) {
			if raceEnabled {
				t.Skip("variant run: skipped under -race (see TestRecallEndToEnd); it runs only in a non-race go test")
			}
			var repeat string
			for _, a := range arts {
				if modelKey(a.CandidateModel) == modelKey(recallPrimaryModel) && a.Trace.Recall.Repeat && (repeat == "" || a.TraceID < repeat) {
					repeat = a.TraceID
				}
			}
			edited := recallE2EFailCell(t, out, recallPrimaryModel, repeat)
			if n, err := loadArtifacts(edited); err != nil || len(n) != wantRequests-1 {
				t.Fatalf("edited copy: %d artifacts (want %d), err %v", len(n), wantRequests-1, err)
			}
			text, v, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{edited}, Run: 1})
			want := "recall report: model " + recallPrimaryModel + ": determinism needs every registered family's arm 1 S2 Trace B original and its repeat; 1 failed (no artifact): " + repeat + "; " + recallRecaptureHint
			if err == nil || err.Error() != want {
				t.Fatalf("verdict %q err %v; want the harness error %q:\n%s", v, err, want, text)
			}
		})
	})

	t.Run("label-ignoring server", func(t *testing.T) {
		t.Parallel()
		srv, rec := newRecallE2EServer(t, cases.Families, recallE2EMode{})
		_, _, arts, text, verdict := run(t, srv.URL)
		if want := len(models) * nFam; len(arts) != wantRequests || rec.labeledRel != want {
			t.Fatalf("artifacts %d (want %d), labeled-relevant requests %d (want %d: the control saw the label)", len(arts), wantRequests, rec.labeledRel, want)
		}
		got := g1(text)
		for _, cmp := range []string{recallCompareArm1, recallCompareArm0} {
			if r, ok := got[cmp]; !ok || r != [3]int{0, 0, nFam} {
				t.Fatalf("G1 vs %s = %v (present %v), want B 0, C 0, neither %d\n%s", cmp, r, ok, nFam, text)
			}
		}
		if verdict != recallFail {
			t.Fatalf("verdict %q, want %q (G1 has no benefit)", verdict, recallFail)
		}
	})

	t.Run("declining labeled Trace B", func(t *testing.T) {
		if raceEnabled {
			t.Skip("variant run: skipped under -race (see TestRecallEndToEnd); it runs only in a non-race go test")
		}
		t.Parallel()
		srv, rec := newRecallE2EServer(t, cases.Families, recallE2EMode{honorLabel: true, declineLabeledS2: 2})
		_, _, _, text, verdict := run(t, srv.URL)
		if len(rec.bodies) != wantRequests {
			t.Fatalf("requests %d, want %d", len(rec.bodies), wantRequests)
		}
		// Arm 2 declines on two S2 questions per model while arm 1 answers
		// them: two slips, past the primary's limit of one and within each
		// other model's two.
		for _, m := range models {
			result := "pass"
			if m == recallPrimaryModel {
				result = "fail"
			}
			if want := fmt.Sprintf("%s: pairs %d, arm 1 answers %d, slips 2 (0 missing), result %s", m, nFam, nFam, result); !strings.Contains(text, want) {
				t.Fatalf("report lacks G2' line %q:\n%s", want, text)
			}
			if want := fmt.Sprintf("- S2 Trace B answers, arm 2 vs arm 1: a %d, b 2, c 0, d 0, missing 0", nFam-2); !strings.Contains(recallE2ESection(t, text, m), want) {
				t.Fatalf("%s section lacks the S2 Trace B slips %q:\n%s", m, want, text)
			}
		}
		if verdict != recallFail {
			t.Fatalf("verdict %q, want %q", verdict, recallFail)
		}
	})
}
