package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// newRecallPilotStub starts an openai-compat server that answers like a model
// over the pilot families: a Trace A request (one tool result) states the
// lesson value for the families in rely and calls read_file on the named file
// for the rest; a Trace B request states the value its read_file result shows.
// prompt_tokens is the request size in bytes / 4, so it grows with the request.
func newRecallPilotStub(t *testing.T, fams []recallFamily, rely map[string]bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		var tools []string
		for _, m := range body.Messages {
			if m.Role == "tool" {
				tools = append(tools, m.Content)
			}
		}
		message := `{"role":"assistant","content":"no matching family"}`
		for _, f := range fams {
			if len(tools) == 0 || !strings.Contains(tools[0], f.Lesson) {
				continue
			}
			value := f.Value.Lesson
			switch {
			case len(tools) == 1 && !rely[f.ID]:
				args := strconv.Quote(fmt.Sprintf(`{"path":%q}`, f.NamedFile))
				message = `{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":` + args + `}}]}`
				value = ""
			case len(tools) == 2 && statesValue(tools[1], f.Value, f.Value.Changed):
				value = f.Value.Changed
			}
			if value != "" {
				message = `{"role":"assistant","content":` + strconv.Quote("It is set to "+value+".") + `}`
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":`+message+
			`}],"usage":{"prompt_tokens":`+strconv.Itoa(len(raw)/4)+`,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recallDryRunRegistration writes a copy of the committed pilot registration
// bound to endpoint and returns its path. The committed registration names no
// endpoint, which means the Stage 0 llama-swap endpoint 127.0.0.1:8090, and a
// capture may reach only the registered endpoint (its destination binding),
// so a dry run against a stub there would need that port and could reach a
// real llama-swap instead. The copy registers the stub's own loopback URL, with
// a serving_stack other than llama-swap's as the loader requires for an
// explicit endpoint; its traces, hashes, cases digest, rule text, scorer
// revision and models are the committed registration's.
func recallDryRunRegistration(t *testing.T, committed, endpoint string) string {
	t.Helper()
	raw, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	var reg map[string]any
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	recallTestEndpoint(endpoint)(reg)
	out, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "registration-pilot-dryrun.json")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPilotFixtureDryRun sends the committed pilot traces through the real
// capture path (registration binding, destination binding, capture) for the
// four lineup models against a stub model, then through the report: the
// pilot view scores every reply as the stub's behavior implies, and a
// gemma-only Trace A arm 0 and 1 capture (the runbook's headroom shape)
// continues when the stub relies on 8 of 12 lessons and stops at 2 of 12. The
// registration is the committed registration-pilot.json bound to the stub's
// endpoint (recallDryRunRegistration).
func TestPilotFixtureDryRun(t *testing.T) {
	t.Parallel()
	cases, err := loadRecallCases(filepath.Join(recallFixtureDir, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	regPath := filepath.Join(recallFixtureDir, "registration-pilot.json")
	reg, _, err := loadRecallRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	var paths, headroomPaths []string
	for _, row := range reg.Traces {
		p := filepath.Join(recallFixtureDir, "pilot", row.ID+".json")
		paths = append(paths, p)
		if _, cell, _ := parseRecallTraceID(row.ID); cell == "A-0" || cell == "A-1" {
			headroomPaths = append(headroomPaths, p)
		}
	}
	traces, err := loadTraces(paths)
	if err != nil {
		t.Fatal(err)
	}
	headroomTraces, err := loadTraces(headroomPaths)
	if err != nil {
		t.Fatal(err)
	}
	relyOn := func(ids ...string) map[string]bool {
		m := map[string]bool{}
		for _, id := range ids {
			m[id] = true
		}
		return m
	}
	many := relyOn("p01", "p02", "p03", "p04", "p05", "p06", "p07", "p08")

	t.Run("pilot view", func(t *testing.T) {
		t.Parallel()
		srv := newRecallPilotStub(t, cases.Families, many)
		dryReg := recallDryRunRegistration(t, regPath, srv.URL)
		out, _ := recallE2ECapture(t, srv.URL, dryReg, traces)
		text, verdict, err := runRecallReport(recallReportOptions{RegistrationPath: dryReg, Captures: []string{out}})
		if err != nil {
			t.Fatal(err)
		}
		if verdict != "" || !strings.Contains(text, "Every registered cell was attempted by a capture given.") {
			t.Fatalf("pilot view verdict %q, want none and every cell attempted:\n%s", verdict, text)
		}
		_, replies, ok := strings.Cut(text, "\n## Replies")
		if !ok {
			t.Fatalf("pilot view has no replies table:\n%s", text)
		}
		seen := map[[2]string]bool{}
		for _, line := range strings.Split(replies, "\n") {
			cols := strings.Split(line, " | ")
			if !strings.HasPrefix(line, "| openai-compat/") || len(cols) < 3 {
				continue
			}
			model, id, score := strings.TrimPrefix(cols[0], "| "), cols[1], cols[2]
			fam, cell, err := parseRecallTraceID(id)
			if err != nil {
				t.Fatal(err)
			}
			want := "answers"
			if strings.HasPrefix(cell, "A-") {
				want = map[bool]string{true: "relied", false: "re-check"}[many[fam]]
			}
			if score != want {
				t.Errorf("%s %s scored %q, want %q: %s", model, id, score, want, line)
			}
			seen[[2]string{model, id}] = true
		}
		if want := len(reg.Models) * len(reg.Traces); len(seen) != want {
			t.Fatalf("replies table has %d distinct (model, trace) rows, want %d", len(seen), want)
		}
	})

	for _, tc := range []struct {
		name    string
		rely    map[string]bool
		verdict recallVerdict
		line    string
	}{
		{"headroom continues", many, "continue", "arm 0 relies on the stale lesson in 8 of 12 families; arm 1 in 8 of 12."},
		{"headroom stops", relyOn("p03", "p10"), "stopped", "relies on the stale lesson in 2 of 12 arm 0 families (at most 2 stops Stage 0); that arm re-checks in 10 of 12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newRecallPilotStub(t, cases.Families, tc.rely)
			dryReg := recallDryRunRegistration(t, regPath, srv.URL)
			out, _ := recallE2ECapture(t, srv.URL, dryReg, headroomTraces, recallPrimaryModel)
			text, verdict, err := runRecallReport(recallReportOptions{RegistrationPath: dryReg, Captures: []string{out}, Headroom: true})
			if err != nil {
				t.Fatal(err)
			}
			if verdict != tc.verdict || !strings.Contains(text, tc.line) {
				t.Fatalf("headroom verdict %q, want %q with %q:\n%s", verdict, tc.verdict, tc.line, text)
			}
		})
	}
}
