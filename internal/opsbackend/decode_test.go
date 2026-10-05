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
	for _, body := range []string{`{"version":"0.12.3"}`, `404 page not found`, `{"version":235,"commit":"x","build_date":"y"}`, `{"version":"v235","commit":"x","build_date":"y"} trailing`, `{"version":"v235","commit":"x"}`, `{"version":"v235","build_date":"y"}`} {
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
		`{"running":[{"state":"ready"}]}`,
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
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.FixedZone("EDT", -4*60*60))
	src := opsfixture.DefaultRows(now)
	rows, err := decodeMetrics([]byte(opsfixture.MetricsJSON(src...)))
	if err != nil {
		t.Fatal(err)
	}
	assertNoSentinels(t, rows)
	if len(rows) != len(src) {
		t.Fatalf("rows = %+v", rows)
	}
	// Every permitted fact survives; the path survives only as its class.
	for i, class := range []string{"chat", "chat", "other"} {
		r, w := rows[i], src[i]
		if r.ID != int64(w.ID) || !r.Timestamp.Equal(w.At) || r.Timestamp.Location() != time.UTC || r.Model != w.Model ||
			r.Status != w.Status || r.PathClass != class || r.InputTokens != int64(w.Input) || r.OutputTokens != int64(w.Output) ||
			r.CacheTokens != int64(w.Cache) || r.PromptPerSecond != w.PromptPS || r.TokensPerSecond != w.TokensPS ||
			r.DurationMs != int64(w.DurationMs) {
			t.Fatalf("row %d = %+v, want the kept facts of %+v", i, r, w)
		}
	}
	for _, body := range []string{`null`, `{}`, `[null]`, `[{"timestamp":"2026-10-05T10:00:00Z","model":"m"}]`, `[{"id":-1,"timestamp":"2026-10-05T10:00:00Z","model":"m"}]`, `[{"id":1,"model":"m"}]`, `[{"id":1,"timestamp":"2026-10-05T10:00:00Z","model":""}]`} {
		if _, err := decodeMetrics([]byte(body)); !isCode(err, CodeMalformed) {
			t.Fatalf("decodeMetrics(%q) = %v, want malformed", body, err)
		}
	}
}

// TestPathClass pins the req_path class map, including the versionless
// routes: v235 strips "/v" from "/v/..." in place before the metrics
// middleware records r.URL.Path, so they usually reach the ring without a
// version, but /upstream/<model>/v/... is recorded unstripped.
func TestPathClass(t *testing.T) {
	for p, want := range map[string]string{
		"/v1/chat/completions": "chat", "/chat/completions": "chat",
		"/v1/completions": "completions", "/completion": "completions", "/completions": "completions",
		"/v1/embeddings": "embeddings", "/embedding": "embeddings", "/embeddings": "embeddings",
		"/infill":    "infill",
		"/v1/rerank": "rerank", "/rerank": "rerank", "/reranking": "rerank", "/v1/reranking": "rerank",
		"/v1/responses": "other", "/responses": "other", "/v1/messages": "other", "/messages": "other",
		"/v/chat/completions": "chat", "/v/completions": "completions", "/v/embeddings": "embeddings",
		"/v/rerank": "rerank", "/v/reranking": "rerank", "/v/responses": "other", "/v/messages": "other",
		"/v1/chat/completions?x=1": "other", "/v/messages/count_tokens": "other", "": "other",
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
	for _, body := range []string{`{"data":null}`, `{"data":[null]}`, `{"data":[{"id":""}]}`, `{"data":[{}]}`} {
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
	if err != nil || len(got) != 1 || got[0].Name != "Llama3:latest" || got[0].Model != "llama3:latest" || got[0].SizeVRAM != 5000 ||
		!got[0].ExpiresAt.Equal(time.Date(2026, 10, 5, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("ps = %+v, %v", got, err)
	}
	for _, body := range []string{`{"models":null}`, `{"models":[null]}`, `{"models":[{"name":""}]}`, `{"models":[{}]}`} {
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
		" llama3 ":                          "llama3:latest",
		"registry.ollama.ai/user/m":         "user/m:latest",
		"example.com/library/m":             "example.com/library/m:latest",
		"registry.ollama.ai/library/x":      "x:latest",
		"registry.ollama.ai/llama3":         "registry.ollama.ai/llama3:latest",
		"library/library/x":                 "library/library/x:latest",
		"localhost:5000/m":                  "localhost:5000/m:latest",
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
