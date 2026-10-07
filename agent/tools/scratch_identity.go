package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Scratch source binding (#553): the snapshot must be a copy of the objects
// recheckExecPlan approved. These checks bind the approved object at the last
// check before the command runs in the clone. They do not bind bytes (an
// in-place rewrite keeps the identity), do not make launch atomic, and are
// not a point-in-time coherence proof of the snapshot; same-UID mutation of
// the host, reference, or work trees after the checks is the accepted
// residual (#484).
var (
	errScratchRootMismatch = errors.New("tools: scratch snapshot does not match approved workspace root; retry")
	errScratchDirMismatch  = errors.New("tools: scratch snapshot does not match approved working directory; retry")
	errScratchExeMismatch  = errors.New("tools: scratch snapshot does not match approved executable; retry")
)

// manifestEntryMatches reports whether one source manifest entry is the
// approved object: the expected type (directory, or regular file) and a
// provable, equal dev/ino pair. A nil approval or an all-zero identity on
// either side never matches.
func manifestEntryMatches(e snapshotEntry, wantDir bool, approved os.FileInfo) bool {
	if approved == nil || wantDir != e.typ.IsDir() || (!wantDir && !e.typ.IsRegular()) {
		return false
	}
	dev, ino, _, _ := statIdentity(approved)
	return (dev != 0 || ino != 0) && (e.dev != 0 || e.ino != 0) &&
		e.dev == dev && e.ino == ino
}

// manifestIdentityMatch looks up rel in the source manifest and reports
// whether that entry is the approved object. A missing entry never matches.
func manifestIdentityMatch(man snapshotManifest, rel string, wantDir bool, approved os.FileInfo) bool {
	for _, e := range man.entries {
		if e.path == rel {
			return manifestEntryMatches(e, wantDir, approved)
		}
	}
	return false
}

// scratchContained returns p relative to base when p stays inside base,
// using rewriteScratchSpec's containment predicate, so the validator checks
// exactly the spellings the rewrite remaps.
func scratchContained(base, p string) (string, bool) {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// referenceCopyOf reports whether located, a stat inside the reference tree,
// is the reference copy of a source manifest entry of the wanted type that
// carries the approved identity. The source entry supplies the identity
// evidence and the reference only locates it, so case, normalization, and
// hard-link spellings resolve without comparing a clone inode with a source
// inode, and an approved object moved to another path is not accepted.
func referenceCopyOf(man snapshotManifest, reference string, wantDir bool, approved, located os.FileInfo) bool {
	for _, e := range man.entries {
		if !manifestEntryMatches(e, wantDir, approved) {
			continue
		}
		copied, err := os.Stat(filepath.Join(reference, filepath.FromSlash(e.path)))
		if err == nil && os.SameFile(copied, located) {
			return true
		}
	}
	return false
}

// validateScratchSource binds the canonical->reference snapshot to the
// approved spec. man is the source manifest of the accepted snapshot pass;
// reference is the pristine copy that pass produced. The workspace root
// entry "." must carry the approved root identity. The approved cwd spelling
// and a workspace-local executable spelling must each locate, inside the
// reference tree, the copy of a source entry carrying the approved identity
// (referenceCopyOf), so case, normalization, and hard-link aliases of either
// keep working. An internal executable spelling whose link resolves outside
// the reference must be the approved external object. External executable
// spellings are not rewritten and not checked here. Where the platform has no
// file identity, the checks are skipped, matching the degraded drift
// comparison.
func validateScratchSource(man snapshotManifest, spec execSpec, canonicalRoot, reference string) error {
	if !scratchIdentitySupported {
		return nil
	}
	if !manifestIdentityMatch(man, ".", true, spec.RootIdentity) {
		return errScratchRootMismatch
	}
	dirRel, ok := scratchContained(canonicalRoot, spec.Dir)
	if !ok {
		return fmt.Errorf("tools: scratch cwd %q escapes workspace root %q", spec.Dir, canonicalRoot)
	}
	cwd, err := os.Stat(filepath.Join(reference, dirRel))
	if err != nil || !cwd.IsDir() || !referenceCopyOf(man, reference, true, spec.DirIdentity, cwd) {
		return errScratchDirMismatch
	}
	exeRel, ok := scratchContained(canonicalRoot, spec.Path)
	if !ok {
		return nil
	}
	if spec.ExeIdentity == nil {
		return errScratchExeMismatch
	}
	// The reference may sit under a symlinked temp prefix (macOS /var), so
	// containment compares fully resolved paths on both sides.
	refResolved, err := filepath.EvalSymlinks(reference)
	if err != nil {
		return errScratchExeMismatch
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(reference, exeRel))
	if err != nil {
		return errScratchExeMismatch
	}
	target, err := os.Stat(resolved)
	if err != nil || !target.Mode().IsRegular() {
		return errScratchExeMismatch
	}
	if _, inside := scratchContained(refResolved, resolved); !inside {
		if !os.SameFile(target, spec.ExeIdentity) {
			return errScratchExeMismatch
		}
		return nil
	}
	if !referenceCopyOf(man, reference, false, spec.ExeIdentity, target) {
		return errScratchExeMismatch
	}
	return nil
}
