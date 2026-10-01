package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// childModel is the identity every routed step reports. A dispatch child with
// no RouteOutcome.ActualModel gets an envelope error of its own ("child model
// identity unavailable"), which would mask the error behaviour under test.
const childModel = "local/fast"

func routed(r agent.ModelResult) agent.ModelResult {
	r.RouteOutcome = &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}}
	return r
}

// writeCredentialFiles creates one real file behind each credential path the
// soft-stop script reads, all holding the sentinel, so an unguarded read
// succeeds and the sentinel would reach the child's next request.
func writeCredentialFiles(t *testing.T, root string) {
	t.Helper()
	writeEnvSentinel(t, root)
	for _, rel := range []string{".ssh/id_ed25519", ".aws/credentials"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(envSentinel+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// requireBlockedObservation fails unless the tool observation answering call
// id is exactly the framed credential_path block.
func requireBlockedObservation(t *testing.T, req provider.ChatRequest, id string) {
	t.Helper()
	m := toolObservation(t, req.Messages, id)
	if m.Content != framedToolResult(toolFrameKey(t, m.Content), credentialBlocked) {
		t.Fatalf("observation for %q = %q, want the framed %q", id, m.Content, credentialBlocked)
	}
}

// dispatchOnce invokes an unscoped single-task dispatch over root directly
// (no parent run) and returns the tool result with its decoded envelope. The
// envelope is checked to hold exactly one child result and never to carry the
// credential sentinel.
func dispatchOnce(t *testing.T, child agent.ModelCaller, root string) (agent.ToolResult, dispatchTestEnvelope) {
	t.Helper()
	readers, err := buildTools(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := newDispatchTool(child, flags{dispatch: true}, agent.Budget{}, dispatchFanout{maxConcurrent: 1}, nil, readers, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string][]string{"tasks": {"inspect"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if strings.Contains(out.Content, envSentinelToken) {
		t.Fatalf("credential sentinel reached the parent-facing envelope: %s", out.Content)
	}
	var env dispatchTestEnvelope
	if err := json.Unmarshal([]byte(out.Content), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Results) != 1 {
		t.Fatalf("envelope = %s, want exactly one child result", out.Content)
	}
	return out, env
}

// TestDefaultGuardsChildSoftStop (#575): three child credential blocks stop
// the child at the tool-error cap with score 90 (3 x 30), a partial summary
// and NO envelope error, so the parent's tool result is not an error. Every
// credential path has a real file behind it, so without the guard the reads
// succeed and the sentinel reaches the child's next request.
func TestDefaultGuardsChildSoftStop(t *testing.T) {
	root := t.TempDir()
	writeCredentialFiles(t, root)
	child := &recordingScript{scriptCaller: scriptCaller{responses: []agent.ModelResult{
		routed(toolStep("b1", "read_file", `{"path":".env"}`)),
		routed(toolStep("b2", "read_file", `{"path":".ssh/id_ed25519"}`)),
		routed(toolStep("b3", "read_file", `{"path":".aws/credentials"}`)),
	}}}
	out, env := dispatchOnce(t, child, root)

	// Wire first: the leak check is the property the guard exists for.
	assertBlockedObservationSeen(t, child.reqs, credentialBlocked)
	if len(child.reqs) != 3 {
		t.Fatalf("child model calls = %d, want 3 (stop at the cap)", len(child.reqs))
	}
	// The third request carries the first two observations; the third block is
	// pinned by the summary, which is the child's last observation.
	requireBlockedObservation(t, child.reqs[2], "b1")
	requireBlockedObservation(t, child.reqs[2], "b2")

	r := env.Results[0]
	if out.IsError || r.Error != "" {
		t.Fatalf("envelope = %s isError = %v, want a soft stop with no error", out.Content, out.IsError)
	}
	if r.StopReason != "tool_error_cap_reached" || r.RiskScore != 90 || r.Model != childModel {
		t.Fatalf("envelope = %s, want tool_error_cap_reached, score 90, model %s", out.Content, childModel)
	}
	if want := "Partial result before tool_error_cap_reached: " + credentialBlocked; r.Summary != want {
		t.Fatalf("summary = %q, want %q", r.Summary, want)
	}
}

// TestDefaultGuardsChildErrorKeepsRisk (#575): a child provider error after a
// finding still errors the envelope and the parent tool result, and the
// child's risk is retained. The .env read would succeed unguarded.
func TestDefaultGuardsChildErrorKeepsRisk(t *testing.T) {
	root := t.TempDir()
	writeEnvSentinel(t, root)
	child := &recordingCaller{next: &errAfterCaller{
		inner:  &scriptCaller{responses: []agent.ModelResult{routed(toolStep("b1", "read_file", `{"path":".env"}`))}},
		failAt: 1, err: errors.New("provider exploded"),
	}}
	out, env := dispatchOnce(t, child, root)

	assertBlockedObservationSeen(t, child.reqs, credentialBlocked)
	if len(child.reqs) != 2 {
		t.Fatalf("child model calls = %d, want 2 (read, then the failing call)", len(child.reqs))
	}
	requireBlockedObservation(t, child.reqs[1], "b1")

	r := env.Results[0]
	if !out.IsError {
		t.Fatalf("envelope = %s isError = false, want the parent tool result to be an error", out.Content)
	}
	if r.StopReason != "error" || r.Model != childModel || r.RiskScore != 30 {
		t.Fatalf("envelope = %s, want stop_reason error, model %s, risk retained at 30", out.Content, childModel)
	}
	// Contains, not equality: whether the orchestrator wraps a provider error
	// is its own business and says nothing about the retained risk (#575).
	if !strings.Contains(r.Error, "provider exploded") {
		t.Fatalf("error = %q, want it to carry the provider error", r.Error)
	}
	if want := "Partial result before error: " + credentialBlocked; r.Summary != want {
		t.Fatalf("summary = %q, want %q", r.Summary, want)
	}
}

// TestDefaultGuardsBlockedReadNeverReachesNativeCounter (#575): in a scoped
// child, an out-of-scope .env read is refused by the invariant before the
// native reader, so the child scores 30 and the parent reports no native
// denial. The control rows make that non-blind: a native refusal of a
// non-credential path IS reported by the parent (score 10, one finding), and
// when the child does both, the parent reports only the native refusal, so the
// child's score is not added to the parent's and the blocked read is not
// counted as a request.
func TestDefaultGuardsBlockedReadNeverReachesNativeCounter(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native scoped dispatch is unsupported")
	}
	const notesMarker = "notes-marker-575"
	for _, tc := range []struct {
		name       string
		paths      []string
		childScore int
		denied     int // native requests the parent must report; 0 means no risk at all
	}{
		{"blocked credential read is invisible to the native counter", []string{"../.env"}, 30, 0},
		{"control: out-of-scope read is a native denial", []string{"../notes.txt"}, 0, 1},
		{"blocked read plus native denial: scores stay separate", []string{"../.env", "../notes.txt"}, 30, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "a"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeEnvSentinel(t, root)
			if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte(notesMarker+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			readers, err := buildTools(root, nil)
			if err != nil {
				t.Fatal(err)
			}
			var steps []agent.ModelResult
			for i, p := range tc.paths {
				args, err := json.Marshal(map[string]string{"path": p})
				if err != nil {
					t.Fatal(err)
				}
				steps = append(steps, routed(toolStep(fmt.Sprintf("c%d", i+1), "read_file", string(args))))
			}
			steps = append(steps, routed(answerStep("done")))
			child := &recordingScript{scriptCaller: scriptCaller{responses: steps}}
			d, err := newDispatchTool(child, flags{dispatch: true}, agent.Budget{}, dispatchFanout{maxConcurrent: 1}, nil, readers, nil)
			if err != nil {
				t.Fatal(err)
			}
			parent := &scriptCaller{responses: []agent.ModelResult{
				routed(toolStep("parent", "dispatch", `{"tasks":[{"task":"inspect","scope":"a"}]}`)),
				routed(answerStep("done")),
			}}
			res, err := newOrchestratorFactory(parent, flags{dispatch: true}, nil, nil)().Run(t.Context(), agent.Request{Goal: "inspect", Tools: []agent.Tool{d}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireRecords(t, res, 1)
			if c := res.ToolCalls[0]; !c.Invoked || c.IsError || c.Blocked {
				t.Fatalf("parent call = %+v, want an invoked, successful dispatch", c)
			}

			// Neither protected file may have been read into the child's context,
			// whatever the row, nor surfaced in the parent's own messages.
			for _, token := range []string{envSentinelToken, notesMarker} {
				for _, req := range child.reqs {
					for _, m := range req.Messages {
						if strings.Contains(m.Content, token) {
							t.Fatalf("protected file content %q reached the child: %q", token, m.Content)
						}
					}
				}
				for _, m := range res.Messages {
					if strings.Contains(m.Content, token) {
						t.Fatalf("protected file content %q reached the parent: %q", token, m.Content)
					}
				}
			}
			if tc.childScore > 0 {
				assertBlockedObservationSeen(t, child.reqs, credentialBlocked)
			}

			var env dispatchTestEnvelope
			for _, m := range res.Messages {
				if m.Role == "tool" {
					if err := json.Unmarshal([]byte(m.Content), &env); err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(env.Results) != 1 {
				t.Fatalf("child envelope = %+v, want exactly one result", env.Results)
			}
			if r := env.Results[0]; r.RiskScore != tc.childScore || r.Summary != "done" || r.Error != "" || r.StopReason != "completed" || r.Model != childModel {
				t.Fatalf("child envelope = %+v, want score %d, summary done, completed, model %s", r, tc.childScore, childModel)
			}

			if tc.denied == 0 {
				if res.Risk != nil {
					t.Fatalf("parent risk = %+v, want nil: the blocked read must not count as a native denial", res.Risk)
				}
				return
			}
			if res.Risk == nil || res.Risk.Score != 10*tc.denied {
				t.Fatalf("parent risk = %+v, want exactly the native score %d (the child's %d is not added)", res.Risk, 10*tc.denied, tc.childScore)
			}
			if got, want := findingPairs(res.Risk.Findings), []findingPair{{"child_scope_denials", "child_scope_denied"}}; !slices.Equal(got, want) {
				t.Fatalf("parent findings = %+v, want %+v", got, want)
			}
			if f := res.Risk.Findings[0]; f.Detail != fmt.Sprintf("dispatch task 1: %d request(s) denied by workspace policy", tc.denied) || f.ToolCallID != "parent" {
				t.Fatalf("parent finding = %+v", f)
			}
		})
	}
}
