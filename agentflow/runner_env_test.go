package agentflow

import (
	"reflect"
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
			"SYSTEMROOT", "TEMP", "TMP", "PATHEXT", "USERPROFILE", "COMSPEC"}, true},
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

func TestBuildChildEnvUnsetApprovedNameFails(t *testing.T) {
	_, err := buildChildEnv(childEnvPolicy{approved: []string{"GOPRIVATE"}},
		mapLookup(map[string]string{"OTHER": "sk-secret-577"}))
	if err == nil || err.Error() != `agentflow: approved environment variable "GOPRIVATE" is not set` {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateEnvNames(t *testing.T) {
	for _, tc := range []struct {
		names   []string
		wantErr string
	}{
		{names: nil},
		{names: []string{"GOPRIVATE", "_X", "https_proxy", "A1", "AGENTFLOW"}},
		{names: []string{"GOPRIVATE", "NAME=sk-SECRET-577"},
			wantErr: "agentflow: environment name #2 is not a variable name (names only; values are read from the environment)"},
		{names: []string{""}, wantErr: "#1 is not a variable name"},
		{names: []string{"1ABC"}, wantErr: "#1 is not a variable name"},
		{names: []string{"A B"}, wantErr: "#1 is not a variable name"},
		{names: []string{"pythonPath"}, wantErr: "agentflow: environment name #1 is runner-owned"},
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
