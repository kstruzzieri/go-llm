package agentflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func mapLookup(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func TestBuildChildEnv(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy childEnvPolicy
		parent map[string]string
		want   []string
	}{
		{name: "empty parent and policy is non-nil empty",
			policy: childEnvPolicy{baseline: agentflowBaselineEnv}, parent: map[string]string{}, want: []string{}},
		{name: "baseline forwarded when set, unrelated names dropped",
			policy: childEnvPolicy{baseline: agentflowBaselineEnv},
			parent: map[string]string{
				"PATH": "/bin", "HOME": "/h", "USER": "test", "TMPDIR": "/tmp", "LANG": "C",
				"OPENAI_API_KEY": "sk-x", "GOFLAGS": "-tags=x",
			},
			want: []string{"HOME=/h", "LANG=C", "PATH=/bin", "TMPDIR=/tmp", "USER=test"}},
		{name: "approved names forwarded, empty value kept",
			policy: childEnvPolicy{baseline: agentflowBaselineEnv, approved: []string{"GOPRIVATE", "EMPTY"}},
			parent: map[string]string{"GOPRIVATE": "example.com", "EMPTY": ""},
			want:   []string{"EMPTY=", "GOPRIVATE=example.com"}},
		{name: "duplicate approved names collapse",
			policy: childEnvPolicy{approved: []string{"GOPRIVATE", "GOPRIVATE"}},
			parent: map[string]string{"GOPRIVATE": "a"},
			want:   []string{"GOPRIVATE=a"}},
		{name: "strict 1 forwarded as constant",
			parent: map[string]string{"AGENTFLOW_STRICT": "1"}, want: []string{"AGENTFLOW_STRICT=1"}},
		{name: "strict true dropped", parent: map[string]string{"AGENTFLOW_STRICT": "true"}, want: []string{}},
		{name: "strict empty dropped", parent: map[string]string{"AGENTFLOW_STRICT": ""}, want: []string{}},
		{name: "confirm-risk and agent-id never forwarded",
			parent: map[string]string{"AGENTFLOW_CONFIRM_RISK": "1", "AGENTFLOW_AGENT_ID": "x"}, want: []string{}},
		{name: "runner-owned wins over parent",
			policy: childEnvPolicy{
				baseline: []string{"PYTHONPATH", "PYTHONDONTWRITEBYTECODE"},
				owned:    []string{"PYTHONPATH=/af/src", "PYTHONDONTWRITEBYTECODE=1"},
			},
			parent: map[string]string{"PYTHONPATH": "/evil", "PYTHONDONTWRITEBYTECODE": ""},
			want:   []string{"PYTHONDONTWRITEBYTECODE=1", "PYTHONPATH=/af/src"}},
		{name: "sorted by name, not by entry text",
			policy: childEnvPolicy{approved: []string{"A1", "A"}},
			parent: map[string]string{"A": "x", "A1": "y"},
			want:   []string{"A=x", "A1=y"}},
		{name: "windows folds case: approved Path replaces baseline PATH",
			policy: childEnvPolicy{baseline: []string{"PATH"}, approved: []string{"Path"}, foldCase: true},
			parent: map[string]string{"PATH": `C:\a`, "Path": `C:\a`},
			want:   []string{`Path=C:\a`}},
		{name: "unix keeps case: Path and PATH distinct",
			policy: childEnvPolicy{baseline: []string{"PATH"}, approved: []string{"Path"}},
			parent: map[string]string{"PATH": "/a", "Path": "/b"},
			want:   []string{"PATH=/a", "Path=/b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildChildEnv(tc.policy, mapLookup(tc.parent))
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("env = %#v, want %#v (non-nil)", got, tc.want)
			}
		})
	}
}

// CI never runs Windows, so this is the only check of the Windows selection.
// Expected lists are literals so a change to either baseline variable shows up.
func TestChildEnvPolicyFor(t *testing.T) {
	approved, owned := []string{"GOPRIVATE"}, []string{"PYTHONPATH=/af/src"}
	for _, tc := range []struct {
		goos     string
		baseline []string
		foldCase bool
	}{
		{"linux", []string{"PATH", "HOME", "USER", "TMPDIR", "LANG"}, false},
		{"darwin", []string{"PATH", "HOME", "USER", "TMPDIR", "LANG"}, false},
		{"windows", []string{"PATH", "HOME", "USER", "TMPDIR", "LANG",
			"SYSTEMROOT", "TEMP", "TMP", "PATHEXT", "USERPROFILE", "COMSPEC", "LOCALAPPDATA", "APPDATA"}, true},
	} {
		p := childEnvPolicyFor(tc.goos, approved, owned)
		if !reflect.DeepEqual(p.baseline, tc.baseline) || p.foldCase != tc.foldCase ||
			!reflect.DeepEqual(p.approved, approved) || !reflect.DeepEqual(p.owned, owned) {
			t.Errorf("%s: policy = %+v, want baseline %v foldCase %v", tc.goos, p, tc.baseline, tc.foldCase)
		}
	}
	if want := []string{"PATH", "HOME", "USER", "TMPDIR", "LANG"}; !reflect.DeepEqual(agentflowBaselineEnv, want) {
		t.Fatalf("building the Windows policy changed the shared baseline: %v", agentflowBaselineEnv)
	}
}

// Go reads its build cache from %LocalAppData% and its saved settings from
// %AppData% on Windows; HOME and USERPROFILE do not substitute. Both are
// optional baseline entries there, like HOME elsewhere.
func TestBuildChildEnvWindowsDirectories(t *testing.T) {
	parent := map[string]string{
		"LOCALAPPDATA": `C:\Users\u\AppData\Local`, "APPDATA": `C:\Users\u\AppData\Roaming`,
		"OPENAI_API_KEY": "sk-x",
	}
	for _, tc := range []struct {
		name     string
		goos     string
		approved []string
		parent   map[string]string
		want     []string
	}{
		{name: "windows forwards both", goos: "windows", parent: parent,
			want: []string{`APPDATA=C:\Users\u\AppData\Roaming`, `LOCALAPPDATA=C:\Users\u\AppData\Local`}},
		{name: "windows treats them as optional", goos: "windows", parent: map[string]string{}, want: []string{}},
		{name: "windows approval of LocalAppData folds into one entry", goos: "windows", approved: []string{"LocalAppData"},
			parent: map[string]string{"LOCALAPPDATA": `C:\L`, "LocalAppData": `C:\L`},
			want:   []string{`LocalAppData=C:\L`}},
		{name: "unix does not forward them", goos: "linux", parent: parent, want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildChildEnv(childEnvPolicyFor(tc.goos, tc.approved, nil), mapLookup(tc.parent))
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("env = %#v, err = %v; want %#v", got, err, tc.want)
			}
		})
	}
}

func TestBuildChildEnvUnsetApprovedNameFails(t *testing.T) {
	_, err := buildChildEnv(childEnvPolicy{approved: []string{"GOPRIVATE"}},
		mapLookup(map[string]string{"OTHER": "sk-secret-577"}))
	var unset *EnvNotSetError
	if !errors.As(err, &unset) || unset.Name != "GOPRIVATE" ||
		err.Error() != `agentflow: approved environment variable "GOPRIVATE" is not set` {
		t.Fatalf("err = %v, want *EnvNotSetError naming GOPRIVATE", err)
	}
}

func TestValidateEnvNames(t *testing.T) {
	for _, tc := range []struct {
		names   []string
		wantErr string
	}{
		{names: nil},
		{names: []string{"GOPRIVATE", "_X", "https_proxy", "A1", "AGENTFLOW", "OLDPWD"}},
		{names: []string{"GOPRIVATE", "NAME=sk-SECRET-577"},
			wantErr: "agentflow: environment name #2 is not a variable name (names only; values are read from the environment)"},
		{names: []string{""}, wantErr: "#1 is not a variable name"},
		{names: []string{"1ABC"}, wantErr: "#1 is not a variable name"},
		{names: []string{"A B"}, wantErr: "#1 is not a variable name"},
		{names: []string{"pythonPath"}, wantErr: "agentflow: environment name #1 is runner-owned"},
		{names: []string{"PWD"}, wantErr: "agentflow: environment name #1 is runner-owned"},
		{names: []string{"X", "pwd"}, wantErr: "agentflow: environment name #2 is runner-owned"},
		{names: []string{"Pwd"}, wantErr: "agentflow: environment name #1 is runner-owned"},
		{names: []string{"X", "PYTHONDONTWRITEBYTECODE"}, wantErr: "agentflow: environment name #2 is runner-owned"},
		{names: []string{"agentflow_strict"}, wantErr: "agentflow: environment name #1 is an Agentflow control variable"},
		{names: []string{"AGENTFLOW_CONFIRM_RISK"}, wantErr: "#1 is an Agentflow control variable"},
	} {
		err := ValidateEnvNames(tc.names)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("ValidateEnvNames(%d names) = %v, want nil", len(tc.names), err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("ValidateEnvNames error = %v, want %q", err, tc.wantErr)
		}
		if err != nil && strings.Contains(err.Error(), "SECRET") {
			t.Errorf("error echoes its input: %v", err)
		}
	}
}

func TestAgentflowTestSource(t *testing.T) {
	for _, tc := range []struct {
		mode, src    string
		installed    bool
		useSrc, skip bool
		wantErr      string
	}{
		{mode: "", src: "", installed: false, skip: true},
		{mode: "", src: "", installed: true},
		{mode: "", src: "/af", installed: true, useSrc: true},
		{mode: "installed", src: "", installed: true},
		{mode: "installed", src: "/af", installed: true, wantErr: "AGENTFLOW_SRC is set"},
		{mode: "installed", src: "", installed: false, wantErr: "agentflow is not on PATH"},
		{mode: "source", src: "/af", useSrc: true},
		{mode: "source", src: "", installed: true, wantErr: "AGENTFLOW_SRC is empty"},
		{mode: "yes", wantErr: `GO_LLM_REQUIRE_AGENTFLOW="yes", want installed or source`},
	} {
		useSrc, skip, err := agentflowTestSource(tc.mode, tc.src, tc.installed)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%+v: err = %v, want %q", tc, err, tc.wantErr)
			}
			continue
		}
		if err != nil || useSrc != tc.useSrc || skip != tc.skip {
			t.Errorf("%+v: got useSrc=%v skip=%v err=%v", tc, useSrc, skip, err)
		}
	}
}

func TestExecRunnerAllowEnvIsAtomic(t *testing.T) {
	r := NewExecRunner(t.TempDir())
	if err := r.AllowEnv("GOPRIVATE"); err != nil {
		t.Fatal(err)
	}
	if err := r.AllowEnv("HTTPS_PROXY", "NAME=sk-SECRET-577"); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("AllowEnv error = %v, want a value-free validation error", err)
	}
	if !reflect.DeepEqual(r.allowed, []string{"GOPRIVATE"}) {
		t.Fatalf("allowed = %v, want only the first batch", r.allowed)
	}
}

const (
	envCanaryName   = "GO_LLM_577_CANARY"
	envCanaryValue  = "sk-canary-577-must-not-reach-children"
	envApprovedName = "GO_LLM_577_APPROVED"
)

// envProbeReport is what the probe child writes: names, a canary flag and a
// digest, never a value, so a failing test cannot print a secret.
type envProbeReport struct {
	Names          []string `json:"names"`
	Canary         bool     `json:"canary"`
	ApprovedSHA256 string   `json:"approved_sha256"`
	PWD            string   `json:"pwd"` // a path, never a secret
}

// TestEnvProbeHelper is not a test. Re-executed as `<test binary>
// -test.run=^TestEnvProbeHelper$ -- envprobe <out>`, it writes an
// envProbeReport to <out>. It is selected by argv rather than an environment
// marker because the policy under test strips unknown variables.
func TestEnvProbeHelper(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 || len(os.Args) != i+3 || os.Args[i+1] != "envprobe" {
		return
	}
	report := envProbeReport{Names: []string{}}
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		report.Names = append(report.Names, name)
		if name == envCanaryName || strings.Contains(value, envCanaryValue) {
			report.Canary = true
		}
		if name == envApprovedName {
			sum := sha256.Sum256([]byte(value))
			report.ApprovedSHA256 = hex.EncodeToString(sum[:])
		}
		if name == "PWD" {
			report.PWD = value
		}
	}
	slices.Sort(report.Names)
	data, err := json.Marshal(report)
	if err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(os.Args[i+2], data, 0o600); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

// envProbeArgv is the argv, after the test binary, that runs the probe.
func envProbeArgv(out string) []string {
	return []string{"-test.run=^TestEnvProbeHelper$", "--", "envprobe", out}
}

func readEnvProbe(t *testing.T, out string) envProbeReport {
	t.Helper()
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("probe report: %v", err)
	}
	var report envProbeReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("probe report: %v", err)
	}
	return report
}

// runEnvProbe swaps only the executable and argv of a constructed runner, so
// the constructor's environment wiring is what gets exercised.
func runEnvProbe(t *testing.T, r *ExecRunner) envProbeReport {
	t.Helper()
	out := filepath.Join(t.TempDir(), "probe.json")
	r.bin, r.prefix = os.Args[0], envProbeArgv(out)
	_, stderr, exit, err := r.Run(t.Context(), nil, nil)
	if err != nil || exit != 0 {
		t.Fatalf("probe exit=%d err=%v stderr=%q", exit, err, stderr)
	}
	return readEnvProbe(t, out)
}

func TestExecRunnerConstructorsDropParentEnvironment(t *testing.T) {
	t.Setenv(envCanaryName, envCanaryValue)
	t.Setenv("OPENAI_API_KEY", envCanaryValue)
	t.Setenv("HOME", t.TempDir())
	checkout := writeSourceCheckoutFixture(t)
	for _, tc := range []struct {
		name      string
		runner    *ExecRunner
		wantNames []string
	}{
		{name: "installed", runner: NewExecRunner(t.TempDir()), wantNames: []string{"HOME", "PATH"}},
		{name: "source", runner: NewSrcExecRunner(t.TempDir(), checkout), wantNames: []string{"HOME", "PATH", "PYTHONPATH"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := os.Environ()
			report := runEnvProbe(t, tc.runner)
			if report.Canary || slices.Contains(report.Names, "OPENAI_API_KEY") {
				t.Fatalf("parent secret reached the child; names=%v", report.Names)
			}
			for _, name := range tc.wantNames {
				if !slices.Contains(report.Names, name) {
					t.Fatalf("names=%v, want %s", report.Names, name)
				}
			}
			if !reflect.DeepEqual(os.Environ(), before) {
				t.Fatal("Run changed the parent environment")
			}
		})
	}
}

func TestExecRunnerReadsApprovedValuesAtEachLaunch(t *testing.T) {
	r := NewExecRunner(t.TempDir())
	if err := r.AllowEnv(envApprovedName); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"first", "second"} {
		t.Setenv(envApprovedName, value)
		sum := sha256.Sum256([]byte(value))
		if got := runEnvProbe(t, r).ApprovedSHA256; got != hex.EncodeToString(sum[:]) {
			t.Fatalf("approved digest = %s, want the digest of the value set before this launch", got)
		}
	}
	if err := os.Unsetenv(envApprovedName); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "probe.json")
	r.prefix = envProbeArgv(out)
	stdout, stderr, exit, err := r.Run(t.Context(), nil, nil)
	var unset *EnvNotSetError
	if !errors.As(err, &unset) || unset.Name != envApprovedName ||
		err.Error() != `agentflow: approved environment variable "GO_LLM_577_APPROVED" is not set` ||
		stdout != nil || stderr != nil || exit != 0 {
		t.Fatalf("unset approved name: stdout=%q stderr=%q exit=%d err=%v", stdout, stderr, exit, err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("probe launched despite an unset approved name: %v", statErr)
	}
}

func TestExecRunnerStartupFailureReturnsError(t *testing.T) {
	r := NewExecRunner(t.TempDir())
	r.bin = filepath.Join(t.TempDir(), "missing-agentflow")
	stdout, stderr, exit, err := r.Run(t.Context(), []string{"--version"}, nil)
	if err == nil || len(stdout) != 0 || len(stderr) != 0 || exit != 0 {
		t.Fatalf("stdout=%q stderr=%q exit=%d err=%v, want a launch error", stdout, stderr, exit, err)
	}
}
