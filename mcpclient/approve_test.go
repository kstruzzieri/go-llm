package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestApproveChecksConnectionBeforeLaunch(t *testing.T) {
	pins := testPins(t)
	s, counter := countedServer(t, []string{"a"}, tool("read"))
	inspected, err := Inspect(context.Background(), Implementation{Name: "test"}, s, pins)
	if err != nil {
		t.Fatal(err)
	}
	before := counter.dials.Load()
	wrong := approvalFor(inspected)
	wrong.Connection = "hmac-sha256:" + strings.Repeat("0", 64)
	_, err = Approve(context.Background(), Implementation{Name: "test"}, s, pins, wrong)
	if failure := admissionOf(err); failure == nil || failure.Reason != "connection_mismatch" || counter.dials.Load() != before || pinBytes(t, pins, "fs") != nil {
		t.Fatalf("mismatched connection = (%v, %d new dials), want connection_mismatch before launch", err, counter.dials.Load()-before)
	}
	if text := err.Error(); !strings.HasSuffix(text, "; "+reviewHintFS) {
		t.Fatalf("connection_mismatch Error() = %q, want the review hint (spec §5.9)", text)
	}
	const (
		badDigest     = "mcpclient: digest must be sha256: followed by 64 lowercase hex digits"
		badConnection = "mcpclient: connection must be hmac-sha256: followed by 64 lowercase hex digits"
	)
	// The specific format errors, not a later connection_mismatch: without
	// format validation a malformed fingerprint would still be refused, but
	// for the wrong reason.
	for _, tt := range []struct {
		bad  ApprovalDigests
		want string
	}{
		{ApprovalDigests{Catalog: inspected.CandidateDigest}, badConnection},
		{ApprovalDigests{Catalog: inspected.CandidateDigest, Connection: inspected.CandidateDigest}, badConnection},
		{ApprovalDigests{Connection: inspected.CandidateConnection.Fingerprint}, badDigest},
	} {
		if _, err := Approve(context.Background(), Implementation{Name: "test"}, s, pins, tt.bad); err == nil || err.Error() != tt.want {
			t.Fatalf("malformed approval %+v = %v, want %q", tt.bad, err, tt.want)
		}
	}
	if counter.dials.Load() != before {
		t.Fatal("malformed approval launched the server")
	}
}

func admissionOf(err error) *AdmissionError {
	failure, _ := err.(*AdmissionError)
	return failure
}

func TestApproveMigratesV1Record(t *testing.T) {
	pins := testPins(t)
	writeV1Pin(t, pins, "fs", pinCatalog(t, "fs", "").toolCatalog)
	s, _ := countedServer(t, nil, tool("read"))
	inspected, err := Inspect(context.Background(), Implementation{Name: "test"}, s, pins)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.PinnedConnection != "" || inspected.ConnectionChanges != nil {
		t.Fatalf("v1 inspection = (%q, %q), want no pinned connection", inspected.PinnedConnection, inspected.ConnectionChanges)
	}
	// An in-memory fixture serves one dial; same identity (nil command).
	fresh, _ := countedServer(t, nil, tool("read"))
	if _, err := Approve(context.Background(), Implementation{Name: "test"}, fresh, pins, approvalFor(inspected)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pinBytes(t, pins, "fs"), []byte(`"version":2`)) {
		t.Fatal("approval did not migrate the record to version 2")
	}
	again, _ := countedServer(t, nil, tool("read"))
	if m, w := connectOnce(t, pins, true, again); len(m.Tools()) != 1 {
		t.Fatalf("approved connection refused: %v", w)
	}
}

// Approve pins the live candidate's connection, never the prior one: after an
// approved identity change the new connection is admitted.
func TestApproveWritesLiveConnection(t *testing.T) {
	pins := testPins(t)
	first, _ := countedServer(t, []string{"a"}, tool("read"))
	if m, w := connectOnce(t, pins, false, first); len(m.Tools()) != 1 {
		t.Fatalf("first contact: %v", w)
	}
	changed, _ := countedServer(t, []string{"b"}, tool("read"))
	inspected, err := Inspect(context.Background(), Implementation{Name: "test"}, changed, pins)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.PinnedConnection == "" || inspected.PinnedConnection == inspected.CandidateConnection.Fingerprint ||
		!slices.Equal(inspected.ConnectionChanges, []string{"launcher", "target", "argv"}) {
		t.Fatalf("changed inspection = (%q, %q), want a distinct pinned fingerprint and [launcher target argv]", inspected.PinnedConnection, inspected.ConnectionChanges)
	}
	if text := inspected.String(); !strings.Contains(text, "connection changed: launcher, target, argv\n") {
		t.Fatalf("inspection lacks the change labels:\n%s", text)
	}
	fresh, _ := countedServer(t, []string{"b"}, tool("read"))
	if _, err := Approve(context.Background(), Implementation{Name: "test"}, fresh, pins, approvalFor(inspected)); err != nil {
		t.Fatal(err)
	}
	again, _ := countedServer(t, []string{"b"}, tool("read"))
	if m, w := connectOnce(t, pins, true, again); len(m.Tools()) != 1 {
		t.Fatalf("approved connection refused: %v", w)
	}
}

// Inspect and Approve launch exactly what Connect launches: the child gets only
// the policy environment and the frozen working directory, under the
// fingerprint Connect pinned.
func TestInspectAndApproveLaunchUnderConnectPolicy(t *testing.T) {
	dir := t.TempDir()
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := probeServer(t).WithDir(dir).WithEnv(InheritEnv("PROBE_TOKEN"), SetEnv("PROBE_SET", "explicit"))
	le := testLaunchEnv(map[string]string{"PATH": "/usr/bin:/bin", "HOME": dir, "CANARY_SECRET": "canary", "PROBE_TOKEN": "tok"})
	pins := testPins(t)
	m, w, err := connectWithHooks(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: pins}, &connectHooks{launch: &le})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tools()) != 1 {
		t.Fatalf("first contact: %v", w)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	inspected, err := inspectOrApprove(context.Background(), Implementation{Name: "test"}, s, pins, le, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.PinnedConnection == "" || inspected.CandidateConnection.Fingerprint != inspected.PinnedConnection || inspected.ConnectionChanges != nil {
		t.Fatalf("inspection = (pinned %q, candidate %q, %q), want the fingerprint Connect pinned", inspected.PinnedConnection, inspected.CandidateConnection.Fingerprint, inspected.ConnectionChanges)
	}
	want := approvalFor(inspected)
	approved, err := inspectOrApprove(context.Background(), Implementation{Name: "test"}, s, pins, le, &want)
	if err != nil {
		t.Fatal(err)
	}
	for op, result := range map[string]*Inspection{"Inspect": inspected, "Approve": approved} {
		var report probeReport
		if err := json.Unmarshal([]byte(result.candidate.entries[0].Description), &report); err != nil {
			t.Fatalf("%s probe report: %v", op, err)
		}
		if wantNames := []string{"HOME", "PATH", "PROBE_SET", "PROBE_TOKEN"}; !slices.Equal(report.Names, wantNames) {
			t.Fatalf("%s child env names = %q, want exactly %q", op, report.Names, wantNames)
		}
		if report.Values["PROBE_TOKEN"] != "tok" || report.Values["PROBE_SET"] != "explicit" || report.Cwd != wantDir {
			t.Fatalf("%s child values/cwd = (%v, %q), want (tok, explicit, %q)", op, report.Values, report.Cwd, wantDir)
		}
	}
}

func TestInspectionRendersConnectionWithoutSecrets(t *testing.T) {
	pins := testPins(t)
	s, _ := countedServer(t, []string{"/bin/launcher", "--token=canary-argv"}, tool("read"))
	s.dir, s.env = "/work", []EnvVar{SetEnv("MODE", "canary-value"), InheritEnv("TOKEN")}
	inspected, err := Inspect(context.Background(), Implementation{Name: "test"}, s, pins)
	if err != nil {
		t.Fatal(err)
	}
	text := inspected.String()
	for _, want := range []string{
		"connection pinned: \nconnection candidate: " + inspected.CandidateConnection.Fingerprint + "\n",
		"connection kind: stdio\n",
		"connection launcher: \"/bin/launcher\"\n",
		"connection dir: \"/work\"\n",
		"connection env: inherit:TOKEN, set:MODE\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("inspection lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "canary") {
		t.Fatalf("inspection leaked argv or an env value:\n%s", text)
	}

	srv := gomcp.NewServer(&gomcp.Implementation{Name: "http-inspect"}, nil)
	srv.AddTool(&gomcp.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		return &gomcp.CallToolResult{}, nil
	})
	httpServer := httptest.NewServer(gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return srv }, nil))
	t.Cleanup(httpServer.Close)
	inspected, err = Inspect(context.Background(), Implementation{Name: "test"}, HTTPServer("api", httpServer.URL+"/canary-path?token=canary-query"), pins)
	if err != nil {
		t.Fatal(err)
	}
	text = inspected.String()
	if !strings.Contains(text, "connection origin: \""+httpServer.URL+"\"\n") || strings.Contains(text, "canary") {
		t.Fatalf("http inspection:\n%s", text)
	}
}
