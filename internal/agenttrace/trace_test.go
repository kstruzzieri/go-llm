package agenttrace

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func sampleResult() agent.Result {
	return agent.Result{
		Answer:     "the answer",
		Steps:      []agent.StepRecord{{Index: 0, Latency: 10 * time.Millisecond}},
		Usage:      provider.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		StopReason: agent.Completed,
	}
}

func TestBuildTrace_StatusAndPartial(t *testing.T) {
	meta := TraceMeta{Goal: "do a thing", System: "sys", MaxSteps: 16}
	res := sampleResult()

	ok := BuildTrace(meta, res, "completed", false, nil)
	if ok.Status != "completed" || ok.Partial {
		t.Fatalf("completed: status=%q partial=%v", ok.Status, ok.Partial)
	}
	if ok.Request.Goal != "do a thing" || ok.Result.Answer != "the answer" {
		t.Fatalf("request/result not embedded: %+v", ok)
	}
	if ok.SchemaVersion != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", ok.SchemaVersion, SchemaVersion)
	}

	bad := BuildTrace(meta, res, "canceled", true, context.Canceled)
	if bad.Status != "canceled" || !bad.Partial || bad.Error == "" {
		t.Fatalf("canceled: status=%q partial=%v error=%q", bad.Status, bad.Partial, bad.Error)
	}
}

func TestWriteTrace_ExclusiveAndSecured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20260628-run7.json")
	rec := BuildTrace(TraceMeta{Goal: "g"}, sampleResult(), "completed", false, nil)

	if err := WriteTrace(path, rec); err != nil {
		t.Fatalf("WriteTrace: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != fs.FileMode(0o600) {
		t.Fatalf("perm = %v, want 0600", info.Mode().Perm())
	}
	var got TraceRecord
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Result.Answer != "the answer" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Exclusive: a second write to the same path must fail, not clobber/append.
	if err := WriteTrace(path, rec); err == nil || !strings.Contains(err.Error(), "exist") {
		t.Fatalf("second WriteTrace err = %v, want exists error", err)
	}
}

// TestTraceRecordGroundingIsAdditive pins the compatibility contract for the
// #348 grounding payload: a trace without one must serialize to the exact bytes
// it did before the field existed, and one with a payload must embed it as a
// JSON object rather than a quoted string. json.RawMessage gets both wrong if
// the field is declared as a plain string or loses omitempty.
func TestTraceRecordGroundingIsAdditive(t *testing.T) {
	rec := BuildTrace(TraceMeta{Goal: "g", System: "s", MaxSteps: 4}, sampleResult(), "completed", false, nil)

	without, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "grounding") {
		t.Fatalf("a trace with no grounding must omit the key entirely:\n%s", without)
	}

	rec.Grounding = json.RawMessage(`{"status":"partial","reason":"","tokens":7}`)
	with, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(with), `"grounding":{"status":"partial","reason":"","tokens":7}`) {
		t.Fatalf("grounding not embedded verbatim as an object:\n%s", with)
	}
	if rec.SchemaVersion != 2 {
		t.Fatalf("an additive omitempty field must not bump the schema: %d", rec.SchemaVersion)
	}

	// The only difference between the two encodings is the appended key: the
	// rest of the record must be byte-identical, so a pre-#348 consumer reading
	// a grounding-free trace sees exactly what it always did.
	var a, b map[string]json.RawMessage
	if err := json.Unmarshal(without, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(with, &b); err != nil {
		t.Fatal(err)
	}
	delete(b, "grounding")
	if len(a) != len(b) {
		t.Fatalf("grounding changed the record shape: %d keys vs %d", len(a), len(b))
	}
	for k, v := range a {
		if string(b[k]) != string(v) {
			t.Fatalf("field %q changed: %s vs %s", k, v, b[k])
		}
	}
}
