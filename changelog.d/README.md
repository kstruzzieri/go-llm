# Changelog fragments

Do not edit `CHANGELOG.md` in a pull request. Every PR that prepends its
entry under `## [Unreleased]` shares one insertion point, so concurrent PRs
conflict there on every merge. Instead add one file here:

    changelog.d/<issue>-<slug>.md

- `<issue>` is the issue number (the PR number when there is no issue);
  `<slug>` is lower-case letters, digits, and dashes.
- The content is exactly the section you would have written in
  `CHANGELOG.md`: first line `### <Category> — <title> (#<issue>)` with
  Category one of Added, Changed, Fixed, Removed, Deprecated, Security,
  Amended, then the body. Consumers read the folded result, so write it for
  them.
- Not every PR needs one. Tooling, CI, and test-only changes carry no
  fragment and are not rejected.

CI runs `scripts/check-changelog <base>` on every pull request: it rejects a
direct `CHANGELOG.md` edit that is not a fold and validates the fragments in
the tree. At release, `scripts/changelog-fold` moves every fragment under
`## [Unreleased]` (newest issue first) and removes it; stamp the version
heading afterwards as before.
