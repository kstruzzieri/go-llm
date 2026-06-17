package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleXlamTools = `[{"name":"get_weather","description":"Get weather for a city","parameters":{"city":{"description":"City name","type":"string"}}},{"name":"stock_price","description":"Look up a stock price","parameters":{"ticker":{"description":"Ticker","type":"string"}}}]`

func TestXlamRecordToTraceGoldenEmpty(t *testing.T) {
	rec := xlamRecord{
		Query:   "Who won the 2019 NCAA Final Four?",
		Tools:   sampleXlamTools,
		Answers: "[]",
	}
	tr, err := xlamRecordToTrace(rec, 7)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if tr.ID != "xlam-irrel-0007" {
		t.Errorf("ID = %q, want xlam-irrel-0007", tr.ID)
	}
	if tr.Source != xlamSource {
		t.Errorf("Source = %q, want %q", tr.Source, xlamSource)
	}
	if tr.System == "" {
		t.Error("System is empty (validateTrace would reject)")
	}
	if len(tr.Turns) != 1 || tr.Turns[0].Role != "user" || tr.Turns[0].Content != rec.Query {
		t.Errorf("Turns = %+v, want one user turn with the query", tr.Turns)
	}
	if len(tr.Golden.ToolCalls) != 0 {
		t.Errorf("golden.tool_calls = %v, want empty (golden-empty)", tr.Golden.ToolCalls)
	}
	if tr.Golden.FinalAnswerCriteria == "" {
		t.Error("golden.final_answer_criteria empty — would be excluded from restraint pairing")
	}
	if tr.Golden.Difficulty != "tempting" {
		t.Errorf("difficulty = %q, want tempting", tr.Golden.Difficulty)
	}
	if tr.Golden.RestraintRationale == "" || tr.Golden.FailureMode == "" {
		t.Errorf("audit fields empty: %+v", tr.Golden)
	}
	if len(tr.Tools) != 2 {
		t.Errorf("Tools len = %d, want 2", len(tr.Tools))
	}
	// Must survive the loader's validation (it is written then re-read by the harness).
	if err := validateTrace(tr); err != nil {
		t.Errorf("validateTrace rejects converted trace: %v", err)
	}
	// The offered tool names must be declarable (so replay exposes them).
	names, err := declaredToolNames(tr.Tools)
	if err != nil {
		t.Fatalf("declaredToolNames: %v", err)
	}
	if _, ok := names["get_weather"]; !ok {
		t.Errorf("declared tool names = %v, want get_weather present", names)
	}
}

// The converted trace must score held when the candidate calls no tool, and
// diverged when it does — i.e. it is a genuine restraint-eligible trace per the
// existing metric (PR #178), and carries enough context for paired inclusion.
func TestXlamConvertedTraceScoresRestraint(t *testing.T) {
	tr, err := xlamRecordToTrace(xlamRecord{Query: "q", Tools: sampleXlamTools, Answers: "[]"}, 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !restraintArtifactTraceContextValid(Artifact{TraceID: tr.ID, Trace: tr}) {
		t.Fatal("trace lacks valid restraint context (id/rubric)")
	}
	held, computed := restraintSignals(tr, []Turn{{Role: "assistant", Content: "I can't help with that from the tools given."}})
	if !computed || held != 1 {
		t.Errorf("no-tool transcript: held=%v computed=%v, want 1/true", held, computed)
	}
	div, computed := restraintSignals(tr, []Turn{{Role: "assistant", ToolCalls: []ToolCall{{Name: "get_weather"}}}})
	if !computed || div != 0 {
		t.Errorf("tool-call transcript: held=%v computed=%v, want 0/true", div, computed)
	}
}

// The real replay path decodes tools via decodeTraceTools (not the looser
// declaredToolNames), which routes bare-name tools through the MCP branch and
// reads `inputSchema`. xLAM tools carry `parameters`, so the converter must emit
// a shape decodeTraceTools accepts — otherwise every replay fails on tool decode.
func TestXlamConvertedToolsDecodeForReplay(t *testing.T) {
	tr, err := xlamRecordToTrace(xlamRecord{Query: "q", Tools: sampleXlamTools, Answers: "[]"}, 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	tools, err := decodeTraceTools(tr.Tools)
	if err != nil {
		t.Fatalf("decodeTraceTools rejected converted tools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("decoded %d tools, want 2", len(tools))
	}
	if strings.TrimSpace(tools[0].Function.Name) == "" {
		t.Errorf("decoded tool missing function name: %+v", tools[0])
	}
	if len(tools[0].Function.Parameters) == 0 {
		t.Errorf("decoded tool missing parameters schema: %+v", tools[0])
	}
}

func TestXlamToolToProviderTool(t *testing.T) {
	// Happy path: wraps xLAM parameters as a JSON-schema object under function.
	out, err := xlamToolToProviderTool(json.RawMessage(`{"name":"f","description":"d","parameters":{"x":{"type":"string"}}}`))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var env struct {
		Type     string `json:"type"`
		Function struct {
			Name       string         `json:"name"`
			Parameters map[string]any `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != "function" || env.Function.Name != "f" {
		t.Errorf("envelope = %+v, want type=function name=f", env)
	}
	if env.Function.Parameters["type"] != "object" {
		t.Errorf("parameters.type = %v, want object", env.Function.Parameters["type"])
	}
	if _, ok := env.Function.Parameters["properties"]; !ok {
		t.Errorf("parameters missing properties: %+v", env.Function.Parameters)
	}

	// No-parameters tool still yields a valid object schema (empty properties).
	out, err = xlamToolToProviderTool(json.RawMessage(`{"name":"g","description":"d"}`))
	if err != nil {
		t.Fatalf("no-params convert: %v", err)
	}
	if tools, err := decodeTraceTools([]json.RawMessage{out}); err != nil || len(tools) != 1 {
		t.Errorf("no-params tool failed to decode: %v (n=%d)", err, len(tools))
	}

	// Missing name is an error.
	if _, err := xlamToolToProviderTool(json.RawMessage(`{"description":"d","parameters":{}}`)); err == nil {
		t.Error("want error for tool missing name")
	}

	// "parameters": null must normalize to an empty object schema, not properties:null.
	out, err = xlamToolToProviderTool(json.RawMessage(`{"name":"h","description":"d","parameters":null}`))
	if err != nil {
		t.Fatalf("null-params convert: %v", err)
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("null-params unmarshal: %v", err)
	}
	if props, ok := env.Function.Parameters["properties"].(map[string]any); !ok || props == nil {
		t.Errorf("null parameters did not normalize to empty object: %+v", env.Function.Parameters["properties"])
	}
	if tools, err := decodeTraceTools([]json.RawMessage{out}); err != nil || len(tools) != 1 {
		t.Errorf("null-params tool failed to decode: %v", err)
	}

	// Non-object parameters (an array) is malformed source → error (trace dropped upstream).
	if _, err := xlamToolToProviderTool(json.RawMessage(`{"name":"i","parameters":[1,2]}`)); err == nil {
		t.Error("want error for non-object parameters")
	}
}

func TestXlamRecordToTraceRejectsNonEmptyAnswers(t *testing.T) {
	rec := xlamRecord{Query: "q", Tools: sampleXlamTools, Answers: `[{"name":"get_weather","arguments":{"city":"x"}}]`}
	if _, err := xlamRecordToTrace(rec, 0); err == nil {
		t.Fatal("want error for non-empty answers (not an irrelevance case)")
	}
}

func TestXlamRecordToTraceRejectsBadInput(t *testing.T) {
	for name, rec := range map[string]xlamRecord{
		"empty query":  {Query: "  ", Tools: sampleXlamTools, Answers: "[]"},
		"bad tools":    {Query: "q", Tools: "not json", Answers: "[]"},
		"bad answers":  {Query: "q", Tools: sampleXlamTools, Answers: "not json"},
		"null answers": {Query: "q", Tools: sampleXlamTools, Answers: "null"},
		"null tools":   {Query: "q", Tools: "null", Answers: "[]"},
		"empty answers string": {Query: "q", Tools: sampleXlamTools, Answers: "  "},
	} {
		if _, err := xlamRecordToTrace(rec, 0); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestImportXlamIrrelevanceFilterSampleDeterministic(t *testing.T) {
	// 5 records: 2 valid (>=1 tool, empty answers), 1 zero-tool, 1 non-empty answers, 1 valid.
	recs := []xlamRecord{
		{Query: "a", Tools: sampleXlamTools, Answers: "[]"},
		{Query: "b", Tools: "[]", Answers: "[]"},                                  // 0 tools -> filtered
		{Query: "c", Tools: sampleXlamTools, Answers: `[{"name":"x"}]`},           // has answer -> filtered
		{Query: "d", Tools: sampleXlamTools, Answers: "[]"},
		{Query: "e", Tools: sampleXlamTools, Answers: "[]"},
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "xlam.json")
	blob, _ := json.Marshal(recs)
	if err := os.WriteFile(src, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(outSub string) ([]ManifestEntry, xlamImportResult) {
		out := filepath.Join(dir, outSub)
		mf := filepath.Join(dir, outSub+"-manifest.jsonl")
		res, err := importXlamIrrelevance(xlamImportOptions{
			SrcPath: src, OutDir: out, ManifestPath: mf, N: 2, Seed: 42, MinTools: 1,
		})
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		m, err := loadManifest(mf)
		if err != nil {
			t.Fatalf("loadManifest: %v", err)
		}
		return m.Entries, res
	}
	e1, res := run("a")
	if res.Written != 2 || res.Eligible != 3 || res.Filtered != 2 {
		t.Errorf("result = %+v, want Written=2 Eligible=3 Filtered=2", res)
	}
	if len(e1) != 2 {
		t.Fatalf("manifest entries = %d, want 2", len(e1))
	}
	for _, e := range e1 {
		if e.Category != "irrelevance" || e.Source != xlamSource || e.Partition != PartitionChallenge || !e.AllowedAsModelEvidence {
			t.Errorf("manifest entry wrong: %+v", e)
		}
		// Every emitted trace loads + is golden-empty.
		traces, err := loadTraces([]string{filepath.Join(dir, "a", e.TraceID+".json")})
		if err != nil {
			t.Errorf("load %s: %v", e.TraceID, err)
			continue
		}
		if len(traces[0].Golden.ToolCalls) != 0 {
			t.Errorf("%s not golden-empty", e.TraceID)
		}
	}
	// Same seed -> same sampled trace IDs (determinism).
	e2, _ := run("b")
	if len(e2) != len(e1) {
		t.Fatalf("determinism: lengths differ")
	}
	for i := range e1 {
		if e1[i].TraceID != e2[i].TraceID {
			t.Errorf("determinism: entry %d %q != %q", i, e1[i].TraceID, e2[i].TraceID)
		}
	}
}

func TestImportXlamIrrelevanceNTakesAllWhenNonPositive(t *testing.T) {
	recs := []xlamRecord{
		{Query: "a", Tools: sampleXlamTools, Answers: "[]"},
		{Query: "b", Tools: sampleXlamTools, Answers: "[]"},
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "x.json")
	blob, _ := json.Marshal(recs)
	if err := os.WriteFile(src, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := importXlamIrrelevance(xlamImportOptions{
		SrcPath: src, OutDir: filepath.Join(dir, "o"), ManifestPath: filepath.Join(dir, "m.jsonl"),
		N: 0, Seed: 1, MinTools: 1,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Written != 2 || res.Eligible != 2 {
		t.Errorf("N<=0 should write all eligible: %+v", res)
	}
}

func TestImportXlamIrrelevanceDeepFilterSkipsUnconvertible(t *testing.T) {
	// Second record passes the shallow filter (one tool element, empty answers)
	// but the tool element is not an object, so conversion (validateTrace) fails.
	// It must be Filtered, not abort the batch.
	recs := []xlamRecord{
		{Query: "a", Tools: sampleXlamTools, Answers: "[]"},
		{Query: "b", Tools: `["notanobject"]`, Answers: "[]"},
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "x.json")
	blob, _ := json.Marshal(recs)
	if err := os.WriteFile(src, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := importXlamIrrelevance(xlamImportOptions{
		SrcPath: src, OutDir: filepath.Join(dir, "o"), ManifestPath: filepath.Join(dir, "m.jsonl"),
		N: 0, Seed: 1, MinTools: 1,
	})
	if err != nil {
		t.Fatalf("one unconvertible row must not abort the batch: %v", err)
	}
	if res.Written != 1 || res.Eligible != 1 || res.Filtered != 1 {
		t.Errorf("result = %+v, want Written=1 Eligible=1 Filtered=1", res)
	}
}

func TestImportXlamIrrelevanceClearsStaleTraces(t *testing.T) {
	recs := []xlamRecord{{Query: "a", Tools: sampleXlamTools, Answers: "[]"}}
	dir := t.TempDir()
	src := filepath.Join(dir, "x.json")
	blob, _ := json.Marshal(recs)
	if err := os.WriteFile(src, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "o")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(out, "xlam-irrel-9999.json")
	if err := os.WriteFile(stale, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := importXlamIrrelevance(xlamImportOptions{
		SrcPath: src, OutDir: out, ManifestPath: filepath.Join(dir, "m.jsonl"),
		N: 0, Seed: 1, MinTools: 1,
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale trace %s not removed (err=%v)", stale, err)
	}
}

func TestImportXlamIrrelevanceRejectsNullFieldsEvenAtMinToolsZero(t *testing.T) {
	// With min-tools=0 a "null" tools/answers row must still be filtered, not
	// imported as a zero-tool / false golden-empty trace.
	recs := []xlamRecord{
		{Query: "ok", Tools: sampleXlamTools, Answers: "[]"},
		{Query: "nulltools", Tools: "null", Answers: "[]"},
		{Query: "nullanswers", Tools: sampleXlamTools, Answers: "null"},
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "x.json")
	blob, _ := json.Marshal(recs)
	if err := os.WriteFile(src, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := importXlamIrrelevance(xlamImportOptions{
		SrcPath: src, OutDir: filepath.Join(dir, "o"), ManifestPath: filepath.Join(dir, "m.jsonl"),
		N: 0, Seed: 1, MinTools: 0,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Written != 1 || res.Eligible != 1 || res.Filtered != 2 {
		t.Errorf("result = %+v, want Written=1 Eligible=1 Filtered=2 (null rows filtered)", res)
	}
}

func TestImportXlamIrrelevanceRejectsNegativeMinTools(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.json")
	if err := os.WriteFile(src, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := importXlamIrrelevance(xlamImportOptions{
		SrcPath: src, OutDir: filepath.Join(dir, "o"), ManifestPath: filepath.Join(dir, "m.jsonl"),
		N: 1, Seed: 1, MinTools: -1,
	})
	if err == nil {
		t.Fatal("want error for negative min-tools, got nil")
	}
}

func TestImportXlamIrrelevanceErrorsWhenNoneEligible(t *testing.T) {
	// All records filtered (0 tools), so nothing is written.
	recs := []xlamRecord{{Query: "a", Tools: "[]", Answers: "[]"}}
	dir := t.TempDir()
	src := filepath.Join(dir, "x.json")
	blob, _ := json.Marshal(recs)
	if err := os.WriteFile(src, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := importXlamIrrelevance(xlamImportOptions{
		SrcPath: src, OutDir: filepath.Join(dir, "o"), ManifestPath: filepath.Join(dir, "m.jsonl"),
		N: 5, Seed: 1, MinTools: 1,
	})
	if err == nil {
		t.Fatal("want error when no eligible records, got nil")
	}
}
