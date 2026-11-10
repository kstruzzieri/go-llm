package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent/interceptor"
)

func TestHarnessReadMarkdownImports(t *testing.T) {
	rows := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "@docs/a.md", []string{"docs/a.md"}},
		{"trailing dot", "see @docs/b.md.", []string{"docs/b.md.", "docs/b.md"}},
		{"paren before at", "(@x/c.md)", nil},
		{"escaped space", "@my\\ file.md", []string{"my file.md"}},
		{"email", "mail a@b.com", nil},
		{"home", "@~/notes.md", []string{"~/notes.md"}},
		{"absolute", "@/abs/p.md", []string{"/abs/p.md"}},
		{"crlf", "line1\r\n@docs/crlf.md\r\n", []string{"docs/crlf.md"}},
		{"backtick", "`@code/span.md`", nil},
		{"tab", "\t@tab.md", []string{"tab.md"}},
		{"bom", "\ufeff@docs/bom.md", []string{"docs/bom.md"}},
		{"nbsp", "see\u00a0@docs/nbsp.md", []string{"docs/nbsp.md"}},
		{"long token", "@" + strings.Repeat("a", 5000), nil},
		{"long text no newline", strings.Repeat("x", 1<<20) + " @docs/late.md", []string{"docs/late.md"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var got []string
			if err := scanMarkdownImports(strings.NewReader(r.in), func(s string) { got = append(got, s) }); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, r.want) {
				t.Fatalf("got %q, want %q", got, r.want)
			}
		})
	}
}

func TestHarnessReadOpencodeInstructions(t *testing.T) {
	var doc any
	if err := json.Unmarshal([]byte(`{"instructions":["CONTRIBUTING.md","docs/*.md","https://x/y.md",3]}`), &doc); err != nil {
		t.Fatal(err)
	}
	paths, patterns := opencodeInstructions(doc)
	if !reflect.DeepEqual(paths, []string{"CONTRIBUTING.md"}) || !reflect.DeepEqual(patterns, []string{"docs/*.md"}) {
		t.Fatalf("got %q %q", paths, patterns)
	}
	if err := json.Unmarshal([]byte(`{"instructions":"x"}`), &doc); err != nil {
		t.Fatal(err)
	}
	if p, g := opencodeInstructions(doc); len(p) != 0 || len(g) != 0 {
		t.Fatalf("got %q %q", p, g)
	}
}

func TestHarnessReadGlobAgrees(t *testing.T) {
	rows := []struct{ pat, name string }{
		{"**/*.md", "a/b/c.md"},
		{"**/*.md", "c.md"},
		{"**/*.md", "a/b/c.txt"},
		{"a/**", "a"},
		{"a/**", "a/b/c"},
		{"a/*/c", "a/b/c"},
		{"a/*/c", "a/b/d/c"},
		{"[ab].md", "a.md"},
		{"[ab].md", "c.md"},
		{"a/**/c", "a/c"},
		{"**", ""},
		{"a/?", "a/bc"},
	}
	for _, r := range rows {
		if got, want := matchGlobBounded(r.pat, r.name), matchGlob(r.pat, r.name); got != want {
			t.Errorf("%q vs %q: bounded=%v matchGlob=%v", r.pat, r.name, got, want)
		}
	}
}

func TestHarnessReadGlobAdversarial(t *testing.T) {
	done := make(chan bool, 1)
	go func() {
		done <- matchGlobBounded(strings.Repeat("**/a/", 20)+"b", strings.Repeat("a/", 60)+"c")
	}()
	select {
	case got := <-done:
		if got {
			t.Fatal("unexpected match")
		}
	case <-time.After(time.Second):
		t.Fatal("matchGlobBounded did not finish within 1s")
	}
}

// absBase returns the absolute folder name at the root of the current
// volume, so wanted paths are portable to the Windows CI step.
func absBase(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(filepath.VolumeName(wd)+string(os.PathSeparator), name)
}

// harnessKeysFor returns the command keys of the D4 file at virtual path file.
func harnessKeysFor(t *testing.T, file string) []string {
	t.Helper()
	keys, ok := commandFileKeysFor(interceptor.HarnessKey(file))
	if !ok {
		t.Fatalf("%s: no command keys", file)
	}
	return keys
}

// harnessResolveDocComplete decodes doc as the D4 file at virtual path file
// and resolves its command tokens, reporting whether the result is complete.
func harnessResolveDocComplete(t *testing.T, file, doc string, bases, projectDirs []string, lookPath func(string) (string, bool)) ([]string, bool) {
	t.Helper()
	keys := harnessKeysFor(t, file)
	if keys == nil {
		strs, err := lexTOMLStrings([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		return resolveCommandTokens(extractTOMLCommands(strs), bases, projectDirs, lookPath)
	}
	v, err := decodeJSONC([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return resolveCommandTokens(extractCommands(keys, v), bases, projectDirs, lookPath)
}

// harnessResolveDoc is harnessResolveDocComplete for a document that must
// resolve completely.
func harnessResolveDoc(t *testing.T, file, doc string, bases, projectDirs []string, lookPath func(string) (string, bool)) []string {
	t.Helper()
	got, complete := harnessResolveDocComplete(t, file, doc, bases, projectDirs, lookPath)
	if !complete {
		t.Fatalf("%s: resolution incomplete", file)
	}
	return got
}

// noLookPath finds no program on PATH.
func noLookPath(string) (string, bool) { return "", false }

func TestHarnessReadJSONC(t *testing.T) {
	v, err := decodeJSONC([]byte("{\"a\":\"http://x//y\", // c\r\n \"b\":[1,2,],}"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": "http://x//y", "b": []any{1.0, 2.0}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("got %#v, want %#v", v, want)
	}

	v, err = decodeJSONC([]byte("\ufeff{\r\n  // VS Code wrote this\r\n  \"a\": /* x */ 1,\r\n  \"s\": \"/* kept */ // kept\"\r\n}\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"a": 1.0, "s": "/* kept */ // kept"}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("BOM+CRLF: got %#v, want %#v", v, want)
	}

	for _, in := range []string{"", "  \n", "// only a comment", "\ufeff/* c */\r\n"} {
		v, err := decodeJSONC([]byte(in))
		if err != nil || !reflect.DeepEqual(v, map[string]any{}) {
			t.Fatalf("%q: got %#v, %v; want empty map", in, v, err)
		}
	}

	if _, err := decodeJSONC([]byte(`{"a":`)); err == nil {
		t.Fatal(`{"a": decoded without error`)
	}
}

func TestHarnessReadTOML(t *testing.T) {
	rows := []struct {
		name string
		in   string
		want []tomlString
	}{
		{"inline table", `mcp_servers.demo = { command = "python3", args = ["scripts/server.py"] }`,
			[]tomlString{{"command", "python3"}, {"", "scripts/server.py"}}},
		{"multi-line literal", "x = '''multi\nline'''", []tomlString{{"x", "multi\nline"}}},
		{"multi-line basic", `y = """a\"b"""`, []tomlString{{"y", `a"b`}}},
		{"comment apostrophe", "# don't panic\nx = \"y\"\n", []tomlString{{"x", "y"}}},
		{"header quoted key", "[mcp_servers.\"my server\"]\ncommand = 'a' # it's\ncwd = \"svc\"\r\n",
			[]tomlString{{"command", "a"}, {"cwd", "svc"}}},
		{"multi-line array", "notify = [\n  \"bash\", # why's\n  \"-lc\",\n]\nk = \"\\u0041\"",
			[]tomlString{{"", "bash"}, {"", "-lc"}, {"k", "A"}}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got, err := lexTOMLStrings([]byte(r.in))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, r.want) {
				t.Fatalf("got %q, want %q", got, r.want)
			}
		})
	}
	for _, in := range []string{`x = "unterminated`, "x = 'a\nb'", `x = """open`} {
		if _, err := lexTOMLStrings([]byte(in)); err == nil {
			t.Errorf("%q: no error", in)
		}
	}

	w := absBase(t, "w")
	got := harnessResolveDoc(t, ".codex/config.toml", rows[0].in, []string{w}, []string{w}, noLookPath)
	if want := filepath.Join(w, "scripts", "server.py"); !slices.Contains(got, want) {
		t.Fatalf("got %q, want it to include %q", got, want)
	}
}

func TestHarnessReadArgvWhole(t *testing.T) {
	v, err := decodeJSONC([]byte(`{"command":"python3","args":["scripts/check one.py","scripts/a&b.py"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, tk := range extractCommands(harnessKeysFor(t, ".mcp.json"), v).tokens {
		if !tk.program {
			ops = append(ops, tk.text)
		}
	}
	slices.Sort(ops)
	if want := []string{"scripts/a&b.py", "scripts/check one.py"}; !reflect.DeepEqual(ops, want) {
		t.Fatalf("operand tokens %q, want %q", ops, want)
	}

	w := absBase(t, "w")
	got := harnessResolveDoc(t, ".golem.json", `{"argv":["sh","-c","cd scripts && sh ./check.sh"]}`, []string{w}, []string{w}, noLookPath)
	if want := filepath.Join(w, "scripts", "check.sh"); !slices.Contains(got, want) {
		t.Fatalf("got %q, want it to include %q", got, want)
	}
}

func TestHarnessReadShellStrings(t *testing.T) {
	w, p := absBase(t, "w"), absBase(t, "p")
	lookPath := func(name string) (string, bool) {
		if name == "mytool" {
			return filepath.Join(w, "bin", "mytool"), true
		}
		return "", false
	}
	// abs maps "w/rel" and "p/rel" to the wanted absolute path.
	abs := func(s string) string {
		base, rel, _ := strings.Cut(s, "/")
		root := w
		if base == "p" {
			root = p
		}
		return filepath.Join(root, filepath.FromSlash(rel))
	}
	const claude, golem, copilot, mcp = ".claude/settings.json", ".golem.json", ".github/hooks/h.json", ".mcp.json"
	rows := []struct {
		name, file, doc string
		want            []string // "w/..." or "p/..."
		exact           bool     // want is the whole result
		absent          []string
	}{
		{"project dir var", claude, `{"command":"\"$CLAUDE_PROJECT_DIR\"/scripts/x.sh"}`, []string{"w/scripts/x.sh", "p/scripts/x.sh"}, false, nil},
		{"unset var alone", claude, `{"command":"$UNSET"}`, nil, true, nil},
		{"node operand", claude, `{"command":"node scripts/server.js"}`, []string{"w/scripts/server.js"}, false, nil},
		{"bash -c", claude, `{"command":"bash -c './a.sh && ./b.sh'"}`, []string{"w/a.sh", "w/b.sh"}, false, nil},
		{"bash -lc cd", claude, `{"command":"bash -lc 'cd scripts && ./check.sh'"}`, []string{"w/scripts/check.sh"}, false, nil},
		{"argv sh -ec", golem, `{"argv":["sh","-ec","./a.sh"]}`, []string{"w/a.sh"}, false, nil},
		{"argv pwsh -command", golem, `{"argv":["pwsh","-command","./p.ps1"]}`, []string{"w/p.ps1"}, false, nil},
		{"powershell key", copilot, `{"powershell":".\\scripts\\w.ps1"}`, []string{"w/scripts/w.ps1"}, false, nil},
		{"quoted space", claude, `{"command":"python3 'my dir/run.py'"}`, []string{"w/my dir/run.py"}, false, nil},
		{"flag value", claude, `{"command":"node --config=cfg/x.json"}`, []string{"w/cfg/x.json"}, false, nil},
		{"url", claude, `{"command":"curl https://x/y"}`, nil, true, nil},
		{"absent operand", claude, `{"command":"python3 server.py"}`, []string{"w/server.py"}, false, nil},
		{"path program", claude, `{"command":"mytool --x"}`, []string{"w/bin/mytool"}, false, nil},
		{"other var", claude, `{"command":"$OTHER/x.sh"}`, []string{"w/x.sh"}, false, nil},
		{"glob literal", claude, `{"command":"eslint src/*.js"}`, []string{"w/src/*.js"}, true, nil},
		// The rows below pin the shell-flag rule and the additions that only add prompts.
		{"argv sh -ec cd", golem, `{"argv":["sh","-ec","cd sub && ./a.sh"]}`, []string{"w/sub/a.sh"}, false, nil},
		{"argv pwsh -Command cd", golem, `{"argv":["pwsh","-Command","cd sub; ./p.ps1"]}`, []string{"w/sub/p.ps1"}, false, nil},
		{"argv cmd /C cd", golem, `{"argv":["C:\\Windows\\cmd.EXE","/C","cd sub & ./run.bat"]}`, []string{"w/sub/run.bat"}, false, nil},
		{"args after shell command", mcp, `{"command":"/bin/bash","args":["-c","cd sub && ./m.sh"]}`, []string{"w/sub/m.sh"}, false, nil},
		{"double-quoted substitution", claude, `{"command":"echo \"$(./scripts/sub.sh)\" \"` + "`./scripts/bt.sh`" + `\""}`, []string{"w/scripts/sub.sh", "w/scripts/bt.sh"}, false, nil},
		{"windows separators", mcp, `{"command":".\\scripts\\win.bat"}`, []string{"w/scripts/win.bat"}, false, nil},
		{"key case folded", golem, `{"verify":{"ARGV":["./scripts/up.sh"]}}`, []string{"w/scripts/up.sh"}, false, nil},
		{"gemini project var", ".gemini/settings.json", `{"command":"$GEMINI_PROJECT_DIR/.gemini/hooks/g.sh"}`, []string{"p/.gemini/hooks/g.sh"}, false, nil},
		{"cursor project var", ".cursor/hooks.json", `{"command":"${CURSOR_PROJECT_DIR}/h.sh"}`, []string{"p/h.sh"}, false, nil},
		{"workspaceFolder arg", ".vscode/mcp.json", `{"command":"node","args":["${workspaceFolder}/server.js"]}`, []string{"p/server.js"}, false, nil},
		{"var name boundary", claude, `{"command":"$CLAUDE_PROJECT_DIRX/y.sh"}`, []string{"w/y.sh"}, false, []string{"p/y.sh"}},
		{"cd to project var", claude, `{"command":"cd \"$CLAUDE_PROJECT_DIR\" && ./scripts/c.sh"}`, []string{"p/scripts/c.sh"}, false, nil},
		// Fix round 1 (rulings R9, R10, R12, R13).
		{"headersHelper", mcp, `{"mcpServers":{"s":{"headersHelper":"./scripts/hdr.sh"}}}`, []string{"w/scripts/hdr.sh"}, false, nil},
		{"redirect input", claude, `{"command":"bash < setup.sh"}`, []string{"w/setup.sh"}, false, nil},
		{"redirect output", claude, `{"command":"./run.sh > out.log"}`, []string{"w/run.sh"}, false, nil},
		{"leading redirect", claude, `{"command":"< in.sh bash"}`, []string{"w/in.sh"}, false, nil},
		{"fd redirects", claude, `{"command":"bash 2>&1 &> all.log < next.sh"}`, []string{"w/next.sh"}, false, nil},
		{"cd with redirect", claude, `{"command":"cd sub 2>/dev/null && ./r.sh"}`, []string{"w/sub/r.sh"}, false, nil},
		{"home residual", claude, `{"command":"~/scripts/x.sh"}`, []string{"w/scripts/x.sh"}, false, nil},
		{"backslash whole token", mcp, `{"command":"tools\\my server.exe"}`, []string{"w/tools/my server.exe"}, false, nil},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got := harnessResolveDoc(t, r.file, r.doc, []string{w}, []string{w, p}, lookPath)
			var want []string
			for _, s := range r.want {
				want = append(want, abs(s))
			}
			if r.exact {
				slices.Sort(want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("got %q, want exactly %q", got, want)
				}
			}
			for _, s := range want {
				if !slices.Contains(got, s) {
					t.Errorf("got %q, want it to include %q", got, s)
				}
			}
			for _, s := range r.absent {
				if slices.Contains(got, abs(s)) {
					t.Errorf("got %q, want it to exclude %q", got, abs(s))
				}
			}
		})
	}
}

func TestHarnessReadKeySelection(t *testing.T) {
	v, err := decodeJSONC([]byte(`{
		"permissions": {"allow": ["Bash(./scripts/y.sh)"]},
		"env": {"LINT": "scripts/z.sh"},
		"hooks": {"PostToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "./scripts/x.sh"}]}]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	cs := extractCommands(harnessKeysFor(t, ".claude/settings.json"), v)
	var texts []string
	for _, tk := range cs.tokens {
		texts = append(texts, tk.text)
	}
	if !slices.Contains(texts, "./scripts/x.sh") {
		t.Fatalf("tokens %q lack the hook command", texts)
	}
	for _, s := range texts {
		if strings.Contains(s, "y.sh") || strings.Contains(s, "z.sh") {
			t.Fatalf("tokens %q include a permission rule or env value", texts)
		}
	}

	rows := []struct{ file, doc string }{
		{".claude/settings.json", `{"env":{"COMMAND":"scripts/z.sh","DIR":"sub"}}`},
		{".claude/settings.json", `{"ENV":{"command":"scripts/z.sh","cwd":"sub"}}`},
		{".claude/settings.json", `{"permissions":{"command":"./scripts/p.sh","allow":["Bash(./scripts/p.sh)"],"dir":"sub"}}`},
		{".mcp.json", `{"mcpServers":{"s":{"env":{"DIR":"sub","COMMAND":"other/z.sh","ARGS":["other/a.sh"]}}}}`},
	}
	for _, r := range rows {
		v, err := decodeJSONC([]byte(r.doc))
		if err != nil {
			t.Fatal(err)
		}
		if cs := extractCommands(harnessKeysFor(t, r.file), v); len(cs.tokens) != 0 || len(cs.dirs) != 0 {
			t.Errorf("%s: got tokens %v and dirs %q, want none", r.doc, cs.tokens, cs.dirs)
		}
	}
	v, err = decodeJSONC([]byte(`{"command":"python3","args":["server.py"],"env":{"DIR":"sub","COMMAND":"other/z.sh"}}`))
	if err != nil {
		t.Fatal(err)
	}
	cs = extractCommands(harnessKeysFor(t, ".mcp.json"), v)
	texts = texts[:0]
	for _, tk := range cs.tokens {
		texts = append(texts, tk.text)
	}
	slices.Sort(texts)
	if want := []string{"python3", "python3", "server.py"}; !reflect.DeepEqual(texts, want) || len(cs.dirs) != 0 {
		t.Fatalf("got tokens %q and dirs %q, want %q and none", texts, cs.dirs, want)
	}
}

func TestHarnessReadCwdDirBases(t *testing.T) {
	w := absBase(t, "w")
	rows := []struct{ file, doc, want string }{
		{".github/hooks/h.json", `{"bash":"./run.sh","cwd":"tools"}`, "tools/run.sh"},
		{".golem.json", `{"argv":["./v.sh"],"dir":"sub"}`, "sub/v.sh"},
		{".codex/config.toml", "[mcp_servers.s]\ncommand = \"./srv.sh\"\ncwd = \"svc\"\n", "svc/srv.sh"},
	}
	for _, r := range rows {
		got := harnessResolveDoc(t, r.file, r.doc, []string{w}, []string{w}, noLookPath)
		if want := filepath.Join(w, filepath.FromSlash(r.want)); !slices.Contains(got, want) {
			t.Errorf("%s: got %q, want it to include %q", r.file, got, want)
		}
	}
}

func TestHarnessReadCommandFileLookup(t *testing.T) {
	claude := []string{"command", "args", "apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "otelHeadersHelper", "headersHelper"}
	gemini := []string{"command", "args", "discoveryCommand", "callCommand", "toolDiscoveryCommand", "toolCallCommand"}
	vscode := []string{"command", "args"}
	copilotHooks := []string{"bash", "powershell", "command"}
	rows := []struct {
		key  string
		want []string
		ok   bool
	}{
		{".claude/settings.json", claude, true},
		{".gemini/settings.json", gemini, true},
		{".vscode/settings.json", vscode, true},
		{"x/.github/hooks/a.json", copilotHooks, true},
		{".github/hooks/sub/a.json", nil, false},
		{".github/hooks/README.md", nil, false},
		{".github/copilot/c.json", append(slices.Clone(copilotHooks), "args"), true},
		{"settings.json", nil, false},
		{"pkg/.CLAUDE/Settings.JSON", claude, true},
		{".codex/config.toml", nil, true},
		{".agent/plan.lock.json", nil, false},
		{".mcp.json", []string{"command", "args", "headersHelper"}, true},
		// .github/hooks and opencode.json tie at 13 characters: the union.
		{".github/hooks/opencode.json", copilotHooks, true},
	}
	for _, r := range rows {
		for range 200 { // map order must not decide a tie
			got, ok := commandFileKeysFor(interceptor.HarnessKey(r.key))
			if ok != r.ok || !reflect.DeepEqual(got, r.want) {
				t.Fatalf("%s: got %q, %v; want %q, %v", r.key, got, ok, r.want, r.ok)
			}
		}
	}
}

func TestHarnessCommandFilesIsACopy(t *testing.T) {
	a := HarnessCommandFiles()
	if len(a) != 15 || !slices.IsSorted(a) {
		t.Fatalf("got %q, want 15 sorted names", a)
	}
	a[0] = "mutated"
	if b := HarnessCommandFiles(); b[0] == "mutated" {
		t.Fatal("HarnessCommandFiles shares its backing array")
	}
}

func TestHarnessReadCommandBounds(t *testing.T) {
	w, p := absBase(t, "w"), absBase(t, "p")
	var pairs []string
	for i := range 2000 {
		pairs = append(pairs, fmt.Sprintf(`{"cwd":"d%d","command":"./c%d.sh"}`, i, i))
	}
	rows := []struct {
		name, file, doc string
		complete        bool
	}{
		{"cd chain 28 KB", ".claude/settings.json", `{"command":"` + strings.Repeat("cd a;x;", 4000) + `"}`, false},
		{"cwd x command", ".mcp.json", `{"s":[` + strings.Join(pairs, ",") + `]}`, false},
		{"64 cd bases", ".claude/settings.json", `{"command":"` + strings.Repeat("cd a;", 64) + `./x.sh"}`, true},
		{"65 cd bases", ".claude/settings.json", `{"command":"` + strings.Repeat("cd a;", 65) + `./x.sh"}`, false},
		{"repeated cd deduped", ".claude/settings.json", `{"command":"` + strings.Repeat("cd a; cd /abs; ", 500) + `./x.sh"}`, true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			keys := harnessKeysFor(t, r.file)
			type result struct {
				complete bool
				err      error
			}
			done := make(chan result, 1)
			go func() { // decode, extract and resolve all count against the bound
				v, err := decodeJSONC([]byte(r.doc))
				if err != nil {
					done <- result{err: err}
					return
				}
				_, complete := resolveCommandTokens(extractCommands(keys, v), []string{w, filepath.Join(w, ".claude")}, []string{w, p}, noLookPath)
				done <- result{complete: complete}
			}()
			select {
			case got := <-done:
				if got.err != nil || got.complete != r.complete {
					t.Fatalf("complete = %v (%v), want %v", got.complete, got.err, r.complete)
				}
			case <-time.After(time.Second):
				t.Fatal("resolution did not finish within 1s")
			}
		})
	}
}
