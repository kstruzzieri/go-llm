# Releasing go-llm

Releases are tagged only on `main`. `develop` takes every feature PR, and
`main` receives develop wholesale through one release PR per version. The
consumer upgrade notes for each release are `docs/releases/vX.Y.Z.md` in this
directory. This runbook records the sequence v0.5.0 shipped with and the
pitfalls earlier releases hit.

## Before the release

1. **Freeze scope.** The milestone description lists the landed work, the
   remaining Must items and what "ready to release" means for that version.
   Epic [#429](https://github.com/kstruzzieri/go-llm/issues/429)'s phase table
   and the README roadmap rows follow the milestone. No release has a delivery
   date until one is set explicitly.
2. **Meet the gate.** A version is ready when all of these hold:
   - every Must item's design is approved;
   - every Must item has complete acceptance evidence. Closing an issue by
     transferring part of its scope to a successor does not count; the
     successor becomes the gate item unless the scope is re-decided;
   - required checks pass on the release-candidate commit, the develop head
     that will go to main;
   - the upgrade notes are reviewed.
3. **Write the upgrade notes** (`docs/releases/vX.Y.Z.md`, its own PR into
   develop). Lead with changed defaults and new requirements on callers. Name
   every protection that is opt-in, and say what a default session does not
   get. List accepted coverage gaps and pre-existing defects under
   `## Known issues`. `## Release scope` lists the milestone, closed issues
   outside it, and items that moved to the next milestone.

## Cut the release

1. **Stamp** (PR into develop, from a branch off a refreshed develop):

   ```bash
   scripts/changelog-fold
   ```

   Then insert `## [X.Y.Z] - YYYY-MM-DD` under `## [Unreleased]`. In the same
   PR, point the README release line and roadmap at the new version. Between
   the stamp and the tag, a PR that folds nothing may still correct the body
   of the newest version section (see `changelog.d/README.md`).
2. **Release PR, develop to main.** Open it with `--base main --head develop`.
   CI requires `changelog.d/` to hold only `README.md`. Merge it with a
   **merge commit**, never a squash.
3. **Tag on main.**

   ```bash
   git fetch origin
   git tag -a vX.Y.Z origin/main -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

   `release.yml` refuses a tag that main does not contain, then runs
   GoReleaser. Never move a published tag; Go module proxies cache it.
4. **Check the published release.** `gh release view vX.Y.Z` must list the
   GoReleaser assets. Fix a failed publish before announcing the release.
5. **Merge back, main to develop.** Make a branch off develop, run
   `git merge --no-ff origin/main`, and open a PR into develop. Merge it with a
   **merge commit**. Mark the release current in the README and pin its
   upgrade-notes link to the tag in the same PR.
6. **Verify ancestry.** The release is not done until both commands succeed:

   ```bash
   git fetch origin
   git merge-base --is-ancestor vX.Y.Z origin/develop
   git describe --tags origin/develop   # expect vX.Y.Z-N-g...
   ```

## Pitfalls

- **A squashed merge-back drops the tag from develop's history.** v0.4.0
  squashed its release PR (#607) and its merge-back (#608). The squashed
  merge-back left the v0.4.0 tag off develop, so `git describe` on develop
  still reported v0.3.0 and pseudo-versions sorted wrongly. #609 repaired it
  with `git merge -s ours origin/main`. v0.5.0 used merge commits both ways
  (#670, #671).
- **Closing keywords on partial PRs.** A `Closes #N` in a squash message
  closes the issue when the first slice merges. Use closing keywords only on
  the PR that completes the issue.
- **Never edit `CHANGELOG.md` outside the stamp.** Add a `changelog.d/`
  fragment instead; CI rejects direct edits.
