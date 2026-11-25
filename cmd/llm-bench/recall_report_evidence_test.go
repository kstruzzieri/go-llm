package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRecallReportRequiresRecordedTurn(t *testing.T) {
	traces := recallTestTraces(recallSplitFamilyCount("pilot"), "pilot")
	reg, digest := recallTestRegistration(t, traces, "pilot")
	selected := recallTestTracesWhere(traces, func(tr Trace) bool { return tr.Recall.Trace == "A" })
	targets, err := parseModelTargets(recallPrimaryModel)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		turns []Turn
		valid bool
	}{
		{"no turn", nil, false},
		{"user turn", []Turn{{Role: "user"}}, false},
		{"two assistant turns", []Turn{{Role: "assistant"}, {Role: "assistant"}}, false},
		{"extra user turn", []Turn{{Role: "user"}, {Role: "assistant"}}, false},
		{"recorded empty reply", []Turn{{Role: "assistant"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var results []Result
			for _, tr := range selected {
				results = append(results, Result{Model: recallPrimaryModel, TraceID: tr.ID, Transcript: tc.turns,
					CandidateProvider: openAICompatCandidateProviderName(recallTestBase)})
			}
			capPath := filepath.Join(t.TempDir(), "art.jsonl")
			err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
				Runner: &scriptedCaptureRunner{rounds: [][]Result{results}}, Targets: targets, Traces: selected, OutputPath: capPath,
				RegistrationDigest: digest, OpenAICompatBaseURL: recallTestBase, InjectionTemperatures: map[string]float64{recallPrimaryModel: 0}, Stdout: io.Discard,
			})
			if err != nil {
				t.Fatal(err)
			}
			report, verdict, err := runRecallReport(recallReportOptions{RegistrationPath: reg, Captures: []string{capPath}, Headroom: true})
			if tc.valid {
				if err != nil || verdict != recallStopped {
					t.Fatalf("recorded empty replies: verdict %q, error %v", verdict, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "exactly one assistant turn") || report != "" || verdict != "" {
				t.Fatalf("malformed transcript: verdict %q, error %v", verdict, err)
			}
		})
	}
}

func TestRecallReportBindsRegisteredCaptureIdentity(t *testing.T) {
	traces := recallTestTraces(recallSplitFamilyCount("pilot"), "pilot")
	const otherCase = "openai-compat/GEMMA4:31B"
	for _, tc := range []struct {
		name, endpoint, want string
		editManifest         func(*captureManifest)
		editArtifact         func(*Artifact)
	}{
		{name: "equivalent endpoint", endpoint: recallTestBase + "/"},
		{name: "different endpoint", endpoint: "http://127.0.0.1:18091", want: "differs from the registered loopback endpoint"},
		{name: "target selector", want: "not a registered model", editManifest: func(m *captureManifest) { m.ModelTargets[0].Selector = otherCase }},
		{name: "ledger selector", want: "ledger model", editManifest: func(m *captureManifest) { m.Expected[0].Model = otherCase }},
		{name: "artifact selector", want: "no matching captured ledger row", editArtifact: func(a *Artifact) { a.CandidateModel = otherCase }},
		{name: "provenance selector", want: "capture.model", editArtifact: func(a *Artifact) { a.Capture.Model = otherCase }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var configure func(map[string]any)
			if tc.endpoint != "" {
				configure = recallTestEndpoint(tc.endpoint)
			}
			regPath := recallExactRegistrationFile(t, traces, "pilot", configure)
			_, digest, err := loadRecallRegistration(regPath)
			if err != nil {
				t.Fatal(err)
			}
			out := recallTestCapture(t, traces[:1], []string{recallPrimaryModel}, digest, recallTestDefault(3, 3), nil)
			manifestPath := captureManifestPath(out)
			m, err := readCaptureManifestV3(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if tc.editManifest != nil {
				tc.editManifest(&m)
			}
			if tc.editArtifact != nil {
				rewriteArtifacts(t, out, func(a *Artifact) {
					tc.editArtifact(a)
					m.PerArtifact[0].ProvenanceHash = captureProvenanceHash(*a)
				})
			}
			writeLedgerManifest(t, filepath.Dir(out), filepath.Base(manifestPath), m)
			report, verdict, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{out}})
			if tc.want == "" {
				if err != nil || report == "" {
					t.Fatalf("valid pilot capture: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || report != "" || verdict != "" {
				t.Fatalf("verdict %q, error %v; want %q", verdict, err, tc.want)
			}
		})
	}
}

func TestRecallReportRequiresExplicitTemperature(t *testing.T) {
	traces := recallTestTraces(recallSplitFamilyCount("pilot"), "pilot")
	reg, digest := recallTestRegistration(t, traces, "pilot")
	for _, missing := range []string{"absent", "null", "string"} {
		change := func(d map[string]any) {
			switch missing {
			case "absent":
				delete(d, "temperature")
			case "null":
				d["temperature"] = nil
			case "string":
				d["temperature"] = "0"
			}
		}
		t.Run("registration/"+missing, func(t *testing.T) {
			path := recallExactRegistrationFile(t, traces, "pilot", func(r map[string]any) {
				change(r["decoding"].(map[string]any)[recallPrimaryModel].(map[string]any))
			})
			if _, _, err := loadRecallRegistration(path); err == nil || !strings.Contains(err.Error(), "temperature") {
				t.Fatalf("registration error = %v", err)
			}
		})
		t.Run("manifest/"+missing, func(t *testing.T) {
			out := recallTestCapture(t, traces[:1], []string{recallPrimaryModel}, digest, recallTestDefault(3, 3), nil)
			path := captureManifestPath(out)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			change(m["decoding"].(map[string]any))
			raw, err = json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			report, verdict, err := runRecallReport(recallReportOptions{RegistrationPath: reg, Captures: []string{out}})
			if err == nil || !strings.Contains(err.Error(), "temperature") || verdict != "" || report != "" {
				t.Fatalf("verdict %q, error %v", verdict, err)
			}
		})
	}
}

func TestRecallReportChecksOrderOfSelectedTrials(t *testing.T) {
	traces := recallTestTraces(recallFamilyCount, "confirmatory")
	regPath, digest := recallTestRegistration(t, traces, "confirmatory")
	reg, _, err := loadRecallRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	selected := recallTestTracesWhere(traces, func(tr Trace) bool {
		return (tr.Recall.FamilyID == "f00" || tr.Recall.FamilyID == "f01") && (tr.Recall.Trace == "A" && tr.Recall.Arm != "2" || tr.Recall.Repeat)
	})
	// Every selected cell is in the ledger, but none has an artifact or an
	// order_index. Failed and interrupted trials still constrain the order.
	out := recallTestCaptureCut(t, selected, []string{recallPrimaryModel}, digest, recallTestDefault(3, 3),
		func(_, _ string) bool { return true }, func(_, id string) bool { return strings.HasSuffix(id, "-repeat") })
	path := captureManifestPath(out)
	if _, _, _, err := loadRecallCaptures(reg, digest, []string{out}); err != nil {
		t.Fatalf("valid sparse capture: %v", err)
	}
	for _, tc := range []struct {
		name string
		i, j int
	}{
		{"repeat before initial trials", 1, 4},
		{"wrong family order", 0, 2},
		{"wrong within-family rotation", 2, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := readCaptureManifestV3(path)
			if err != nil {
				t.Fatal(err)
			}
			m.RequestedTraces[tc.i], m.RequestedTraces[tc.j] = m.RequestedTraces[tc.j], m.RequestedTraces[tc.i]
			m.Expected[tc.i], m.Expected[tc.j] = m.Expected[tc.j], m.Expected[tc.i]
			// Keep the ledger self-consistent; only the registered order can reject it.
			if err := validateCaptureLedgerV3(m); err != nil {
				t.Fatal(err)
			}
			copyOut := filepath.Join(t.TempDir(), "art.jsonl")
			if err := os.WriteFile(copyOut, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			writeLedgerManifest(t, filepath.Dir(copyOut), filepath.Base(captureManifestPath(copyOut)), m)
			report, verdict, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{copyOut}, Run: 1})
			if err == nil || !strings.Contains(err.Error(), "registered trial order") || report != "" || verdict != "" {
				t.Fatalf("verdict %q, error %v", verdict, err)
			}
		})
	}
}

// Both successful artifacts, their provenance hashes, and the ledger agree on
// the recorded order. The registration still requires the repeat to run last.
func TestRecallReportRejectsResequencedArtifacts(t *testing.T) {
	traces := recallTestTraces(recallFamilyCount, "confirmatory")
	regPath, digest := recallTestRegistration(t, traces, "confirmatory")
	selected := recallTestTracesWhere(traces, func(tr Trace) bool {
		return tr.ID == "recall-f00-B-s2-1-repeat" || tr.ID == "recall-f01-A-0"
	})
	out := recallTestCapture(t, selected, []string{recallPrimaryModel}, digest, recallTestDefault(3, 3), nil)
	path := captureManifestPath(out)
	m, err := readCaptureManifestV3(path)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(m.RequestedTraces)
	slices.Reverse(m.Expected)
	rewriteArtifacts(t, out, func(a *Artifact) {
		i := a.Capture.OrderIndex
		a.Capture.OrderIndex = 1 - i
		m.PerArtifact[i].OrderIndex = a.Capture.OrderIndex
		m.PerArtifact[i].ProvenanceHash = captureProvenanceHash(*a)
	})
	writeLedgerManifest(t, filepath.Dir(out), filepath.Base(path), m)
	report, verdict, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{out}, Run: 1})
	if err == nil || !strings.Contains(err.Error(), "registered trial order") || report != "" || verdict != "" {
		t.Fatalf("verdict %q, error %v", verdict, err)
	}
}

func TestRecallReportCountsEachObservedArm(t *testing.T) {
	defaultReply := recallTestDefault(40, 40)
	reply := func(m string, fam int, cell string) (string, []injectionRawCall, int) {
		if m == recallPrimaryModel && fam == 0 && cell == "A-2" {
			return "", []injectionRawCall{{Name: "agent_memory_create", Arguments: `{}`, ArgumentsPresent: true, DecodeOK: true}}, 106
		}
		return defaultReply(m, fam, cell)
	}
	fail := func(m, id string) bool {
		return m == recallPrimaryModel && (id == "recall-f00-A-1" || id == "recall-f01-A-2" || id == "recall-f02-A-1" || id == "recall-f02-A-2")
	}
	report, verdict, err := recallTestRun(t, reply, fail, 1)
	if err != nil || verdict != recallPass {
		t.Fatalf("verdict %q, error %v", verdict, err)
	}
	section := recallE2ESection(t, report, recallPrimaryModel)
	for _, want := range []string{
		"memory writes 1, other tool calls 0, different values 0, empty replies 0, missing 2",
		"1 missing only arm 1, 1 missing only arm 2, 1 missing both",
		"S1 Trace A rely, arm 2 vs arm 1: a 0, b 37, c 0, d 0, missing 3",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("primary model section lacks %q", want)
		}
	}
	if !strings.Contains(report, "G1 vs arm 1: B 37, C 3 (3 missing), neither 0") {
		t.Error("G1 did not count all three incomplete pairs against the gate")
	}
}
