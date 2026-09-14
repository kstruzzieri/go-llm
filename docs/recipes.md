# Reusable recipes

The `recipe` package validates and expands versioned JSON prompt bundles.
Golem loads these bundles as user-defined slash commands in its interactive
REPL. Recipe text becomes an ordinary user goal under the existing trust,
ingress inspection, approval, persistence, and cancellation rules.

Files conventionally use the name `<name>.recipe.json`. `Load` accepts any
explicit path; it does not enforce the extension or require the filename to
match the recipe name.

```go
r, err := recipe.Load("review-change.recipe.json")
if err != nil {
	return err
}
fmt.Println(r.Name)
```

Recipes can also be compiled into a consumer and passed directly to `Parse`:

```go
import (
	_ "embed"

	"github.com/kstruzzieri/go-llm/recipe"
)

//go:embed recipes/review-change.recipe.json
var reviewChange []byte

func loadReviewChange() (recipe.Recipe, error) {
	return recipe.Parse(reviewChange)
}
```

## Version 1 format

A complete version 1 document looks like this:

```json
{
  "version": 1,
  "name": "review-change",
  "description": "Review a change for actionable defects.",
  "goal": "Review {{inputs.target}}. Focus on {{inputs.focus}}.",
  "context": "Report defects with file references and explain their impact.",
  "inputs": [
    {"name": "target", "description": "Change or path to review"},
    {"name": "focus", "description": "Review emphasis", "default": "correctness"}
  ],
  "model_hint": {"use_case": "code-review"}
}
```

| Field | Required | Meaning |
|---|---:|---|
| `version` | yes | Integer `1`. This versions the format; Git can version recipe content. |
| `name` | yes | Identity matching `[a-z][a-z0-9_-]*`, without a leading slash. |
| `description` | yes | Nonblank task description for consumers such as help listings. |
| `goal` | yes | Nonblank template-bearing text. Markdown and workflow instructions remain prose. |
| `context` | no | Supporting template-bearing text with the same trust level as `goal`. |
| `inputs` | no | Ordered input declarations. Omitted, `null`, and `[]` all mean no declarations. |
| `model_hint` | no | Advisory routing key. Omitted or `null` means no hint. |

Each input has a required `name` matching `[a-z][a-z0-9_]*`; names must be
unique. Its `description` is optional literal text. A missing or `null`
`default` means the caller must supply the input. Any string default makes the
input optional, including an explicitly empty `"default": ""`. Input order is
preserved, unused declarations are valid, and there is no `required` field.
Missing or `null` optional string fields decode to an empty string. Missing or
`null` required metadata fails validation, and a `null` input-array element
fails its indexed input-name validation.

A present `model_hint` contains exactly one nonblank `role` or `use_case`.
Unicode control (`Cc`) and format (`Cf`) characters, including bidi overrides
and zero-width joiners, are rejected in hints. Other spelling, case, and
whitespace are preserved exactly, and the vocabulary is open. This restriction
applies only to routing hints; authored prose is preserved. The hint is advisory:
a caller may ignore it. A role is only a configured role key and a use case is
only a configured use-case key;
neither selects a provider, endpoint, or model directly.

Version 1 has a closed schema. `$schema`, `provider`, `model`, `endpoint`,
`tools`, `permissions`, `system`, `commands`, `steps`, `includes`, and other
unknown fields are rejected. `$schema` is not accepted in version 1.

## Decoding rules

`Parse` accepts at most 65,536 raw bytes. The limit includes trailing
whitespace. It rejects invalid raw UTF-8, an empty or JSON-whitespace-only
document, a non-object root, malformed JSON, duplicate decoded object keys,
case variants of field names, unknown or misplaced fields, incompatible field
types, and any value after the root object. Duplicate checks apply separately
to every object and compare keys after JSON unescaping.

Here JSON whitespace means space, tab, carriage return, and line feed; other
Unicode whitespace is parsed as input rather than treated as an empty document.
JSON comments, byte-order marks, and trailing commas are not supported. The
standard library performs JSON string decoding, including replacing an escaped
unpaired surrogate with U+FFFD; accepted decoded strings are otherwise
preserved exactly. Trimming is used only to decide whether required text and
hints are blank. `Parse` and `Load` return a zero `Recipe` on every error. I/O and JSON
errors are wrapped, so callers can use `errors.Is` and `errors.As`; diagnostics
do not include goal, context, or default values. Containers in any scalar field
are rejected before their contents are inspected. Only `inputs` arrays, their object entries, and `model_hint` objects
are supported containers.

## Template expansion

Only `goal` and `context` are template-bearing. Descriptions and defaults are
literal data. `Parse` and `Load` validate the format; `ValidateTemplates`
checks active references without requiring input values. `Expand` validates
metadata, supplied input names and UTF-8, required/default binding, and both
templates, returning `(goal, context, error)`. Both strings are empty on every
error. Its positive `maxBytes` limit bounds their combined expanded bytes;
checked appends reject overflow before allocating an oversized expansion.

A reference is exactly `{{inputs.name}}`, where `name` is a declared,
case-sensitive input name matching `[a-z][a-z0-9_]*`. It contains no whitespace.
Count consecutive backslashes immediately before `{{inputs.` in the
JSON-decoded text. Emit one literal backslash for every pair. An even number of
backslashes activates the reference; an odd remainder quotes the opening
marker. A quoted marker is ordinary text and needs neither a declared name nor
closing braces, and scanning resumes after that marker. Backslashes elsewhere
are unchanged. These counts apply after JSON decoding, so each backslash must
be doubled again in a JSON string literal.

With input `x` supplied as `VALUE`:

| Decoded template text | Rendered result |
|---|---|
| `{{inputs.x}}` | `VALUE` |
| `\{{inputs.x}}` | `{{inputs.x}}` |
| `\\{{inputs.x}}` | `\VALUE` |
| `\\\{{inputs.x}}` | `\{{inputs.x}}` |
| `{{ inputs.x }}` or `{{Inputs.x}}` | Unchanged literal text |

References are resolved once from left to right. Repeated references are valid.
Supplied values and defaults are inserted literally and are never rescanned.
An omitted input without a default, an unknown supplied input, a malformed
reserved `{{inputs.` construct, or an undeclared active reference is an
invocation-validation error. An explicitly supplied empty string counts as
supplied. Other brace text remains literal. The language has no expressions,
conditions, loops, filters, positional tokens, recursive substitution, or
environment and file access.

## Golem recipe commands

Place immediate `<name>.recipe.json` files in
`filepath.Join(os.UserConfigDir(), "go-llm", "commands")`. On macOS this is
`~/Library/Application Support/go-llm/commands/`; on Linux it follows the
standard XDG config directory. The directory is independent of the workspace
and `models.json` path. Golem does not create it automatically. There is no
workspace recipe search, recursive discovery, path override, or file watcher.
An absent or empty directory is silent and loads no commands. Unavailable or
unreadable directories produce a startup diagnostic on stderr; built-ins stay
usable. Recipes are not discovered for `-p`, `-goal`, or machine output modes.

The JSON `name` is the command identity and need not match the filename.
Only the exact `.recipe.json` suffix is selected. Directory symlinks are
accepted, including a `go-llm` or `commands` directory in user-managed dotfiles.
Individual candidate symlinks and nonregular files are rejected. All existing
built-ins and aliases, including `/recipes`, are reserved. All files claiming
a duplicate name are excluded, including a parsed claimant with an invalid
template. An exact loaded `/recipe` is allowed; otherwise `/recipe` suggests
`/recipes` without executing it.

At startup and explicit reload, Golem validates JSON, metadata, and active
references in both templates before publishing the catalog. Errors identify
filenames, fields, and keys without printing template bodies, defaults, or
supplied arguments. Model hints are advisory: syntactically valid unconfigured
roles/use cases and offline providers do not remove commands from the catalog.
Discovery performs no provider calls or capability probes. Bindings resolve at
invocation, and a missing binding falls back to the current model with a notice.
A resolved hint selects the model for that invocation only, through ordinary
model preparation and destination consent. It does not change the session's
permanent `/model set` selection or grant new authority.

`/recipes` lists the directory and valid commands sorted by name, with positional
usage and descriptions. `/help` appends those same command entries. Unknown
commands list sorted prefix matches, or all available names and aliases if no
prefix matches. Suggestions never execute automatically.

```text
recipes: /tmp/example-config/go-llm/commands
  /review <target> [focus] - Review a change for actionable defects.
  /summary <path> - Summarize a markdown file.
```

`/recipes reload` synchronously replaces the catalog. Edits and additions become
available, deleted or newly invalid files disappear, and an absent directory
clears the catalog. Candidate diagnostics precede the success line, such as
`recipes: reloaded (1 command available)`. A directory-level failure retains
the previous catalog and prints `recipes: reload failed; previous commands
kept: <cause>`. Invocation uses this loaded snapshot without reopening files;
changes require reload or restart. Other forms print `usage: /recipes [reload]`.

### Positional arguments and quoting

Inputs bind in declaration order. Omitted suffix inputs use their exact
defaults. A nil/missing default requires a supplied value, even for an unused
input. Quoted empty strings count as supplied. An optional input before a
required input still occupies its slot; supply it to reach the later input.
Extra arguments and unterminated quotes fail with metadata-derived usage and
perform no model preparation, goal, or history write.

The argument grammar is Golem's existing MCP command parser:

- Unicode whitespace separates arguments. Single or double quotes group text,
  and adjacent quoted/unquoted pieces form one argument. `''` and `""` each
  supply an empty argument.
- Single quotes preserve all content literally. Outside quotes, backslash
  escapes whitespace or either quote. Within double quotes, it escapes only a
  double quote. All other backslashes remain literal, including a final one.
- The REPL and parser trim outer whitespace before parsing. Escaping a terminal
  unquoted space does not preserve it; quote a value to retain trailing spaces.
- `$HOME`, `$(...)`, backticks, `*`, `;`, and `@file` are literal text. Nothing
  runs a shell or reads an argument file. `name=value` is a positional value.
- Invalid UTF-8 is rejected before parsing or provider transport.

For the version 1 example above saved as `review-change.recipe.json`:

```text
/review-change "Project A"
```

The expanded user message is exactly:

```text
Review Project A. Focus on correctness.

Report defects with file references and explain their impact.
```

`/review-change '' security` supplies an empty target and `security` focus.
Bare `/review-change` prints:

```text
recipe: missing required input "target"
usage: /review-change <target> [focus]
  target: Change or path to review (required)
  focus: Review emphasis (optional)
```

Golem joins a nonempty expanded context to the goal with exactly two newlines;
an empty expanded context adds no separator. Authored and substituted whitespace
is preserved. The complete message, including any separator, is limited to
1,048,576 bytes and must be valid UTF-8 and nonblank. A context template that
expands to empty does not consume separator bytes.

Expanded text beginning with `/allow-exec`, `/model`, `/quit`, or any other
slash command is model-goal text, bypassing slash dispatch exactly once.
Context is part of that user message and never a system instruction. Conversation
persistence and line-editor recall store the expanded message under ordinary
secret/canary rules. Raw invocation recall is deferred: unused arguments may
contain bytes absent from the inspected expansion, so recording them would
require separate inspection of those exact recall bytes.

## File and trust boundary

`Load` follows ordinary operating-system symlink behavior, including a symlink
in the final path component, and requires the resolved target to be a regular
file. It checks the target before opening, checks the opened handle again, and
uses `os.SameFile` to reject an identity change. It then reads at most 65,537
bytes and rejects an oversized document instead of parsing a truncated one.
Missing files and broken symlinks preserve `fs.ErrNotExist` through wrapping.

This explicit-path behavior does not confine a path to a workspace or allowed
root. Golem discovery trusts the designated user config tree, including its
directory-symlink targets; a future automatic workspace source requires the
workspace trust gate tracked in #431. The file
checks also do not make loading race-proof: a path can change before `Open`,
and in-place edits can retain identity. On Unix, a nonblocking open prevents a
path swapped to a FIFO from waiting for a writer; the opened-file check rejects
the FIFO. Other platforms use their ordinary file-open behavior.
`os.SameFile` establishes identity, not immutable content. The byte limit bounds
reads and retained data, not elapsed time or concurrent edits; slow filesystems
can still stall I/O.

Loading or parsing grants no consent, instruction authority, tools, or
permissions. Goal and context text can contain prompt injection; parsing does
not sanitize it or make it safe for system instructions. Consumers must apply
their current trust and model-admission rules. When consent is bound to exact
bytes, read the file once with a 65,537-byte bound, review or hash those bytes,
and pass those same bytes to `Parse`. Hashing a file and reopening it through
`Load` does not preserve same-byte consent.
