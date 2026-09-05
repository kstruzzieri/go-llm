package interceptor

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// execEffect is the static class run_command and start_command declare.
var execEffect = agent.Effect{Class: agent.Read | agent.Write | agent.Exec | agent.Network}

// egressCall builds an inspection for run_command with raw arguments and the
// given static effect.
func egressCall(effect agent.Effect, args string) agent.ToolCallInspection {
	return agent.ToolCallInspection{Step: 2, Effect: effect,
		Call: provider.ToolCall{ID: "c9", Type: "function", Function: provider.ToolCallFunction{Name: "run_command", Arguments: json.RawMessage(args)}}}
}

// argvJSON encodes an argv as the run_command argument object.
func argvJSON(t *testing.T, argv ...string) string {
	t.Helper()
	raw, err := json.Marshal(map[string][]string{"argv": argv})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// classifyArgv runs the classifier through InspectToolCall on an exec-class
// call and checks the input slice is untouched.
func classifyArgv(t *testing.T, argv ...string) []agent.Finding {
	t.Helper()
	before := slices.Clone(argv)
	found, err := Egress{}.InspectToolCall(context.Background(), egressCall(execEffect, argvJSON(t, argv...)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, before) {
		t.Fatalf("classifier modified its input: %q -> %q", before, argv)
	}
	return found
}

type egressWant struct {
	rule  string // "" means no finding (quiet)
	risk  int
	label string
}

func expectEgress(t *testing.T, found []agent.Finding, want egressWant) {
	t.Helper()
	if want.rule == "" {
		if len(found) != 0 {
			t.Fatalf("findings = %+v, want none (quiet)", found)
		}
		return
	}
	if len(found) != 1 || found[0].Rule != want.rule || found[0].Risk != want.risk || found[0].Detail != want.label || found[0].Verdict != agent.VerdictTag {
		t.Fatalf("findings = %+v, want one %s tag risk %d label %q", found, want.rule, want.risk, want.label)
	}
}

func TestEgressDirectNames(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want egressWant
	}{
		{"curl", []string{"curl", "https://x"}, egressWant{"network", 20, "curl"}},
		{"absolute path", []string{"/usr/bin/curl", "x"}, egressWant{"network", 20, "curl"}},
		{"windows exe and case", []string{"CURL.EXE", "x"}, egressWant{"network", 20, "curl"}},
		{"mixed case", []string{"Wget", "x"}, egressWant{"network", 20, "wget"}},
		{"httpie is a client", []string{"http", "GET", "example.com"}, egressWant{"network", 20, "http"}},
		{"docker", []string{"docker", "run", "x"}, egressWant{"network", 20, "docker"}},
		{"cloud cli", []string{"aws", "s3", "ls"}, egressWant{"network", 20, "aws"}},
		{"sudo", []string{"sudo", "curl", "x"}, egressWant{"privileged", 20, "sudo"}},
		{"doas", []string{"doas", "ls"}, egressWant{"privileged", 20, "doas"}},
		{"su", []string{"su", "-c", "ls"}, egressWant{"privileged", 20, "su"}},
		{"npm", []string{"npm", "install"}, egressWant{"package-manager", 10, "npm"}},
		{"pip", []string{"pip", "install", "x"}, egressWant{"package-manager", 10, "pip"}},
		{"brew", []string{"brew", "install", "x"}, egressWant{"package-manager", 10, "brew"}},
		{"bash script file", []string{"bash", "build.sh"}, egressWant{"interpreter", 0, "bash"}},
		{"fish inline", []string{"fish", "-c", "curl x"}, egressWant{"interpreter", 0, "fish"}},
		{"python script", []string{"python", "foo.py"}, egressWant{"interpreter", 0, "python"}},
		{"node script", []string{"node", "app.js"}, egressWant{"interpreter", 0, "node"}},
		{"versioned python is unknown", []string{"python3.12", "x.py"}, egressWant{"unknown", 10, `"python3.12"`}},
		{"ls", []string{"ls", "-la"}, egressWant{}},
		{"make", []string{"make", "test"}, egressWant{}},
		{"gofmt", []string{"gofmt", "-l", "."}, egressWant{}},
		{"golangci-lint", []string{"golangci-lint", "run", "./..."}, egressWant{}},
		{"pytest", []string{"pytest", "-q"}, egressWant{}},
		{"unknown", []string{"frob"}, egressWant{"unknown", 10, `"frob"`}},
		{"unknown keeps raw basename not directory", []string{"/very/long/directory/name/frob", "x"}, egressWant{"unknown", 10, `"frob"`}},
		{"unknown raw case kept", []string{"Frob.EXE"}, egressWant{"unknown", 10, `"Frob.EXE"`}},
		{"xargs is not peeled", []string{"xargs", "-n1", "curl"}, egressWant{"unknown", 10, `"xargs"`}},
		{"command is not a builtin here", []string{"command", "curl", "x"}, egressWant{"unknown", 10, `"command"`}},
		{"exec is not a builtin here", []string{"exec", "curl", "x"}, egressWant{"unknown", 10, `"exec"`}},
		{"dash alone", []string{"-"}, egressWant{"unknown", 10, `"-"`}},
		{"empty argv0", []string{"", "x"}, egressWant{"unknown", 10, `""`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectEgress(t, classifyArgv(t, tc.argv...), tc.want)
		})
	}
}

func TestEgressPythonModuleMode(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want egressWant
	}{
		{"python3 -m pip", []string{"python3", "-m", "pip", "install", "x"}, egressWant{"package-manager", 10, "python3 -m pip"}},
		{"python -m pip", []string{"python", "-m", "pip", "--version"}, egressWant{"package-manager", 10, "python -m pip"}},
		{"option before -m is interpreter", []string{"python", "-u", "-m", "pip", "install"}, egressWant{"interpreter", 0, "python"}},
		{"script operand before -m", []string{"python", "foo.py", "-m", "pip"}, egressWant{"interpreter", 0, "python"}},
		{"terminator before -m", []string{"python", "--", "-m", "pip"}, egressWant{"interpreter", 0, "python"}},
		{"other module", []string{"python3", "-m", "pipx", "run", "x"}, egressWant{"interpreter", 0, "python3"}},
		{"-m without module", []string{"python3", "-m"}, egressWant{"interpreter", 0, "python3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectEgress(t, classifyArgv(t, tc.argv...), tc.want)
		})
	}
}

func TestEgressGitAndGoSubcommands(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want egressWant
	}{
		{"git push", []string{"git", "push", "origin", "main"}, egressWant{"network", 20, "git push"}},
		{"git -C push", []string{"git", "-C", "sub", "push"}, egressWant{"network", 20, "git push"}},
		{"git -c fetch", []string{"git", "-c", "a=b", "fetch"}, egressWant{"network", 20, "git fetch"}},
		{"git --git-dir= pull", []string{"git", "--git-dir=.git", "pull"}, egressWant{"network", 20, "git pull"}},
		{"git -c= is not a git form", []string{"git", "-c=a=b", "push"}, egressWant{"unknown", 10, `"git"`}},
		{"git --no-pager= is not a git form", []string{"git", "--no-pager=1", "status"}, egressWant{"unknown", 10, `"git"`}},
		{"git --work-tree clone", []string{"git", "--work-tree", "x", "clone", "u"}, egressWant{"network", 20, "git clone"}},
		{"git --no-pager log", []string{"git", "--no-pager", "log"}, egressWant{}},
		{"git --paginate diff", []string{"git", "--paginate", "diff"}, egressWant{}},
		{"git status", []string{"git", "status"}, egressWant{}},
		{"git --version", []string{"git", "--version"}, egressWant{}},
		{"git --help", []string{"git", "--help"}, egressWant{}},
		{"git alias or external", []string{"git", "foo"}, egressWant{"unknown", 10, `"git foo"`}},
		{"git long unknown subcommand capped", []string{"git", strings.Repeat("s", 40)}, egressWant{"unknown", 10, `"git ` + strings.Repeat("s", 20) + `"`}},
		{"git alone", []string{"git"}, egressWant{"unknown", 10, `"git"`}},
		{"git -C missing value", []string{"git", "-C"}, egressWant{"unknown", 10, `"git"`}},
		{"git -C then nothing", []string{"git", "-C", "sub"}, egressWant{"unknown", 10, `"git"`}},
		{"git unknown global option", []string{"git", "--bogus", "status"}, egressWant{"unknown", 10, `"git"`}},
		{"git unknown option cannot hide push", []string{"git", "--bogus", "push"}, egressWant{"unknown", 10, `"git"`}},
		{"go get", []string{"go", "get", "x"}, egressWant{"package-manager", 10, "go get"}},
		{"go install", []string{"go", "install", "x@latest"}, egressWant{"package-manager", 10, "go install"}},
		{"go mod tidy", []string{"go", "mod", "tidy"}, egressWant{"package-manager", 10, "go mod"}},
		{"go -C build", []string{"go", "-C", "dir", "build", "./..."}, egressWant{}},
		{"go -C= get", []string{"go", "-C=dir", "get", "x"}, egressWant{"package-manager", 10, "go get"}},
		{"go test", []string{"go", "test", "./..."}, egressWant{}},
		{"go run", []string{"go", "run", "."}, egressWant{}},
		{"go generate is unknown", []string{"go", "generate", "./..."}, egressWant{"unknown", 10, `"go generate"`}},
		{"go alone", []string{"go"}, egressWant{"unknown", 10, `"go"`}},
		{"go unknown global option", []string{"go", "-x", "build"}, egressWant{"unknown", 10, `"go"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectEgress(t, classifyArgv(t, tc.argv...), tc.want)
		})
	}
}

func TestEgressWrapperMatrix(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want egressWant
	}{
		{"env -u then go test", []string{"env", "-u", "GOROOT", "go", "test", "./..."}, egressWant{}},
		{"env assignment then curl", []string{"env", "FOO=1", "curl", "x"}, egressWant{"network", 20, "curl"}},
		{"env full supported grammar", []string{"env", "-i", "--ignore-environment", "-u", "A", "--unset=B", "-C", "/tmp", "--chdir=/tmp", "X_1=y", "--", "curl", "x"}, egressWant{"network", 20, "curl"}},
		{"env terminator protects assignment-looking command", []string{"env", "--", "A=b", "x"}, egressWant{"unknown", 10, `"A=b"`}},
		{"env split-string is unsupported", []string{"env", "-S", "curl x"}, egressWant{"unknown", 10, `"env" unsupported form`}},
		{"env --split-string= is unsupported", []string{"env", "--split-string=curl x"}, egressWant{"unknown", 10, `"env" unsupported form`}},
		{"env unknown flag", []string{"env", "-0", "curl"}, egressWant{"unknown", 10, `"env" unsupported form`}},
		{"env malformed assignment", []string{"env", "1BAD=x", "curl"}, egressWant{"unknown", 10, `"env" unsupported form`}},
		{"env -u missing value", []string{"env", "-u"}, egressWant{"unknown", 10, `"env" unsupported form`}},
		{"env alone", []string{"env"}, egressWant{"unknown", 10, `"env" missing command`}},
		{"env only assignments", []string{"env", "A=b"}, egressWant{"unknown", 10, `"env" missing command`}},
		{"env terminator alone", []string{"env", "--"}, egressWant{"unknown", 10, `"env" missing command`}},
		{"nohup", []string{"nohup", "npm", "install"}, egressWant{"package-manager", 10, "npm"}},
		{"nohup terminator", []string{"nohup", "--", "npm", "i"}, egressWant{"package-manager", 10, "npm"}},
		{"nohup option", []string{"nohup", "-x", "ls"}, egressWant{"unknown", 10, `"nohup" unsupported form`}},
		{"nice -n", []string{"nice", "-n", "10", "curl", "x"}, egressWant{"network", 20, "curl"}},
		{"nice --adjustment=", []string{"nice", "--adjustment=5", "ls"}, egressWant{}},
		{"nice legacy form", []string{"nice", "-10", "curl"}, egressWant{"unknown", 10, `"nice" unsupported form`}},
		{"nice attached value", []string{"nice", "-n10", "curl"}, egressWant{"unknown", 10, `"nice" unsupported form`}},
		{"time -p", []string{"time", "-p", "go", "test"}, egressWant{}},
		{"time terminator", []string{"time", "--", "ls"}, egressWant{}},
		{"time other option", []string{"time", "-v", "ls"}, egressWant{"unknown", 10, `"time" unsupported form`}},
		{"timeout duration", []string{"timeout", "10", "curl", "x"}, egressWant{"network", 20, "curl"}},
		{"timeout full grammar", []string{"timeout", "-k", "5", "--signal=TERM", "--preserve-status", "--foreground", "--verbose", "1.5m", "curl", "x"}, egressWant{"network", 20, "curl"}},
		{"timeout terminator then duration", []string{"timeout", "--", "10", "ls"}, egressWant{}},
		{"timeout missing command", []string{"timeout", "10"}, egressWant{"unknown", 10, `"timeout" missing command`}},
		{"timeout bad duration", []string{"timeout", "curl", "x"}, egressWant{"unknown", 10, `"timeout" unsupported form`}},
		{"timeout -k missing value", []string{"timeout", "-k"}, egressWant{"unknown", 10, `"timeout" unsupported form`}},
		{"stdbuf separate values", []string{"stdbuf", "-o", "L", "-e", "0", "curl", "x"}, egressWant{"network", 20, "curl"}},
		{"stdbuf attached value", []string{"stdbuf", "-oL", "curl", "x"}, egressWant{"unknown", 10, `"stdbuf" unsupported form`}},
		{"nested wrappers", []string{"env", "FOO=1", "nice", "-n", "5", "timeout", "10", "curl", "x"}, egressWant{"network", 20, "curl"}},
		{"nested wrappers to quiet", []string{"nohup", "env", "-u", "X", "make", "test"}, egressWant{}},
		{"sudo is never peeled", []string{"env", "-u", "X", "sudo", "curl"}, egressWant{"privileged", 20, "sudo"}},
		{"wrapper uncertainty cannot be quiet", []string{"env", "--bogus", "ls"}, egressWant{"unknown", 10, `"env" unsupported form`}},
		{"wrapper uncertainty cannot hide network", []string{"timeout", "--bogus", "10", "curl"}, egressWant{"unknown", 10, `"timeout" unsupported form`}},
		{"windows wrapper name", []string{"ENV.EXE", "FOO=1", "ls"}, egressWant{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectEgress(t, classifyArgv(t, tc.argv...), tc.want)
		})
	}
}

func TestEgressUnknownLabelBounds(t *testing.T) {
	long := strings.Repeat("a", 40)
	cases := []struct {
		name  string
		argv0 string
		label string
	}{
		{"capped at 24 bytes", long, `"` + long[:24] + `"`},
		{"rune boundary", strings.Repeat("a", 23) + "éx", `"` + strings.Repeat("a", 23) + `"`},
		{"exactly 24 bytes", strings.Repeat("b", 24), `"` + strings.Repeat("b", 24) + `"`},
		{"escape control", "\x1b[2Jfrob", `"\x1b[2Jfrob"`},
		{"escape newline", "fr\nob", `"fr\nob"`},
		{"escape quote", `fr"ob`, `"fr\"ob"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectEgress(t, classifyArgv(t, tc.argv0, "x"), egressWant{"unknown", 10, tc.label})
		})
	}
}

// TestEgressGates: the static Exec class gates classification; argv must be
// a present, unambiguous, non-empty string array decoded the way the exec
// tools decode it (a null element is an empty string).
func TestEgressGates(t *testing.T) {
	cases := []struct {
		name   string
		effect agent.Effect
		args   string
		want   egressWant
	}{
		{"read class with argv", agent.Effect{Class: agent.Read}, `{"argv":["curl","x"]}`, egressWant{}},
		{"network without exec", agent.Effect{Class: agent.Read | agent.Network}, `{"argv":["curl","x"]}`, egressWant{}},
		{"empty argv", execEffect, `{"argv":[]}`, egressWant{}},
		{"null argv", execEffect, `{"argv":null}`, egressWant{}},
		{"numeric argv", execEffect, `{"argv":[1]}`, egressWant{}},
		{"mixed argv", execEffect, `{"argv":["curl",1]}`, egressWant{}},
		{"string argv", execEffect, `{"argv":"curl"}`, egressWant{}},
		{"no argv", execEffect, `{"dir":"x"}`, egressWant{}},
		{"non-object", execEffect, `["curl"]`, egressWant{}},
		{"null element decodes as empty string", execEffect, `{"argv":["curl",null]}`, egressWant{"network", 20, "curl"}},
		{"case-equivalent key is guarded", execEffect, `{"ARGV":["curl","x"]}`, egressWant{"network", 20, "curl"}},
		{"ambiguous argv stays visible", execEffect, `{"argv":["ls"],"Argv":["curl"]}`, egressWant{"unknown", 10, `"argv" ambiguous`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, err := Egress{}.InspectToolCall(context.Background(), egressCall(tc.effect, tc.args))
			if err != nil {
				t.Fatal(err)
			}
			expectEgress(t, found, tc.want)
		})
	}
}

func TestEgressFindingLiteralAndHooks(t *testing.T) {
	found := classifyArgv(t, "curl", "https://x")
	want := []agent.Finding{{
		Rule: "network", Verdict: agent.VerdictTag, Risk: 20, Origin: agent.OriginModel,
		Target: agent.TargetToolCall, StateIndex: -1, Group: -1, Alternative: -1, ToolCallID: "c9", Detail: "curl",
	}}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("finding = %+v, want %+v", found, want)
	}
	var e Egress
	if e.Name() != "egress" {
		t.Fatalf("name = %q", e.Name())
	}
	if got, err := e.InspectInput(context.Background(), agent.InputInspection{System: "curl"}); err != nil || got != nil {
		t.Fatalf("input = %+v, %v", got, err)
	}
	if got, err := e.InspectOutput(context.Background(), agent.OutputInspection{Content: "curl"}); err != nil || got != nil {
		t.Fatalf("output = %+v, %v", got, err)
	}
}

// TestEgressClassificationData pins the exact v1 tables from the spec so a
// membership change is a reviewed policy change.
func TestEgressClassificationData(t *testing.T) {
	sorted := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	pin := func(name string, got map[string]bool, want string) {
		t.Helper()
		w := strings.Fields(want)
		slices.Sort(w)
		if g := sorted(got); !slices.Equal(g, w) {
			t.Errorf("%s = %v, want %v", name, g, w)
		}
	}
	pin("network", networkBins, "curl wget nc ncat netcat ssh scp sftp rsync telnet ftp socat aria2c http https docker podman kubectl helm terraform gh gcloud aws az")
	pin("privileged", privilegedBins, "sudo doas su")
	pin("package", packageBins, "npm npx yarn pnpm bun pip pip3 pipx uv poetry conda cargo gem bundle composer brew apt apt-get dnf yum pacman apk nix mvn gradle")
	pin("shells", shellNames, "sh bash zsh dash ksh fish")
	pin("interpreters", interpreterNames, "python python3 perl ruby node deno php")
	pin("git network", gitNetworkSubs, "push fetch pull clone ls-remote remote submodule")
	pin("git quiet", gitQuietSubs, "status diff log show rev-parse rev-list ls-files ls-tree cat-file check-ignore check-attr describe branch tag config help version")
	pin("go package", goPackageSubs, "get install mod")
	pin("go quiet", goQuietSubs, "build test run fmt vet list env version doc help")
	pin("quiet", quietNames, "ls cat head tail wc grep rg ag find fd sed awk sort uniq cut tr diff cmp echo printf true false test pwd mkdir rmdir cp mv rm touch chmod ln stat file which date sleep tee basename dirname realpath readlink jq yq tar gzip gunzip zip unzip make cmake ninja gofmt goimports golangci-lint staticcheck gcc clang cc rustc tsc eslint prettier black ruff mypy pytest jest vitest")
	if got := []int{EgressNetworkRisk, EgressPrivilegedRisk, EgressPackageManagerRisk, EgressUnknownRisk, EgressInterpreterRisk}; !slices.Equal(got, []int{20, 20, 10, 10, 0}) {
		t.Errorf("weights = %v, want [20 20 10 10 0]", got)
	}
}
