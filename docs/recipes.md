# Reusable recipes

The `recipe` package validates versioned JSON prompt bundles for use by go-llm
consumers. It does not discover recipes, expand templates, choose a model, or
run commands. Those runtime behaviors are planned in
[#353](https://github.com/kstruzzieri/go-llm/issues/353).

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

## Placeholder contract for issue #353

Only `goal` and `context` are template-bearing. Descriptions and defaults are
literal data. Version 1 reserves the following grammar for the scanner and
renderer planned in #353; this package preserves the text but does not validate
or expand it.

A reference is exactly `{{inputs.name}}`, where `name` is a declared,
case-sensitive input name matching `[a-z][a-z0-9_]*`. It contains no whitespace.
Count consecutive backslashes immediately before `{{inputs.` in the
JSON-decoded text. Emit one literal backslash for every pair. An even number of
backslashes activates the reference; an odd remainder quotes the opening
marker. A quoted marker is ordinary text and needs neither a declared name nor
closing braces, and scanning resumes after that marker. Backslashes elsewhere
are unchanged. These counts apply after JSON decoding, so each backslash must
be doubled again in a JSON string literal.

With input `x` supplied as `VALUE`, the required #353 behavior is:

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

#353 must validate active references during discovery or reload, before a
recipe is listed as available or invoked. A successful `Parse` only establishes
format validity; it does not establish invocability. #353 also owns argument
binding, CLI quoting, one-turn model-hint precedence, capability and destination
admission, and prompt dispatch. Recipe commands and discovery are not available
yet. #353 will place pure template validation and substitution in `recipe` for
reuse by Firn, Flux ML, and Quantum Trader; CLI binding, trust decisions,
routing, and dispatch remain in consumers.

## File and trust boundary

`Load` follows ordinary operating-system symlink behavior, including a symlink
in the final path component, and requires the resolved target to be a regular
file. It checks the target before opening, checks the opened handle again, and
uses `os.SameFile` to reject an identity change. It then reads at most 65,537
bytes and rejects an oversized document instead of parsing a truncated one.
Missing files and broken symlinks preserve `fs.ErrNotExist` through wrapping.

This explicit-path behavior does not confine a path to a workspace or allowed
root. #353 must define containment and symlink policy for discovery. The file
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
