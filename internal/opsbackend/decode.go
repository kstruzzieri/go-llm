package opsbackend

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// RunningModel is one /running entry narrowed to model, state and ttl; cmd,
// proxy, name and description are never decoded.
type RunningModel struct {
	Model string
	State string
	TTL   int
}

// ActivityRow is one /api/metrics row narrowed to counts, rates and timing.
// The raw request path is reduced to PathClass; error text and metadata are
// never decoded.
type ActivityRow struct {
	ID              int64
	Timestamp       time.Time
	Model           string
	Status          int
	PathClass       string
	InputTokens     int64
	OutputTokens    int64
	CacheTokens     int64
	PromptPerSecond float64
	TokensPerSecond float64
	DurationMs      int64
}

// ListedModel is one /v1/models entry: its ID and whether llama-swap marks it
// as a peer (remote) model.
type ListedModel struct {
	ID   string
	Peer bool
}

// PSModel is one Ollama /api/ps entry narrowed to identity, VRAM and expiry.
type PSModel struct {
	Name      string
	Model     string
	SizeVRAM  int64
	ExpiresAt time.Time
}

// decodeStrict requires exactly one JSON value followed by EOF; trailing
// whitespace (v235's encoder ends every body in a newline) is allowed, which
// json.Unmarshal enforces without copying the body.
func decodeStrict(b []byte, v any) error {
	if err := json.Unmarshal(b, v); err != nil {
		return newCoded(CodeMalformed, "invalid JSON")
	}
	return nil
}

// decodeVersion reports whether b is llama-swap's build metadata, one object
// whose version, commit and build_date are all strings, and returns its
// version. Commit and build date only classify; they are not kept.
func decodeVersion(b []byte) (string, bool) {
	var v struct {
		Version   *string `json:"version"`
		Commit    *string `json:"commit"`
		BuildDate *string `json:"build_date"`
	}
	if err := decodeStrict(b, &v); err != nil || v.Version == nil || v.Commit == nil || v.BuildDate == nil {
		return "", false
	}
	return *v.Version, true
}

var runningStates = map[string]bool{"starting": true, "ready": true, "stopping": true, "stopped": true, "shutdown": true}

// decodeRunning validates the whole sample; any invalid entry rejects it, so
// a malformed reply never reads as "nothing is loaded".
func decodeRunning(b []byte) ([]RunningModel, error) {
	var env struct {
		Running *[]*struct {
			Model *string `json:"model"`
			State *string `json:"state"`
			TTL   int     `json:"ttl"`
		} `json:"running"`
	}
	if err := decodeStrict(b, &env); err != nil {
		return nil, err
	}
	if env.Running == nil {
		return nil, newCoded(CodeMalformed, "running list absent or null")
	}
	out := make([]RunningModel, 0, len(*env.Running))
	for _, e := range *env.Running {
		if e == nil || e.Model == nil || *e.Model == "" || e.State == nil || !runningStates[*e.State] {
			return nil, newCoded(CodeMalformed, "invalid running entry")
		}
		out = append(out, RunningModel{Model: *e.Model, State: *e.State, TTL: e.TTL})
	}
	return out, nil
}

// metricsRow is the approved subset of one activity row.
type metricsRow struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Model     string    `json:"model"`
	ReqPath   string    `json:"req_path"`
	Status    int       `json:"resp_status_code"`
	Tokens    struct {
		Input    int64   `json:"input_tokens"`
		Output   int64   `json:"output_tokens"`
		Cache    int64   `json:"cache_tokens"`
		PromptPS float64 `json:"prompt_per_second"`
		TokensPS float64 `json:"tokens_per_second"`
	} `json:"tokens"`
	DurationMs int64 `json:"duration_ms"`
}

// decodeMetrics decodes the activity ring one row at a time into a single
// reused value, so an 8 MiB body of empty or null rows is rejected at the
// first row instead of first materializing every row. The raw path is read
// only to classify it and is not retained.
func decodeMetrics(b []byte) ([]ActivityRow, error) {
	malformed := newCoded(CodeMalformed, "invalid activity list")
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, malformed
	}
	out := []ActivityRow{}
	var r metricsRow
	for dec.More() {
		// Reset per row. ID -1 makes an absent or null id fail the id >= 0
		// check; JSON null leaves a non-pointer field untouched, so a null
		// row or field reads as absent and is rejected below.
		r = metricsRow{ID: -1}
		if err := dec.Decode(&r); err != nil || r.ID < 0 || r.Timestamp.IsZero() || r.Model == "" {
			return nil, malformed
		}
		out = append(out, ActivityRow{
			ID: r.ID, Timestamp: r.Timestamp.UTC(), Model: r.Model, Status: r.Status,
			PathClass: pathClass(r.ReqPath), InputTokens: r.Tokens.Input, OutputTokens: r.Tokens.Output,
			CacheTokens: r.Tokens.Cache, PromptPerSecond: r.Tokens.PromptPS, TokensPerSecond: r.Tokens.TokensPS,
			DurationMs: r.DurationMs,
		})
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
		return nil, malformed
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, malformed
	}
	return out, nil
}

// decodeModels decodes /v1/models into IDs and peer flags. A meta that is not
// an object fails the whole sample, so peer confirmation fails closed.
func decodeModels(b []byte) ([]ListedModel, error) {
	var env struct {
		Data *[]*struct {
			ID   *string `json:"id"`
			Meta *struct {
				LlamaSwap json.RawMessage `json:"llamaswap"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := decodeStrict(b, &env); err != nil {
		return nil, err
	}
	if env.Data == nil {
		return nil, newCoded(CodeMalformed, "model list absent or null")
	}
	out := make([]ListedModel, 0, len(*env.Data))
	for _, e := range *env.Data {
		if e == nil || e.ID == nil || *e.ID == "" {
			return nil, newCoded(CodeMalformed, "invalid model entry")
		}
		// Key presence decides: a peerID of any value, even null, marks a
		// peer, so a malformed marker excludes the model (fails closed). A
		// llamaswap meta that is present must be an object; null fails the
		// sample.
		peer := false
		if e.Meta != nil && e.Meta.LlamaSwap != nil {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(e.Meta.LlamaSwap, &fields); err != nil || fields == nil {
				return nil, newCoded(CodeMalformed, "invalid model meta")
			}
			_, peer = fields["peerID"]
		}
		out = append(out, ListedModel{ID: *e.ID, Peer: peer})
	}
	return out, nil
}

// decodePS decodes Ollama's /api/ps.
func decodePS(b []byte) ([]PSModel, error) {
	var env struct {
		Models *[]*struct {
			Name      *string   `json:"name"`
			Model     string    `json:"model"`
			SizeVRAM  int64     `json:"size_vram"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"models"`
	}
	if err := decodeStrict(b, &env); err != nil {
		return nil, err
	}
	if env.Models == nil {
		return nil, newCoded(CodeMalformed, "ps list absent or null")
	}
	out := make([]PSModel, 0, len(*env.Models))
	for _, e := range *env.Models {
		if e == nil || e.Name == nil || *e.Name == "" {
			return nil, newCoded(CodeMalformed, "invalid ps entry")
		}
		out = append(out, PSModel{Name: *e.Name, Model: e.Model, SizeVRAM: e.SizeVRAM, ExpiresAt: e.ExpiresAt.UTC()})
	}
	return out, nil
}

// pathClasses maps exact recorded request paths to classes. v235 strips "/v"
// from versionless "/v/..." routes in place before its metrics middleware
// records r.URL.Path, so "/chat/completions", "/completions", "/embeddings",
// "/rerank" and "/reranking" read like their "/v1/" twins; requests through
// /upstream/<model>/v/... are recorded unstripped, so the "/v/" forms do too.
// "/responses" and "/messages" in either form stay "other" like
// "/v1/responses" and "/v1/messages".
var pathClasses = map[string]string{
	"/v1/chat/completions": "chat", "/chat/completions": "chat", "/v/chat/completions": "chat",
	"/v1/completions": "completions", "/completion": "completions", "/completions": "completions", "/v/completions": "completions",
	"/v1/embeddings": "embeddings", "/embedding": "embeddings", "/embeddings": "embeddings", "/v/embeddings": "embeddings",
	"/infill":    "infill",
	"/v1/rerank": "rerank", "/rerank": "rerank", "/reranking": "rerank", "/v1/reranking": "rerank",
	"/v/rerank": "rerank", "/v/reranking": "rerank",
}

// pathClass maps an exact known request path to a class; anything else,
// including paths with queries, is "other".
func pathClass(p string) string {
	if c, ok := pathClasses[p]; ok {
		return c
	}
	return "other"
}
