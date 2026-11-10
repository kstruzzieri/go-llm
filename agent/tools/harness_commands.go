package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent/interceptor"
)

// commandFileKeys lists the hook and MCP configuration files whose commands
// another tool runs (#784, spec D4), keyed by interceptor.HarnessKey of the
// file name, with the keys whose values are read; nil means every string. The
// .github/hooks and .github/copilot entries stand for their *.json files.
// Values are read only under the listed keys, matched by key name anywhere in
// the file's tree. Key names match case-insensitively: Golem decodes
// .golem.json with encoding/json, which does, and for the other files a wider
// match only adds prompts.
//
//	| File | Keys read | What runs from it |
//	|---|---|---|
//	| `.claude/settings.json`, `.claude/settings.local.json` | `command`, `args`, `apiKeyHelper`, `awsAuthRefresh`, `awsCredentialExport`, `otelHeadersHelper` | hooks, `statusLine`, MCP servers, helper commands |
//	| `.mcp.json` | `command`, `args` | Claude Code project MCP servers |
//	| `.cursor/hooks.json`, `.cursor/mcp.json` | `command`, `args` | Cursor hooks and MCP servers |
//	| `.windsurf/hooks.json` | `command`, `args` | Windsurf hooks |
//	| `.gemini/settings.json` | `command`, `args`, `discoveryCommand`, `callCommand` | Gemini CLI hooks, MCP servers, tool discovery and call |
//	| `.github/hooks/*.json`, `.github/copilot/*.json` | `bash`, `powershell`, `command` | Copilot CLI hooks |
//	| `.vscode/mcp.json`, `.vscode/settings.json` | `command`, `args` | VS Code MCP servers (also declarable in settings) |
//	| `opencode.json`, `opencode.jsonc` | `command` | OpenCode MCP servers (and D5) |
//	| `.golem.json` | `argv` | Golem's own post-write verify |
//	| `.codex/config.toml` | every string (see Extraction) | Codex project MCP servers, `notify` and other commands |
//
// Excluded on purpose:
//
//   - Claude `permissions` allow-rules: they pre-approve an action a future session's model may choose; they run nothing on their own.
//   - `env` values.
//   - Every other JSON file in a harness folder: `.claude/tasks/`, `.claude/plans/` and similar working state name ordinary files constantly.
//   - `.agent/`.
//
// Three additions to the table, each only adding prompts: .gemini/settings.json
// also reads Gemini CLI's older top-level toolDiscoveryCommand and
// toolCallCommand; .github/copilot/*.json also reads args; and .mcp.json,
// .claude/settings.json and .claude/settings.local.json also read an MCP
// server's headersHelper, which "Claude Code runs ... in a shell" when
// connecting (https://code.claude.com/docs/en/mcp, ruling R9). A top-level
// permissions or env subtree, and the env of an object that also holds a
// listed key with a string or array value (a server or hook's own env), are
// skipped whole (ruling R14); an env or permissions key anywhere else is read
// like any other, so a server named "env" cannot hide its command, not even
// beside a server named "command".
var commandFileKeys = map[string][]string{
	interceptor.HarnessKey(".claude/settings.json"):       {"command", "args", "apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "otelHeadersHelper", "headersHelper"},
	interceptor.HarnessKey(".claude/settings.local.json"): {"command", "args", "apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "otelHeadersHelper", "headersHelper"},
	interceptor.HarnessKey(".mcp.json"):                   {"command", "args", "headersHelper"},
	interceptor.HarnessKey(".cursor/hooks.json"):          {"command", "args"},
	interceptor.HarnessKey(".cursor/mcp.json"):            {"command", "args"},
	interceptor.HarnessKey(".windsurf/hooks.json"):        {"command", "args"},
	interceptor.HarnessKey(".gemini/settings.json"):       {"command", "args", "discoveryCommand", "callCommand", "toolDiscoveryCommand", "toolCallCommand"},
	interceptor.HarnessKey(".github/hooks"):               {"bash", "powershell", "command"},
	interceptor.HarnessKey(".github/copilot"):             {"bash", "powershell", "command", "args"},
	interceptor.HarnessKey(".vscode/mcp.json"):            {"command", "args"},
	interceptor.HarnessKey(".vscode/settings.json"):       {"command", "args"},
	interceptor.HarnessKey("opencode.json"):               {"command"},
	interceptor.HarnessKey("opencode.jsonc"):              {"command"},
	interceptor.HarnessKey(".golem.json"):                 {"argv"},
	interceptor.HarnessKey(".codex/config.toml"):          nil,
}

// HarnessCommandFiles returns a fresh, sorted copy of the configuration file
// names the command reader reads (spec D4); .github/hooks and .github/copilot
// stand for the *.json files directly inside them.
func HarnessCommandFiles() []string { return slices.Sorted(maps.Keys(commandFileKeys)) }

// commandFileKeysFor returns the keys to read from the file whose virtual
// path has HarnessKey virtualKey. The longest commandFileKeys entries that are
// a whole-component suffix of virtualKey win; when several tie, the result is
// the union of their keys in sorted-name order (ruling R11), so map order never
// decides it. A folder entry (.github/hooks, .github/copilot) matches only a
// *.json file directly inside that folder. ok is false for a file the command
// reader does not read. The result is a fresh slice.
func commandFileKeysFor(virtualKey string) (keys []string, ok bool) {
	dir, base := path.Split(virtualKey)
	dir = strings.TrimSuffix(dir, "/")
	best := 0
	for _, name := range slices.Sorted(maps.Keys(commandFileKeys)) {
		target := virtualKey
		if path.Ext(name) == "" {
			if !strings.HasSuffix(base, ".json") {
				continue
			}
			target = dir
		}
		if target != name && !strings.HasSuffix(target, "/"+name) || len(name) < best {
			continue
		}
		if len(name) > best {
			best, keys = len(name), nil
		}
		ok = true
		for _, k := range commandFileKeys[name] {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	return keys, ok
}

// decodeJSONC decodes a JSON-with-comments configuration file. It strips a
// leading UTF-8 BOM, then in one string-aware pass removes "//" comments (up
// to a line break, which is kept), "/* */" comments and trailing commas before
// "}" or "]". Input that is only whitespace after that pass decodes as an
// empty object: an empty or comment-only file configures nothing. Anything
// else goes to encoding/json, whose error is returned as is.
func decodeJSONC(data []byte) (any, error) {
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		switch c := data[i]; {
		case c == '"':
			j := i + 1
			for ; j < len(data) && data[j] != '"'; j++ {
				if data[j] == '\\' {
					j++
				}
			}
			j = min(j+1, len(data))
			out = append(out, data[i:j]...)
			i = j - 1
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i+1 < len(data) && data[i+1] != '\n' && data[i+1] != '\r' {
				i++
			}
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			end := bytes.Index(data[i+2:], []byte("*/"))
			if end < 0 {
				i = len(data)
			} else {
				i += end + 3 // i is on the closing '/'
			}
			out = append(out, ' ')
		case c == '}' || c == ']':
			if k := len(bytes.TrimRight(out, " \t\r\n")); k > 0 && out[k-1] == ',' {
				out[k-1] = ' '
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	if len(bytes.Trim(out, " \t\r\n")) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// tomlString is one string literal from a TOML file: its value, the key on
// the left of the nearest "=" ("" for an array element), and for an array
// element the array's number, counting from 1 in the order arrays open (0
// otherwise), so the elements of one array can be read as one argv.
type tomlString struct {
	key, value string
	array      int
}

// errTOMLUnterminated reports a TOML string with no closing delimiter.
var errTOMLUnterminated = errors.New("unterminated TOML string")

// lexTOMLStrings collects every basic, literal, multi-line basic and
// multi-line literal string value of a TOML file, with basic-string escapes
// decoded, whatever its key or table (spec D4: .codex/config.toml). Quoted
// keys and table headers are keys, not values, and are not collected. "#"
// outside a string comments out the rest of the line. It is a lexer, not a
// validator: only an unterminated string is an error.
func lexTOMLStrings(data []byte) ([]tomlString, error) {
	// level is the top-level line, an inline table or an array.
	type level struct {
		array, keyPos bool // keyPos: a key, not a value, comes next
		key           string
		id            int // the array's number
	}
	s := string(data)
	stack := []level{{keyPos: true}}
	var out []tomlString
	arrays := 0
	for i := 0; i < len(s); {
		top := &stack[len(stack)-1]
		switch c := s[i]; {
		case c == '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		case c == '"' || c == '\'':
			v, n, err := lexTOMLString(s[i:])
			if err != nil {
				return nil, err
			}
			switch {
			case top.keyPos:
				top.key = v
			case top.array:
				out = append(out, tomlString{"", v, top.id})
			default:
				out = append(out, tomlString{key: top.key, value: v})
			}
			i += n
			continue
		case c == '\n' && len(stack) == 1:
			*top = level{keyPos: true}
		case c == '=' && !top.array:
			top.keyPos = false
		case c == '[' && (len(stack) > 1 || !top.keyPos): // not a table header
			arrays++
			stack = append(stack, level{array: true, id: arrays})
		case c == '{':
			stack = append(stack, level{keyPos: true})
		case len(stack) > 1 && (c == ']' && top.array || c == '}' && !top.array):
			stack = stack[:len(stack)-1]
		case c == ',' && len(stack) > 1 && !top.array:
			*top = level{keyPos: true}
		case top.keyPos && isTOMLBareKeyByte(c):
			j := i
			for j < len(s) && isTOMLBareKeyByte(s[j]) {
				j++
			}
			top.key, i = s[i:j], j
			continue
		}
		i++
	}
	return out, nil
}

// isTOMLBareKeyByte reports whether c may appear in a TOML bare key.
func isTOMLBareKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// lexTOMLString reads the TOML string that starts s, returning its decoded
// value and the number of bytes it spans.
func lexTOMLString(s string) (string, int, error) {
	q := s[0]
	basic := q == '"'
	if delim := strings.Repeat(s[:1], 3); strings.HasPrefix(s, delim) {
		start := 3 // a newline right after the opening delimiter is trimmed
		if strings.HasPrefix(s[3:], "\r\n") {
			start = 5
		} else if strings.HasPrefix(s[3:], "\n") {
			start = 4
		}
		for j := start; j < len(s); j++ {
			if basic && s[j] == '\\' {
				j++
				continue
			}
			if strings.HasPrefix(s[j:], delim) {
				end := j // up to two more quotes belong to the content
				for end-j < 2 && end+3 < len(s) && s[end+3] == q {
					end++
				}
				if basic {
					return unescapeTOML(s[start:end]), end + 3, nil
				}
				return s[start:end], end + 3, nil
			}
		}
		return "", 0, errTOMLUnterminated
	}
	for j := 1; j < len(s); j++ {
		switch {
		case s[j] == '\n':
			return "", 0, errTOMLUnterminated
		case basic && s[j] == '\\':
			j++
		case s[j] == q:
			if basic {
				return unescapeTOML(s[1:j]), j + 1, nil
			}
			return s[1:j], j + 1, nil
		}
	}
	return "", 0, errTOMLUnterminated
}

// unescapeTOML decodes the escapes of a TOML basic string body. An escape it
// does not know is kept as written.
func unescapeTOML(raw string) string {
	if !strings.Contains(raw, `\`) {
		return raw
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' || i+1 == len(raw) {
			b.WriteByte(raw[i])
			continue
		}
		i++
		switch e := raw[i]; e {
		case 'b', 't', 'n', 'f', 'r':
			b.WriteByte("\b\t\n\f\r"[strings.IndexByte("btnfr", e)])
		case '"', '\\':
			b.WriteByte(e)
		case 'u', 'U':
			n := 4
			if e == 'U' {
				n = 8
			}
			if i+n < len(raw) {
				if r, err := strconv.ParseUint(raw[i+1:i+1+n], 16, 32); err == nil && utf8.ValidRune(rune(r)) {
					b.WriteRune(rune(r))
					i += n
					continue
				}
			}
			b.WriteByte('\\')
			b.WriteByte(e)
		case ' ', '\t', '\r', '\n': // a line-ending backslash trims to the next text
			for i < len(raw) && strings.IndexByte(" \t\r\n", raw[i]) >= 0 {
				i++
			}
			i--
		default:
			b.WriteByte('\\')
			b.WriteByte(e)
		}
	}
	return b.String()
}

// cmdToken is one word a configuration file may run or pass to a program.
// program marks a command's first word; bases are the folders earlier "cd"
// commands in the same shell string, and earlier working-directory flags in
// the same command, moved to, relative to its start.
type cmdToken struct {
	text    string
	program bool
	bases   []string
}

// commandSet is what one configuration file names: its tokens, and the
// working folders (cwd, dir) its commands may run in. capped reports that a
// shell string crossed maxCdBases; reading stopped there, so the set is
// incomplete.
type commandSet struct {
	tokens []cmdToken
	dirs   []string
	capped bool
}

// maxCdBases bounds the distinct "cd" and working-directory flag targets one
// shell string or argv records (rulings R8, R24). A real hook changes folder
// once or twice; with no bound a string of repeated "cd a;x;" grows the bases,
// and the resolution work with them, without limit (a 28 KB string took 99 s).
// Crossing it makes the file's command set incomplete.
const maxCdBases = 64

// maxCommandPaths bounds the work resolveCommandTokens does for one file
// (ruling R8): every resolution step costs one unit, or one per path when it
// builds more, so a step that yields nothing (a "$A" token tried against each
// start folder) is charged too, and every PATH lookup costs one. A real hook or MCP file builds a few hundred; 2,000 cwd and command
// pairs (69 KB) would build four million. Crossing it stops the work and
// reports the result incomplete.
const maxCommandPaths = 100_000

// shellState is what earlier commands in a shell string tell the reader about
// later ones: every "cd" target so far (bases) and the folder reached if every
// "cd" succeeded (cur). Both are relative to the shell string's start.
type shellState struct {
	bases []string
	cur   string
}

// wholeTokenMeta are the characters that make a "command" string shell
// syntax rather than one program path (ruling R2); space and tab are not, so
// "/opt/My Tools/server" stays one token. A "\" alone does not either: the
// value is kept whole with "\" folded to "/" (ruling R13), so the Windows
// path "tools\my server.exe" stays one token.
const wholeTokenMeta = "'\"\\$`;&|()<>\n\r*?["

// extractCommands collects the command tokens and working folders of a
// decoded JSON configuration file, reading values only under keys (see
// commandFileKeys), at any depth:
//   - an array under args: every element is an operand; when the object's
//     "command" is a shell program, a shell-command flag in it sends the next
//     element through shellCommands too;
//   - an array under argv or command: element 0 is the program, the rest
//     are operands, and the shell-command flag rule applies;
//   - a string under any other listed key is a shell string; a powershell
//     value has "\" folded to "/" first (R3), and a command value with no
//     wholeTokenMeta once "\" is folded to "/" is also one whole program
//     token (R2, R13);
//   - a string under cwd or dir, whatever the file's keys, is a working folder;
//   - a top-level env or permissions subtree, and the env of an object that
//     also holds a listed key with a string or array value, are skipped whole
//     (spec D4's exclusions, R14);
//   - --directory, -C, --cwd and --project, with their value as the next word
//     or after "=", make the value a base for the rest of that argv or shell
//     command, as "cd" does for the rest of a shell string (ruling R24).
func extractCommands(keys []string, doc any) commandSet {
	var cs commandSet
	cs.walkJSON(keys, doc, true)
	return cs
}

// walkJSON visits every object in v, reading the listed keys of each; top
// marks the document's own top-level value.
func (cs *commandSet) walkJSON(keys []string, v any, top bool) {
	listed := func(k string) bool {
		return slices.ContainsFunc(keys, func(want string) bool { return strings.EqualFold(k, want) })
	}
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			cs.walkJSON(keys, e, false)
		}
	case map[string]any:
		names := slices.Sorted(maps.Keys(v))
		// Only a listed key holding a command (a string or an array) makes a
		// sibling env the command's own; a server named "command" does not.
		ownsEnv := top || slices.ContainsFunc(names, func(k string) bool {
			switch v[k].(type) {
			case string, []any:
				return listed(k)
			}
			return false
		})
		for _, k := range names {
			if cs.capped {
				return
			}
			if strings.EqualFold(k, "env") && ownsEnv || strings.EqualFold(k, "permissions") && top {
				continue
			}
			e := v[k]
			if s, ok := e.(string); ok && (strings.EqualFold(k, "cwd") || strings.EqualFold(k, "dir")) {
				cs.dirs = append(cs.dirs, s)
			}
			if listed(k) {
				cs.readValue(strings.ToLower(k), e, v)
			}
			cs.walkJSON(keys, e, false)
		}
	}
}

// readValue adds the tokens of e, the value of the listed key k (lower case)
// in the object parent.
func (cs *commandSet) readValue(k string, e any, parent map[string]any) {
	switch e := e.(type) {
	case string:
		switch k {
		case "args", "argv":
			return
		case "powershell":
			e = strings.ReplaceAll(e, `\`, "/")
		case "command":
			if whole := strings.ReplaceAll(e, `\`, "/"); !strings.ContainsAny(whole, wholeTokenMeta) {
				cs.tokens = append(cs.tokens, cmdToken{text: whole, program: true})
			}
		}
		cs.shell(e, shellState{})
	case []any:
		var words []string
		for _, x := range e {
			if s, ok := x.(string); ok {
				words = append(words, s)
			}
		}
		switch k {
		case "args":
			cs.argv(words, false, shellKind(siblingCommand(parent)), shellState{})
		case "argv", "command":
			cs.argv(words, true, "", shellState{})
		}
	}
}

// siblingCommand returns the string "command" of an object, or "".
func siblingCommand(m map[string]any) string {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if s, ok := m[k].(string); ok && strings.EqualFold(k, "command") {
			return s
		}
	}
	return ""
}

// argv adds words as tokens, words[0] as the program when programFirst. A
// shell-command flag after a shell program (sh is the kind of one already
// seen) sends the next word through shell. A working-directory flag makes its
// value a base for the words after it, as "cd" does (ruling R24); it stops,
// marking cs capped, when that would cross maxCdBases.
func (cs *commandSet) argv(words []string, programFirst bool, sh string, st shellState) {
	st.bases = slices.Clip(st.bases) // appends here never write into the caller's tokens
	for i, w := range words {
		if cs.capped {
			return
		}
		cs.tokens = append(cs.tokens, cmdToken{text: w, program: programFirst && i == 0, bases: st.bases})
		if sh != "" && i+1 < len(words) && isShellFlag(sh, w) {
			cs.shell(words[i+1], st)
		}
		if k := shellKind(w); k != "" {
			sh = k
		}
		dir, ok := dirFlagValue(w)
		if !ok && i > 0 && slices.Contains(dirFlags, words[i-1]) {
			dir, ok = w, true
		}
		if ok && dir != "" {
			if st, ok = st.cd(dir); !ok {
				cs.capped = true
				return
			}
		}
	}
}

// dirFlags are the tool working-directory flags followed like "cd" (ruling
// R24): uv's --directory and --project, git's and make's -C, and --cwd (bun,
// yarn). Other tools' flags are a documented residual.
var dirFlags = []string{"--directory", "-C", "--cwd", "--project"}

// dirFlagValue returns X for a "--directory=X", "--cwd=X" or "--project=X"
// word.
func dirFlagValue(w string) (string, bool) {
	for _, f := range dirFlags {
		if v, ok := strings.CutPrefix(w, f+"="); ok && f != "-C" {
			return v, true
		}
	}
	return "", false
}

// shellKind classifies a program word by basename: "sh" for sh, bash and zsh,
// "cmd" for cmd, "ps" for powershell and pwsh (".exe" ignored), else "".
func shellKind(word string) string {
	base := strings.ToLower(path.Base(strings.ReplaceAll(word, `\`, "/")))
	switch strings.TrimSuffix(base, ".exe") {
	case "sh", "bash", "zsh":
		return "sh"
	case "cmd":
		return "cmd"
	case "powershell", "pwsh":
		return "ps"
	}
	return ""
}

// isShellFlag reports whether f makes a shell of kind sh run its next
// argument as a command: a single-dash cluster holding "c" (-c, -lc, -ec) for
// sh, "/c" for cmd, "-Command" or "-c" for PowerShell, in any case.
func isShellFlag(sh, f string) bool {
	switch sh {
	case "sh":
		return len(f) > 1 && f[0] == '-' && f[1] != '-' && strings.Contains(f[1:], "c")
	case "cmd":
		return strings.EqualFold(f, "/c")
	case "ps":
		return strings.EqualFold(f, "-command") || strings.EqualFold(f, "-c")
	}
	return false
}

// shell adds the tokens of shell string s, run in state st. Each command's
// first word is its program and the rest are operands; "cd X" (X its first
// argument that is not a "-" flag or cmd's "/d") makes X a base for later
// commands; the shell-command flag rule applies to each command's
// words; and a word holding "$(" or a backtick, which survives double quotes,
// is read again from there. A string holding "\" is also read with "\" folded
// to "/", so a Windows path in a command does not lose its separators. Both
// readings only add tokens.
func (cs *commandSet) shell(s string, st shellState) {
	st.bases = slices.Clip(st.bases) // appends here never write into the caller's tokens
	cs.shellOnce(s, st)
	if strings.Contains(s, `\`) {
		cs.shellOnce(strings.ReplaceAll(s, `\`, "/"), st)
	}
}

// shellOnce is one reading of shell; st is its own copy. It stops, marking cs
// capped, when a "cd" would cross maxCdBases.
func (cs *commandSet) shellOnce(s string, st shellState) {
	for _, words := range shellCommands(s) {
		if cs.capped {
			return
		}
		cs.argv(words, true, "", st)
		for _, w := range words {
			if i := substitutionStart(w); i >= 0 {
				cs.shell(w[i:], st)
			}
		}
		if words[0] != "cd" {
			continue
		}
		if i := slices.IndexFunc(words[1:], func(a string) bool { return a != "" && a[0] != '-' && !strings.EqualFold(a, "/d") }); i >= 0 {
			var ok bool
			if st, ok = st.cd(words[1+i]); !ok {
				cs.capped = true
				return
			}
		}
	}
}

// cd returns the state after "cd x", x non-empty. A relative target is
// recorded both as given and joined to the folder reached so far, since an
// earlier "cd" may have failed; a base already recorded is not added again.
// ok is false when a new base would cross maxCdBases.
func (st shellState) cd(x string) (_ shellState, ok bool) {
	next := x
	if st.cur != "" && !strings.ContainsAny(x[:1], `/\$%~`) && !filepath.IsAbs(x) {
		next = path.Join(st.cur, x)
	}
	for _, b := range []string{x, next} {
		if slices.Contains(st.bases, b) {
			continue
		}
		if len(st.bases) == maxCdBases {
			return st, false
		}
		st.bases = append(st.bases, b)
	}
	st.cur = next
	return st, true
}

// substitutionStart returns the index just past the first "$(" or backtick in
// w, or -1.
func substitutionStart(w string) int {
	i := strings.Index(w, "$(")
	if i >= 0 {
		i += 2
	}
	if j := strings.IndexByte(w, '`'); j >= 0 && (i < 0 || j+1 < i) {
		i = j + 1
	}
	return i
}

// shellCommands splits a shell string into simple commands, each a list of
// words with quotes and escapes removed. It is lenient and never fails: single
// quotes, double quotes (with \" \\ \$ \` and line continuation) and
// backslash escapes are honored; unquoted ; & | ( ), backticks and newlines
// end a command; an unterminated quote runs to the end. A redirect operator
// (< > >> 2> &> >& <<< and the like) only ends a word: the word after it is an
// operand of the current command, never its program (ruling R10). A target met
// before the program is moved after the command's other words, and a command
// of redirects alone gets an empty program word.
func shellCommands(s string) [][]string {
	var cmds [][]string
	var words, targets []string // targets: redirect targets met before the program
	var w strings.Builder
	inWord, redirect := false, false
	endWord := func() {
		if !inWord {
			return
		}
		if redirect && len(words) == 0 {
			targets = append(targets, w.String())
		} else {
			words = append(words, w.String())
		}
		w.Reset()
		inWord, redirect = false, false
	}
	endCommand := func() {
		endWord()
		if len(words) == 0 && len(targets) > 0 {
			words = []string{""}
		}
		if words = append(words, targets...); len(words) > 0 {
			cmds = append(cmds, words)
		}
		words, targets, redirect = nil, nil, false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			inWord = true
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				end = len(s) - i - 1
			}
			w.WriteString(s[i+1 : i+1+end])
			i += end + 1
		case c == '"':
			inWord = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					if i++; s[i] == '\n' {
						continue
					}
				}
				w.WriteByte(s[i])
			}
		case c == '\\':
			if i+1 == len(s) {
				inWord = true
				w.WriteByte(c)
			} else if i++; s[i] != '\n' {
				inWord = true
				w.WriteByte(s[i])
			}
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
		case c == '<' || c == '>' || c == '&' && i+1 < len(s) && s[i+1] == '>':
			endWord()
			for i+1 < len(s) && strings.IndexByte("<>&|", s[i+1]) >= 0 {
				i++
			}
			redirect = true
		case strings.IndexByte(";&|()`\n", c) >= 0:
			endCommand()
		default:
			inWord = true
			w.WriteByte(c)
		}
	}
	endCommand()
	return cmds
}

// extractTOMLCommands turns the strings of a TOML file (.codex/config.toml)
// into a commandSet: every value is one whole operand token and also a shell
// string, a value whose key is cwd is also a working folder, and the elements
// of each array are also one argv of operands, so a working-directory flag in
// args bases the elements after it (ruling R24).
func extractTOMLCommands(strs []tomlString) commandSet {
	var cs commandSet
	var arrays [][]string // arrays[n-1] holds the elements of array n
	for _, s := range strs {
		if cs.capped {
			return cs
		}
		if s.array == 0 {
			cs.tokens = append(cs.tokens, cmdToken{text: s.value})
		} else {
			for len(arrays) < s.array {
				arrays = append(arrays, nil)
			}
			arrays[s.array-1] = append(arrays[s.array-1], s.value)
		}
		cs.shell(s.value, shellState{})
		if strings.EqualFold(s.key, "cwd") {
			cs.dirs = append(cs.dirs, s.value)
		}
	}
	for _, words := range arrays {
		cs.argv(words, false, "", shellState{}) // adds each element as a token
	}
	return cs
}

// projectDirVars are the leading spellings that name the project folder; each
// resolves to every projectDirs entry (spec D4).
//   - CLAUDE_PROJECT_DIR: "the project root where the session started",
//     https://code.claude.com/docs/en/hooks
//   - workspaceFolder: VS Code's and Cursor's project folder,
//     https://code.visualstudio.com/docs/reference/variables-reference and
//     https://cursor.com/docs/context/mcp
//   - GEMINI_PROJECT_DIR: "The absolute path to the project root",
//     https://geminicli.com/docs/hooks/
//   - CURSOR_PROJECT_DIR: "Workspace root directory",
//     https://cursor.com/docs/agent/hooks
var projectDirVars = []string{
	"$CLAUDE_PROJECT_DIR", "${CLAUDE_PROJECT_DIR}", "${workspaceFolder}",
	"$GEMINI_PROJECT_DIR", "${GEMINI_PROJECT_DIR}",
	"$CURSOR_PROJECT_DIR", "${CURSOR_PROJECT_DIR}",
}

// resolveCommandTokens returns the absolute, cleaned, sorted and deduplicated
// paths that cs's tokens may name, and whether that set is complete. bases are
// the folders relative tokens resolve against (spec D4: the anchor and the
// file's folder); projectDirs are what a project-folder variable means;
// lookPath finds a bare program on PATH and returns an absolute path. For each
// token:
//   - a "-flag=value" token is replaced by its value; then tokens starting
//     with "-" or holding "://" are dropped;
//   - a program token with no separator and no leading "$", "%" or "~" counts
//     only as its lookPath hit (ruling R1);
//   - anything else resolves through resolveUnder against every base, every
//     base joined with every cs.dirs value, and each of those joined with the
//     token's own "cd" bases. Glob characters are literal.
//
// complete is false, with no paths, when cs is capped or the work crosses
// maxCommandPaths; the work stops at that point (ruling R8), and the caller
// treats the reference set as incomplete.
func resolveCommandTokens(cs commandSet, bases []string, projectDirs []string, lookPath func(name string) (string, bool)) (paths []string, complete bool) {
	if cs.capped {
		return nil, false
	}
	budget := maxCommandPaths
	// grow appends resolveUnder(start, tok) to *dst, charging one unit per
	// path and at least one per call; false once over budget.
	grow := func(dst *[]string, start, tok string) bool {
		r := resolveUnder(start, tok, projectDirs)
		if budget -= max(1, len(r)); budget < 0 {
			return false
		}
		*dst = append(*dst, r...)
		return true
	}
	dirs := slices.Compact(slices.Sorted(slices.Values(cs.dirs)))
	starts := slices.Clone(bases)
	for _, b := range bases {
		for _, d := range dirs {
			if !grow(&starts, b, d) {
				return nil, false
			}
		}
	}
	slices.Sort(starts)
	starts = slices.Compact(starts)
	looked := map[string]bool{}
	for _, tk := range cs.tokens {
		text := tk.text
		if strings.HasPrefix(text, "-") {
			_, text, _ = strings.Cut(text, "=")
		}
		if text == "" || text[0] == '-' || strings.Contains(text, "://") {
			continue
		}
		if tk.program && !strings.ContainsAny(text, `/\`) && !strings.ContainsAny(text[:1], "$%~") {
			if !looked[text] {
				looked[text] = true
				if budget--; budget < 0 {
					return nil, false
				}
				if hit, ok := lookPath(text); ok {
					paths = append(paths, filepath.Clean(hit))
				}
			}
			continue
		}
		from := starts
		if len(tk.bases) > 0 {
			from = slices.Clone(starts)
			for _, cd := range tk.bases {
				for _, s := range starts {
					if !grow(&from, s, cd) {
						return nil, false
					}
				}
			}
		}
		for _, s := range from {
			if !grow(&paths, s, text) {
				return nil, false
			}
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths), true
}

// resolveUnder resolves one path token against the folder start. A leading
// project-folder variable yields one path per projectDirs entry. Any other
// leading "$" or "%" form, and a leading "~" (ruling R12), keeps what follows
// its first separator (a residual of spec D4); with nothing there the token
// names nothing. An absolute token stands as is; a relative one is joined to
// start.
func resolveUnder(start, tok string, projectDirs []string) []string {
	if tok == "" {
		return nil
	}
	for _, v := range projectDirVars {
		if rest, ok := strings.CutPrefix(tok, v); ok && (rest == "" || rest[0] == '/' || rest[0] == '\\') {
			out := make([]string, 0, len(projectDirs))
			for _, d := range projectDirs {
				out = append(out, filepath.Join(d, rest))
			}
			return out
		}
	}
	if tok[0] == '$' || tok[0] == '%' || tok[0] == '~' {
		i := strings.IndexAny(tok, `/\`)
		if i < 0 {
			return nil
		}
		if tok = strings.TrimLeft(tok[i:], `/\`); tok == "" {
			return nil
		}
	}
	if filepath.IsAbs(tok) {
		return []string{filepath.Clean(tok)}
	}
	return []string{filepath.Join(start, tok)}
}
