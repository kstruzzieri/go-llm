# Full .gitignore Support in IndexDirectory

**Issue:** #6
**Date:** 2026-03-14
**Status:** Design
**Approach:** Pure stdlib implementation (no new dependencies)

## Purpose

Prevent `IndexDirectory` from indexing files the project's `.gitignore` says should be ignored. The current implementation only reads the root `.gitignore` with basic basename matching, missing path-scoped rules, negation, nested `.gitignore` files, `**` globs, and character classes. This causes generated code, build artifacts, and other junk to pollute the vector store, degrading retrieval quality.

## Scope

- Repository `.gitignore` files only (root + nested subdirectories)
- NOT in scope: `.git/info/exclude`, global gitignore (`~/.config/git/ignore`)
- No new external dependencies — pure Go stdlib implementation
- No public API changes to `IndexDirectory`, `IndexDirOption`, or any exported types

## Architecture

### New file: `rag/gitignore.go`

A self-contained gitignore matching engine with unexported types.

#### Types

```go
// gitignorePattern represents a single parsed .gitignore rule.
type gitignorePattern struct {
    original string // raw line from .gitignore (for debugging)
    pattern  string // cleaned pattern (no leading !, no trailing /)
    negation bool   // starts with ! -> un-ignores a previously ignored path
    dirOnly  bool   // ends with / -> only matches directories
    anchored bool   // contains / (other than trailing) -> match against full path
}

// matchRule pairs a pattern with the directory scope of its .gitignore file.
type matchRule struct {
    pattern gitignorePattern
    baseDir string // directory containing the .gitignore (for scoping)
}

// gitignoreMatcher holds a stack of rules from multiple .gitignore files.
type gitignoreMatcher struct {
    rules []matchRule // ordered, later rules take precedence
}
```

#### Key Functions

```go
// newGitignoreMatcher creates an empty matcher.
func newGitignoreMatcher() *gitignoreMatcher

// addFromFile parses a .gitignore file and appends its rules scoped to baseDir.
// Returns silently if the file doesn't exist or can't be read.
func (m *gitignoreMatcher) addFromFile(path string, baseDir string)

// isIgnored checks if a relative path should be ignored.
// isDir indicates whether the path is a directory (for directory-only patterns).
// Returns true if the path matches an ignore rule and is not un-ignored by a later negation.
func (m *gitignoreMatcher) isIgnored(relPath string, isDir bool) bool

// globMatch matches a path against a gitignore glob pattern with ** support.
func globMatch(pattern, path string) bool
```

### Modified file: `rag/indexer.go`

#### Removed

- `loadGitignore()` function
- `isIgnored()` function

#### Changed: `IndexDirectory`

The walk phase gains gitignore awareness:

1. Create `gitignoreMatcher` at start
2. Load root `.gitignore` immediately
3. During `filepath.Walk`:
   - For each directory: check if ignored (SkipDir), check for nested `.gitignore` (append scoped rules)
   - For each file: check if ignored, then check extension filter
4. Gitignore check runs AFTER `WithExclude` (hardcoded excludes) but BEFORE extension filtering

## Pattern Parsing Rules

From `man gitignore`:

1. Blank lines and comments (`#`) are skipped
2. Trailing spaces are stripped (unless escaped with `\`)
3. Leading `!` marks a negation pattern (un-ignores), then stripped
4. Trailing `/` marks directory-only match, then stripped
5. Pattern contains `/` (other than trailing) means anchored (match against relative path from `.gitignore` location)
6. No `/` in pattern means unanchored (match against basename anywhere in the tree)

## Glob Matching

Custom `globMatch` function extending `filepath.Match` with `**` support:

| Pattern | Meaning | Example |
|---------|---------|---------|
| `*` | matches anything except `/` | `*.log` matches `app.log` |
| `?` | matches single char except `/` | `?.go` matches `a.go` |
| `[abc]` | character class | `[Mm]akefile` |
| `**/` | leading — match in all directories | `**/logs` matches `a/logs`, `a/b/logs` |
| `/**` | trailing — match everything inside | `abc/**` matches all under `abc/` |
| `/**/` | middle — zero or more directories | `a/**/b` matches `a/b`, `a/x/b`, `a/x/y/b` |

Implementation: split pattern on `/**/` segments and match each segment against path components recursively. For leading `**/`, match against all possible suffixes. For trailing `/**`, match prefix against path prefix.

## Match Precedence

1. Walk through all rules in order (root `.gitignore` first, then nested by directory depth)
2. Each rule is scoped to its `.gitignore` file's directory — patterns only apply to paths within that subtree
3. Last matching rule wins (this is how negation works)
4. Return final verdict: ignored or not

## Interaction with WithExclude

`WithExclude` operates as a separate, first-pass filter before gitignore matching:

- `WithExclude` patterns always apply, even without a `.gitignore`
- A negation pattern like `!vendor/important.go` CANNOT override `WithExclude("vendor")` — this is intentional since these are structural excludes the caller explicitly set
- Matches current behavior — no breaking change

## Error Handling

- Missing `.gitignore` file: silently skip (current behavior)
- Malformed pattern line: skip that line, continue parsing (lenient, matches git behavior)
- Permission error reading nested `.gitignore`: append to walk errors, continue indexing

## Testing

### New file: `rag/gitignore_test.go`

Unit tests for the pattern matching engine in isolation:

**Pattern parsing:**
- Comment and blank line skipping
- Negation detection (`!pattern`)
- Directory-only detection (`pattern/`)
- Anchored vs unanchored detection (`src/foo` vs `foo`)
- Trailing space stripping

**Glob matching (`**` support):**
- Leading `**/logs` matches `logs`, `a/logs`, `a/b/logs`
- Trailing `abc/**` matches `abc/x`, `abc/x/y/z`
- Middle `a/**/b` matches `a/b`, `a/x/b`, `a/x/y/z/b`
- Standard globs: `*.log`, `?.go`, `[Mm]akefile`
- Edge cases: literal match (no wildcards), empty path

**Precedence & negation:**
- `*.log` then `!important.log` — `important.log` is NOT ignored
- Later rule overrides earlier rule

**Directory-only patterns:**
- `build/` ignores directory `build` but not file named `build`

**Anchored vs unanchored:**
- `foo` matches `foo`, `a/foo`, `a/b/foo` (unanchored)
- `src/foo` matches only `src/foo` (anchored)

### Updated: `rag/indexer_test.go`

Integration tests with temp directories and nested `.gitignore` files:

- Nested `.gitignore` scoping (root + subdirectory patterns both respected)
- Negation in subdirectory un-ignores a file
- Directory skip (`build/` prevents walking entire subtree)
- `WithExclude` takes priority over `.gitignore` negation
- No `.gitignore` present (same behavior as before)
- Path-scoped rules (`src/generated/` only ignores that specific path)

### Existing tests

All current tests pass unchanged — no public API changes.

## Files Changed

| File | Change |
|------|--------|
| `rag/gitignore.go` | NEW — pattern parsing, glob matching, matcher |
| `rag/gitignore_test.go` | NEW — unit tests for matching engine |
| `rag/indexer.go` | MODIFIED — replace `loadGitignore`/`isIgnored` with `gitignoreMatcher`, add nested loading during walk |
| `rag/indexer_test.go` | MODIFIED — add integration tests for nested gitignore scenarios |

## Estimated Size

- `rag/gitignore.go`: ~250 lines
- `rag/gitignore_test.go`: ~300 lines
- `rag/indexer.go` changes: ~30 lines modified (net neutral, replacing old functions)
- `rag/indexer_test.go` additions: ~150 lines
