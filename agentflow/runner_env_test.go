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
