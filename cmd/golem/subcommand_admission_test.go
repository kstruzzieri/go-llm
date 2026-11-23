package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// subcommandHarness: a LOCAL provider serving models/embeddings plus a REMOTE
// embedding role, so every embedding-reaching subcommand must fail closed
// without the exact allowlist. counter observes the local provider.
func subcommandHarness(t *testing.T, embeddingProvider string) (configPath string, counter *atomic.Int64) {
	t.Helper()
	counter = &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"m1"},{"id":"embed-model"}]}`)
		case "/v1/embeddings":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1,0.2]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	configPath = filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
  "providers": {
    "local":  {"base_url": %q, "api_format": "openai-compat", "timeout": "5s"},
    "remote": {"base_url": "https://embed.invalid/api", "api_format": "openai-compat", "timeout": "5s"}
  },
  "models": {
    "agent":     {"name": "m1", "provider": "local", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream", "tool_call"]},
    "embedding": {"name": "embed-model", "provider": %q, "type": "embedding", "context_window": 8192}
  },
  "defaults": {"agent": "agent", "embedding": "embedding"}
}`, server.URL, embeddingProvider)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, counter
}

// opsConfig writes a config with a never-dialed local provider and the
// remote "cloud" (https://ops.invalid/api); models and defaults are the
// caller's JSON members.
func opsConfig(t *testing.T, models, defaults string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	configJSON := `{
  "providers": {
    "local": {"base_url": "http://127.0.0.1:9", "api_format": "openai-compat", "timeout": "5s"},
    "cloud": {"base_url": "https://ops.invalid/api", "api_format": "openai-compat", "timeout": "5s"}
  },
  "models": {` + models + `},
  "defaults": {` + defaults + `}
}`
	if err := os.WriteFile(path, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	opsLocalChat  = `"agent": {"name": "m1", "provider": "local", "type": "dense", "context_window": 32768, "capabilities": ["chat", "generate", "stream", "tool_call"]`
	opsCloudChat  = `"cloudchat": {"name": "c1", "provider": "cloud", "type": "dense", "context_window": 32768, "capabilities": ["chat", "generate", "stream", "tool_call"]}`
	opsCloudEmbed = `"embedding": {"name": "e1", "provider": "cloud", "type": "embedding", "context_window": 8192}`
)

// golem models with an uncovered remote on its route draws the refused row
// under the models subtitle and fails with the unchanged denial.
func TestSubcommandRefusedRow(t *testing.T) {
	configPath := opsConfig(t, opsCloudChat, `"agent": "cloudchat"`)
	var out, errOut strings.Builder
	err := runModels(context.Background(), []string{"-config", configPath}, &out, &errOut)
	const want = `provider: destination cloud/https://ops.invalid/api not admitted for agent; pass -allow-destination "cloud/https://ops.invalid/api" to permit it`
	if err == nil || err.Error() != want {
		t.Fatalf("denial = %v\nwant     %s", err, want)
	}
	got := errOut.String()
	for _, w := range []string{
		"\u2500\u2500 model servers \u2500\u2500 servers golem models lists and checks ",
		"\n  cloud  https://ops.invalid/api  not allowed (needs -allow-destination)\n         in route  chat\n",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("models section missing %q:\n%s", w, got)
		}
	}
}

// Each subcommand describes its own operation: the subtitle names it, and
// models says "in route" where the others say "used for". Every case is
// refused, so none sends a request.
func TestSubcommandOperations(t *testing.T) {
	withFallback := opsConfig(t, opsLocalChat+`, "fallbacks": ["cloudchat"]}, `+opsCloudChat, `"agent": "agent"`)
	everyRoute := opsConfig(t, opsLocalChat+`}, `+opsCloudChat+`, `+opsCloudEmbed,
		`"agent": "agent", "embedding": "embedding", "summarize": "cloudchat"`)
	for _, tc := range []struct {
		name string
		run  func(errOut io.Writer) error
		want []string
	}{
		{"models", func(errOut io.Writer) error {
			return runModels(context.Background(), []string{"-config", withFallback}, io.Discard, errOut)
		}, []string{"\u2500\u2500 servers golem models lists and checks ", "\n         in route  chat (fallback)\n"}},
		{"models -json", func(errOut io.Writer) error {
			return runModels(context.Background(), []string{"-config", everyRoute, "-json"}, io.Discard, errOut)
		}, []string{"\u2500\u2500 servers golem models lists and checks ", "\n         in route  chat\n"}},
		{"index -progressive", func(errOut io.Writer) error {
			return runIndex(context.Background(), []string{"-config", everyRoute, "-root", t.TempDir(), "-progressive"}, io.Discard, errOut)
		}, []string{"\u2500\u2500 where golem index sends model requests ", "\n         used for  embeddings, summaries\n"}},
		{"source add", func(errOut io.Writer) error {
			return runSource(context.Background(),
				[]string{"add", "-config", everyRoute, "-root", t.TempDir(), "-text", "-name", "doc"},
				strings.NewReader("hello world"), io.Discard, errOut)
		}, []string{"\u2500\u2500 where golem source sends model requests ", "\n         used for  embeddings\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errOut strings.Builder
			if err := tc.run(&errOut); err == nil || !strings.Contains(err.Error()+errOut.String(), "-allow-destination") {
				t.Fatalf("%s with an uncovered remote = %v, want the denial\nstderr:\n%s", tc.name, err, errOut.String())
			}
			for _, w := range tc.want {
				if !strings.Contains(errOut.String(), w) {
					t.Errorf("section missing %q:\n%s", w, errOut.String())
				}
			}
		})
	}
}

// golem index with a remote embedding role fails closed — no prompt, the
// diagnostic names destination and flag, zero requests anywhere (I6/I18).
func TestRunIndexDeniesRemoteEmbedding(t *testing.T) {
	configPath, counter := subcommandHarness(t, "remote")
	var out, errOut strings.Builder

	err := runIndex(context.Background(), []string{"-config", configPath, "-root", t.TempDir()}, &out, &errOut)
	if err == nil {
		t.Fatalf("index with remote embedding succeeded\nstderr:\n%s", errOut.String())
	}
	for _, want := range []string{"remote", "https://embed.invalid/api", "-allow-destination"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("denial missing %q: %v", want, err)
		}
	}
	if got := counter.Load(); got != 0 {
		t.Errorf("denied index sent %d requests to the local provider, want 0", got)
	}
	if !strings.Contains(errOut.String(), "\u2500\u2500 model servers") {
		t.Errorf("manifest not rendered before denial:\n%s", errOut.String())
	}
}

// golem source add via -text with a remote embedding role: same fail-closed
// contract through the source path.
func TestRunSourceAddDeniesRemoteEmbedding(t *testing.T) {
	configPath, counter := subcommandHarness(t, "remote")
	var out, errOut strings.Builder

	err := runSource(context.Background(),
		[]string{"add", "-config", configPath, "-root", t.TempDir(), "-text", "-name", "doc"},
		strings.NewReader("hello world"), &out, &errOut)
	if err == nil {
		t.Fatalf("source add with remote embedding succeeded\nstderr:\n%s", errOut.String())
	}
	if !strings.Contains(err.Error()+errOut.String(), "-allow-destination") {
		t.Errorf("source add denial does not name the fix flag:\nerr: %v\nstderr:\n%s", err, errOut.String())
	}
	if got := counter.Load(); got != 0 {
		t.Errorf("denied source add sent %d requests, want 0", got)
	}
}

// A local embedding role keeps index working with zero consent machinery in
// the way (I20): the run reaches the local provider.
func TestRunIndexLocalEmbeddingProceeds(t *testing.T) {
	configPath, counter := subcommandHarness(t, "local")
	var out, errOut strings.Builder
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# doc\n\nsome text\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runIndex(context.Background(), []string{"-config", configPath, "-root", root}, &out, &errOut)
	if err != nil {
		t.Fatalf("local index: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}
	if strings.Contains(errOut.String(), "Send data to") {
		t.Errorf("local index prompted:\n%s", errOut.String())
	}
	if counter.Load() == 0 {
		t.Error("local provider never contacted; index cannot have run")
	}
}

// The allowlist unblocks index against a remote embedding role. The remote
// is unreachable (embed.invalid), so indexing FAILS at the network stage —
// but it fails at the provider, not at admission, and the local provider's
// refresh went through: consent worked, the network is what it is.
func TestRunIndexAllowlistedRemotePassesAdmission(t *testing.T) {
	configPath, counter := subcommandHarness(t, "remote")
	var out, errOut strings.Builder
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# doc\n\nsome text\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runIndex(context.Background(),
		[]string{"-config", configPath, "-root", root,
			"-allow-destination", "remote/https://embed.invalid/api"},
		&out, &errOut)
	// Admission passed if and only if nothing reports a denial. Indexing
	// itself then fails at the network (embed.invalid is unreachable) — at
	// the provider, not the boundary. The LOCAL provider is on no index
	// route, so I7 says it sees zero requests even though it is configured.
	if err != nil && strings.Contains(err.Error(), "not admitted") {
		t.Fatalf("allowlisted index still denied: %v", err)
	}
	combined := out.String() + errOut.String()
	if strings.Contains(combined, "not admitted") {
		t.Fatalf("allowlisted index reported a denial:\n%s", combined)
	}
	if got := counter.Load(); got != 0 {
		t.Errorf("unrouted local provider received %d requests during index, want 0 (I7)", got)
	}
}

// golem source list and rm are local-only operations: no bootstrap, no
// network, no admission — they must work untouched even when the config
// names an uncovered remote (I20).
func TestRunSourceListNetworkFreeDespiteRemoteConfig(t *testing.T) {
	configPath, counter := subcommandHarness(t, "remote")
	var out, errOut strings.Builder

	_ = configPath // list is local-only and takes no -config at all
	err := runSource(context.Background(),
		[]string{"list", "-root", t.TempDir()},
		strings.NewReader(""), &out, &errOut)
	if err != nil && !strings.Contains(errOut.String(), "no workspace index") {
		t.Fatalf("source list: %v\nstderr:\n%s", err, errOut.String())
	}
	if strings.Contains(errOut.String(), "-allow-destination") || strings.Contains(errOut.String(), "not admitted") {
		t.Errorf("local-only source list hit the admission path:\n%s", errOut.String())
	}
	if got := counter.Load(); got != 0 {
		t.Errorf("source list sent %d requests, want 0", got)
	}
}

// golem models -json activates EVERY configured provider (D10): with an
// uncovered remote in the config it fails closed; with the allowlist it
// proceeds and the local provider is refreshed.
func TestRunModelsJSONActivatesEveryProvider(t *testing.T) {
	configPath, counter := subcommandHarness(t, "local") // embedding local; remote provider still configured

	t.Run("uncovered remote fails closed", func(t *testing.T) {
		var out, errOut strings.Builder
		err := runModels(context.Background(),
			[]string{"-config", configPath, "-json"}, &out, &errOut)
		if err == nil {
			t.Fatal("models -json with an uncovered configured remote succeeded")
		}
		if !strings.Contains(err.Error(), "-allow-destination") {
			t.Errorf("denial does not name the fix flag: %v", err)
		}
		if got := counter.Load(); got != 0 {
			t.Errorf("denied models -json sent %d requests, want 0", got)
		}
	})

	t.Run("allowlisted fails at the network, not the boundary", func(t *testing.T) {
		// The remote is admitted but unreachable (embed.invalid). -json
		// deliberately propagates provider errors — an empty inventory must
		// mean "nothing found", never "something failed" — so the run fails
		// with a NETWORK error. What admission guarantees is only that the
		// failure is not a denial and that the local provider was reached.
		var out, errOut strings.Builder
		err := runModels(context.Background(),
			[]string{"-config", configPath, "-json",
				"-allow-destination", "remote/https://embed.invalid/api"}, &out, &errOut)
		if err == nil {
			t.Fatal("unreachable admitted remote should fail the -json inventory")
		}
		if strings.Contains(err.Error(), "not admitted") {
			t.Fatalf("allowlisted -json still denied: %v", err)
		}
		if counter.Load() == 0 {
			t.Error("local provider never refreshed; bootstrap cannot have run")
		}
	})
}

// The normal models listing activates only the agent route: the configured
// remote embedding provider is NOT reachable from it, so no allowlist is
// needed and the remote receives nothing (I7 for subcommands).
func TestRunModelsNormalListingIgnoresUnroutedRemote(t *testing.T) {
	configPath, counter := subcommandHarness(t, "remote")
	var out, errOut strings.Builder

	err := runModels(context.Background(),
		[]string{"-config", configPath, "-no-probe"}, &out, &errOut)
	if err != nil {
		t.Fatalf("models listing: %v\nstderr:\n%s", err, errOut.String())
	}
	if counter.Load() == 0 {
		t.Error("local provider never refreshed")
	}
	if !strings.Contains(out.String(), "m1") {
		t.Errorf("listing missing the agent model:\n%s", out.String())
	}
}
