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
- Prospective filtering only: future `IndexDirectory` runs stop indexing ignored paths, but this issue does NOT retroactively purge already-indexed sources that have become ignored

## Architecture

### New file: `rag/gitignore.go`

A self-contained gitignore matching engine with unexported types.

#### Types

```go
// gitignorePattern represents a single parsed .gitignore rule.
type gitignorePattern struct {
    original string // raw line from .gitignore (for debugging)
    pattern  string // cleaned pattern (no leading !, no leading /, no trailing /)
    negation bool   // starts with ! -> un-ignores a previously ignored path
    dirOnly  bool   // ends with / -> only matches directories
    anchored bool   // leading / or internal / -> match against scoped path from baseDir root
}

// matchRule pairs a pattern with the directory scope of its .gitignore file.
type matchRule struct {
    pattern gitignorePattern
    baseDir string // slash-normalized repo-relative directory containing the .gitignore
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
// Returns nil if the file doesn't exist. Read errors are returned to the caller.
func (m *gitignoreMatcher) addFromFile(path string, baseDir string) error

// isIgnored checks if a relative path should be ignored.
// isDir indicates whether the path is a directory (for directory-only patterns).
// Returns true if the path matches an ignore rule and is not un-ignored by a later negation.
func (m *gitignoreMatcher) isIgnored(relPath string, isDir bool) bool

// globMatch matches a path against a gitignore glob pattern with ** support.
// Returns false for malformed patterns (e.g., unclosed character classes).
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
   - For each path, compute the repo-relative path with `filepath.Rel`, then normalize it with `filepath.ToSlash` before matching
   - For each directory: (a) check `WithExclude` first, (b) check gitignore final verdict for that directory and `SkipDir` only if still ignored after all currently loaded rules, (c) if NOT skipped, look for `.gitignore` inside and append scoped rules
   - For each file: check if ignored by gitignore, then check extension filter
4. Gitignore check runs AFTER `WithExclude` (hardcoded excludes) but BEFORE extension filtering

**Note:** When a directory's final verdict is ignored (via `WithExclude` or gitignore), its entire subtree — including any nested `.gitignore` files — is never visited. This matches git's behavior: ancestor rules already loaded from parent `.gitignore` files can still un-ignore the directory itself (for example `!build/`), but a `.gitignore` inside a skipped directory is never consulted.

## Pattern Parsing Rules

From `man gitignore`:

1. Blank lines and comments (`#`) are skipped
2. Trailing spaces are stripped (unless escaped with `\`)
3. Leading `\` escapes special characters: `\#` is a literal `#` (not a comment), `\!` is a literal `!` (not a negation)
4. Leading `!` marks a negation pattern (un-ignores), then stripped
5. Trailing `/` marks directory-only match, then stripped
6. Leading `/` anchors the pattern to the `.gitignore` directory scope, then stripped. Example: `/build` matches only `build` at the `.gitignore` root, not `src/build`
7. Leading `/` sets `anchored=true` even after the slash is stripped
8. Pattern contains `/` (other than leading or trailing) also means anchored (match against relative path from `.gitignore` location)
9. No `/` in pattern means unanchored (match against basename anywhere in the scoped tree)

## Path Normalization

Gitignore syntax is defined in terms of `/`, regardless of host OS. The matcher therefore works only with slash-normalized repo-relative paths:

- `filepath.Walk`, `filepath.Rel`, and file I/O remain OS-native
- Before any ignore check, `relPath` and rule `baseDir` are normalized with `filepath.ToSlash`
- Matching logic uses slash semantics (`/`) consistently on all platforms
- Helper code should use the `path` package for slash-based basename/dir handling where needed, not `filepath`

## Glob Matching

Custom `globMatch` function implementing gitignore-style slash matching with `**` support. For segments without `**`, use `path.Match` semantics against slash-normalized strings:

| Pattern | Meaning | Example |
|---------|---------|---------|
| `*` | matches anything except `/` | `*.log` matches `app.log` |
| `?` | matches single char except `/` | `?.go` matches `a.go` |
| `[abc]` | character class | `[Mm]akefile` |
| `**/` | leading — match in all directories (including root) | `**/logs` matches `logs`, `a/logs`, `a/b/logs` |
| `/**` | trailing — match everything inside | `abc/**` matches all under `abc/` |
| `/**/` | middle — zero or more directories | `a/**/b` matches `a/b`, `a/x/b`, `a/x/y/b` |

Implementation: split pattern on `/**/` segments and match each segment against path components recursively. For leading `**/`, strip it and attempt matching against every possible suffix of the path (including the full path for zero-directory match). For trailing `/**`, match the prefix against the path prefix. Malformed patterns (e.g., unclosed `[` brackets) return `false` (no match) rather than propagating an error — this is consistent with the "skip malformed patterns" error handling policy.

### Worked examples for `globMatch`

**Example 1: `**/test` matching `a/b/test`**
1. Detect leading `**/` → strip, pattern becomes `test`
2. Try suffix `a/b/test` → match `test` against `a/b/test`? No
3. Try suffix `b/test` → match `test` against `b/test`? No
4. Try suffix `test` → match `test` against `test`? Yes → return true

**Example 2: `a/**/b` matching `a/x/y/b`**
1. Split on `/**/` → segments: `["a", "b"]`
2. Match segment `a` against path prefix → `a` matches, remaining path: `x/y/b`
3. Match segment `b` against all suffixes of remaining: `x/y/b`, `y/b`, `b` → `b` matches → return true

**Example 3: `a/**/b` matching `a/b` (zero intermediary directories)**
1. Split on `/**/` → segments: `["a", "b"]`
2. Match segment `a` against path prefix → `a` matches, remaining path: `b`
3. Match segment `b` against remaining: `b` → matches → return true

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

## Interaction with Existing Indexed Data

This change affects which files are considered for indexing during the current run; it does not scan the vector store for stale sources that have become ignored since a previous run.

- If `foo/generated.go` was already indexed and later becomes ignored, a subsequent `IndexDirectory` run will skip re-indexing it, but will not delete its existing chunks
- This preserves the current `VectorStore` surface area and keeps the implementation scoped to filesystem-side filtering
- Callers that need exact store reconciliation after ignore-rule changes should rebuild the index from a clean store for now

## Error Handling

- Missing `.gitignore` file (`os.ErrNotExist`): silently skip (current behavior)
- Permission or other read error opening `.gitignore`: return error from `addFromFile`; walk callback appends it to walk errors and continues indexing
- Malformed pattern line: skip that line, continue parsing (lenient, matches git behavior)

## Testing

### New file: `rag/gitignore_test.go`

Unit tests for the pattern matching engine in isolation:

**Pattern parsing:**
- Comment and blank line skipping
- Negation detection (`!pattern`)
- Directory-only detection (`pattern/`)
- Anchored vs unanchored detection (`src/foo` vs `foo`)
- Leading `/` anchoring and stripping (`/build` → anchored, pattern `build`)
- Trailing space stripping
- Backslash escaping (`\#not-a-comment`, `\!literal-bang`)

**Glob matching (`**` support):**
- Leading `**/logs` matches `logs`, `a/logs`, `a/b/logs`
- Trailing `abc/**` matches `abc/x`, `abc/x/y/z`
- Middle `a/**/b` matches `a/b`, `a/x/b`, `a/x/y/z/b`
- Standard globs: `*.log`, `?.go`, `[Mm]akefile`
- Edge cases: literal match (no wildcards), empty path
- Path normalization: Windows-style relative paths are converted to slash form before matching

**Precedence & negation:**
- `*.log` then `!important.log` — `important.log` is NOT ignored
- Later rule overrides earlier rule
- `build/`, `!build/`, `!build/keep.go` — `build/keep.go` is NOT ignored
- `build/`, `!build/keep.go` — `build/keep.go` remains ignored because the parent directory was never un-ignored

**Directory-only patterns:**
- `build/` ignores directory `build` but not file named `build`

**Anchored vs unanchored:**
- `foo` matches `foo`, `a/foo`, `a/b/foo` (unanchored)
- `src/foo` matches only `src/foo` (anchored)
- `/build` matches only `build` at root (leading-slash anchoring)

**`**` zero-directory edge case:**
- `a/**/b` matches `a/b` (zero intermediary directories)

### Updated: `rag/indexer_test.go`

Integration tests with temp directories and nested `.gitignore` files:

- Nested `.gitignore` scoping (root + subdirectory patterns both respected)
- Negation in subdirectory un-ignores a file
- Overlapping nested rules: nested `.gitignore` re-ignores something parent negated (last-rule-wins across files)
- Directory skip (`build/` prevents walking entire subtree)
- Directory un-ignore (`build/`, `!build/`) re-enables walking the subtree so descendant rules are evaluated normally
- `WithExclude` takes priority over `.gitignore` negation
- No `.gitignore` present (same behavior as before)
- Path-scoped rules (`src/generated/` only ignores that specific path)
- Nested `.gitignore` scoping does NOT affect sibling directories (e.g., `src/.gitignore` with `*.log` does NOT ignore `lib/debug.log`)
- Already-indexed files that become ignored are left untouched on later runs (explicitly documenting the current scope)

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
