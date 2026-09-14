package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/internal/mcpstdio"
	"github.com/kstruzzieri/go-llm/recipe"
)

// Keep this list in agreement with dispatchSlash's command switch. Recipe
// lookup stays in its default clause so built-ins always retain precedence.
var recipeReservedCommands = []string{
	"/exit", "/quit", "/trust", "/help", "/recipes", "/context", "/clear", "/new",
	"/sessions", "/search-sessions", "/resume", "/consult", "/model", "/undo",
	"/checkpoints", "/jobs", "/remember", "/forget", "/memories", "/records",
	"/tools", "/auto-edits", "/grants", "/edit", "/allow-write", "/allow-exec",
	"/git-context", "/compact", "/think",
}

// recipeContextSeparator joins an expanded goal and its nonempty context into
// the ONE user message; the byte check in invokeRecipe counts it once.
const recipeContextSeparator = "\n\n"

// recipeInvocationHint carries only routing metadata across slash dispatch.
// Copying the hint prevents a later catalog edit from changing an invocation.
type recipeInvocationHint struct {
	command string
	hint    recipe.ModelHint
}

func discoverRecipes(dir string) (map[string]recipe.Recipe, []string, error) {
	catalog := make(map[string]recipe.Recipe)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return catalog, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	reserved := make(map[string]bool, len(recipeReservedCommands))
	for _, command := range recipeReservedCommands {
		reserved[strings.TrimPrefix(command, "/")] = true
	}
	// ReadDir sorts candidates. Keep diagnostics in that order, even when the
	// second claimant reveals a duplicate after the first file was validated.
	type candidate struct {
		filename string
		r        recipe.Recipe
		problem  string
	}
	var candidates []candidate
	claims := make(map[string]int)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".recipe.json") {
			continue
		}
		c := candidate{filename: entry.Name()}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		switch {
		case err != nil:
			c.problem = err.Error()
		case info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular():
			c.problem = "candidate is not a regular file (symlinks are not allowed)"
		default:
			c.r, err = recipe.Load(path)
			if err != nil {
				cause := strings.TrimPrefix(err.Error(), fmt.Sprintf("recipe: load %q: ", path))
				c.problem = "invalid recipe: " + strings.TrimPrefix(cause, "recipe: ")
			} else {
				claims[c.r.Name]++
				if reserved[c.r.Name] {
					c.problem = "/" + c.r.Name + " is reserved by a built-in command"
				} else if err := recipe.ValidateTemplates(c.r); err != nil {
					c.problem = "invalid recipe: " + strings.TrimPrefix(err.Error(), "recipe: ")
				}
			}
		}
		candidates = append(candidates, c)
	}
	var diagnostics []string
	for _, c := range candidates {
		if c.r.Name != "" && claims[c.r.Name] > 1 {
			c.problem = "/" + c.r.Name + " has duplicate recipe names"
		}
		if c.problem != "" {
			diagnostics = append(diagnostics, fmt.Sprintf("recipes: %q: %s", gitContextText(c.filename), gitContextText(c.problem)))
		} else {
			catalog[c.r.Name] = c.r
		}
	}
	return catalog, diagnostics, nil
}

func loadRecipes(out io.Writer, sess *replSession) {
	if sess.commandsDir == "" {
		_, _ = fmt.Fprintln(out, "recipes: directory unavailable")
		return
	}
	catalog, diagnostics, err := discoverRecipes(sess.commandsDir)
	if err != nil {
		_, _ = fmt.Fprintf(out, "recipes: directory unavailable: %s\n", gitContextText(err.Error()))
		return
	}
	sess.recipes = catalog
	for _, diagnostic := range diagnostics {
		_, _ = fmt.Fprintln(out, diagnostic)
	}
}

func handleRecipes(out io.Writer, sess *replSession, fields []string) {
	if len(fields) == 1 {
		dir := gitContextText(sess.commandsDir)
		if dir == "" {
			dir = "directory unavailable"
		}
		_, _ = fmt.Fprintf(out, "recipes: %s\n", dir)
		if len(sess.recipes) == 0 {
			_, _ = fmt.Fprintln(out, "  (no recipe commands loaded)")
		} else {
			printRecipeEntries(out, sess.recipes)
		}
		return
	}
	if len(fields) != 2 || fields[1] != "reload" {
		_, _ = fmt.Fprintln(out, "usage: /recipes [reload]")
		return
	}
	if sess.commandsDir == "" {
		_, _ = fmt.Fprintln(out, "recipes: reload failed; previous commands kept: directory unavailable")
		return
	}
	catalog, diagnostics, err := discoverRecipes(sess.commandsDir)
	if err != nil {
		_, _ = fmt.Fprintf(out, "recipes: reload failed; previous commands kept: %s\n", gitContextText(err.Error()))
		return
	}
	sess.recipes = catalog
	for _, diagnostic := range diagnostics {
		_, _ = fmt.Fprintln(out, diagnostic)
	}
	_, _ = fmt.Fprintf(out, "recipes: reloaded (%s available)\n", plural(len(catalog), "command", "commands"))
}

func recipeUsage(r recipe.Recipe) string {
	usage := "/" + r.Name
	for _, input := range r.Inputs {
		if input.Default == nil {
			usage += " <" + input.Name + ">"
		} else {
			usage += " [" + input.Name + "]"
		}
	}
	return usage
}

func printRecipeEntries(out io.Writer, catalog map[string]recipe.Recipe) {
	names := make([]string, 0, len(catalog))
	for name := range catalog {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r := catalog[name]
		_, _ = fmt.Fprintf(out, "  %s - %s\n", recipeUsage(r), gitContextText(r.Description))
	}
}

func bindPositionalArgs(inputs []recipe.Input, argv []string) (map[string]string, error) {
	if len(argv) > len(inputs) {
		return nil, fmt.Errorf("recipe: too many arguments (want at most %d)", len(inputs))
	}
	values := make(map[string]string, len(argv))
	for i, value := range argv {
		values[inputs[i].Name] = value
	}
	return values, nil
}

func invokeRecipe(out io.Writer, sess *replSession, r recipe.Recipe, line, command string) string {
	fail := func(err error) string {
		_, _ = fmt.Fprintln(out, gitContextText(err.Error()))
		_, _ = fmt.Fprintf(out, "usage: %s\n", recipeUsage(r))
		for _, input := range r.Inputs {
			required := "required"
			if input.Default != nil {
				required = "optional"
			}
			_, _ = fmt.Fprintf(out, "  %s: %s (%s)\n", input.Name, gitContextText(input.Description), required)
		}
		return ""
	}
	if !utf8.ValidString(line) {
		return fail(errors.New("recipe: invocation contains invalid UTF-8"))
	}
	argv, err := mcpstdio.ParseCommand(strings.TrimPrefix(line, command))
	if err != nil {
		return fail(fmt.Errorf("recipe: %w", err))
	}
	values, err := bindPositionalArgs(r.Inputs, argv)
	if err != nil {
		return fail(err)
	}
	goal, expandedContext, err := recipe.Expand(r, values, maxGoalBytes)
	if err != nil {
		return fail(err)
	}
	separatorBytes := 0
	if expandedContext != "" {
		separatorBytes = len(recipeContextSeparator)
	}
	if len(goal)+len(expandedContext)+separatorBytes > maxGoalBytes {
		return fail(fmt.Errorf("recipe: expanded message exceeds %d bytes", maxGoalBytes))
	}
	if expandedContext != "" {
		goal += recipeContextSeparator + expandedContext
	}
	if !utf8.ValidString(goal) {
		return fail(errors.New("recipe: expanded message contains invalid UTF-8"))
	}
	if strings.TrimSpace(goal) == "" {
		return fail(errors.New("recipe: expanded message is blank"))
	}
	if r.ModelHint != nil {
		sess.recipeHint = &recipeInvocationHint{command: command, hint: *r.ModelHint}
	}
	return goal
}

func printUnknownCommand(out io.Writer, sess *replSession, command string) {
	_, _ = fmt.Fprintf(out, "unknown command: %s (try /help)\n", gitContextText(command))
	names := append([]string(nil), recipeReservedCommands...)
	for name := range sess.recipes {
		names = append(names, "/"+name)
	}
	sort.Strings(names)
	var matches []string
	for _, name := range names {
		if strings.HasPrefix(name, command) {
			matches = append(matches, name)
		}
	}
	if len(matches) == 0 {
		_, _ = fmt.Fprintln(out, "available commands:")
		matches = names
	} else {
		_, _ = fmt.Fprintln(out, "matching commands:")
	}
	for _, name := range matches {
		_, _ = fmt.Fprintf(out, "  %s\n", name)
	}
}
