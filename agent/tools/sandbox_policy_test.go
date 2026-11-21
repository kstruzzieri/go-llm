package tools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestValidateSandboxEnv pins the pure #722 checks: the closed Env and TempEnv
// tables and the spelling of declared roots. A refusal never echoes the value
// it refused.
func TestValidateSandboxEnv(t *testing.T) {
	valid := func() SandboxConfig {
		return SandboxConfig{
			Runtime:       SandboxRuntimeSeatbelt,
			ReadOnlyRoots: []string{"/opt/x"},
			Env: []SandboxEnv{
				{Name: "GOMODCACHE", Value: "/opt/x/mod"},
				{Name: "GOPROXY", Value: "off"},
				{Name: "GOTOOLCHAIN", Value: "local"},
				{Name: "GOFLAGS", Value: "-buildvcs=false"},
			},
			TempEnv: []string{"GOCACHE"},
		}
	}
	if err := validateToolchainFields(valid()); err != nil {
		t.Fatalf("valid control refused: %v", err)
	}
	equal := valid()
	equal.Env[0].Value = "/opt/x"
	devDir := valid()
	devDir.DeveloperDirectory = "/Library/Developer/CommandLineTools"
	devDir.Env[0].Value = "/Library/Developer/CommandLineTools/mod"
	for name, cfg := range map[string]SandboxConfig{"GOMODCACHE equal to a root": equal, "GOMODCACHE in the developer directory": devDir} {
		if err := validateToolchainFields(cfg); err != nil {
			t.Fatalf("%s refused: %v", name, err)
		}
	}

	tests := []struct {
		name    string
		edit    func(*SandboxConfig)
		reason  string
		refused []string // must not appear in the error
	}{
		{"unknown name", func(c *SandboxConfig) { c.Env[1] = SandboxEnv{Name: "LD_PRELOAD", Value: "/tmp/evil.so"} },
			"outside the allowed table", []string{"/tmp/evil.so", "LD_PRELOAD"}},
		{"GOPROXY value", func(c *SandboxConfig) { c.Env[1].Value = "https://proxy.golang.org" },
			`GOPROXY accepts only "off"`, []string{"https://proxy.golang.org"}},
		{"GOFLAGS value", func(c *SandboxConfig) { c.Env[3].Value = "-toolexec=/bin/x" },
			`GOFLAGS accepts only "-buildvcs=false"`, []string{"-toolexec=/bin/x"}},
		{"GOMODCACHE outside every root", func(c *SandboxConfig) { c.Env[0].Value = "/opt/xy/mod" },
			"inside a declared root", []string{"/opt/xy/mod"}},
		{"GOMODCACHE not clean", func(c *SandboxConfig) { c.Env[0].Value = "/opt/x/../y" },
			"inside a declared root", []string{"/opt/x/../y"}},
		{"GOMODCACHE control character", func(c *SandboxConfig) { c.Env[0].Value = "/opt/x/m\nod" },
			"inside a declared root", []string{"m\nod"}},
		{"duplicate name", func(c *SandboxConfig) { c.Env = append(c.Env, SandboxEnv{Name: "GOPROXY", Value: "off"}) },
			"Env[4] repeats", nil},
		{"unknown temp name", func(c *SandboxConfig) { c.TempEnv = []string{"XDG_CACHE_HOME"} },
			"TempEnv[0] names a variable outside the allowed table", []string{"XDG_CACHE_HOME"}},
		{"duplicate temp name", func(c *SandboxConfig) { c.TempEnv = []string{"GOCACHE", "GOCACHE"} },
			"TempEnv[1] repeats", nil},
		{"root newline", func(c *SandboxConfig) { c.ReadOnlyRoots = []string{"/opt/x", "/opt/n\nl"} },
			"ReadOnlyRoots[1]", []string{"n\nl"}},
		{"root DEL", func(c *SandboxConfig) { c.ReadOnlyRoots = []string{"/opt/d\x7fel"} },
			"ReadOnlyRoots[0]", []string{"d\x7fel"}},
		{"root invalid UTF-8", func(c *SandboxConfig) { c.ReadOnlyRoots = []string{"/opt/\xffbad"} },
			"ReadOnlyRoots[0]", []string{"\xffbad"}},
		{"root relative", func(c *SandboxConfig) { c.ReadOnlyRoots = []string{"opt/rel"} },
			"ReadOnlyRoots[0]", []string{"opt/rel"}},
		{"root not clean", func(c *SandboxConfig) { c.ReadOnlyRoots = []string{"/opt/x/"} },
			"ReadOnlyRoots[0]", []string{"/opt/x/"}},
		{"root empty", func(c *SandboxConfig) { c.ReadOnlyRoots = []string{""} },
			"ReadOnlyRoots[0]", nil},
		{"developer directory relative", func(c *SandboxConfig) { c.DeveloperDirectory = "Developer/rel" },
			"DeveloperDirectory", []string{"Developer/rel"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.edit(&cfg)
			err := validateToolchainFields(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("validateToolchainFields() = %v, want reason %q", err, tt.reason)
			}
			for _, refused := range tt.refused {
				if strings.Contains(err.Error(), refused) {
					t.Fatalf("error echoes the refused %q: %v", refused, err)
				}
			}
			// The same refusal reaches every public path through normalization.
			if _, err := normalizeSandboxConfig(cfg); err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("normalizeSandboxConfig() = %v, want reason %q", err, tt.reason)
			}
		})
	}

	// Host accepts none of the new fields, each on its own and with valid
	// values, so the host check is the only reason. ValidateSandbox refuses
	// before any probe and wraps ErrSandboxPolicy.
	hostRows := map[string]SandboxConfig{
		"ReadOnlyRoots":      {ReadOnlyRoots: []string{"/opt/x"}},
		"DeveloperDirectory": {DeveloperDirectory: "/Library/Developer/CommandLineTools"},
		"Env":                {Env: []SandboxEnv{{Name: "GOPROXY", Value: "off"}}},
		"TempEnv":            {Runtime: SandboxRuntimeHost, TempEnv: []string{"GOCACHE"}},
	}
	for name, cfg := range hostRows {
		t.Run("host "+name, func(t *testing.T) {
			err := ValidateSandbox(cfg, t.TempDir())
			if !errors.Is(err, ErrSandboxPolicy) || !strings.Contains(err.Error(), "host runtime accepts no constraints") {
				t.Fatalf("ValidateSandbox(host + %s) = %v, want host refusal wrapping ErrSandboxPolicy", name, err)
			}
		})
	}
}

// mkdirT creates dir and its parents, failing the test on error.
func mkdirT(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// standInHome points HOME at a fresh canonical directory and clears
// XDG_DATA_HOME, so construction-time admission never looks under the real
// home or Golem's real data directory. It returns the stand-in home.
func standInHome(t *testing.T) string {
	t.Helper()
	_, base := canonicalTempDir(t)
	home := mkdirT(t, filepath.Join(base, "home"))
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	return home
}

// TestAdmitRoots pins admission of declared roots (#722 D7) against a
// stand-in host: a home, a temp base and an environment that names only
// them. The excluded stores are listed here, not read from the code under
// test.
func TestAdmitRoots(t *testing.T) {
	_, homeParent := canonicalTempDir(t)
	_, tempParent := canonicalTempDir(t)
	_, scratch := canonicalTempDir(t)
	home := mkdirT(t, filepath.Join(homeParent, "home"))
	tempBase := mkdirT(t, filepath.Join(tempParent, "tmp"))
	xdg := mkdirT(t, filepath.Join(scratch, "xdg"))
	env := map[string]string{"HOME": home}
	getenv := func(k string) string { return env[k] }
	// with returns getenv with one more variable set.
	with := func(name, value string) func(string) string {
		return func(k string) string {
			if k == name {
				return value
			}
			return env[k]
		}
	}
	// go-llm's config store sits where os.UserConfigDir puts it.
	configStore := ".config/go-llm"
	if runtime.GOOS == "darwin" {
		configStore = "Library/Application Support/go-llm"
	}

	target := mkdirT(t, filepath.Join(scratch, "target"))
	link := filepath.Join(scratch, "link")
	dangling := filepath.Join(scratch, "dangling")
	file := filepath.Join(scratch, "file")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(scratch, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	type row struct {
		name, root, reason string
		getenv             func(string) string
	}
	rows := []row{
		{"volume root", "/", "broad", nil},
		{"System", "/System", "broad", nil},
		{"System Volumes", "/System/Volumes", "broad", nil},
		{"data firmlink child", "/System/Volumes/Data/x", "broad", nil},
		{"symlink", link, "canonical spelling " + `"` + target + `"`, nil},
		{"dangling link", dangling, "cannot be resolved", nil},
		{"regular file", file, "not a directory", nil},
		{"home", home, "home directory", nil},
		{"covering home", homeParent, "home directory", nil},
		{"temp base", tempBase, "temp base", nil},
		{"covering temp base", tempParent, "temp base", nil},
		{"seatbelt private temp", mkdirT(t, filepath.Join(tempBase, "go-llm-seatbelt-x")), "runtime temp", nil},
		{"scratch reference", mkdirT(t, filepath.Join(tempBase, "go-llm-scratch-ref-x")), "runtime temp", nil},
		{"data directory unknown", target, "data directory", func(string) string { return "" }},
	}
	excluded := []string{
		".ssh", ".gnupg", ".aws", ".docker", ".kube", ".netrc", ".config", ".claude", ".codex",
		"Library/Keychains", ".local/share/golem", ".local/share/go-llm", configStore,
	}
	for _, rel := range excluded {
		e := mkdirT(t, filepath.Join(home, rel))
		rows = append(rows,
			row{"equal " + rel, e, "excluded", nil},
			row{"inside " + rel, mkdirT(t, filepath.Join(e, "x")), "excluded", nil})
		if parent := filepath.Dir(e); parent != home {
			rows = append(rows, row{"covering " + rel, parent, "excluded", nil})
		}
	}
	if os.Geteuid() != 0 { // root lists any directory, so nothing is unverified
		locked := mkdirT(t, filepath.Join(scratch, "locked"))
		child := mkdirT(t, filepath.Join(locked, "child"))
		if err := os.Chmod(locked, 0o311); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		rows = append(rows, row{"unverifiable spelling", child, "cannot be verified", nil})
		// An unlistable store turns "inside .ssh" into an ordering check: it is
		// refused as excluded, never reported unverifiable, so admission did not
		// try to list the store.
		ssh := filepath.Join(home, ".ssh")
		if err := os.Chmod(ssh, 0o311); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ssh, 0o700) })
	}
	// The control runs with every excluded store present, as on a real host.
	admitted := []string{
		mkdirT(t, filepath.Join(tempBase, "go-llm-a0-fixture", "modcache")),
		mkdirT(t, filepath.Join(home, "go", "pkg", "mod")),
	}
	if err := admitRoots(admitted, home, tempBase, getenv); err != nil {
		t.Fatalf("valid control refused: %v", err)
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			ge := getenv
			if r.getenv != nil {
				ge = r.getenv
			}
			err := admitRoots([]string{r.root}, home, tempBase, ge)
			if err == nil || !strings.Contains(err.Error(), r.reason) {
				t.Fatalf("admitRoots(%q) = %v, want reason %q", r.root, err, r.reason)
			}
		})
	}

	// go-llm's own stores move with XDG_DATA_HOME; the default stores stay
	// excluded, since old sessions and the signing key may remain there.
	t.Run("relocated XDG_DATA_HOME", func(t *testing.T) {
		relocated := with("XDG_DATA_HOME", xdg)
		refused := []string{
			mkdirT(t, filepath.Join(xdg, "golem")),
			mkdirT(t, filepath.Join(xdg, "go-llm", "x")),
			xdg,
			filepath.Join(home, ".local", "share", "golem"),
			filepath.Join(home, ".local", "share"),
		}
		control := mkdirT(t, filepath.Join(xdg, "other"))
		if err := admitRoots([]string{control}, home, tempBase, relocated); err != nil {
			t.Fatalf("control refused: %v", err)
		}
		for _, root := range refused {
			if err := admitRoots([]string{root}, home, tempBase, relocated); err == nil || !strings.Contains(err.Error(), "excluded") {
				t.Errorf("admitRoots(%q) = %v, want an excluded-store refusal", root, err)
			}
		}
	})

	// The config store follows XDG_CONFIG_HOME where os.UserConfigDir does:
	// everywhere but darwin.
	t.Run("relocated XDG_CONFIG_HOME", func(t *testing.T) {
		xcfg := mkdirT(t, filepath.Join(scratch, "xcfg"))
		relocated := with("XDG_CONFIG_HOME", xcfg)
		store := mkdirT(t, filepath.Join(xcfg, "go-llm"))
		control := mkdirT(t, filepath.Join(xcfg, "other"))
		if err := admitRoots([]string{control}, home, tempBase, relocated); err != nil {
			t.Fatalf("control refused: %v", err)
		}
		for _, root := range []string{store, xcfg} {
			err := admitRoots([]string{root}, home, tempBase, relocated)
			if runtime.GOOS == "darwin" {
				if err != nil {
					t.Errorf("admitRoots(%q) = %v; darwin's config dir ignores XDG_CONFIG_HOME", root, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "excluded") {
				t.Errorf("admitRoots(%q) = %v, want an excluded-store refusal", root, err)
			}
		}
	})

	// A store that does not exist yet is still excluded: a root covering
	// where it will be created is refused.
	t.Run("store not created yet", func(t *testing.T) {
		_, p := canonicalTempDir(t)
		h := mkdirT(t, filepath.Join(p, "home"))
		envNew := map[string]string{"HOME": h}
		parent := filepath.Join(h, "Library", "Application Support")
		if runtime.GOOS != "darwin" {
			parent = filepath.Join(p, "xdg-config")
			envNew["XDG_CONFIG_HOME"] = parent
		}
		mkdirT(t, parent)
		store := filepath.Join(parent, "go-llm")
		if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("store %q must not exist: %v", store, err)
		}
		err := admitRoots([]string{parent}, h, tempBase, func(k string) string { return envNew[k] })
		if err == nil || !strings.Contains(err.Error(), `excluded "`+store+`"`) {
			t.Fatalf("admitRoots(%q) = %v, want refusal naming the excluded %q", parent, err, store)
		}
	})
}

// TestCanonicalToolchainRoot pins the host's one canonicalization: a symlink
// resolves to its target's spelling, which then passes admission, and a
// missing path is refused.
func TestCanonicalToolchainRoot(t *testing.T) {
	_, base := canonicalTempDir(t)
	home := mkdirT(t, filepath.Join(base, "home"))
	tempBase := mkdirT(t, filepath.Join(base, "tmp"))
	target := mkdirT(t, filepath.Join(base, "go", "libexec"))
	link := filepath.Join(base, "golink")
	if err := os.Symlink(filepath.Join(base, "go"), link); err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalToolchainRoot(filepath.Join(link, "libexec"))
	if err != nil || got != target {
		t.Fatalf("CanonicalToolchainRoot(link) = %q, %v; want %q", got, err, target)
	}
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	if err := admitRoots([]string{got}, home, tempBase, getenv); err != nil {
		t.Fatalf("canonical spelling refused: %v", err)
	}
	if _, err := CanonicalToolchainRoot(filepath.Join(base, "missing")); err == nil {
		t.Fatal("missing root accepted")
	}
}

// TestSandboxRootWorkspaceOverlap pins validateWorkspace's overlap check: a
// declared root, the developer directory included, may not be, cover or lie
// inside the workspace.
func TestSandboxRootWorkspaceOverlap(t *testing.T) {
	_, base := canonicalTempDir(t)
	root := mkdirT(t, filepath.Join(base, "root"))
	ws := mkdirT(t, filepath.Join(base, "ws"))
	approval := func(cfg SandboxConfig) sandboxApproval {
		cfg.Runtime = SandboxRuntimeSeatbelt
		return approvalForSandbox(mustNormalizeSandbox(t, cfg))
	}
	if err := approval(SandboxConfig{ReadOnlyRoots: []string{root}, DeveloperDirectory: mkdirT(t, filepath.Join(base, "dev"))}).validateWorkspace(ws); err != nil {
		t.Fatalf("valid control refused: %v", err)
	}
	rows := map[string]struct {
		cfg SandboxConfig
		ws  string
	}{
		"workspace is the root":                      {SandboxConfig{ReadOnlyRoots: []string{root}}, root},
		"workspace inside the root":                  {SandboxConfig{ReadOnlyRoots: []string{root}}, mkdirT(t, filepath.Join(root, "ws"))},
		"root inside the workspace":                  {SandboxConfig{ReadOnlyRoots: []string{mkdirT(t, filepath.Join(ws, "tools"))}}, ws},
		"developer directory covering the workspace": {SandboxConfig{DeveloperDirectory: base}, ws},
	}
	for name, r := range rows {
		t.Run(name, func(t *testing.T) {
			if err := approval(r.cfg).validateWorkspace(r.ws); err == nil || !strings.Contains(err.Error(), "overlaps") {
				t.Fatalf("validateWorkspace(%q) = %v, want an overlap refusal", r.ws, err)
			}
		})
	}
}
