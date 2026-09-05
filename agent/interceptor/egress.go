package interceptor

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
)

// Egress classifies exec-class tool calls by argv shape (#439 D5): what the
// command visibly reaches, not what it says or does. Findings are tags; the
// badge on the approval prompt is human information and the sandbox is the
// boundary. A class states capability, never actual traffic or intent, and
// a call with no finding is not thereby offline or safe. Command names are
// hints: nothing here resolves an executable.
type Egress struct{}

var _ agent.Interceptor = Egress{}

// Egress classes: the Finding.Rule values, in priority order.
const (
	EgressPrivileged     = "privileged"
	EgressNetwork        = "network"
	EgressPackageManager = "package-manager"
	EgressInterpreter    = "interpreter"
	EgressUnknown        = "unknown"
)

// Risk contributions per class: informational policy weights.
const (
	EgressNetworkRisk        = 20
	EgressPrivilegedRisk     = 20
	EgressPackageManagerRisk = 10
	EgressUnknownRisk        = 10
	EgressInterpreterRisk    = 0
)

// unknownLabelBytes caps the model-authored bytes that reach a badge.
const unknownLabelBytes = 24

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// Exact v1 classification data (spec). A change here is a policy change with
// a test. Names are matched after commandName normalization.
var (
	networkBins = set("curl", "wget", "nc", "ncat", "netcat", "ssh", "scp", "sftp", "rsync",
		"telnet", "ftp", "socat", "aria2c", "http", "https", "docker", "podman", "kubectl",
		"helm", "terraform", "gh", "gcloud", "aws", "az")
	privilegedBins = set("sudo", "doas", "su")
	packageBins    = set("npm", "npx", "yarn", "pnpm", "bun", "pip", "pip3", "pipx", "uv", "poetry",
		"conda", "cargo", "gem", "bundle", "composer", "brew", "apt", "apt-get", "dnf", "yum",
		"pacman", "apk", "nix", "mvn", "gradle")
	shellNames       = set("sh", "bash", "zsh", "dash", "ksh", "fish")
	interpreterNames = set("python", "python3", "perl", "ruby", "node", "deno", "php")
	gitNetworkSubs   = set("push", "fetch", "pull", "clone", "ls-remote", "remote", "submodule")
	gitQuietSubs     = set("status", "diff", "log", "show", "rev-parse", "rev-list", "ls-files", "ls-tree",
		"cat-file", "check-ignore", "check-attr", "describe", "branch", "tag", "config", "help", "version")
	gitValued     = set("-C", "-c", "--git-dir", "--work-tree", "--namespace")
	gitBare       = set("--no-pager", "--paginate")
	goPackageSubs = set("get", "install", "mod")
	goQuietSubs   = set("build", "test", "run", "fmt", "vet", "list", "env", "version", "doc", "help")
	goValued      = set("-C")
	quietNames    = set(
		"ls", "cat", "head", "tail", "wc", "grep", "rg", "ag", "find", "fd", "sed", "awk", "sort",
		"uniq", "cut", "tr", "diff", "cmp", "echo", "printf", "true", "false", "test", "pwd",
		"mkdir", "rmdir", "cp", "mv", "rm", "touch", "chmod", "ln", "stat", "file", "which",
		"date", "sleep", "tee", "basename", "dirname", "realpath", "readlink", "jq", "yq",
		"tar", "gzip", "gunzip", "zip", "unzip", "make", "cmake", "ninja",
		"gofmt", "goimports", "golangci-lint", "staticcheck", "gcc", "clang", "cc", "rustc",
		"tsc", "eslint", "prettier", "black", "ruff", "mypy", "pytest", "jest", "vitest")
)

// wrapperGrammar is one wrapper's supported, argv-preserving option forms
// (spec matrix). Anything outside it stops peeling and classifies unknown, so
// an unmodeled transformation can never make a call quiet.
type wrapperGrammar struct {
	bare        map[string]bool // options with no value
	valued      map[string]bool // options consuming the next argument; long forms also accept --name=value
	assignments bool            // env: NAME=VALUE operands before the command
	duration    bool            // timeout: exactly one duration operand after the options
}

var wrapperGrammars = map[string]wrapperGrammar{
	"env":     {bare: set("-i", "--ignore-environment"), valued: set("-u", "--unset", "-C", "--chdir"), assignments: true},
	"nohup":   {},
	"nice":    {valued: set("-n", "--adjustment")},
	"time":    {bare: set("-p")},
	"timeout": {bare: set("--foreground", "--preserve-status", "--verbose"), valued: set("-k", "--kill-after", "-s", "--signal"), duration: true},
	"stdbuf":  {valued: set("-i", "-o", "-e")},
}

var (
	envAssignment   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	timeoutDuration = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[smhd]?$`)
)

// commandName is the normalized name hint for an argv word: the native
// basename, lower-cased, without a trailing .exe. It attests nothing about
// the executable that will actually run.
func commandName(arg string) string {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(arg)), ".exe")
}

// rawBase is the unnormalized basename used for unknown labels; an empty
// word stays empty rather than becoming ".".
func rawBase(arg string) string {
	if arg == "" {
		return ""
	}
	return filepath.Base(arg)
}

// quoteLabel bounds model-authored text for a badge: at most
// unknownLabelBytes on a rune boundary, then quoted with escapes.
func quoteLabel(s string) string {
	if len(s) > unknownLabelBytes {
		n := unknownLabelBytes
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return strconv.Quote(s)
}

// peelStatus reports how wrapper peeling ended.
type peelStatus int

const (
	peelOK          peelStatus = iota
	peelUnsupported            // an option or operand outside the wrapper's grammar
	peelMissing                // the wrapper had no command left to run
)

// peelWrappers strips leading wrapper commands whose forms are in the
// matrix and returns the argv they would run. The input is never modified.
// On an unsupported form or a missing command it names the wrapper so the
// caller can report the uncertainty instead of guessing.
func peelWrappers(argv []string) (rest []string, wrapper string, status peelStatus) {
	for len(argv) > 0 {
		name := commandName(argv[0])
		g, ok := wrapperGrammars[name]
		if !ok {
			return argv, "", peelOK
		}
		rest, status := stripWrapperOptions(g, argv[1:])
		if status != peelOK {
			return nil, name, status
		}
		if len(rest) == 0 {
			return nil, name, peelMissing
		}
		argv = rest
	}
	return argv, "", peelOK
}

// stripWrapperOptions consumes one wrapper's options and operands per its
// grammar and returns the child argv.
func stripWrapperOptions(g wrapperGrammar, args []string) ([]string, peelStatus) {
	i := 0
scan:
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			i++
			break scan
		case strings.HasPrefix(a, "--"):
			name, _, hasEq := strings.Cut(a, "=")
			switch {
			case hasEq && g.valued[name]:
			case !hasEq && g.valued[name]:
				if i+1 >= len(args) {
					return nil, peelUnsupported
				}
				i++
			case !hasEq && g.bare[name]:
			default:
				return nil, peelUnsupported
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			switch {
			case g.valued[a]:
				if i+1 >= len(args) {
					return nil, peelUnsupported
				}
				i++
			case g.bare[a]:
			default:
				return nil, peelUnsupported // clusters, attached values, unknown short options
			}
		case g.assignments && strings.Contains(a, "="):
			if !envAssignment.MatchString(a) {
				return nil, peelUnsupported
			}
		default:
			break scan
		}
	}
	args = args[i:]
	if g.duration {
		if len(args) == 0 || !timeoutDuration.MatchString(args[0]) {
			return nil, peelUnsupported
		}
		args = args[1:]
		// GNU getopt permutes, so "timeout 10 -- cmd" is the same command.
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
	}
	return args, peelOK
}

// Shell recognition (spec matrix). This is a finite recognizer over literal
// words, not shell evaluation: it accepts only the forms it can interpret
// without expansion and reports everything else as unsupported, so the
// invariant never hard-blocks on text it did not actually understand.
var (
	// inlineShells are the shells recognized both as an outer command running
	// an inline script under the exact options below and as a bare stdin
	// sink; the spec lists the same five for both roles.
	inlineShells = set("sh", "bash", "dash", "ksh", "zsh")
	inlineFlags  = set("-c", "-lc", "-ec", "-euc")
	bashPreamble = set("--norc", "--noprofile")
	// curlFetchFlags are the only curl options a recognized stdout fetch may
	// carry, singly or clustered.
	curlFetchFlags = regexp.MustCompile(`^-[fsSL]+$`)
	// shellUnsupported lists the unquoted characters whose meaning needs
	// evaluation or a second command: control operators, redirections,
	// expansions, globs, braces and tilde.
	shellUnsupported = ";&()<>\n$`\\*?[]{}~"
)

// inlineShellScript recognizes an outer shell in command-execution mode:
// sh/bash/dash/ksh/zsh, for bash optionally --norc/--noprofile first, then
// exactly one of -c, -lc, -ec, -euc and the script operand. Arguments after
// the script are the script's own and are not interpreted.
func inlineShellScript(argv []string) (shell, flag, script string, ok bool) {
	if len(argv) == 0 {
		return "", "", "", false
	}
	shell = commandName(argv[0])
	if !inlineShells[shell] {
		return "", "", "", false
	}
	i := 1
	for shell == "bash" && i < len(argv) && bashPreamble[argv[i]] {
		i++
	}
	if i+1 >= len(argv) || !inlineFlags[argv[i]] {
		return "", "", "", false
	}
	return shell, argv[i], argv[i+1], true
}

// splitShellWords tokenizes a script into simple commands of literal words:
// unquoted whitespace separates words, an unquoted pipe separates commands,
// single quotes are literal, double quotes are literal unless they contain
// $, backtick or backslash, and a # at a word start comments out the rest
// of the script. Trailing blanks and newlines are ignored: they change
// nothing about what runs and need no evaluation. Any other unquoted
// metacharacter (an interior newline included), an unterminated quote, an
// empty command, or a comment that is followed by more script lines makes
// the script unsupported.
func splitShellWords(script string) (cmds [][]string, ok bool) {
	script = strings.TrimRight(script, " \t\n")
	var (
		cur     []string
		word    []rune
		inWord  bool
		quote   rune // 0, '\'' or '"'
		endWord = func() {
			if inWord {
				cur = append(cur, string(word))
				word, inWord = word[:0], false
			}
		}
	)
	rs := []rune(script)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				word = append(word, r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '$', '`', '\\':
				return nil, false
			default:
				word = append(word, r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			endWord()
		case r == '|':
			endWord()
			if len(cur) == 0 {
				return nil, false
			}
			cmds, cur = append(cmds, cur), nil
		case r == '#' && !inWord:
			if slices.Contains(rs[i:], '\n') {
				return nil, false
			}
			i = len(rs)
		case strings.ContainsRune(shellUnsupported, r):
			return nil, false
		default:
			word, inWord = append(word, r), true
		}
	}
	if quote != 0 {
		return nil, false
	}
	endWord()
	if len(cur) == 0 {
		return nil, false
	}
	return append(cmds, cur), true
}

// commandWords drops the leading NAME=VALUE assignment words of a simple
// command: in shell grammar they set the command's environment and the
// first remaining word is what runs.
func commandWords(words []string) []string {
	for len(words) > 0 && envAssignment.MatchString(words[0]) {
		words = words[1:]
	}
	return words
}

// recognizeFetch reports a simple command that delivers a remote fetch on
// stdout: curl with only -f/-s/-S/-L (singly or clustered), or wget with
// optional -q and the stdout marker -O -, -O-, -qO- or -qO -; either may
// use -- and must have exactly one literal operand. Other clusters are not
// parsed.
func recognizeFetch(words []string) (name string, ok bool) {
	words = commandWords(words)
	if len(words) == 0 {
		return "", false
	}
	name = commandName(words[0])
	operands, stdout, dashdash := 0, false, false
	for i := 1; i < len(words); i++ {
		w := words[i]
		switch {
		case dashdash, w == "-", !strings.HasPrefix(w, "-"):
			operands++
		case w == "--":
			dashdash = true
		case name == "curl" && curlFetchFlags.MatchString(w):
		case name == "wget" && w == "-q":
		case name == "wget" && (w == "-O-" || w == "-qO-"):
			stdout = true
		case name == "wget" && (w == "-O" || w == "-qO") && i+1 < len(words) && words[i+1] == "-":
			stdout, i = true, i+1
		default:
			return "", false
		}
	}
	switch name {
	case "curl":
		return name, operands == 1
	case "wget":
		return name, stdout && operands == 1
	}
	return "", false
}

// recognizeSink reports a simple command that executes its stdin: a bare
// sh/bash/dash/ksh/zsh, optionally with -s, optionally preceded by a bare
// sudo. The label names what would run.
func recognizeSink(words []string) (label string, ok bool) {
	words = commandWords(words)
	i := 0
	if len(words) > 0 && commandName(words[0]) == "sudo" {
		label, i = "sudo ", 1
	}
	if i >= len(words) {
		return "", false
	}
	shell := commandName(words[i])
	if !inlineShells[shell] {
		return "", false
	}
	rest := words[i+1:]
	if len(rest) > 1 || (len(rest) == 1 && rest[0] != "-s") {
		return "", false
	}
	return label + shell, true
}

// egressClass is one classification: the rule, its risk, and the badge label.
type egressClass struct {
	rule  string
	risk  int
	label string
}

func unknownClass(label string) (egressClass, bool) {
	return egressClass{EgressUnknown, EgressUnknownRisk, label}, true
}

// classify returns the class of an argv, or false for an explicitly quiet
// command. Priority: privileged, network, package-manager, interpreter,
// unknown. A recognized inline shell script contributes network evidence
// from the command position of its literal simple commands, one level deep.
func classify(argv []string) (egressClass, bool) {
	return classifyCommand(argv, true)
}

// scriptEvidence is what an inline script contributes to classification.
type scriptEvidence int

const (
	scriptNone        scriptEvidence = iota // not an inline script, or readable with no network command
	scriptUnsupported                       // an inline script the recognizer cannot read
	scriptNetwork                           // a readable script with a network command in command position
)

// scriptNetworkEvidence inspects a recognized inline script. A readable
// script whose simple commands include one that classifies as network on
// its own (a direct client, or a network git subcommand) yields that label;
// a script the tokenizer cannot read is reported as such so the caller can
// keep the uncertainty visible instead of dropping to interpreter; anything
// else contributes nothing.
func scriptNetworkEvidence(rest []string) (label string, ev scriptEvidence) {
	shell, flag, script, ok := inlineShellScript(rest)
	if !ok {
		return "", scriptNone
	}
	cmds, ok := splitShellWords(script)
	if !ok {
		return strconv.Quote(shell+" "+flag) + " unsupported script", scriptUnsupported
	}
	for _, c := range cmds {
		if cls, ok := classifyCommand(commandWords(c), false); ok && cls.rule == EgressNetwork {
			return cls.label + " via " + shell + " " + flag, scriptNetwork
		}
	}
	return "", scriptNone
}

func classifyCommand(argv []string, withScript bool) (egressClass, bool) {
	rest, wrapper, status := peelWrappers(argv)
	switch status {
	case peelUnsupported:
		return unknownClass(strconv.Quote(wrapper) + " unsupported form")
	case peelMissing:
		return unknownClass(strconv.Quote(wrapper) + " missing command")
	}
	if len(rest) == 0 {
		return unknownClass(`""`)
	}
	name := commandName(rest[0])
	args := rest[1:]
	switch {
	case privilegedBins[name]:
		return egressClass{EgressPrivileged, EgressPrivilegedRisk, name}, true
	case networkBins[name]:
		return egressClass{EgressNetwork, EgressNetworkRisk, name}, true
	case name == "git":
		return classifySubcommand(gitGrammar, args)
	case name == "go":
		return classifySubcommand(goGrammar, args)
	case packageBins[name]:
		return egressClass{EgressPackageManager, EgressPackageManagerRisk, name}, true
	case shellNames[name]:
		if withScript {
			switch label, ev := scriptNetworkEvidence(rest); ev {
			case scriptNetwork:
				return egressClass{EgressNetwork, EgressNetworkRisk, label}, true
			case scriptUnsupported:
				return unknownClass(label)
			}
		}
		return egressClass{EgressInterpreter, EgressInterpreterRisk, name}, true
	case interpreterNames[name]:
		if strings.HasPrefix(name, "python") && len(args) >= 2 && args[0] == "-m" && args[1] == "pip" {
			return egressClass{EgressPackageManager, EgressPackageManagerRisk, name + " -m pip"}, true
		}
		return egressClass{EgressInterpreter, EgressInterpreterRisk, name}, true
	case quietNames[name]:
		return egressClass{}, false
	}
	return unknownClass(quoteLabel(rawBase(rest[0])))
}

// subcommandGrammar describes a tool whose first operand selects behavior
// (git, go): its supported global options before that operand, the
// subcommands that classify and the ones that are quiet.
type subcommandGrammar struct {
	tool          string
	valued, bare  map[string]bool // global options; valued long forms accept --name=value
	eqOnShort     bool            // valued short options also accept -X=value (go's flag parser does, git's does not)
	informational map[string]bool // standalone options that only print and are quiet
	classified    map[string]bool
	quiet         map[string]bool
	rule          string
	risk          int
}

var (
	gitGrammar = subcommandGrammar{tool: "git", valued: gitValued, bare: gitBare,
		informational: set("--version", "--help"), classified: gitNetworkSubs, quiet: gitQuietSubs,
		rule: EgressNetwork, risk: EgressNetworkRisk}
	goGrammar = subcommandGrammar{tool: "go", valued: goValued, eqOnShort: true,
		classified: goPackageSubs, quiet: goQuietSubs, rule: EgressPackageManager, risk: EgressPackageManagerRisk}
)

// classifySubcommand handles git and go: supported global options, then the
// first operand as the subcommand. A classified subcommand gets the grammar's
// rule with the label "<tool> <sub>"; a quiet one no finding; anything else,
// including a missing subcommand or an unsupported global option, is unknown,
// because aliases and external subcommands can run anything.
func classifySubcommand(g subcommandGrammar, args []string) (egressClass, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			switch {
			case g.classified[a]:
				return egressClass{g.rule, g.risk, g.tool + " " + a}, true
			case g.quiet[a]:
				return egressClass{}, false
			}
			return unknownClass(quoteLabel(g.tool + " " + a))
		}
		if g.informational[a] {
			return egressClass{}, false
		}
		name, _, hasEq := strings.Cut(a, "=")
		switch {
		case hasEq && g.valued[name] && (g.eqOnShort || strings.HasPrefix(name, "--")):
		case !hasEq && g.valued[a]:
			if i+1 >= len(args) {
				return unknownClass(strconv.Quote(g.tool))
			}
			i++
		case !hasEq && g.bare[a]:
		default:
			return unknownClass(strconv.Quote(g.tool))
		}
	}
	return unknownClass(strconv.Quote(g.tool))
}

// Name returns "egress".
func (Egress) Name() string { return "egress" }

// InspectInput returns nothing: egress is about the command, not content.
func (Egress) InspectInput(context.Context, agent.InputInspection) ([]agent.Finding, error) {
	return nil, nil
}

// InspectOutput returns nothing: model output policy belongs to #438/#437.
func (Egress) InspectOutput(context.Context, agent.OutputInspection) ([]agent.Finding, error) {
	return nil, nil
}

// InspectToolCall emits at most one tag finding for an exec-class call whose
// argv member (found with the tool decoder's own name equivalence) decodes
// as the exec tools decode it: a string array, a null element being an empty
// string. An ambiguous argv spelling stays visible as unknown; the
// invariant on the same field blocks it.
func (Egress) InspectToolCall(_ context.Context, call agent.ToolCallInspection) ([]agent.Finding, error) {
	if !call.Effect.Class.Has(agent.Exec) {
		return nil, nil
	}
	members, ok := topLevelMembers(call.Call.Function.Arguments)
	if !ok {
		return nil, nil
	}
	tg := toolCallTarget(call.Call.ID)
	raw, n := findMember(members, "argv")
	switch {
	case n == 0:
		return nil, nil
	case n > 1:
		return []agent.Finding{tg.finding(EgressUnknown, agent.VerdictTag, EgressUnknownRisk, `"argv" ambiguous`)}, nil
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil || len(argv) == 0 {
		return nil, nil
	}
	c, ok := classify(argv)
	if !ok {
		return nil, nil
	}
	return []agent.Finding{tg.finding(c.rule, agent.VerdictTag, c.risk, c.label)}, nil
}
