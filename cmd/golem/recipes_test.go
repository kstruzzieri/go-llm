package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/recipe"
)

func TestRecipeUnknownPrefix(t *testing.T) {
	var out strings.Builder
	forced, exit := dispatchSlash(t.Context(), &out, &replSession{}, "/recipe")
	want := "unknown command: /recipe (try /help)\nmatching commands:\n  /recipes\n"
	if forced != "" || exit || out.String() != want {
		t.Fatalf("got %q, forced=%q exit=%v; want %q", out.String(), forced, exit, want)
	}
}

func reviewRecipe() recipe.Recipe {
	focus := "correctness"
	return recipe.Recipe{Version: 1, Name: "review", Description: "Review a change for actionable defects.",
		Goal: "Review {{inputs.target}}. Focus on {{inputs.focus}}.", Context: "Report defects.",
		Inputs: []recipe.Input{{Name: "target", Description: "Change or path to review"}, {Name: "focus", Description: "Review emphasis", Default: &focus}}}
}

func writeRecipe(t *testing.T, dir, filename string, r recipe.Recipe) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func catalogNames(c map[string]recipe.Recipe) []string {
	names := make([]string, 0, len(c))
	for n := range c {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func TestRecipeDiscovery(t *testing.T) {
	dir := t.TempDir()
	base := reviewRecipe()
	writeRecipe(t, dir, "review.recipe.json", base)
	base.Name = "alpha"
	writeRecipe(t, dir, "alpha.recipe.json", base)
	for _, name := range []string{"help", "quit", "recipes"} {
		base.Name = name
		writeRecipe(t, dir, name+".recipe.json", base)
	}
	base.Name = "dupe"
	writeRecipe(t, dir, "a.recipe.json", base)
	writeRecipe(t, dir, "b.recipe.json", base)
	base.Name = "badgoal"
	base.Goal = "{{inputs.undeclared}}"
	writeRecipe(t, dir, "badgoal.recipe.json", base)
	base = reviewRecipe()
	base.Name = "badcontext"
	base.Context = "{{inputs.undeclared}}"
	writeRecipe(t, dir, "badcontext.recipe.json", base)
	base = reviewRecipe()
	base.Name = "ignored"
	writeRecipe(t, dir, "ignored.json", base)
	if err := os.WriteFile(filepath.Join(dir, "invalid.recipe.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested.recipe.json"), 0700); err != nil {
		t.Fatal(err)
	}
	writeRecipe(t, filepath.Join(dir, "nested.recipe.json"), "nested.recipe.json", base)
	if err := os.Symlink(filepath.Join(dir, "review.recipe.json"), filepath.Join(dir, "linked.recipe.json")); err != nil {
		t.Fatal(err)
	}
	catalog, diags, err := discoverRecipes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalogNames(catalog); !reflect.DeepEqual(got, []string{"alpha", "review"}) {
		t.Fatalf("catalog %v", got)
	}
	want := []string{
		`recipes: "a.recipe.json": /dupe has duplicate recipe names`,
		`recipes: "b.recipe.json": /dupe has duplicate recipe names`,
		`recipes: "badcontext.recipe.json": invalid recipe: context: input "undeclared" is not declared`,
		`recipes: "badgoal.recipe.json": invalid recipe: goal: input "undeclared" is not declared`,
		`recipes: "help.recipe.json": /help is reserved by a built-in command`,
		`recipes: "invalid.recipe.json": invalid recipe: decode: EOF`,
		`recipes: "linked.recipe.json": candidate is not a regular file (symlinks are not allowed)`,
		`recipes: "nested.recipe.json": candidate is not a regular file (symlinks are not allowed)`,
		`recipes: "quit.recipe.json": /quit is reserved by a built-in command`,
		`recipes: "recipes.recipe.json": /recipes is reserved by a built-in command`,
	}
	if !reflect.DeepEqual(diags, want) {
		t.Fatalf("diagnostics:\n%s\nwant:\n%s", strings.Join(diags, "\n"), strings.Join(want, "\n"))
	}
}

func TestRecipeDuplicatesExcludeInvalidTemplateClaimant(t *testing.T) {
	dir := t.TempDir()
	r := reviewRecipe()
	writeRecipe(t, dir, "a.recipe.json", r)
	r.Goal = "{{inputs.missing}}"
	writeRecipe(t, dir, "b.recipe.json", r)
	cat, diags, err := discoverRecipes(dir)
	if err != nil || len(cat) != 0 || len(diags) != 2 {
		t.Fatalf("catalog=%v diagnostics=%v err=%v", cat, diags, err)
	}
	for i, name := range []string{"a.recipe.json", "b.recipe.json"} {
		if !strings.Contains(diags[i], name) {
			t.Fatal(diags)
		}
	}
}

func TestRecipeDirectoryPolicy(t *testing.T) {
	base := t.TempDir()
	actual := filepath.Join(base, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	writeRecipe(t, actual, "different-filename.recipe.json", reviewRecipe())
	for _, name := range []string{"go-llm", "commands"} {
		link := filepath.Join(base, name)
		if err := os.Symlink(actual, link); err != nil {
			t.Fatal(err)
		}
		cat, diags, err := discoverRecipes(link)
		if err != nil || len(cat) != 1 || len(diags) != 0 {
			t.Fatalf("directory symlink: %v %v %v", cat, diags, err)
		}
	}
	for _, dir := range []string{t.TempDir(), filepath.Join(base, "absent")} {
		cat, diags, err := discoverRecipes(dir)
		if err != nil || len(cat) != 0 || len(diags) != 0 {
			t.Fatalf("empty/missing: %v %v %v", cat, diags, err)
		}
	}
	if _, _, err := discoverRecipes(filepath.Join(actual, "different-filename.recipe.json")); err == nil {
		t.Fatal("file accepted as directory")
	}
}

func TestRecipeReservedNamesMatchDispatchSwitch(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "repl.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "dispatchSlash" {
			continue
		}
		for _, stmt := range fn.Body.List {
			sw, ok := stmt.(*ast.SwitchStmt)
			if !ok {
				continue
			}
			id, ok := sw.Tag.(*ast.Ident)
			if !ok || id.Name != "cmd" {
				continue
			}
			for _, clause := range sw.Body.List {
				for _, item := range clause.(*ast.CaseClause).List {
					lit, ok := item.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatal("nonliteral dispatch command")
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					names = append(names, value)
				}
			}
		}
	}
	sort.Strings(names)
	got := append([]string(nil), recipeReservedCommands...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, names) {
		t.Fatalf("reserved %v != switch %v", got, names)
	}
}

func TestRecipeArgumentsAndExpansion(t *testing.T) {
	for _, tc := range []struct{ tail, target, focus string }{
		{`"Project A"`, "Project A", "correctness"}, {`'' security`, "", "security"},
		{`pre"Project A"post`, "preProject Apost", "correctness"}, {`"a\"b"`, `a"b`, "correctness"},
		{`C:\work\folder`, `C:\work\folder`, "correctness"}, {`$HOME`, "$HOME", "correctness"},
		{`'$(echo x)'`, "$(echo x)", "correctness"}, {`target=a=b`, "target=a=b", "correctness"},
		{"first\u2003second", "first", "second"}, {"'a\\b'", `a\b`, "correctness"},
		{"last\\", "last\\", "correctness"}, {"'ending '", "ending ", "correctness"},
		{"'`whoami` * ; @file'", "`whoami` * ; @file", "correctness"},
	} {
		t.Run(tc.tail, func(t *testing.T) {
			sess := &replSession{recipes: map[string]recipe.Recipe{"review": reviewRecipe()}}
			var out strings.Builder
			got, exit := dispatchSlash(t.Context(), &out, sess, "/review "+tc.tail)
			want := "Review " + tc.target + ". Focus on " + tc.focus + ".\n\nReport defects."
			if exit || got != want || out.Len() != 0 {
				t.Fatalf("got %q, exit=%v, output=%q; want %q", got, exit, out.String(), want)
			}
		})
	}
}

func TestRecipeArgumentErrors(t *testing.T) {
	usage := "usage: /review <target> [focus]\n  target: Change or path to review (required)\n  focus: Review emphasis (optional)\n"
	for _, tc := range []struct{ tail, diagnostic string }{
		{"", `recipe: missing required input "target"`},
		{"a b c", "recipe: too many arguments (want at most 2)"},
		{`"open`, `recipe: unterminated '"' quote`},
		{string([]byte{0xff}), "recipe: invocation contains invalid UTF-8"},
	} {
		t.Run(tc.diagnostic, func(t *testing.T) {
			r := reviewRecipe()
			r.ModelHint = &recipe.ModelHint{Role: "swap"}
			sess := &replSession{recipes: map[string]recipe.Recipe{"review": r}}
			var out strings.Builder
			goal, exit := dispatchSlash(t.Context(), &out, sess, "/review "+tc.tail)
			if goal != "" || exit || sess.recipeHint != nil || out.String() != tc.diagnostic+"\n"+usage {
				t.Fatalf("goal=%q exit=%v hint=%v out=%q", goal, exit, sess.recipeHint, out.String())
			}
		})
	}
}

func TestRecipePositionalBindingLeavesDefaultsToExpand(t *testing.T) {
	d := "default"
	inputs := []recipe.Input{{Name: "optional", Default: &d}, {Name: "required"}}
	got, err := bindPositionalArgs(inputs, []string{""})
	if err != nil || !reflect.DeepEqual(got, map[string]string{"optional": ""}) {
		t.Fatalf("%v %v", got, err)
	}
	r := reviewRecipe()
	r.Inputs = inputs
	r.Goal = "constant"
	r.Context = ""
	if _, _, err := recipe.Expand(r, got, maxGoalBytes); err == nil {
		t.Fatal("missing unused required input accepted")
	}
}

func TestRecipeSeparatorBoundary(t *testing.T) {
	for _, tc := range []struct {
		goalBytes int
		focus     string
		good      bool
	}{
		{1048576, "", true}, {1048573, "x", true}, {1048574, "x", false},
	} {
		t.Run(fmt.Sprintf("%d-%q", tc.goalBytes, tc.focus), func(t *testing.T) {
			r := reviewRecipe()
			r.Goal = "{{inputs.target}}"
			r.Context = "{{inputs.focus}}"
			r.Inputs[1].Default = &tc.focus
			sess := &replSession{recipes: map[string]recipe.Recipe{"review": r}}
			var out strings.Builder
			goal, _ := dispatchSlash(t.Context(), &out, sess, "/review "+strings.Repeat("a", tc.goalBytes))
			if tc.good {
				want := strings.Repeat("a", tc.goalBytes)
				if tc.focus != "" {
					want += "\n\n" + tc.focus
				}
				if goal != want || out.Len() != 0 {
					t.Fatalf("len=%d output=%q", len(goal), out.String())
				}
			} else if goal != "" || !strings.Contains(out.String(), "1048576 bytes") {
				t.Fatalf("len=%d output=%q", len(goal), out.String())
			}
		})
	}
}

func TestRecipeListingReloadAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	r := reviewRecipe()
	writeRecipe(t, dir, "review.recipe.json", r)
	sess := &replSession{commandsDir: dir}
	var startup strings.Builder
	loadRecipes(&startup, sess)
	if startup.Len() != 0 {
		t.Fatal(startup.String())
	}
	entry := "  /review <target> [focus] - Review a change for actionable defects.\n"
	if got := slash(t, sess, "/recipes"); got != "recipes: "+dir+"\n"+entry {
		t.Fatal(got)
	}
	if got := slash(t, sess, "/help"); got != golemHelp+"recipe commands:\n"+entry {
		t.Fatal(got)
	}
	if got := slash(t, sess, "/recipes nope"); got != "usage: /recipes [reload]\n" {
		t.Fatal(got)
	}
	if got := slash(t, sess, "/rev"); got != "unknown command: /rev (try /help)\nmatching commands:\n  /review\n" {
		t.Fatal(got)
	}
	r.Goal = "new goal"
	r.Context = ""
	writeRecipe(t, dir, "review.recipe.json", r)
	var out strings.Builder
	goal, _ := dispatchSlash(t.Context(), &out, sess, "/review a")
	if goal != "Review a. Focus on correctness.\n\nReport defects." {
		t.Fatal(goal)
	}
	r.Name = "alpha"
	writeRecipe(t, dir, "alpha.recipe.json", r)
	if got := slash(t, sess, "/recipes reload"); got != "recipes: reloaded (2 commands available)\n" {
		t.Fatal(got)
	}
	goal, _ = dispatchSlash(t.Context(), &out, sess, "/review a")
	if goal != "new goal" {
		t.Fatal(goal)
	}
	if err := os.Remove(filepath.Join(dir, "alpha.recipe.json")); err != nil {
		t.Fatal(err)
	}
	r.Name = "review"
	r.Goal = "{{inputs.missing}}"
	writeRecipe(t, dir, "review.recipe.json", r)
	got := slash(t, sess, "/recipes reload")
	if !strings.HasSuffix(got, "recipes: reloaded (0 commands available)\n") || len(sess.recipes) != 0 {
		t.Fatal(got)
	}
	r = reviewRecipe()
	writeRecipe(t, dir, "review.recipe.json", r)
	slash(t, sess, "/recipes reload")
	sess.commandsDir = filepath.Join(dir, "review.recipe.json")
	got = slash(t, sess, "/recipes reload")
	if !strings.HasPrefix(got, "recipes: reload failed; previous commands kept: ") || len(sess.recipes) != 1 {
		t.Fatal(got)
	}
	sess.commandsDir = filepath.Join(dir, "absent")
	if got := slash(t, sess, "/recipes reload"); got != "recipes: reloaded (0 commands available)\n" || len(sess.recipes) != 0 {
		t.Fatal(got)
	}
	if got := slash(t, sess, "/recipes"); got != "recipes: "+sess.commandsDir+"\n  (no recipe commands loaded)\n" {
		t.Fatal(got)
	}
	sess.commandsDir = ""
	if got := slash(t, sess, "/recipes"); got != "recipes: directory unavailable\n  (no recipe commands loaded)\n" {
		t.Fatal(got)
	}
}

func TestRecipeHintDiscoveryAndStaging(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"recipe", "unknownrole", "unknownuse", "offline"} {
		r := reviewRecipe()
		r.Name = name
		r.ModelHint = &recipe.ModelHint{Role: "unconfigured"}
		if name == "unknownuse" {
			r.ModelHint = &recipe.ModelHint{UseCase: "unconfigured"}
		}
		if name == "offline" {
			r.ModelHint = &recipe.ModelHint{Role: "cloudrole"}
		}
		writeRecipe(t, dir, name+".recipe.json", r)
	}
	r := reviewRecipe()
	r.Name = "invalidhint"
	r.ModelHint = &recipe.ModelHint{Role: "a", UseCase: "b"}
	writeRecipe(t, dir, "invalidhint.recipe.json", r)
	cat, diags, err := discoverRecipes(dir)
	if err != nil || !reflect.DeepEqual(catalogNames(cat), []string{"offline", "recipe", "unknownrole", "unknownuse"}) || len(diags) != 1 {
		t.Fatalf("%v %v %v", catalogNames(cat), diags, err)
	}
	sess := &replSession{recipes: cat}
	var out strings.Builder
	goal, _ := dispatchSlash(t.Context(), &out, sess, "/recipe x")
	if goal == "" || out.Len() != 0 || sess.recipeHint == nil || sess.recipeHint.command != "/recipe" || sess.recipeHint.hint.Role != "unconfigured" {
		t.Fatalf("goal=%q output=%q hint=%v", goal, out.String(), sess.recipeHint)
	}
	cat["recipe"].ModelHint.Role = "mutated"
	if sess.recipeHint.hint.Role != "unconfigured" {
		t.Fatal("staged hint aliases catalog")
	}
	slash(t, sess, "/help")
	if sess.recipeHint != nil {
		t.Fatal("dispatch did not clear staged hint")
	}
}

func TestRecipeUnknownAvailableAndBuiltinPrecedence(t *testing.T) {
	r := reviewRecipe()
	sess := &replSession{recipes: map[string]recipe.Recipe{"review": r, "help": r}}
	if got := slash(t, sess, "/help"); !strings.HasPrefix(got, golemHelp) {
		t.Fatal(got)
	}
	delete(sess.recipes, "help")
	names := append(append([]string(nil), recipeReservedCommands...), "/review")
	sort.Strings(names)
	want := "unknown command: /zzz (try /help)\navailable commands:\n"
	for _, name := range names {
		want += "  " + name + "\n"
	}
	if got := slash(t, sess, "/zzz"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := slash(t, sess, "/\x1b[31m"); strings.Contains(got, "\x1b") {
		t.Fatal("unsafe unknown diagnostic")
	}
}

func TestRecipeGoalsUseOrdinaryRuntimeAndRecall(t *testing.T) {
	for _, tc := range []struct {
		name, template, arg string
		valid               bool
	}{
		{"slash expansion", "/allow-exec", "unused", true},
		{"unused secret", "safe expansion", secretTestValue(), true},
		{"expanded secret", "{{inputs.target}}", secretTestValue(), false},
		{"missing argument", "safe expansion", "", false},
		{"surplus argument", "safe expansion", "a b c", false},
		{"bad quote", "safe expansion", `"unterminated`, false},
		{"invalid bytes", "safe expansion", string([]byte{0xff}), false},
		{"blank expansion", "{{inputs.target}}", `'   '`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			fx.withSessionOpts(t, false, []string{"-interceptors"}, func(t *testing.T, sess *replSession) {
				r := reviewRecipe()
				r.Goal = tc.template
				r.Context = ""
				sess.recipes = map[string]recipe.Recipe{"review": r}
				beforeTools := orderedToolNames(sess.tools)
				before := fx.primary.requests() + fx.alt.requests()
				src := newCountingSource(sess, "/review "+tc.arg+"\n")
				var out strings.Builder
				if err := runREPL(t.Context(), src, &out, nil, sess); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(orderedToolNames(sess.tools), beforeTools) || sess.allowExec || sess.allowWrite {
					t.Fatal("recipe mounted tools")
				}
				history, err := json.Marshal(sess.session.history())
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(history), secretTestValue()) || strings.Contains(strings.Join(src.goals(), "\n"), secretTestValue()) {
					t.Fatal("raw or refused argument entered history")
				}
				if tc.valid {
					if got := src.goals(); !reflect.DeepEqual(got, []string{tc.template}) {
						t.Fatalf("recall=%v output=%s", got, out.String())
					}
					if fx.primary.chats.Load() != 1 || fx.alt.chats.Load() != 0 {
						t.Fatalf("wrong model calls: %d %d", fx.primary.chats.Load(), fx.alt.chats.Load())
					}
					if !strings.Contains(string(history), tc.template) {
						t.Fatal("expansion not persisted")
					}
				} else if len(src.goals()) != 0 || len(sess.session.history()) != 0 || fx.primary.requests()+fx.alt.requests() != before {
					t.Fatalf("rejected invocation had effects: recall=%v history=%s output=%s", src.goals(), history, out.String())
				}
			})
		})
	}
}

func TestRecipeCanaryRecall(t *testing.T) {
	for _, expanded := range []bool{false, true} {
		t.Run(fmt.Sprint(expanded), func(t *testing.T) {
			caller := &captureCaller{answer: "ok"}
			sess := newCanarySession(t, caller)
			r := reviewRecipe()
			r.Goal = "safe expansion"
			r.Context = ""
			if expanded {
				r.Goal = "{{inputs.target}}"
			}
			sess.recipes = map[string]recipe.Recipe{"review": r}
			src := newCountingSource(sess, "/review "+canaryNonceA+"\n")
			var out strings.Builder
			if err := runREPL(t.Context(), src, &out, nil, sess); err != nil {
				t.Fatal(err)
			}
			history, err := json.Marshal(sess.session.history())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(history), canaryNonceA) || strings.Contains(strings.Join(src.goals(), "\n"), canaryNonceA) {
				t.Fatal("canary entered history")
			}
			if expanded {
				if len(src.goals()) != 0 || len(sess.session.history()) != 0 || len(caller.messages) != 0 {
					t.Fatal("expanded canary was not refused")
				}
			} else if !reflect.DeepEqual(src.goals(), []string{"safe expansion"}) || len(caller.messages) == 0 {
				t.Fatal("safe expansion was not recorded and called once")
			}
		})
	}
}

func TestRecipeStartupDirectory(t *testing.T) {
	for _, kind := range []string{"empty", "missing", "unavailable", "unreadable", "file", "valid"} {
		t.Run(kind, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			dir := t.TempDir()
			switch kind {
			case "missing":
				dir = filepath.Join(dir, "missing")
			case "unavailable":
				dir = ""
			case "unreadable":
				if os.Getuid() == 0 {
					t.Skip("root can read mode 000")
				}
				if err := os.Chmod(dir, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			case "file":
				path := filepath.Join(dir, "file")
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
				dir = path
			case "valid":
				writeRecipe(t, dir, "review.recipe.json", reviewRecipe())
			}
			stdin, stdout, stderr := runTestFiles(t)
			var sess *replSession
			args := []string{"-config", fx.configPath, "-root", fx.root, "-no-probe", "-no-cap-probe", "-no-memory", "-no-rag", "-no-project-context", "-no-git-context", "-no-auto-index", "-no-editor", "-no-session"}
			err := run(args, stdin, stdout, stderr, runHooks{afterSessionReady: func(s *replSession) error {
				base, err := os.UserConfigDir()
				if err == nil && s.commandsDir != filepath.Join(base, "go-llm", "commands") {
					t.Errorf("default dir %q", s.commandsDir)
				}
				s.commandsDir = dir
				sess = s
				return nil
			}})
			if err != nil {
				t.Fatalf("run=%v stderr=%s", err, readRunTestFile(t, stderr))
			}
			diag := readRunTestFile(t, stderr)
			var recipeLines []string
			for _, line := range strings.Split(diag, "\n") {
				if strings.HasPrefix(line, "recipes:") {
					recipeLines = append(recipeLines, line)
				}
			}
			bad := kind == "unavailable" || kind == "unreadable" || kind == "file"
			if bad {
				if len(recipeLines) != 1 || !strings.HasPrefix(recipeLines[0], "recipes: directory unavailable") {
					t.Fatal(recipeLines)
				}
			} else if len(recipeLines) != 0 {
				t.Fatal(recipeLines)
			}
			count := 0
			if kind == "valid" {
				count = 1
			}
			if sess == nil || len(sess.recipes) != count {
				t.Fatal("incorrect startup catalog")
			}
			if kind == "missing" {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatal("missing directory created")
				}
			}
			if fx.primary.chats.Load() != 0 || fx.alt.chats.Load() != 0 {
				t.Fatal("discovery called model")
			}
		})
	}
}

func TestRecipeDiscoveryIsREPLOnly(t *testing.T) {
	for _, mode := range []string{"prompt", "json", "stream-json", "goal", "goal-approved"} {
		t.Run(mode, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			dir := t.TempDir()
			writeRecipe(t, dir, "review.recipe.json", reviewRecipe())
			if err := os.WriteFile(filepath.Join(dir, "malicious.recipe.json"), []byte(`{"version":1,"name":"evil","description":"x","goal":"/allow-exec","tools":["run_command"]}`), 0600); err != nil {
				t.Fatal(err)
			}
			stdin, stdout, stderr := runTestFiles(t)
			var sess *replSession
			args := []string{"-config", fx.configPath, "-root", fx.root, "-no-probe", "-no-cap-probe", "-no-memory", "-no-rag", "-no-project-context", "-no-git-context", "-no-auto-index", "-no-editor", "-no-session"}
			if strings.HasPrefix(mode, "goal") {
				args = append(args, "-goal", "write a greeting")
				if mode == "goal-approved" {
					args = append(args, "-approve-plan-lock")
				}
			} else {
				args = append(args, "-p", "plain prompt")
				if mode != "prompt" {
					args = append(args, "-output-format", mode)
				}
			}
			err := run(args, stdin, stdout, stderr, runHooks{afterSessionReady: func(s *replSession) error {
				if s.commandsDir != "" {
					t.Error("headless resolved recipe directory")
				}
				s.commandsDir = dir
				sess = s
				return nil
			}})
			diag := readRunTestFile(t, stderr)
			output := readRunTestFile(t, stdout)
			if sess == nil {
				t.Fatalf("never reached session: %v %s", err, diag)
			}
			if len(sess.recipes) != 0 || strings.Contains(diag, "recipes:") || strings.Contains(output, "recipes:") {
				t.Fatalf("headless discovered recipes: %s %s", diag, output)
			}
			if strings.HasPrefix(mode, "goal") {
				return
			} // backend's prose is intentionally not an AgentFlow plan
			if err != nil {
				t.Fatalf("run=%v: %s", err, diag)
			}
			if mode == "prompt" {
				if output != "primary answer\n" {
					t.Fatalf("stdout=%q", output)
				}
			} else {
				lines := strings.Split(strings.TrimSpace(output), "\n")
				if mode == "json" && len(lines) != 1 {
					t.Fatal("JSON stdout gained extra lines")
				}
				for _, line := range lines {
					if !json.Valid([]byte(line)) {
						t.Fatal("machine stdout polluted")
					}
				}
				if !strings.Contains(lines[len(lines)-1], `"answer":"primary answer"`) {
					t.Fatal("machine result changed")
				}
			}
		})
	}
}

func TestRecipeNonregularCandidate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket candidate")
	}
	// macOS Unix socket addresses cannot hold the long default test temp path.
	dir, err := os.MkdirTemp("", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listener, err := net.Listen("unix", filepath.Join(dir, "socket.recipe.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	cat, diags, err := discoverRecipes(dir)
	if err != nil || len(cat) != 0 || !reflect.DeepEqual(diags, []string{`recipes: "socket.recipe.json": candidate is not a regular file (symlinks are not allowed)`}) {
		t.Fatalf("%v %v %v", cat, diags, err)
	}
}

func TestRecipeDiagnosticsDoNotRevealContent(t *testing.T) {
	dir := t.TempDir()
	r := reviewRecipe()
	r.Goal = "PRIVATE-GOAL {{inputs.bad}}"
	r.Inputs[1].Default = new(string)
	*r.Inputs[1].Default = "PRIVATE-DEFAULT"
	writeRecipe(t, dir, "bad\x1b[31m.recipe.json", r)
	cat, diags, err := discoverRecipes(dir)
	if err != nil || len(cat) != 0 || len(diags) != 1 {
		t.Fatalf("%v %v %v", cat, diags, err)
	}
	if strings.Contains(diags[0], "\x1b") || strings.Contains(diags[0], "PRIVATE-") {
		t.Fatal("unsafe candidate diagnostic")
	}
	r = reviewRecipe()
	r.Description = "one\ntwo\x1b[31m"
	r.Inputs[0].Description = "path\tname"
	sess := &replSession{commandsDir: dir + "\x1b", recipes: map[string]recipe.Recipe{"review": r}}
	if got := slash(t, sess, "/recipes"); strings.Contains(got, "\x1b") || !strings.Contains(got, `one\ntwo\x1b[31m`) {
		t.Fatal(got)
	}
	var out strings.Builder
	dispatchSlash(t.Context(), &out, sess, "/review")
	if !strings.Contains(out.String(), `path\tname`) {
		t.Fatal(out.String())
	}
}

func TestRecipeDiscoveryNeverProbesHints(t *testing.T) {
	fx := newModelSwitchFixture(t, "https://never-resolves.invalid")
	fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
		before := fx.primary.requests() + fx.alt.requests()
		sess.commandsDir = t.TempDir()
		for _, name := range []string{"cloudrole", "notconfigured"} {
			r := reviewRecipe()
			r.Name = name
			r.ModelHint = &recipe.ModelHint{Role: name}
			writeRecipe(t, sess.commandsDir, name+".recipe.json", r)
		}
		r := reviewRecipe()
		r.Name = "usecase"
		r.ModelHint = &recipe.ModelHint{UseCase: "notconfigured"}
		writeRecipe(t, sess.commandsDir, "usecase.recipe.json", r)
		var out strings.Builder
		loadRecipes(&out, sess)
		if len(sess.recipes) != 3 || out.Len() != 0 || fx.primary.requests()+fx.alt.requests() != before {
			t.Fatalf("discovery effects: %s", out.String())
		}
		if got := slash(t, sess, "/recipes"); strings.Contains(got, "unavailable") || strings.Contains(got, "fallback") {
			t.Fatal(got)
		}
	})
}
