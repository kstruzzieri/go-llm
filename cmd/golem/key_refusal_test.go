package main

import (
	"errors"
	"fmt"
	"net"
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

// refusalWorkingDir writes a ./models.json whose provider points at a
// counting listener and carries a ${CANARY_VAR} key, isolates discovery, and
// makes it the working directory. The variable is set, so a refusal (not an
// absent variable) is what keeps the request count at zero.
func refusalWorkingDir(t *testing.T, host string) *atomic.Int64 {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.NotFound(w, nil)
	}))
	if host != "127.0.0.1" {
		l, err := net.Listen("tcp", host+":0")
		if err != nil {
			t.Skipf("cannot listen on %s: %v", host, err)
		}
		srv.Listener = l
	}
	srv.Start()
	t.Cleanup(srv.Close)

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
}`, srv.URL)
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return &hits
}

// nonLoopbackHost returns a non-loopback address of this machine, or "".
func nonLoopbackHost() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			return ipn.IP.String()
		}
	}
	return ""
}

func refusalHosts(t *testing.T) []string {
	hosts := []string{"127.0.0.1"}
	if h := nonLoopbackHost(); h != "" {
		hosts = append(hosts, h)
	} else {
		t.Log("non-loopback leg skipped: no non-loopback IPv4 address on this machine")
	}
	return hosts
}

func TestGolemPromptModeRefusesWorkingDirKey(t *testing.T) {
	for _, host := range refusalHosts(t) {
		t.Run(host, func(t *testing.T) {
			hits := refusalWorkingDir(t, host)
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
	for _, host := range refusalHosts(t) {
		t.Run(host, func(t *testing.T) {
			hits := refusalWorkingDir(t, host)
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

func TestWithKeyRefusalRemedyPassesOtherErrorsThrough(t *testing.T) {
	other := errors.New("boom")
	if got := withKeyRefusalRemedy(other); got != other {
		t.Errorf("got %v, want the same error", got)
	}
}
