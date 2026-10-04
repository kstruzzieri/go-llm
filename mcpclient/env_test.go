package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestEnvVarAndServerFormatRedact(t *testing.T) {
	v := SetEnv("TOKEN", "canary-env")
	if got := fmt.Sprintf("%v|%+v|%#v|%s|%q", v, v, v, v, v); strings.Contains(got, "canary") || !strings.Contains(got, "set:TOKEN") {
		t.Fatalf("EnvVar formatting = %q", got)
	}
	stdio := StdioServer("fs", []string{"server", "--token", "canary-argv"}).WithEnv(v).WithDir("/canary-dir")
	http := HTTPServer("api", "https://example.com/mcp?token=canary-query")
	for _, s := range []Server{stdio, http} {
		got := fmt.Sprintf("%v|%+v|%#v|%s", s, s, s, []Server{s})
		if strings.Contains(got, "canary") {
			t.Fatalf("Server formatting leaked: %q", got)
		}
	}
	if got := fmt.Sprint(stdio); got != "stdio:fs" {
		t.Fatalf("fmt.Sprint(stdio) = %q, want stdio:fs", got)
	}
	if got := fmt.Sprint(http); got != "http:api" {
		t.Fatalf("fmt.Sprint(http) = %q, want http:api", got)
	}
}

// holder keeps its fields unexported, where fmt cannot call Format and
// reflects the raw fields instead.
type holder struct {
	v EnvVar
	s Server
}

func TestEnvValueNotRenderedThroughUnexportedFields(t *testing.T) {
	h := holder{SetEnv("T", "canary-env"), StdioServer("fs", []string{"x"}).WithEnv(SetEnv("T", "canary-env"))}
	if got := fmt.Sprintf("%v|%+v|%#v", h, h, h); strings.Contains(got, "canary-env") {
		t.Fatalf("holder formatting leaked an environment value: %q", got)
	}
}

func TestEnvVarFormatNeverPrintsInvalidNames(t *testing.T) {
	for _, tt := range []struct {
		name string
		v    EnvVar
		want string
	}{
		{"valid set", SetEnv("TOKEN", "x"), "set:TOKEN"},
		{"valid inherit", InheritEnv("TOKEN"), "inherit:TOKEN"},
		{"set name carrying a secret", SetEnv("API_KEY=sk-canary", ""), "set:<invalid>"},
		{"inherit leading digit", InheritEnv("1BAD"), "inherit:<invalid>"},
		{"inherit empty", InheritEnv(""), "inherit:<invalid>"},
		{"zero value", EnvVar{}, "invalid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := fmt.Sprintf("%v", tt.v); got != tt.want {
				t.Fatalf("Sprintf(%%v) = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHostEnvPolicy(t *testing.T) {
	want := "unix-v1"
	if runtime.GOOS == "windows" {
		want = "windows-v1"
	}
	if got := hostEnvPolicy().id; got != want {
		t.Fatalf("hostEnvPolicy().id = %q, want %q on %s", got, want, runtime.GOOS)
	}
}

func TestEnvPolicyIDsAndIdentity(t *testing.T) {
	if unixEnvPolicy.id != "unix-v1" || windowsEnvPolicy.id != "windows-v1" {
		t.Fatalf("policy ids = (%q, %q), want (unix-v1, windows-v1)", unixEnvPolicy.id, windowsEnvPolicy.id)
	}
	got := envIdentity([]EnvVar{SetEnv("B", "x"), InheritEnv("A")}, unixEnvPolicy)
	if want := []string{"inherit:A", "set:B"}; !slices.Equal(got, want) {
		t.Fatalf("envIdentity = %q, want %q", got, want)
	}
	// Names are fingerprinted as the policy matches them: Windows folds case,
	// so Path and PATH are one identity there and two on Unix.
	for _, tt := range []struct {
		policy envPolicy
		name   string
		want   []string
	}{
		{windowsEnvPolicy, "Path", []string{"inherit:PATH"}},
		{windowsEnvPolicy, "PATH", []string{"inherit:PATH"}},
		{unixEnvPolicy, "Path", []string{"inherit:Path"}},
		{unixEnvPolicy, "PATH", []string{"inherit:PATH"}},
	} {
		if got := envIdentity([]EnvVar{InheritEnv(tt.name)}, tt.policy); !slices.Equal(got, tt.want) {
			t.Errorf("%s envIdentity(inherit %s) = %q, want %q", tt.policy.id, tt.name, got, tt.want)
		}
	}
}

func TestWithEnvAndWithDirCopyAndReplace(t *testing.T) {
	vars := []EnvVar{InheritEnv("A")}
	s := StdioServer("fs", []string{"server"}).WithEnv(vars...).WithDir("/w")
	vars[0] = InheritEnv("B")
	if len(s.env) != 1 || s.env[0].name != "A" || s.dir != "/w" {
		t.Fatalf("server env/dir = (%v, %q), want ([inherit:A], /w)", s.env, s.dir)
	}
	r := s.WithEnv(SetEnv("C", "x"))
	if len(r.env) != 1 || r.env[0].name != "C" || s.env[0].name != "A" {
		t.Fatal("WithEnv did not replace on a copy")
	}
}

func TestValidateEnvAdditions(t *testing.T) {
	for _, tt := range []struct {
		name   string
		policy envPolicy
		vars   []EnvVar
		want   string
	}{
		{"valid", unixEnvPolicy, []EnvVar{InheritEnv("TOKEN"), SetEnv("MODE", "")}, ""},
		{"inherit overlaps baseline", unixEnvPolicy, []EnvVar{InheritEnv("PATH")}, ""},
		{"zero value", unixEnvPolicy, []EnvVar{InheritEnv("A"), {}}, "mcpclient: environment entry 2 was not built with InheritEnv or SetEnv"},
		{"leading digit", unixEnvPolicy, []EnvVar{InheritEnv("1BAD")}, "mcpclient: environment entry 1 is not a variable name"},
		{"empty", unixEnvPolicy, []EnvVar{InheritEnv("")}, "mcpclient: environment entry 1 is not a variable name"},
		{"windows drive entry", windowsEnvPolicy, []EnvVar{InheritEnv("=C:")}, "mcpclient: environment entry 1 is not a variable name"},
		{"duplicate", unixEnvPolicy, []EnvVar{InheritEnv("A"), SetEnv("A", "x")}, "mcpclient: environment entry 2 repeats a name"},
		{"case differs on unix", unixEnvPolicy, []EnvVar{InheritEnv("Path"), InheritEnv("PATH")}, ""},
		{"case folds on windows", windowsEnvPolicy, []EnvVar{InheritEnv("Path"), InheritEnv("PATH")}, "mcpclient: environment entry 2 repeats a name"},
		{"set baseline", unixEnvPolicy, []EnvVar{SetEnv("PATH", "/x")}, "mcpclient: environment entry 1 sets a baseline variable"},
		{"set baseline lowercase on unix", unixEnvPolicy, []EnvVar{SetEnv("path", "/x")}, ""},
		{"set baseline folded on windows", windowsEnvPolicy, []EnvVar{SetEnv("Path", "/x")}, "mcpclient: environment entry 1 sets a baseline variable"},
		{"inherit forced on windows", windowsEnvPolicy, []EnvVar{InheritEnv("NoDefaultCurrentDirectoryInExePath")}, "mcpclient: environment entry 1 names a variable the platform policy sets"},
		{"set forced folded on windows", windowsEnvPolicy, []EnvVar{InheritEnv("A"), SetEnv("nodefaultcurrentdirectoryinexepath", "")}, "mcpclient: environment entry 2 names a variable the platform policy sets"},
		{"forced name is ordinary on unix", unixEnvPolicy, []EnvVar{InheritEnv("NoDefaultCurrentDirectoryInExePath")}, ""},
		{"NUL in set value", unixEnvPolicy, []EnvVar{InheritEnv("A"), SetEnv("B", "x\x00y")}, "mcpclient: environment entry 2 value contains NUL"},
		{"newline in set value", unixEnvPolicy, []EnvVar{SetEnv("B", "x\ny")}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEnvAdditions(tt.vars, tt.policy)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tt.want {
				t.Fatalf("validateEnvAdditions = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildServerEnv(t *testing.T) {
	parent := map[string]string{"PATH": "/bin", "HOME": "/h", "CANARY_SECRET": "canary", "TOKEN": "t", "EMPTY": ""}
	lookup := func(name string) (string, bool) { v, ok := parent[name]; return v, ok }
	env, unset := buildServerEnv(unixEnvPolicy, []EnvVar{InheritEnv("TOKEN"), InheritEnv("EMPTY"), SetEnv("MODE", "x"), InheritEnv("MISSING"), InheritEnv("USER")}, lookup)
	if want := []string{"EMPTY=", "HOME=/h", "MODE=x", "PATH=/bin", "TOKEN=t"}; !slices.Equal(env, want) {
		t.Fatalf("env = %q, want %q", env, want)
	}
	if want := []string{"MISSING", "USER"}; !slices.Equal(unset, want) {
		t.Fatalf("unset = %q, want %q (baseline overlap is required)", unset, want)
	}
	empty, unset := buildServerEnv(unixEnvPolicy, nil, func(string) (string, bool) { return "", false })
	if empty == nil || len(empty) != 0 || unset != nil {
		t.Fatalf("empty parent = (%#v, %q), want non-nil empty and no unset", empty, unset)
	}
	folded, _ := buildServerEnv(windowsEnvPolicy, []EnvVar{InheritEnv("Path")}, func(name string) (string, bool) {
		return map[string]string{"PATH": "C:\\bin", "Path": "C:\\bin"}[name], name == "PATH" || name == "Path"
	})
	if want := []string{"NoDefaultCurrentDirectoryInExePath=1", "Path=C:\\bin"}; !slices.Equal(folded, want) {
		t.Fatalf("windows folding = %q, want one Path entry %q", folded, want)
	}
}

func TestBuildServerEnvCompleteBaselines(t *testing.T) {
	// PATH carries an absolute entry so relative-entry filtering keeps it.
	everything := func(path string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			if name == "PATH" {
				return path, true
			}
			return strings.ToLower(name), true
		}
	}
	unix, _ := buildServerEnv(unixEnvPolicy, nil, everything("/path"))
	if want := []string{"HOME=home", "LANG=lang", "PATH=/path", "TMPDIR=tmpdir", "USER=user"}; !slices.Equal(unix, want) {
		t.Fatalf("unix baseline = %q, want %q", unix, want)
	}
	windows, _ := buildServerEnv(windowsEnvPolicy, nil, everything(`C:\path`))
	if want := []string{
		"APPDATA=appdata", "COMSPEC=comspec", "HOME=home", "LANG=lang", "LOCALAPPDATA=localappdata", "NoDefaultCurrentDirectoryInExePath=1", `PATH=C:\path`, "PATHEXT=pathext",
		"SYSTEMROOT=systemroot", "TEMP=temp", "TMP=tmp", "TMPDIR=tmpdir", "USER=user", "USERPROFILE=userprofile",
	}; !slices.Equal(windows, want) {
		t.Fatalf("windows baseline = %q, want %q", windows, want)
	}
}

// Windows programs search the current directory, now the workspace root,
// before PATH unless NoDefaultCurrentDirectoryInExePath is set, so the Windows
// policy always sets it, last, where no addition can replace it. Unix forces
// nothing.
func TestBuildServerEnvForcedEntries(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	zero := func(name string) (string, bool) {
		return "0", strings.EqualFold(name, "NoDefaultCurrentDirectoryInExePath")
	}
	for _, tt := range []struct {
		name   string
		policy envPolicy
		vars   []EnvVar
		lookup func(string) (string, bool)
		want   []string
	}{
		{"windows empty parent", windowsEnvPolicy, nil, none, []string{"NoDefaultCurrentDirectoryInExePath=1"}},
		// Validation rejects these additions; the builder must still win.
		{"windows inherited addition", windowsEnvPolicy, []EnvVar{InheritEnv("NODEFAULTCURRENTDIRECTORYINEXEPATH")}, zero, []string{"NoDefaultCurrentDirectoryInExePath=1"}},
		{"windows set addition", windowsEnvPolicy, []EnvVar{SetEnv("NoDefaultCurrentDirectoryInExePath", "0")}, none, []string{"NoDefaultCurrentDirectoryInExePath=1"}},
		{"unix empty parent", unixEnvPolicy, nil, none, []string{}},
		{"unix inherited name", unixEnvPolicy, []EnvVar{InheritEnv("NoDefaultCurrentDirectoryInExePath")}, zero, []string{"NoDefaultCurrentDirectoryInExePath=0"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env, _ := buildServerEnv(tt.policy, tt.vars, tt.lookup)
			if !slices.Equal(env, tt.want) {
				t.Fatalf("env = %q, want %q", env, tt.want)
			}
		})
	}
	unix, _ := buildServerEnv(unixEnvPolicy, nil, func(name string) (string, bool) { return "/" + name, true })
	for _, kv := range unix {
		if strings.HasPrefix(strings.ToUpper(kv), "NODEFAULTCURRENTDIRECTORYINEXEPATH=") {
			t.Fatalf("unix env %q carries the Windows-only variable", unix)
		}
	}
}

// The child's PATH keeps only absolute entries, in order, under the policy's
// list separator and path rules; with none left PATH is omitted, because an
// empty PATH means the current directory to POSIX lookups.
func TestBuildServerEnvDropsRelativePathEntries(t *testing.T) {
	const forced = "NoDefaultCurrentDirectoryInExePath=1"
	for _, tt := range []struct {
		name   string
		policy envPolicy
		vars   []EnvVar
		key    string // the parent's spelling of PATH
		parent string
		want   []string
	}{
		{"unix baseline", unixEnvPolicy, nil, "PATH", "/usr/bin::./node_modules/.bin:.:/bin:rel", []string{"PATH=/usr/bin:/bin"}},
		{"unix all relative", unixEnvPolicy, nil, "PATH", ".:bin", []string{}},
		{"unix empty", unixEnvPolicy, nil, "PATH", "", []string{}},
		{"unix inherited overlap", unixEnvPolicy, []EnvVar{InheritEnv("PATH")}, "PATH", "/usr/bin::./node_modules/.bin:.:/bin:rel", []string{"PATH=/usr/bin:/bin"}},
		{"unix inherited all relative", unixEnvPolicy, []EnvVar{InheritEnv("PATH")}, "PATH", ".:bin", []string{}},
		{"unix quotes are literal", unixEnvPolicy, nil, "PATH", `"/usr/bin":/bin`, []string{"PATH=/bin"}},
		{"windows baseline", windowsEnvPolicy, nil, "PATH", `C:\bin;;.\x;\rooted;C:rel;\\srv\share\bin;D:/y`, []string{forced, `PATH=C:\bin;\\srv\share\bin;D:/y`}},
		{"windows inherited Path", windowsEnvPolicy, []EnvVar{InheritEnv("Path")}, "Path", `C:\bin;;.\x;\rooted;C:rel;\\srv\share\bin;D:/y`, []string{forced, `Path=C:\bin;\\srv\share\bin;D:/y`}},
		{"windows colon is not a separator", windowsEnvPolicy, nil, "PATH", `C:\a:b;rel`, []string{forced, `PATH=C:\a:b`}},
		{"windows all relative", windowsEnvPolicy, nil, "PATH", `.;bin`, []string{forced}},
		// Quoted entries split and are judged as filepath.SplitList does, and
		// are forwarded with their quotes.
		{"windows quoted entries kept", windowsEnvPolicy, nil, "PATH", `"C:\Program Files\nodejs";C:\Windows\system32;"C:\a;b";C:\bin`, []string{forced, `PATH="C:\Program Files\nodejs";C:\Windows\system32;"C:\a;b";C:\bin`}},
		{"windows quoted relative dropped", windowsEnvPolicy, nil, "PATH", `"rel";C:\x`, []string{forced, `PATH=C:\x`}},
		{"windows inner quotes", windowsEnvPolicy, nil, "PATH", `C:\"Program Files"\x;rel`, []string{forced, `PATH=C:\"Program Files"\x`}},
		// An unbalanced quote runs to the end of the list, as in
		// filepath.SplitList; the whole tail is one entry, judged unquoted.
		{"windows unbalanced quote absolute", windowsEnvPolicy, nil, "PATH", `C:\x;"C:\y;rel`, []string{forced, `PATH=C:\x;"C:\y;rel`}},
		{"windows unbalanced quote relative", windowsEnvPolicy, nil, "PATH", `C:\x;"rel;C:\y`, []string{forced, `PATH=C:\x`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(name string) (string, bool) {
				if tt.policy.key(name) == tt.policy.key(tt.key) {
					return tt.parent, true
				}
				return "", false
			}
			env, unset := buildServerEnv(tt.policy, tt.vars, lookup)
			if !slices.Equal(env, tt.want) || env == nil || unset != nil {
				t.Fatalf("env = %#v (unset %q), want %#v and nothing unset", env, unset, tt.want)
			}
		})
	}
}

func TestEnvPolicyPathRules(t *testing.T) {
	for _, tt := range []struct {
		policy envPolicy
		entry  string
		abs    bool
	}{
		{unixEnvPolicy, "/usr/bin", true},
		{unixEnvPolicy, "", false},
		{unixEnvPolicy, ".", false},
		{unixEnvPolicy, "./node_modules/.bin", false},
		{unixEnvPolicy, "bin", false},
		{unixEnvPolicy, `C:\bin`, false},
		{windowsEnvPolicy, `C:\bin`, true},
		{windowsEnvPolicy, "c:/bin", true},
		{windowsEnvPolicy, `\\srv\share`, true},
		{windowsEnvPolicy, "//srv/share", true},
		{windowsEnvPolicy, `\\\x`, false},
		{windowsEnvPolicy, `\rooted`, false},
		{windowsEnvPolicy, "/rooted", false},
		{windowsEnvPolicy, "C:rel", false},
		{windowsEnvPolicy, "C:", false},
		{windowsEnvPolicy, `1:\x`, false},
		{windowsEnvPolicy, ".", false},
		{windowsEnvPolicy, "", false},
	} {
		if got := tt.policy.isAbs(tt.entry); got != tt.abs {
			t.Errorf("%s isAbs(%q) = %t, want %t", tt.policy.id, tt.entry, got, tt.abs)
		}
	}
	if unixEnvPolicy.listSep != ':' || windowsEnvPolicy.listSep != ';' {
		t.Fatalf("list separators = (%q, %q), want (':', ';')", unixEnvPolicy.listSep, windowsEnvPolicy.listSep)
	}
}

func TestConnectRejectsInvalidLaunchPolicy(t *testing.T) {
	// failingTransport and the never-resolving .invalid TLD keep a mutation
	// that drops validation from launching a PATH program or dialing out.
	stdio := func() Server { return Server{Alias: "fs", tr: &failingTransport{err: errors.New("unreachable")}} }
	for name, s := range map[string]Server{
		"env on http":    HTTPServer("api", "https://mcp.invalid/mcp").WithEnv(InheritEnv("A")),
		"dir on http":    HTTPServer("api", "https://mcp.invalid/mcp").WithDir("/w"),
		"relative dir":   stdio().WithDir("rel"),
		"bad env name":   stdio().WithEnv(InheritEnv("1BAD")),
		"bad endpoint":   HTTPServer("api", "https://mcp.invalid/a/../mcp"),
		"empty command":  StdioServer("fs", nil),
		"empty endpoint": HTTPServer("api", ""),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: testPins(t)})
			var failure *AdmissionError
			if !errors.As(err, &failure) || failure.Reason != "invalid_config" {
				t.Fatalf("Connect(%s) = %v, want fatal invalid_config", name, err)
			}
			if name == "bad env name" {
				// AdmissionError.Error omits its cause, so the position-only
				// diagnostic must be appended explicitly.
				if want := `server "fs": invalid_config: mcpclient: environment entry 1 is not a variable name`; err.Error() != want {
					t.Fatalf("Connect(%s) error = %q, want %q", name, err.Error(), want)
				}
			}
		})
	}
}

func TestConnectAcceptsValidLaunchPolicy(t *testing.T) {
	// The test-only transport is never resolved or launched, so InheritEnv("A")
	// need not be set; only validation is under test. The dial failure shows
	// the server got past validation and preparation.
	s := Server{Alias: "fs", tr: &failingTransport{err: errors.New("unreachable")}}.
		WithEnv(InheritEnv("A"), SetEnv("B", "x")).WithDir(t.TempDir())
	m, warns, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: testPins(t)})
	if err != nil {
		t.Fatalf("Connect error = %v, want nil", err)
	}
	if m != nil {
		t.Cleanup(func() { _ = m.Close() })
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want one", warns)
	}
	var failure *AdmissionError
	if !errors.As(warns[0], &failure) || failure.Reason != "unavailable" {
		t.Fatalf("warning = %v, want per-alias unavailable", warns[0])
	}
}
