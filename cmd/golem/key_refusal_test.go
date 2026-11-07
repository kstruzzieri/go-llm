package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
)

const refusalCanary = "CANARY-776-golem-9f3a1c"

// refusalLegs are the provider destinations each refusal test covers: a
// counting loopback listener, and a remote address with no listener.
var refusalLegs = []string{"loopback", "remote"}

// refusalWorkingDir writes a ./models.json whose provider carries a
// ${CANARY_VAR} key, isolates discovery, and makes it the working directory.
// The loopback leg points the provider at a counting listener; the remote
// leg at an unroutable documentation address (RFC 5737) with nothing bound,
// so no test listens on a LAN interface. The variable is set, so a refusal
// (not an absent variable) is what keeps the request count at zero.
func refusalWorkingDir(t *testing.T, leg string) *atomic.Int64 {
	t.Helper()
	var hits atomic.Int64
	baseURL := "http://192.0.2.1:9"
	if leg == "loopback" {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			http.NotFound(w, nil)
		}))
		t.Cleanup(srv.Close)
		baseURL = srv.URL
	}

	t.Setenv("CANARY_VAR", refusalCanary)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	t.Setenv("GO_LLM_CONFIG", "")
	if err := os.Unsetenv("GO_LLM_CONFIG"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	body := fmt.Sprintf(`{
  "providers": {"openai": {"base_url": %q, "api_format": "openai-compat", "api_key": "${CANARY_VAR}", "timeout": "5s"}},
  "models": {"agent": {"name": "agent-model", "provider": "openai", "type": "dense", "context_window": 32768,
    "capabilities": ["chat", "generate", "stream", "tool_call"]}},
  "defaults": {"agent": "agent"}
}`, baseURL)
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return &hits
}

func TestGolemPromptModeRefusesWorkingDirKey(t *testing.T) {
	for _, leg := range refusalLegs {
		t.Run(leg, func(t *testing.T) {
			hits := refusalWorkingDir(t, leg)
			stdin, stdout, stderr := runTestFiles(t)
			err := run([]string{"-root", t.TempDir(), "-p", "say done", "-no-probe", "-no-cap-probe",
				"-no-rag", "-no-project-context", "-no-session", "-no-memory"}, stdin, stdout, stderr)
			if err == nil {
				t.Fatal("run succeeded; want a key_reference_refused error")
			}
			for _, want := range []string{"key_reference_refused", keyRefusalRemedy, "CANARY_VAR"} {
				if want == "key_reference_refused" {
					if d, ok := config.DiagnosticOf(err); !ok || d.Code != config.CodeKeyReferenceRefused {
						t.Errorf("DiagnosticOf = %+v, %v; want the refusal code", d, ok)
					}
					continue
				}
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q:\n%v", want, err)
				}
			}
			if got := hits.Load(); got != 0 {
				t.Errorf("listener received %d requests, want 0", got)
			}
			for name, s := range map[string]string{"error": err.Error(), "stdout": readRunTestFile(t, stdout), "stderr": readRunTestFile(t, stderr)} {
				if strings.Contains(s, refusalCanary) {
					t.Errorf("%s leaks the canary", name)
				}
			}
		})
	}
}

func TestGolemModelsInventoryRefusesWorkingDirKey(t *testing.T) {
	for _, leg := range refusalLegs {
		t.Run(leg, func(t *testing.T) {
			hits := refusalWorkingDir(t, leg)
			doc, err := loadDocumentFor("")
			if err == nil || doc != nil {
				t.Fatalf("loadDocumentFor = %v, %v; want a refusal", doc, err)
			}
			if d, ok := config.DiagnosticOf(err); !ok || d.Code != config.CodeKeyReferenceRefused {
				t.Errorf("DiagnosticOf = %+v, %v; want the refusal code", d, ok)
			}
			if !strings.Contains(err.Error(), keyRefusalRemedy) {
				t.Errorf("remedy missing:\n%v", err)
			}
			if strings.Contains(err.Error(), refusalCanary) {
				t.Error("error leaks the canary")
			}
			if got := hits.Load(); got != 0 {
				t.Errorf("listener received %d requests, want 0", got)
			}
		})
	}
}

// Another config diagnostic, not just any error, so the code comparison is
// what keeps it unwrapped.
func TestWithKeyRefusalRemedyPassesOtherErrorsThrough(t *testing.T) {
	_, other := config.ParseDocument([]byte(`{"providers":{"openai":{"base_url":"http://127.0.0.1:1","api_format":"openai-compat","api_key":"abc${"}},"models":{},"defaults":{}}`),
		config.Origin{Source: config.OriginExplicit}, config.DocumentOptions{})
	if d, ok := config.DiagnosticOf(other); !ok || d.Code != config.CodeKeyReferenceMalformed {
		t.Fatalf("fixture: DiagnosticOf = %+v, %v; want key_reference_malformed", d, ok)
	}
	if got := withKeyRefusalRemedy(other); got != other || got.Error() != other.Error() {
		t.Errorf("got %v, want the same error unchanged", got)
	}
}
