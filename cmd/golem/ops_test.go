package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/internal/opsfixture"
	"github.com/kstruzzieri/go-llm/internal/opsview"
)

// pinNoDiscovery points config auto-discovery at a missing file for this
// test and every golem process it starts (children inherit os.Environ), so a
// regression that ignores -config fails instead of loading the developer's
// real models.json and contacting a live backend.
func pinNoDiscovery(t *testing.T) {
	t.Helper()
	t.Setenv("GO_LLM_CONFIG", filepath.Join(t.TempDir(), "absent-models.json"))
}

func writeOpsConfig(t *testing.T, providers map[string]config.ProviderConfig, models map[string]config.ModelConfig, defaults map[string]string) string {
	t.Helper()
	pinNoDiscovery(t)
	data, err := json.Marshal(config.Config{Providers: providers, Models: models, Defaults: defaults})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fixtureConfig mirrors Keith's real models.json shape: a loopback llama-swap
// with slot discovery beside a hosted provider whose base carries a path. The
// hosted host is under the reserved .invalid TLD, so a classification
// regression can never send a live outbound request.
func fixtureConfig(t *testing.T, baseURL string) string {
	return writeOpsConfig(t,
		map[string]config.ProviderConfig{
			"llamacpp": {BaseURL: baseURL, APIFormat: "openai-compat", APIKey: opsfixture.SentinelAPIKey, SlotDiscovery: true},
			"opencode": {BaseURL: "https://opencode.invalid/zen/go", APIFormat: "openai-compat"},
		},
		map[string]config.ModelConfig{
			"agent":  {Name: "gemma4:31b", Provider: "llamacpp", Type: "dense", Capabilities: []string{"chat", "stream", "tool_call"}},
			"hosted": {Name: "kimi-k3", Provider: "opencode", Type: "dense", Capabilities: []string{"chat", "stream"}},
		},
		map[string]string{"agent": "agent"},
	)
}

func runOpsJSON(t *testing.T, args ...string) (opsview.Snapshot, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := runOps(context.Background(), append([]string{"-json"}, args...), strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatalf("runOps: %v (stderr %q)", err, errOut.String())
	}
	var snap opsview.Snapshot
	if err := json.Unmarshal(out.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	return snap, out.String(), errOut.String()
}

// requireFreshTickRequests pins one fresh tick against a healthy llama-swap:
// exactly these four reads, in order, and no would-be model load.
func requireFreshTickRequests(t *testing.T, f *opsfixture.Server) {
	t.Helper()
	want := []opsfixture.Request{
		{Method: "GET", URI: "/api/version"},
		{Method: "GET", URI: "/running"},
		{Method: "GET", URI: "/api/metrics"},
		{Method: "GET", URI: "/v1/models"},
	}
	got := f.Requests()
	if len(got) != len(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d = %v, want %v", i, got[i], want[i])
		}
	}
	if f.Loads.Load() != 0 {
		t.Fatal("golem ops reached a model-dispatched route")
	}
}

// TestOpsDispatchCanary is the positive-control canary: expected requests and
// a projected fact must appear, and nothing may reach a dispatch route.
func TestOpsDispatchCanary(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	snap, _, _ := runOpsJSON(t, "-config", fixtureConfig(t, f.URL()))
	requireFreshTickRequests(t, f)
	if snap.Mode != opsview.ModeOnce || snap.Config.Revision == nil {
		t.Fatalf("mode/revision = %q / %v, want once and the document revision", snap.Mode, snap.Config.Revision)
	}
	var found bool
	for _, m := range snap.Models {
		if m.ID == "llamacpp/gemma4:31b" {
			found = m.Configured && m.Residency.State == opsview.StateLoaded
		}
	}
	if !found {
		t.Fatalf("projected fact missing: %+v", snap.Models)
	}
}

func TestOpsCanaryOnFailurePaths(t *testing.T) {
	for _, path := range []string{"/api/version", "/running", "/api/metrics", "/v1/models"} {
		f := opsfixture.NewLlamaSwap(t)
		f.SetStatus(path, http.StatusInternalServerError)
		runOpsJSON(t, "-config", fixtureConfig(t, f.URL()))
		if f.Loads.Load() != 0 {
			t.Fatalf("%s failing: golem ops reached a dispatch route", path)
		}
		var sawFailing bool
		for _, r := range f.Requests() {
			switch r.URI {
			case "/api/version", "/running", "/api/metrics", "/v1/models":
			default:
				t.Fatalf("%s failing: unexpected request %v", path, r)
			}
			sawFailing = sawFailing || r.URI == path
		}
		if !sawFailing {
			t.Fatalf("%s failing: never requested (requests %v)", path, f.Requests())
		}
	}
}

func TestOpsBasePrefixMakesNoRequests(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	snap, _, _ := runOpsJSON(t, "-config", fixtureConfig(t, f.URL()+"/upstream/gemma4:31b"))
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
	var found bool
	for _, b := range snap.Backends {
		if b.ID == "llamacpp" {
			found = true
			if b.Runtime.Support != "invalid_configuration" {
				t.Fatalf("prefix backend = %+v", b)
			}
		}
	}
	if !found {
		t.Fatalf("prefix backend missing: %+v", snap.Backends)
	}
}

// TestOpsConfiglessRun pins auto-discovery finding nothing: the run still
// renders configview's not-ready snapshot, observes nothing and exits 0.
func TestOpsConfiglessRun(t *testing.T) {
	home := t.TempDir()
	for _, k := range []string{"HOME", "USERPROFILE", "AppData"} {
		t.Setenv(k, home)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("GO_LLM_CONFIG", "") // registers the restore; unset below
	if err := os.Unsetenv("GO_LLM_CONFIG"); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	check := func(where, out string) {
		t.Helper()
		var snap opsview.Snapshot
		if err := json.Unmarshal([]byte(out), &snap); err != nil {
			t.Fatalf("%s: decode: %v\n%s", where, err, out)
		}
		var missing, attn bool
		for _, d := range snap.Config.Diagnostics {
			missing = missing || d.Code == "config_missing"
		}
		for _, a := range snap.Attention {
			attn = attn || (a.Reason == opsview.AttnConfigProblem && strings.Contains(a.Text, "config_missing"))
		}
		if snap.Config.Ready || len(snap.Backends) != 0 || !missing || !attn {
			t.Fatalf("%s: configless snapshot = %+v, want not ready, no backends and a config_missing problem", where, snap)
		}
	}
	_, out, _ := runOpsJSON(t)
	check("in process", out)
	exit, stdout, stderr := runGolemMain(t, "ops", "-json")
	if exit != 0 || stderr != "" {
		t.Fatalf("exit/stderr = %d / %q, want 0 / \"\"", exit, stderr)
	}
	check("process", stdout)
}

// TestOpsHostedProviderReadsRemote pins Keith's real configuration shape: the
// hosted provider's base path does not make it misconfigured, it reads remote
// and is never observed, while the loopback llama-swap beside it is read.
func TestOpsHostedProviderReadsRemote(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	snap, _, _ := runOpsJSON(t, "-config", fixtureConfig(t, f.URL()))
	requireFreshTickRequests(t, f)
	byID := map[string]opsview.Backend{}
	for _, b := range snap.Backends {
		byID[b.ID] = b
	}
	if b := byID["llamacpp"]; b.Hosting != "local" || b.Runtime.Support != "supported" || b.Reachability.State != opsview.ReachOK {
		t.Fatalf("loopback llama-swap = %+v", b)
	}
	b, ok := byID["opencode"]
	if !ok || b.Hosting != "remote" || b.Runtime.Support != "not_observed" || b.Reachability.State != opsview.StateNotObserved ||
		b.Reachability.Code == nil || *b.Reachability.Code != opsview.ReasonRemote || len(b.Surfaces) != 0 {
		t.Fatalf("hosted provider = %+v, want remote and not observed", b)
	}
	var hosted bool
	for _, m := range snap.Models {
		if m.ID == "opencode/kimi-k3" {
			hosted = m.Configured && m.Residency.State == opsview.StateNotObserved
		}
	}
	if !hosted {
		t.Fatalf("hosted model missing or observed: %+v", snap.Models)
	}
}

func TestOpsPrivacySentinels(t *testing.T) {
	leaked := func(s string) bool {
		for _, sentinel := range opsfixture.AllSentinels {
			if strings.Contains(s, sentinel) {
				return true
			}
		}
		return opsfixture.ContainsSentinel(s)
	}
	for _, tc := range []struct {
		name  string
		setup func(*opsfixture.Server)
	}{
		{"happy", func(*opsfixture.Server) {}},
		{"non-2xx bodies", func(f *opsfixture.Server) { f.SetStatus("/api/metrics", 500); f.SetStatus("/v1/models", 502) }},
		{"malformed running", func(f *opsfixture.Server) {
			f.SetBody("/running", `{"running":[{"model":"`+opsfixture.SentinelCmd+`","state":"bogus"}]}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := opsfixture.NewLlamaSwap(t)
			tc.setup(f)
			snap, out, errOut := runOpsJSON(t, "-config", fixtureConfig(t, f.URL()))
			if leaked(out + errOut) {
				t.Fatalf("sentinel leaked:\n%s\n%s", out, errOut)
			}
			if len(snap.Backends) == 0 || snap.Backends[0].ID != "llamacpp" {
				t.Fatalf("permitted fact missing: %+v", snap.Backends)
			}
			if a := f.Auth(); len(a) == 0 || a[0] != "Bearer "+opsfixture.SentinelAPIKey {
				t.Fatalf("api key not sent: %v", a)
			}
			requireFreshTickRequests(t, f)
		})
	}
	// table output too
	f := opsfixture.NewLlamaSwap(t)
	var out, errOut bytes.Buffer
	if err := runOps(context.Background(), []string{"-config", fixtureConfig(t, f.URL())}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 || leaked(out.String()+errOut.String()) {
		t.Fatalf("table leaked a sentinel or printed nothing: %q %q", out.String(), errOut.String())
	}
	// and the process streams, which also catch output that bypasses the
	// injected writers
	f = opsfixture.NewLlamaSwap(t)
	exit, stdout, stderr := runGolemMain(t, "ops", "-json", "-config", fixtureConfig(t, f.URL()))
	if exit != 0 || !strings.Contains(stdout, `"id": "llamacpp"`) || leaked(stdout+stderr) {
		t.Fatalf("process exit/stdout/stderr = %d / %q / %q, want 0, the llamacpp backend and no sentinel", exit, stdout, stderr)
	}
}

// TestOpsExitCodes pins spec §6.1: an unreachable backend is an observation,
// so the run still exits 0; a failed write is an operational failure.
func TestOpsExitCodes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	exit, stdout, stderr := runGolemMain(t, "ops", "-json", "-config", fixtureConfig(t, closed))
	var snap opsview.Snapshot
	if opsfixture.ContainsSentinel(stdout + stderr) {
		t.Fatalf("unreachable backend: process output leaked a sentinel: %q %q", stdout, stderr)
	}
	if exit != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &snap) != nil {
		t.Fatalf("unreachable backend: exit/stdout/stderr = %d / %q / %q, want 0 and a snapshot", exit, stdout, stderr)
	}
	if len(snap.Backends) == 0 || snap.Backends[0].ID != "llamacpp" || snap.Backends[0].Reachability.State != opsview.ReachUnreachable {
		t.Fatalf("unreachable backend projected as %+v", snap.Backends)
	}

	for _, args := range [][]string{{"-json"}, {}} {
		var errOut bytes.Buffer
		err := runOps(context.Background(), append(args, "-config", fixtureConfig(t, closed)), strings.NewReader(""), failingWriter{}, &errOut)
		if !errors.Is(err, errOpsWrite) || exitCodeFor(err) == 0 {
			t.Fatalf("%v: write failure returned %v (exit %d), want errOpsWrite and a nonzero exit", args, err, exitCodeFor(err))
		}
	}
}

var errOpsWrite = errors.New("write refused")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errOpsWrite }

func TestOpsConfigErrorIsBounded(t *testing.T) {
	pinNoDiscovery(t)
	path := filepath.Join(t.TempDir(), "models.json")
	raw := `{"providers":{"llamacpp":{"base_url":"http://[::1/` + opsfixture.SentinelPath + `","api_format":"openai-compat"}},"models":{},"defaults":{}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := runOps(context.Background(), []string{"-config", path}, strings.NewReader(""), &out, &errOut)
	if err == nil || strings.Contains(err.Error()+out.String()+errOut.String(), opsfixture.SentinelPath) {
		t.Fatalf("config error leaked the base URL: %v", err)
	}
	if want := "golem ops: models.json not loaded: provider_endpoint_invalid llamacpp"; err.Error() != want {
		t.Fatalf("config error = %q, want %q", err, want)
	}
	missing := runOps(context.Background(), []string{"-config", filepath.Join(t.TempDir(), "absent.json")}, strings.NewReader(""), &out, &errOut)
	if want := "golem ops: models.json not loaded: io"; missing == nil || missing.Error() != want {
		t.Fatalf("subjectless config error = %v, want %q", missing, want)
	}
	exit, stdout, stderr := runGolemMain(t, "ops", "-config", path)
	if strings.Contains(stdout+stderr, opsfixture.SentinelPath) {
		t.Fatalf("process output leaked the base URL: %q %q", stdout, stderr)
	}
	if exit != 1 || stdout != "" || stderr != "golem: "+err.Error()+"\n" {
		t.Fatalf("exit/stdout/stderr = %d / %q / %q, want 1 / \"\" / the bounded error", exit, stdout, stderr)
	}
}

func TestOpsFlagErrors(t *testing.T) {
	// Every case must fail before loading configuration; if one ever reaches
	// auto-discovery it finds nothing rather than a real models.json.
	pinNoDiscovery(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-json", "-watch"}, "golem ops: -json cannot be combined with -watch"},
		{[]string{"serve-me"}, "golem ops: unexpected argument (run with -help for usage)"},
		{[]string{"-watch"}, "golem ops: -watch needs a terminal on stdout"},
	} {
		var out, errOut bytes.Buffer
		err := runOps(context.Background(), tc.args, strings.NewReader(""), &out, &errOut)
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
}
