package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSBPLQuotePreservesBytesAndEscapes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "/private/tmp/ws", `"/private/tmp/ws"`},
		{"quote and backslash", `/tmp/we"ird\dir (1)`, `"/tmp/we\"ird\\dir (1)"`},
		{"non-ascii preserved", "/tmp/wörk/über", `"/tmp/wörk/über"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sbplQuote(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("sbplQuote(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestSBPLQuoteRejectsHostileBytes(t *testing.T) {
	tests := map[string]string{
		"newline":      "/tmp/a\nb",
		"nul":          "/tmp/a\x00b",
		"esc":          "/tmp/a\x1bb",
		"del":          "/tmp/a\x7fb",
		"carriage":     "/tmp/a\rb",
		"c1 nel":       "/tmp/a\u0085b",
		"invalid utf8": "/tmp/a\xffb",
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := sbplQuote(path); err == nil {
				t.Fatalf("sbplQuote(%q) accepted hostile bytes", path)
			}
		})
	}
}

func TestSeatbeltMetadataAncestors(t *testing.T) {
	got, err := seatbeltMetadataAncestors([]string{
		"/private/tmp/ws", "/usr/lib", "/private/tmp/pt", "/bin/echo",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/", "/bin", "/private", "/private/tmp", "/usr"}
	if len(got) != len(want) {
		t.Fatalf("ancestors = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ancestors = %q, want %q", got, want)
		}
	}
}

func TestSeatbeltMetadataAncestorsRejectsRelative(t *testing.T) {
	if _, err := seatbeltMetadataAncestors([]string{"relative/path"}); err == nil {
		t.Fatal("relative path accepted")
	}
}

func basePolicy() seatbeltPolicy {
	return seatbeltPolicy{
		workspaceRoot:     "/private/tmp/ws",
		tempRoot:          "/private/tmp/pt",
		exePath:           "/bin/echo",
		systemReadRoots:   []string{"/usr/lib", "/bin"},
		metadataAncestors: []string{"/", "/usr", "/private", "/private/tmp"},
	}
}

func mustProfile(t *testing.T, p seatbeltPolicy) string {
	t.Helper()
	prof, err := buildSeatbeltProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	return prof
}

// TestSeatbeltProfileGolden pins the ENTIRE rendered profile byte-for-byte.
// The profile is the enforced security policy; any change to the template must
// consciously rewrite this expectation.
func TestSeatbeltProfileGolden(t *testing.T) {
	want := `(version 1)
(deny default)
(allow process-exec*)
(allow process-fork)
(allow file-read*
  (subpath "/bin")
  (subpath "/usr/lib")
  (subpath "/private/tmp/ws")
  (subpath "/private/tmp/pt")
  (literal "/bin/echo")
  (literal "/dev/null")
  (literal "/dev/random")
  (literal "/dev/urandom"))
(allow file-read-metadata
  (literal "/")
  (literal "/private")
  (literal "/private/tmp")
  (literal "/usr"))
(allow file-read-data (literal "/"))
(allow sysctl-read (sysctl-name-prefix "hw."))
(allow file-write*
  (subpath "/private/tmp/ws")
  (subpath "/private/tmp/pt"))
(allow file-write-data (literal "/dev/null"))
`
	if got := mustProfile(t, basePolicy()); got != want {
		t.Fatalf("profile mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestSeatbeltProfileNetworkGate(t *testing.T) {
	denied := mustProfile(t, basePolicy())
	if strings.Contains(denied, "network") {
		t.Fatalf("default profile must not mention network:\n%s", denied)
	}
	p := basePolicy()
	p.allowNetwork = true
	allowed := mustProfile(t, p)
	if !strings.HasSuffix(allowed, "(allow network*)\n") {
		t.Fatalf("allowNetwork profile missing trailing network allowance:\n%s", allowed)
	}
	if strings.TrimSuffix(allowed, "(allow network*)\n") != denied {
		t.Fatal("network gate must be the only difference between the two profiles")
	}
}

// TestSeatbeltProfileScopesMetadata proves metadata permission is never
// emitted without filters: the unfiltered forms would grant global stat.
func TestSeatbeltProfileScopesMetadata(t *testing.T) {
	prof := mustProfile(t, basePolicy())
	for _, forbidden := range []string{
		"(allow file-read-metadata)",
		"(allow file-read-metadata\n)",
		"(allow sysctl-read)",
	} {
		if strings.Contains(prof, forbidden) {
			t.Fatalf("profile contains unfiltered metadata allowance:\n%s", prof)
		}
	}
	if !strings.Contains(prof, "(allow file-read-metadata\n  (literal \"/\")") {
		t.Fatalf("profile missing filtered metadata block:\n%s", prof)
	}
}

func TestSeatbeltProfileRequiredFields(t *testing.T) {
	tests := map[string]func(*seatbeltPolicy){
		"workspace": func(p *seatbeltPolicy) { p.workspaceRoot = "" },
		"temp":      func(p *seatbeltPolicy) { p.tempRoot = "" },
		"exe":       func(p *seatbeltPolicy) { p.exePath = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := basePolicy()
			mutate(&p)
			if _, err := buildSeatbeltProfile(p); err == nil {
				t.Fatalf("profile built without required %s field", name)
			}
		})
	}
}

// TestSeatbeltProfileRejectsBroadRoots pins the D1 fail-closed rule: the
// volume root and the Data-volume alias (which reaches user data) can never
// be rendered into any allowance position.
func TestSeatbeltProfileRejectsBroadRoots(t *testing.T) {
	tests := map[string]func(*seatbeltPolicy){
		"slash system root":     func(p *seatbeltPolicy) { p.systemReadRoots = []string{"/"} },
		"system":                func(p *seatbeltPolicy) { p.systemReadRoots = []string{"/System"} },
		"data volume":           func(p *seatbeltPolicy) { p.systemReadRoots = []string{"/System/Volumes/Data"} },
		"data descendant":       func(p *seatbeltPolicy) { p.systemReadRoots = []string{"/System/Volumes/Data/Users"} },
		"slash workspace":       func(p *seatbeltPolicy) { p.workspaceRoot = "/" },
		"data alias workspace":  func(p *seatbeltPolicy) { p.workspaceRoot = "/System/Volumes/Data/private/tmp/ws" },
		"slash temp":            func(p *seatbeltPolicy) { p.tempRoot = "/" },
		"data alias temp":       func(p *seatbeltPolicy) { p.tempRoot = "/System/Volumes/Data/tmp/x" },
		"data alias executable": func(p *seatbeltPolicy) { p.exePath = "/System/Volumes/Data/Users/x/tool" },
		"relative system root":  func(p *seatbeltPolicy) { p.systemReadRoots = []string{"usr/lib"} },
		"relative workspace":    func(p *seatbeltPolicy) { p.workspaceRoot = "private/tmp/ws" },
		// Un-normalized spellings must be rejected outright: the kernel
		// normalizes paths before matching, so a dot-dot spelling could evade
		// the broad-root string check while enforcing something else entirely.
		"dot-dot data evasion": func(p *seatbeltPolicy) {
			p.systemReadRoots = []string{"/System/Volumes/../Volumes/Data/Users"}
		},
		"dot-dot to volume root": func(p *seatbeltPolicy) { p.workspaceRoot = "/private/tmp/ws/../../.." },
		"trailing slash":         func(p *seatbeltPolicy) { p.tempRoot = "/private/tmp/pt/" },
		"trailing dot":           func(p *seatbeltPolicy) { p.workspaceRoot = "/private/tmp/ws/." },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := basePolicy()
			mutate(&p)
			if _, err := buildSeatbeltProfile(p); err == nil {
				t.Fatalf("%s rendered into the profile", name)
			}
		})
	}
}

func TestSeatbeltProfileRejectsHostilePathBytes(t *testing.T) {
	p := basePolicy()
	p.workspaceRoot = "/tmp/ws\n(allow default)"
	if _, err := buildSeatbeltProfile(p); err == nil {
		t.Fatal("control character in workspace path must fail profile generation")
	}
}

func TestSeatbeltProfileDeterministicOverInputOrder(t *testing.T) {
	a := basePolicy()
	a.systemReadRoots = []string{"/usr/lib", "/bin", "/bin"}
	a.metadataAncestors = []string{"/private/tmp", "/", "/usr", "/private", "/usr"}
	b := basePolicy()
	if mustProfile(t, a) != mustProfile(t, b) {
		t.Fatal("input order/duplication changed the rendered profile")
	}
}

func TestSeatbeltConfigSupport(t *testing.T) {
	for name, cfg := range map[string]SandboxConfig{
		"memory": {Runtime: SandboxRuntimeSeatbelt, MemoryCapMB: 512},
		"cpu":    {Runtime: SandboxRuntimeSeatbelt, CPULimit: 1.5},
		"caps":   {Runtime: SandboxRuntimeSeatbelt, DropCaps: []string{"NET_RAW"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := seatbeltConfigSupport(cfg)
			if err == nil || !strings.Contains(err.Error(), "seatbelt") {
				t.Fatalf("unenforceable field accepted: %v", err)
			}
		})
	}
	if err := seatbeltConfigSupport(SandboxConfig{
		Runtime: SandboxRuntimeSeatbelt, AllowNetwork: true,
	}); err != nil {
		t.Fatalf("AllowNetwork must be supported: %v", err)
	}
}

func TestSeatbeltChildEnvReplacesTMPDIR(t *testing.T) {
	env := seatbeltChildEnv(
		[]string{"PATH=/usr/bin", "TMPDIR=/", "HOME=/Users/x", "TMPDIR=/etc"},
		"/private/tmp/pt1")
	var tmpdirs, rest []string
	for _, e := range env {
		if strings.HasPrefix(e, "TMPDIR=") {
			tmpdirs = append(tmpdirs, e)
		} else {
			rest = append(rest, e)
		}
	}
	if len(tmpdirs) != 1 || tmpdirs[0] != "TMPDIR=/private/tmp/pt1" {
		t.Fatalf("TMPDIR entries = %q, want exactly the private directory", tmpdirs)
	}
	if len(rest) != 2 || rest[0] != "PATH=/usr/bin" || rest[1] != "HOME=/Users/x" {
		t.Fatalf("non-TMPDIR entries altered: %q", rest)
	}
}

func TestSeatbeltChildEnvAddsTMPDIRWhenAbsent(t *testing.T) {
	env := seatbeltChildEnv([]string{"PATH=/usr/bin"}, "/private/tmp/pt2")
	if len(env) != 2 || env[1] != "TMPDIR=/private/tmp/pt2" {
		t.Fatalf("env = %q, want appended private TMPDIR", env)
	}
}

func TestSeatbeltCollectSystemRootsCanonicalizesAndOmitsMissing(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	canonReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	got, err := seatbeltCollectSystemRoots(
		[]string{link, filepath.Join(base, "missing")}, "/w", "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != canonReal {
		t.Fatalf("roots = %q, want [%q] (canonicalized, missing omitted)", got, canonReal)
	}
}

// TestSeatbeltCollectSystemRootsFailsClosed pins D1's collector rule: a fixed
// root that canonicalizes to a broad root or to a parent of the workspace or
// home errors out — it never silently enters or leaves the profile.
func TestSeatbeltCollectSystemRootsFailsClosed(t *testing.T) {
	base := t.TempDir()
	toRoot := filepath.Join(base, "toroot")
	if err := os.Symlink("/", toRoot); err != nil {
		t.Fatal(err)
	}
	canonBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		roots []string
		ws    string
		home  string
	}{
		"canonicalizes to volume root": {[]string{toRoot}, "/w", "/home/u"},
		"parent of workspace":          {[]string{canonBase}, filepath.Join(canonBase, "ws"), "/home/u"},
		"equals workspace":             {[]string{canonBase}, canonBase, "/home/u"},
		"parent of home":               {[]string{canonBase}, "/w", filepath.Join(canonBase, "me")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := seatbeltCollectSystemRoots(tt.roots, tt.ws, tt.home); err == nil {
				t.Fatalf("%s accepted into the profile", name)
			}
		})
	}
}

// TestRenderSandboxLineSeatbeltTempPrivate pins the seatbelt preview: the user
// approves the private-temp policy as part of the sandbox line. Other runtimes
// must not gain the marker (see TestRenderSandboxLine's container pin).
func TestRenderSandboxLineSeatbeltTempPrivate(t *testing.T) {
	cfg := mustNormalizeSandbox(t, SandboxConfig{Runtime: SandboxRuntimeSeatbelt})
	got := renderSandboxLine(cfg)
	want := `runtime="seatbelt" network=denied memory_cap=none cpu_limit=none drop_caps=[] temp=private`
	if got != want {
		t.Fatalf("renderSandboxLine() = %q, want %q", got, want)
	}
}
