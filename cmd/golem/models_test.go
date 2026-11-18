package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// fakeModelsReg is a fake modelsRegistry: canned Lookup profiles and canned
// ExplainToolCall provenance.
type fakeModelsReg struct {
	profiles map[provider.ModelKey]*provider.ModelProfile
	explain  map[provider.ModelKey]provider.ToolCallExplanation
}

func (f *fakeModelsReg) Lookup(_ context.Context, key provider.ModelKey) (*provider.ModelProfile, error) {
	if p, ok := f.profiles[key]; ok {
		return p, nil
	}
	return nil, context.Canceled // any non-nil error stands in for not-found
}

func (f *fakeModelsReg) ExplainToolCall(_ context.Context, key provider.ModelKey) (provider.ToolCallExplanation, error) {
	return f.explain[key], nil
}

func keyOf(sel string) provider.ModelKey {
	p, m, _ := strings.Cut(sel, "/")
	return provider.ModelKey{Provider: p, Model: m}
}

func TestRunModels_ListsChainWithProvenance(t *testing.T) {
	ctx := context.Background()
	aKey := keyOf("llamacpp/gemma4:31b")
	bKey := keyOf("llamacpp/byo-model")
	reg := &fakeModelsReg{
		profiles: map[provider.ModelKey]*provider.ModelProfile{
			aKey: {Key: aKey, Caps: provider.CapChat | provider.CapGenerate | provider.CapStream | provider.CapToolCall},
			bKey: {Key: bKey, Caps: provider.CapChat | provider.CapGenerate | provider.CapStream},
		},
		explain: map[provider.ModelKey]provider.ToolCallExplanation{
			aKey: {Caps: provider.CapChat | provider.CapGenerate | provider.CapStream | provider.CapToolCall, Has: true, Source: "explicit"},
			bKey: {Caps: provider.CapChat | provider.CapGenerate | provider.CapStream, Has: false, Source: "probe", State: fingerprint.CapProbeNo, TestedAt: time.Now().Add(-2 * time.Hour)},
		},
	}
	var out, errOut bytes.Buffer
	chain := []string{"llamacpp/gemma4:31b", "llamacpp/byo-model"}
	cfg := &config.Config{Models: map[string]config.ModelConfig{
		"agent": {
			Provider: "llamacpp", Name: "gemma4:31b",
			Options: &config.SamplingOptions{
				Temperature: provider.Ptr(0.0),
				TopP:        provider.Ptr(0.9),
				TopK:        provider.Ptr(40),
			},
		},
	}}
	if err := runModelsWith(ctx, reg, chain, cfg, nil, nil, &out, &errOut); err != nil {
		t.Fatalf("runModelsWith() error: %v", err)
	}
	got := out.String()
	// One line per entry, with caps rendered.
	if !strings.Contains(got, "llamacpp/gemma4:31b") || !strings.Contains(got, "llamacpp/byo-model") {
		t.Fatalf("output missing chain entries:\n%s", got)
	}
	// A carries tool_call with its provenance.
	if !strings.Contains(got, "tool_call=yes") || !strings.Contains(got, "explicit") {
		t.Fatalf("output missing A tool_call provenance:\n%s", got)
	}
	if !strings.Contains(got, "sampling=temperature=0,top_p=0.9,top_k=40") {
		t.Fatalf("output missing A sampling defaults:\n%s", got)
	}
	// B is flagged MISSING with the probe verdict.
	if !strings.Contains(got, "MISSING") {
		t.Fatalf("output missing MISSING flag for B:\n%s", got)
	}
	// The remediation hint appears under the MISSING entry.
	if !strings.Contains(got, remediationHint("llamacpp/byo-model")) {
		t.Fatalf("output missing remediation hint for B:\n%s", got)
	}
}

func TestRunModels_ReadsCachedProbeOnPlainListing(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"mystery"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	root := filepath.Join(dir, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	mcSeed(t, root, mcToolAnswers(t, srv.URL, "mystery", fingerprint.CapProbeYes, time.Now())...)

	cfgPath := filepath.Join(dir, "models.json")
	if err := os.WriteFile(cfgPath, []byte(`{
		"providers": {"llamacpp": {"base_url": "`+srv.URL+`", "api_format": "openai-compat"}},
		"models": {"agent": {"name": "mystery", "provider": "llamacpp", "type": "dense"}},
		"defaults": {"agent": "agent"}
	}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out, errOut bytes.Buffer
	if err := runModels(ctx, []string{"-config", cfgPath, "-root", root, "-no-probe"}, &out, &errOut); err != nil {
		t.Fatalf("runModels() error: %v\nstderr:\n%s", err, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "tool_call=yes (probe)") {
		t.Fatalf("output = %q, want cached probe provenance", got)
	}
}

func TestRunModels_SharedKeyNamesDeclaringRole(t *testing.T) {
	ctx := context.Background()
	// The agent chain uses key llamacpp/shared with NO explicit caps of its
	// own; role "chat" in cfg declares tool_call for the SAME key.
	key := keyOf("llamacpp/shared")
	reg := &fakeModelsReg{
		profiles: map[provider.ModelKey]*provider.ModelProfile{
			key: {Key: key, Caps: provider.CapChat | provider.CapStream | provider.CapToolCall},
		},
		explain: map[provider.ModelKey]provider.ToolCallExplanation{
			key: {Caps: provider.CapChat | provider.CapStream | provider.CapToolCall, Has: true, Source: "explicit"},
		},
	}
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"chat":  {Name: "shared", Provider: "llamacpp", Type: "dense", Capabilities: []string{"chat", "stream", "tool_call"}},
			"agent": {Name: "shared", Provider: "llamacpp", Type: "dense"},
		},
	}
	var out, errOut bytes.Buffer
	if err := runModelsWith(ctx, reg, []string{"llamacpp/shared"}, cfg, nil, nil, &out, &errOut); err != nil {
		t.Fatalf("runModelsWith() error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `declared by model entry "chat"`) {
		t.Fatalf("output missing shared-key declaring-role annotation:\n%s", got)
	}
}

// TestGolemModels_OutputUnchangedForEachSource pins what `golem models` prints,
// as text and as -json, for a real registry reading the evidence log in front
// of each source of the tool_call verdict. Answers are log rows; one fixture
// keeps a legacy capability_probes row to show it no longer counts. The goldens
// under testdata/models_baseline were reviewed by hand. Relative probe ages and the test
// server's address are replaced before comparing. Fixtures declare no shared
// roles: their annotations follow map order, and the parity tests cover them.
//
// To rewrite the goldens after a reviewed change: temporarily add
// os.WriteFile(path, []byte(got), 0o644) before the os.ReadFile below, run this
// test once, read `git diff testdata/models_baseline`, then remove the line.
func TestGolemModels_OutputUnchangedForEachSource(t *testing.T) {
	type fixture struct {
		name   string
		model  string
		apiFmt string // provider api_format; "ollama" serves /api/tags and /api/show
		ollama string // capabilities /api/show reports
		extra  string // extra JSON members of the model entry
		row    fingerprint.CapProbeState
		rowOld bool // row filed under a digest the runtime does not report
		legacy bool // row is a legacy capability_probes verdict, which no longer counts
	}
	fixtures := []fixture{
		{name: "explicit", model: "mystery", apiFmt: "openai-compat", extra: `, "capabilities": ["chat", "stream", "tool_call"]`},
		{name: "catalog", model: "qwen3:8b", apiFmt: "openai-compat"},
		{name: "probe_yes", model: "mystery", apiFmt: "openai-compat", row: fingerprint.CapProbeYes},
		{name: "probe_yes_stale", model: "mystery", apiFmt: "openai-compat", row: fingerprint.CapProbeYes, rowOld: true},
		{name: "probe_no", model: "mystery", apiFmt: "openai-compat", row: fingerprint.CapProbeNo},
		{name: "probe_inconclusive", model: "mystery", apiFmt: "openai-compat", row: fingerprint.CapProbeInconclusive},
		{name: "catalog_beside_probe_no", model: "qwen3:8b", apiFmt: "openai-compat", row: fingerprint.CapProbeNo},
		{name: "legacy_row_ignored", model: "mystery", apiFmt: "openai-compat", row: fingerprint.CapProbeYes, legacy: true},
		{name: "runtime", model: "mystery", apiFmt: "ollama", ollama: `["completion","tools"]`},
		{name: "runtime_no", model: "mystery", apiFmt: "ollama", ollama: `["completion"]`},
		{name: "unknown", model: "mystery", apiFmt: "openai-compat"},
	}
	age := regexp.MustCompile(`\d+[hms] ago`)

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			ctx := t.Context()
			// A developer's own endpoint settings must not leak into the output.
			for _, env := range []string{"GO_LLM_BASE_URL", "GO_LLM_CONFIG"} {
				if previous, ok := os.LookupEnv(env); ok {
					t.Cleanup(func() { _ = os.Setenv(env, previous) })
				}
				if err := os.Unsetenv(env); err != nil {
					t.Fatal(err)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/models":
					_, _ = w.Write([]byte(`{"data":[{"id":"` + fx.model + `"}]}`))
				case "/api/tags":
					_, _ = w.Write([]byte(`{"models":[{"name":"` + fx.model + `"}]}`))
				case "/api/show":
					_, _ = w.Write([]byte(`{"details":{"family":"mystery","parameter_size":"8B"},"capabilities":` + fx.ollama + `}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			dir := t.TempDir()
			t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
			root := filepath.Join(dir, "workspace")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			switch {
			case fx.row == "":
			case fx.legacy:
				h := mcOpen(t, root)
				if err := h.Probes().SaveCapProbe(ctx, fingerprint.CapProbe{
					BackendID: "llamacpp", ModelName: fx.model, Capability: "tool_call", State: fx.row,
					ModelDigest: "llamacpp/" + fx.model, ProbeVersion: fingerprint.CurrentToolProbeVersion,
					TestedAt: time.Now().Add(-2 * time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
				if err := h.Close(); err != nil {
					t.Fatal(err)
				}
			default:
				digest := ""
				if fx.rowOld {
					digest = "sha256:old"
				}
				answer := fingerprint.ProbeAnswer{Capability: "tool_call", Source: fingerprint.EvidenceSourceProbe, Kind: fingerprint.EvidenceKindAnswer, State: fx.row}
				if fx.row == fingerprint.CapProbeInconclusive {
					answer.Kind = fingerprint.EvidenceKindAttempt
				}
				mcSeed(t, root, mcAnswers(t, srv.URL, fx.apiFmt, fx.model, digest, fingerprint.MethodChatStreamTools, time.Now().Add(-2*time.Hour), answer)...)
			}
			cfgPath := filepath.Join(dir, "models.json")
			if err := os.WriteFile(cfgPath, []byte(`{
				"providers": {"llamacpp": {"base_url": "`+srv.URL+`", "api_format": "`+fx.apiFmt+`"}},
				"models": {"agent": {"name": "`+fx.model+`", "provider": "llamacpp", "type": "dense"`+fx.extra+`}},
				"defaults": {"agent": "agent"}
			}`), 0o600); err != nil {
				t.Fatal(err)
			}

			normalize := func(b []byte) string {
				out := strings.ReplaceAll(string(b), srv.URL, "<url>")
				return age.ReplaceAllString(out, "<age>")
			}
			var text, jsonOut, errOut bytes.Buffer
			if err := runModels(ctx, []string{"-config", cfgPath, "-root", root, "-no-probe"}, &text, &errOut); err != nil {
				t.Fatalf("runModels text: %v\nstderr:\n%s", err, errOut.String())
			}
			errOut.Reset()
			if err := runModels(ctx, []string{"-json", "-config", cfgPath, "-root", root}, &jsonOut, &errOut); err != nil {
				t.Fatalf("runModels -json: %v\nstderr:\n%s", err, errOut.String())
			}
			for ext, got := range map[string]string{"txt": normalize(text.Bytes()), "json": normalize(jsonOut.Bytes())} {
				path := filepath.Join("testdata", "models_baseline", fx.name+"."+ext)
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if got != string(want) {
					t.Errorf("%s output changed\n got:\n%s\nwant:\n%s", ext, got, want)
				}
			}
		})
	}
}

// TestGolemEvidenceActivation_AllCommandPathsUseLog pins that startup, the
// listing and -reprobe agree on the evidence log: startup's check lands there,
// the listing reads it without a check, -reprobe appends to it, the next
// startup sends nothing, and the legacy table gets nothing throughout.
func TestGolemEvidenceActivation_AllCommandPathsUseLog(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"mystery"}
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "mystery"))
	rows := func() int { return len(mcRows(t, e.root, srv.URL, "openai-compat", "mystery")) }

	if stderr, err := autoRun(t, e, nil, stopWhenReady); !errors.Is(err, errAutoReady) || srv.totalChecks() != 1 || rows() != 3 {
		t.Fatalf("startup: run = %v, %d checks, %d rows; want ready, 1, 3\nstderr:\n%s", err, srv.totalChecks(), rows(), stderr)
	}
	if stdout, _ := e.mustRun(t, srv); !strings.Contains(stdout.String(), "tool_call=yes (probe)") || srv.totalChecks() != 1 {
		t.Fatalf("listing: %d checks\n%s", srv.totalChecks(), stdout)
	}
	if _, _ = e.mustRun(t, srv, "-reprobe"); srv.totalChecks() != 2 || rows() != 6 {
		t.Fatalf("-reprobe: %d checks, %d rows; want 2, 6", srv.totalChecks(), rows())
	}
	if stderr, err := autoRun(t, e, nil, stopWhenReady); !errors.Is(err, errAutoReady) || srv.totalChecks() != 2 {
		t.Fatalf("second startup: run = %v, %d checks; want ready, 2\nstderr:\n%s", err, srv.totalChecks(), stderr)
	}
	h := mcOpen(t, e.root)
	defer func() { _ = h.Close() }()
	if _, err := h.Probes().GetCapProbe(context.Background(), "lc", "mystery", "tool_call"); !errors.Is(err, fingerprint.ErrNotFound) {
		t.Fatalf("legacy row lookup = %v; want none", err)
	}
}
