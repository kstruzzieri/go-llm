package tools

// Declared toolchain policy (#722): the closed tables behind SandboxConfig.Env
// and TempEnv, the pure checks on the declared roots, and their admission
// against the host (admitRoots), which stats the filesystem, and the
// developer directory's link check over an lstat seam (checkDeveloperLinks).

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/internal/datadir"
	"github.com/kstruzzieri/go-llm/internal/pathguard"
)

// sandboxEnvFixed is SandboxConfig.Env's closed table: each name with its one
// accepted value. GOMODCACHE, the remaining row, takes a path inside a
// declared root instead (validateToolchainFields). Adding a row needs its own
// review.
var sandboxEnvFixed = map[string]string{
	"GOPROXY":     "off",
	"GOTOOLCHAIN": "local",
	"GOFLAGS":     "-buildvcs=false",
}

// sandboxTempEnv is SandboxConfig.TempEnv's closed table: each name with its
// own directory under the command's private temp.
var sandboxTempEnv = map[string]string{
	"GOCACHE": "gocache",
}

// developerDirectoryEnv is added whenever DeveloperDirectory is set: git
// reads no system or global config and python writes no bytecode beside its
// sources.
var developerDirectoryEnv = []string{
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_CONFIG_NOSYSTEM=1",
	"PYTHONDONTWRITEBYTECODE=1",
}

// toolchainPath reports whether p is a clean absolute POSIX path of valid
// UTF-8 with no control characters: the spelling every declared root and
// GOMODCACHE must have.
func toolchainPath(p string) bool {
	return posixCleanAbs(p) && utf8.ValidString(p) && !containsControl(p)
}

// validateToolchainFields checks c's #722 fields without I/O: roots and the
// developer directory are clean absolute paths, Env and TempEnv come from
// their tables with no name repeated, and GOMODCACHE lies in a declared root
// (the developer directory counts). Errors name the field and index, never
// the refused value.
func validateToolchainFields(c SandboxConfig) error {
	for i, root := range c.ReadOnlyRoots {
		if !toolchainPath(root) {
			return fmt.Errorf("tools: sandbox ReadOnlyRoots[%d] must be a clean absolute path of valid UTF-8 with no control characters", i)
		}
	}
	if c.DeveloperDirectory != "" && !toolchainPath(c.DeveloperDirectory) {
		return fmt.Errorf("tools: sandbox DeveloperDirectory must be a clean absolute path of valid UTF-8 with no control characters")
	}
	seen := make(map[string]bool, len(c.Env))
	for i, e := range c.Env {
		if e.Name == "GOMODCACHE" {
			inRoot := slices.ContainsFunc(c.ReadOnlyRoots, func(r string) bool { return pathCovers(r, e.Value) }) ||
				(c.DeveloperDirectory != "" && pathCovers(c.DeveloperDirectory, e.Value))
			if !toolchainPath(e.Value) || !inRoot {
				return fmt.Errorf("tools: sandbox Env[%d] GOMODCACHE must be a clean absolute path inside a declared root", i)
			}
		} else if want, ok := sandboxEnvFixed[e.Name]; !ok {
			return fmt.Errorf("tools: sandbox Env[%d] names a variable outside the allowed table", i)
		} else if e.Value != want {
			return fmt.Errorf("tools: sandbox Env[%d] %s accepts only %q", i, e.Name, want)
		}
		if seen[e.Name] {
			return fmt.Errorf("tools: sandbox Env[%d] repeats %s", i, e.Name)
		}
		seen[e.Name] = true
	}
	seen = make(map[string]bool, len(c.TempEnv))
	for i, name := range c.TempEnv {
		if _, ok := sandboxTempEnv[name]; !ok {
			return fmt.Errorf("tools: sandbox TempEnv[%d] names a variable outside the allowed table", i)
		}
		if seen[name] {
			return fmt.Errorf("tools: sandbox TempEnv[%d] repeats %s", i, name)
		}
		seen[name] = true
	}
	return nil
}

// declaredRoots returns c's declared roots: ReadOnlyRoots, then a non-empty
// DeveloperDirectory, which is admitted and overlap-checked like any root.
func declaredRoots(c SandboxConfig) []string {
	roots := slices.Clone(c.ReadOnlyRoots)
	if c.DeveloperDirectory != "" {
		roots = append(roots, c.DeveloperDirectory)
	}
	return roots
}

// CanonicalToolchainRoot returns path's canonical spelling, the one a
// declared root (SandboxConfig.ReadOnlyRoots, DeveloperDirectory) must use:
// absolute, symlinks resolved, every component in its stored filesystem
// spelling. It rejects a ".." component in the original path before cleaning
// because a preceding symlink can change which parent it denotes.
// A host calls it once before building the config; backends never
// re-resolve, so the config pins the resolved folder: retargeting the link
// you resolved changes nothing until you resolve again and rebuild. A root
// whose own path later becomes a link, or that disappears, fails construction.
func CanonicalToolchainRoot(path string) (string, error) {
	pathWithoutVolume := strings.TrimPrefix(path, filepath.VolumeName(path))
	if slices.Contains(strings.Split(filepath.ToSlash(pathWithoutVolume), "/"), "..") {
		return "", fmt.Errorf("%w: toolchain root must not contain a '..' component", ErrSandboxPolicy)
	}
	canon, unverified, err := canonicalWorkspaceRoot(path)
	if err != nil {
		return "", err
	}
	if unverified != "" {
		return "", fmt.Errorf("tools: the spelling of %q cannot be verified (its parent cannot be listed)", unverified)
	}
	return canon, nil
}

// toolchainExcludedHome lists home-relative stores no declared root may be,
// cover or lie inside (#722 D7). go-llm's own data and config stores are
// added by toolchainExclusions.
var toolchainExcludedHome = []string{
	".ssh", ".gnupg", ".aws", ".docker", ".kube", ".netrc", ".config", ".claude", ".codex",
	"Library/Keychains",
}

// toolchainRuntimeTemps are the name prefixes of go-llm's own directories
// under the temp base: Seatbelt private temps and probe workspaces, and
// scratch copies.
var toolchainRuntimeTemps = []string{"go-llm-seatbelt-", "go-llm-scratch-"}

// toolchainExclusions returns the stores admitRoots refuses: the
// home-relative list; go-llm's data stores under datadir.Base (which honors
// XDG_DATA_HOME) and, when that is relocated, still under the default
// ~/.local/share, where old sessions and the signing key may remain; and its
// config store under the user config directory, derived as os.UserConfigDir
// does but from home and getenv.
func toolchainExclusions(home string, getenv func(string) string) ([]string, error) {
	out := make([]string, 0, len(toolchainExcludedHome)+5)
	for _, rel := range toolchainExcludedHome {
		out = append(out, filepath.Join(home, rel))
	}
	data, err := datadir.Base(getenv)
	if err != nil {
		return nil, fmt.Errorf("tools: cannot locate go-llm's data directory: %w", err)
	}
	for _, base := range []string{data, filepath.Join(home, ".local", "share")} {
		out = append(out, filepath.Join(base, "golem"), filepath.Join(base, "go-llm"))
	}
	config := filepath.Join(home, ".config")
	if runtime.GOOS == "darwin" {
		config = filepath.Join(home, "Library", "Application Support")
	} else if dir := getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		config = dir
	}
	return append(out, filepath.Join(config, "go-llm")), nil
}

// overlaps reports whether a and b are the same directory or one lies inside
// the other, by file identity in both directions (pathguard.ValidateOutside),
// so a case or Unicode alias cannot hide it. An error comparing them counts as
// an overlap: it fails closed.
func overlaps(a, b string) bool {
	return pathguard.ValidateOutside(a, b) != nil || pathguard.ValidateOutside(b, a) != nil
}

// admitRoots checks each declared root against the host (#722 D7) before any
// backend uses it: not a broad root nor covering one; resolvable; not home
// nor covering it; not the temp base nor covering it, nor one of go-llm's
// runtime directories under it; not one of the excluded stores
// (toolchainExclusions), inside one, or covering one; its own canonical
// spelling (CanonicalToolchainRoot), verified; and an existing directory.
// home, tempBase and getenv are the host's; tests pass stand-ins. The
// identity checks come before the spelling check because they only stat and
// resolve links, while verifying a spelling lists the root's parents: an
// excluded store is never listed. The exclusions are specific, not a proof
// that a root holds nothing sensitive.
func admitRoots(roots []string, home, tempBase string, getenv func(string) string) error {
	if len(roots) == 0 {
		return nil
	}
	if !filepath.IsAbs(home) {
		return fmt.Errorf("tools: declared roots: home directory %q is not absolute", home)
	}
	excluded, err := toolchainExclusions(home, getenv)
	if err != nil {
		return err
	}
	canonBase, err := filepath.EvalSymlinks(tempBase)
	if err != nil {
		return fmt.Errorf("tools: declared roots: temp base: %w", err)
	}
	for _, root := range roots {
		if seatbeltBroadRoot(root) || pathCovers(root, "/System/Volumes/Data") {
			return fmt.Errorf("tools: declared root %q is or covers a broad root", root)
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return fmt.Errorf("tools: declared root %q cannot be resolved: %w", root, err)
		}
		if pathguard.ValidateOutside(home, root) != nil {
			return fmt.Errorf("tools: declared root %q is or covers the home directory", root)
		}
		if pathguard.ValidateOutside(tempBase, root) != nil {
			return fmt.Errorf("tools: declared root %q is or covers the temp base", root)
		}
		if rel, ok := strings.CutPrefix(resolved, canonBase+"/"); ok {
			first, _, _ := strings.Cut(strings.ToLower(rel), "/")
			for _, prefix := range toolchainRuntimeTemps {
				if strings.HasPrefix(first, prefix) {
					return fmt.Errorf("tools: declared root %q lies in a go-llm runtime temp directory", root)
				}
			}
		}
		for _, e := range excluded {
			if overlaps(root, e) {
				return fmt.Errorf("tools: declared root %q is, covers or lies inside the excluded %q", root, e)
			}
		}
		canon, err := CanonicalToolchainRoot(root)
		if err != nil {
			return fmt.Errorf("tools: declared root %q: %w", root, err)
		}
		if canon != root {
			return fmt.Errorf("tools: declared root %q is not in its canonical spelling %q", root, canon)
		}
		fi, err := os.Stat(root)
		if err != nil {
			return fmt.Errorf("tools: declared root %q cannot be inspected: %w", root, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("tools: declared root %q is not a directory", root)
		}
	}
	return nil
}

// linkInfo is the portable view of one lstat: whether the path exists,
// whether it is a symlink, and its owner. A path that does not exist is
// reported as exists false, not as an error.
type linkInfo struct {
	exists, symlink bool
	uid             uint32
}

// developerSelectionLinks are the links through which xcode-select records
// the active developer directory.
var developerSelectionLinks = []string{"/private/var/select/developer_dir", "/private/var/db/xcode_select_link"}

// checkDeveloperLinks is the issue's link-ownership check, run when a config
// opts into a developer directory (#722 D3): each selection link that exists
// must be a symlink root owns, and every directory above it, up to /, an
// existing directory root owns. A missing link is fine; any other lstat error
// refuses. The developer directory is still a declared root that admission
// bounds; this check is not the confinement boundary.
func checkDeveloperLinks(lstat func(string) (linkInfo, error)) error {
	for _, link := range developerSelectionLinks {
		info, err := lstat(link)
		if err != nil {
			return fmt.Errorf("tools: developer directory link %q cannot be inspected: %w", link, err)
		}
		if !info.exists {
			continue
		}
		if !info.symlink || info.uid != 0 {
			return fmt.Errorf("tools: developer directory link %q is not a root-owned symlink", link)
		}
		for dir := filepath.Dir(link); ; dir = filepath.Dir(dir) {
			d, err := lstat(dir)
			if err != nil {
				return fmt.Errorf("tools: developer directory link %q: %q cannot be inspected: %w", link, dir, err)
			}
			if !d.exists || d.symlink || d.uid != 0 {
				return fmt.Errorf("tools: developer directory link %q: %q is not a root-owned directory", link, dir)
			}
			if dir == "/" {
				break
			}
		}
	}
	return nil
}
