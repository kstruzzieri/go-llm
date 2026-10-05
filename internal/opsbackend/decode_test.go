package opsbackend

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsfixture"
)

func TestDecodeVersion(t *testing.T) {
	if v, ok := decodeVersion([]byte(opsfixture.DefaultVersion)); !ok || v.Version != "v235" {
		t.Fatalf("llama-swap version = %+v, %v", v, ok)
	}
	for _, body := range []string{`{"version":"0.12.3"}`, `404 page not found`, `{"version":235,"commit":"x","build_date":"y"}`, `{"version":"v235","commit":"x","build_date":"y"} trailing`} {
		if _, ok := decodeVersion([]byte(body)); ok {
			t.Fatalf("decodeVersion(%q) classified as llama-swap", body)
		}
	}
}

func TestDecodeRunningKeepsOnlyApprovedFields(t *testing.T) {
	got, err := decodeRunning([]byte(opsfixture.DefaultRunning))
	if err != nil {
		t.Fatal(err)
	}
	want := []RunningModel{{Model: "gemma4:31b", State: "ready", TTL: 600}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("running = %+v, want %+v", got, want)
	}
}

func TestDecodeRunningRejectsWholeSample(t *testing.T) {
	for _, body := range []string{
		`{"running":null}`,
		`{}`,
		`null`,
		`[]`,
		`{"running":[{"model":"","state":"ready"}]}`,
		`{"running":[{"model":"a","state":"bogus"}]}`,
		`{"running":[{"model":"a"}]}`,
		`{"running":[null]}`,
		`{"running":[]} {"running":[]}`,
	} {
		if _, err := decodeRunning([]byte(body)); !isCode(err, CodeMalformed) {
			t.Fatalf("decodeRunning(%q) = %v, want malformed", body, err)
		}
	}
	if got, err := decodeRunning([]byte(`{"running":[]}`)); err != nil || len(got) != 0 || got == nil {
		t.Fatalf("explicit empty list = %v, %v; want empty non-nil", got, err)
	}
}

func TestDecodeMetricsDropsRawPathAndText(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	rows, err := decodeMetrics([]byte(opsfixture.MetricsJSON(opsfixture.DefaultRows(now)...)))
	if err != nil {
		t.Fatal(err)
	}
	assertNoSentinels(t, rows)
	if len(rows) != 3 || rows[0].PathClass != "chat" || rows[2].PathClass != "other" || rows[2].Status != 500 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[1].PromptPerSecond != -1 || rows[1].InputTokens != 1200 {
		t.Fatalf("usage-only row = %+v", rows[1])
	}
	for _, body := range []string{`null`, `{}`, `[null]`, `[{"id":-1,"timestamp":"2026-10-05T10:00:00Z","model":"m"}]`, `[{"id":1,"model":"m"}]`, `[{"id":1,"timestamp":"2026-10-05T10:00:00Z","model":""}]`} {
		if _, err := decodeMetrics([]byte(body)); !isCode(err, CodeMalformed) {
			t.Fatalf("decodeMetrics(%q) = %v, want malformed", body, err)
		}
	}
}

// TestPathClass pins the req_path class map, including the versionless
// routes: v235 strips "/v" from "/v/..." in place before the metrics
// middleware records r.URL.Path, so they reach the ring without a version.
func TestPathClass(t *testing.T) {
	for p, want := range map[string]string{
		"/v1/chat/completions": "chat", "/chat/completions": "chat",
		"/v1/completions": "completions", "/completion": "completions", "/completions": "completions",
		"/v1/embeddings": "embeddings", "/embedding": "embeddings", "/embeddings": "embeddings",
		"/infill":    "infill",
		"/v1/rerank": "rerank", "/rerank": "rerank", "/reranking": "rerank", "/v1/reranking": "rerank",
		"/v1/responses": "other", "/responses": "other", "/v1/messages": "other", "/messages": "other",
		"/v1/chat/completions?x=1": "other", "/v/chat/completions": "other", "": "other",
	} {
		if got := pathClass(p); got != want {
			t.Fatalf("pathClass(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestDecodeModelsMarksPeers(t *testing.T) {
	got, err := decodeModels([]byte(opsfixture.DefaultModels))
	if err != nil {
		t.Fatal(err)
	}
	want := []ListedModel{{ID: "gemma4:31b"}, {ID: "qwen3-embedding:8b"}, {ID: "peer-model", Peer: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %+v", got)
	}
	for _, body := range []string{`{"data":null}`, `{"data":[null]}`} {
		if _, err := decodeModels([]byte(body)); !isCode(err, CodeMalformed) {
			t.Fatalf("decodeModels(%q) = %v, want malformed", body, err)
		}
	}
	for _, marker := range []string{`null`, `7`, `{}`} {
		got, err := decodeModels([]byte(`{"data":[{"id":"x","meta":{"llamaswap":{"peerID":` + marker + `}}}]}`))
		if err != nil || len(got) != 1 || !got[0].Peer {
			t.Fatalf("peerID %s must mark a peer (fail closed): %+v, %v", marker, got, err)
		}
	}
	for _, meta := range []string{`"lab"`, `null`, `[]`} {
		if _, err := decodeModels([]byte(`{"data":[{"id":"x","meta":{"llamaswap":` + meta + `}}]}`)); !isCode(err, CodeMalformed) {
			t.Fatalf("llamaswap meta %s must fail the sample: %v", meta, err)
		}
	}
}

func TestDecodePS(t *testing.T) {
	got, err := decodePS([]byte(`{"models":[{"name":"Llama3:latest","model":"llama3:latest","size_vram":5000,"expires_at":"2026-10-05T10:05:00Z","digest":"sha256:x"}]}`))
	if err != nil || len(got) != 1 || got[0].Name != "Llama3:latest" || got[0].SizeVRAM != 5000 {
		t.Fatalf("ps = %+v, %v", got, err)
	}
	for _, body := range []string{`{"models":null}`, `{"models":[null]}`} {
		if _, err := decodePS([]byte(body)); !isCode(err, CodeMalformed) {
			t.Fatalf("decodePS(%q) = %v, want malformed", body, err)
		}
	}
}

func TestNormalizeOllama(t *testing.T) {
	for in, want := range map[string]string{
		"llama3":                            "llama3:latest",
		"Llama3:8B":                         "llama3:8b",
		"registry.ollama.ai/library/llama3": "llama3:latest",
		"library/qwen3:8b":                  "qwen3:8b",
		"hf.co/user/model:Q4_K_M":           "hf.co/user/model:q4_k_m",
		"localhost:5000/team/model":         "localhost:5000/team/model:latest",
	} {
		if got := NormalizeOllama(in); got != want {
			t.Fatalf("NormalizeOllama(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResidencyOf(t *testing.T) {
	for raw, want := range map[string]string{"starting": "loading", "ready": "loaded", "stopping": "unloading", "stopped": "unloaded", "shutdown": "unloaded", "": "unloaded"} {
		if got := ResidencyOf(raw); got != want {
			t.Fatalf("ResidencyOf(%q) = %q, want %q", raw, got, want)
		}
	}
}

// assertNoSentinels walks every string reachable from v.
func assertNoSentinels(t *testing.T, v any) {
	t.Helper()
	var walk func(reflect.Value)
	walk = func(rv reflect.Value) {
		switch rv.Kind() {
		case reflect.String:
			for _, s := range opsfixture.AllSentinels {
				if strings.Contains(rv.String(), s) {
					t.Fatalf("sentinel %q retained in %q", s, rv.String())
				}
			}
			if opsfixture.ContainsSentinel(rv.String()) {
				t.Fatalf("sentinel prefix retained in %q", rv.String())
			}
		case reflect.Pointer, reflect.Interface:
			if !rv.IsNil() {
				walk(rv.Elem())
			}
		case reflect.Struct:
			for i := 0; i < rv.NumField(); i++ {
				walk(rv.Field(i))
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < rv.Len(); i++ {
				walk(rv.Index(i))
			}
		case reflect.Map:
			it := rv.MapRange()
			for it.Next() {
				walk(it.Key())
				walk(it.Value())
			}
		}
	}
	walk(reflect.ValueOf(v))
}
