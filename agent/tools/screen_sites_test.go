package tools

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// testScreen is the screen a hand-built test spec carries: no keys, nothing
// withheld, so output passes through as the command wrote it.
func testScreen() *outputScreen { return newOutputScreen(nil) }

// TestOnlyProbesCaptureWithoutWorkspaceScreen pins, over the package's
// non-test source, the only places exec output is captured without a screen
// built from the workspace: the Seatbelt probe's keyless screen, the unix
// runner's two capture buffers behind the screen, the bwrap probe's buffer,
// and the background manager's rings (screened by Task 8).
func TestOnlyProbesCaptureWithoutWorkspaceScreen(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	type site struct {
		call, def string
		want      map[string]int // file -> count; 0 means at least one
	}
	sites := []site{
		{"probeScreen(", "func probeScreen(", map[string]int{"exec_darwin.go": 1}},
		{"&cappedBuffer{", "", map[string]int{"exec_unix.go": 2, "exec_linux.go": 1}},
		{"newTailRing(", "func newTailRing(", map[string]int{"background_manager.go": 0}},
	}
	got := make([]map[string]int, len(sites))
	for i := range got {
		got[i] = map[string]int{}
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		for i, s := range sites {
			n := strings.Count(text, s.call)
			if s.def != "" {
				n -= strings.Count(text, s.def)
			}
			if n > 0 {
				got[i][name] = n
			}
		}
	}
	for i, s := range sites {
		for file, n := range got[i] {
			want, ok := s.want[file]
			if !ok {
				t.Errorf("site %d: %s has %d unexpected call(s)", i, file, n)
			} else if want != 0 && n != want {
				t.Errorf("site %d: %s has %d call(s), want %d", i, file, n, want)
			}
		}
		for file := range s.want {
			if got[i][file] == 0 {
				t.Errorf("site %d: %s has no call", i, file)
			}
		}
	}
}

// captureForm is one way source text can capture, redirect or start a child
// process's output. call names a function whose definition ("func " + call)
// is not a call site; it is empty for forms that are not function calls.
type captureForm struct {
	label string
	re    *regexp.Regexp
	def   string
}

var captureForms = []captureForm{
	{"assign .Stdout", regexp.MustCompile(`\.Stdout\s*(?:=|,[^=\n]*=)[^=]`), ""},
	{"assign .Stderr", regexp.MustCompile(`\.Stderr\s*(?:=|,[^=\n]*=)[^=]`), ""},
	{"keyed Stdout:", regexp.MustCompile(`\bStdout:`), ""},
	{"keyed Stderr:", regexp.MustCompile(`\bStderr:`), ""},
	{".Output()", regexp.MustCompile(`\.Output\(\)`), ""},
	{"CombinedOutput(", regexp.MustCompile(`\bCombinedOutput\(`), ""},
	{"StdoutPipe(", regexp.MustCompile(`\bStdoutPipe\(`), ""},
	{"StderrPipe(", regexp.MustCompile(`\bStderrPipe\(`), ""},
	{"cappedBuffer literal", regexp.MustCompile(`\bcappedBuffer\{`), ""},
	{"new(cappedBuffer)", regexp.MustCompile(`\bnew\(cappedBuffer\)`), ""},
	{"os.Pipe(", regexp.MustCompile(`\bos\.Pipe\(`), ""},
	{"exec.Command", regexp.MustCompile(`\bexec\.Command(?:Context)?\(`), ""},
	{"os.StartProcess(", regexp.MustCompile(`\bos\.StartProcess\(`), ""},
	{"ForkExec(", regexp.MustCompile(`\bForkExec\(`), ""},
	{"spawn(", regexp.MustCompile(`\bspawn\(`), "func spawn("},
	{"osExecSpawn(", regexp.MustCompile(`\bosExecSpawn\(`), "func osExecSpawn("},
	{"nativeSpawn(", regexp.MustCompile(`\bnativeSpawn\(`), "func nativeSpawn("},
	{"newNativeCmd(", regexp.MustCompile(`\bnewNativeCmd\(`), "func newNativeCmd("},
	{"posixSpawn(", regexp.MustCompile(`\bposixSpawn\(`), "func posixSpawn("},
	{"posixSpawnFileActionsAdddup2(", regexp.MustCompile(`\bposixSpawnFileActionsAdddup2\(`), "func posixSpawnFileActionsAdddup2("},
}

// captureSites is every non-test file with a capture form, with the exact
// count of each form and where that output goes. Every other file must have
// none.
var captureSites = map[string]struct {
	counts map[string]int
	reason string
}{
	"exec_unix.go": {map[string]int{
		"assign .Stdout": 1, "assign .Stderr": 1, "keyed Stdout:": 1, "keyed Stderr:": 1,
		"cappedBuffer literal": 2, "exec.Command": 2, "spawn(": 1,
	}, "osExecSpawn wires the writers it is given; unixRunner.Run hands it screen streams over its two cappedBuffers and copies them into execResult"},
	"exec_linux.go": {map[string]int{
		"assign .Stdout": 1, "assign .Stderr": 1, "cappedBuffer literal": 1, "exec.Command": 1,
	}, "probeBwrap: a probe with fixed argv; its output goes only into the probe's own error"},
	"background_unix.go": {map[string]int{"spawn(": 1},
		"unixStarter.Start hands the background manager's rings to the spawner; Task 8 screens them"},
	"spawn_darwin.go": {map[string]int{
		"assign .Stdout": 1, "assign .Stderr": 1, "os.Pipe(": 1, "newNativeCmd(": 1,
		"posixSpawn(": 1, "posixSpawnFileActionsAdddup2(": 2,
	}, "nativeSpawn and nativeCmd: the posix_spawn twin of osExecSpawn, wiring only the writers the runner or starter hands it"},
	"verify.go": {map[string]int{"keyed Stdout:": 1, "keyed Stderr:": 1},
		"VerifyResult copies the runner's already-screened execResult"},
}

// stripCommentsAndStrings blanks comments and the contents of string and
// rune literals, so a form named in a comment or a message is not counted.
// Newlines are kept so a line comment ends where it did.
func stripCommentsAndStrings(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			b.WriteString(strings.Repeat("\n", strings.Count(src[i:i+2+end+2], "\n")))
			i += 2 + end + 1
		case c == '"' || c == '\'' || c == '`':
			b.WriteByte(c)
			for i++; i < len(src) && src[i] != c; i++ {
				if src[i] == '\\' && c != '`' {
					i++
				} else if src[i] == '\n' {
					b.WriteByte('\n')
				}
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// TestCaptureSitesAreKnown counts, in every non-test file of the package
// whatever its build tag, each form that captures, redirects or starts a
// child's output, and requires the exact counts in captureSites.
func TestCaptureSitesAreKnown(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	const fix = "a new capture site needs a screen from recheckExecPlan or probeScreen, then update this table"
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := stripCommentsAndStrings(string(b))
		site, listed := captureSites[name]
		seen[name] = listed
		for _, f := range captureForms {
			n := len(f.re.FindAllStringIndex(text, -1))
			if f.def != "" {
				n -= strings.Count(text, f.def)
			}
			if want := site.counts[f.label]; n != want {
				t.Errorf("%s: form %q counted %d, want %d: %s", name, f.label, n, want, fix)
			}
		}
	}
	for name := range captureSites {
		if !seen[name] {
			t.Errorf("%s: listed in captureSites but not found", name)
		}
	}
}
