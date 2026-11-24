//go:build linux

package tools

// Setting aside the git machinery a sandboxed command created (#802). Each
// item newGitMachinery found is renamed in place, from a parent opened one
// component at a time without following a symbolic link, after its identity
// is rechecked, and never over an existing name. A directory whose mode
// refuses the work (a command can chmod its own directories) is opened up
// for one more try and given its exact mode back (ownerAccess). Nothing is
// deleted or replaced; each rename is one note, and each failure one warning.

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// heldNameTries bounds the names tried for one entry: <name>.golem-held-<run
// id>, then the same with -2, -3 and so on, this many names in all.
const heldNameTries = 100

// heldGitBeforeRename is a test seam, nil in production. When set, it runs
// just before each rename, once the entry's parent is pinned and its identity
// rechecked, with the entry's root-relative path.
var heldGitBeforeRename func(rel string)

// errHeldChanged reports an entry that is no longer the one the after-walk
// recorded.
var errHeldChanged = errors.New("a different entry is there now")

// holdGitMachinery sets aside items, newGitMachinery's diff of the walks
// ending in after (the after-walk of after.root), naming each held entry
// <name>.golem-held-<runID>; runID must be one path component. Every item's
// Path and InnerHEADs, and every unlisted directory, is absolute, as the walks
// spell them; one that is not, or that lies outside the root, is not touched.
//
// For each item it opens the parent from the root one component at a time
// with O_NOFOLLOW, so a parent swapped for a symbolic link fails rather than
// leading elsewhere, and rechecks the item's identity (recheck). Then it
// renames the item's InnerHEADs and, except for a repository layout, the
// item itself, whole. A layout keeps its directory: it may be one the user
// already had, or the root. HEADs go first, so a held directory stops being a
// repository even if its own rename then fails. Each rename is
// renameAside's: no-replace, retrying the next name only when one is taken.
// A lookup or rename the kernel refuses for a directory's mode is tried once
// more after ownerAccess opens that directory up, and every mode it changed
// is put back once the item is done, even on a panic. Two items with one
// directory's identity are one directory under two spellings, held once
// (mergeHeldDirs); an item gone from its path after this run set aside the
// same identity at the same name in another spelling is done.
//
// It returns one note per rename, in order, so undoing them runs backwards;
// one warning per failure, which skips the rest of that item but not the
// other items; one warning per mode it could not put back; then one warning
// per directory the after-walk could not check, and the after-walk's own
// warnings. Paths in notes are relative to the root, quoted when a name could
// break or forge a line (noteName).
func holdGitMachinery(after sandboxGitLayout, items []heldItem, runID string) (notes []string) {
	acc := newOwnerAccess(after.root)
	defer acc.close()
	h := &holder{acc: acc, rootPath: after.root, runID: runID, renamed: make(map[gitEntryID][]string)}
	for _, it := range mergeHeldDirs(items) {
		r, in := h.rel(it.Path)
		var err error
		switch {
		case !in:
			err = errors.New("outside the workspace")
		case acc.rootErr != nil:
			err = fmt.Errorf("open the workspace: %w", acc.rootErr)
		default:
			err = h.holdRestoring(it, r)
		}
		if err != nil {
			h.warn(r, err)
		}
	}
	for _, u := range after.unlisted {
		r, _ := h.rel(u)
		why := after.unlistedWhy[u]
		if why == "" {
			why = "it could not be read"
		}
		h.notes = append(h.notes, "WARNING: could not check "+noteName(r)+" for new git metadata ("+why+
			"); do not run git in it or below it until you have checked it")
	}
	return append(h.notes, after.warnings...)
}

// holder is one holdGitMachinery call: its ownerAccess, the notes so far, and
// the root-relative paths it renamed whole, by identity.
type holder struct {
	acc      *ownerAccess
	rootPath string
	runID    string
	notes    []string
	renamed  map[gitEntryID][]string
}

// rel spells p relative to the root; in is false unless p is absolute and
// inside the root.
func (h *holder) rel(p string) (r string, in bool) {
	r, err := filepath.Rel(h.rootPath, p)
	if err != nil || !filepath.IsAbs(p) {
		return p, false
	}
	return r, r != ".." && !strings.HasPrefix(r, "../")
}

// warn notes a failure to set aside the entry at r, which may still be live
// git machinery, saying so first when the entry or a folder above it was
// moved or replaced during the run.
func (h *holder) warn(r string, err error) {
	moved := ""
	if errors.Is(err, errAccessMoved) || errors.Is(err, errHeldChanged) || errors.Is(err, unix.ENOENT) ||
		errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		moved = " (it was moved or replaced during the run)"
	}
	h.notes = append(h.notes, "WARNING: could not set aside new git metadata at "+noteName(r)+moved+
		" ("+err.Error()+"); do not run git in that directory until you have checked it")
}

// holdRestoring holds it, found at r, and then puts back every mode the hold
// opened up for it, even on a panic, noting any it could not.
func (h *holder) holdRestoring(it heldItem, r string) error {
	defer func() { h.notes = append(h.notes, h.acc.restore()...) }()
	return h.hold(it, r)
}

// hold sets aside it, found at r: its InnerHEADs, then, unless it is a
// layout, itself. An error is the item's warning; a HEAD that fails is warned
// of on its own and the item goes on.
func (h *holder) hold(it heldItem, r string) error {
	if r == "." && it.Kind != heldRepoLayout {
		return errors.New("the workspace root is never renamed")
	}
	parent, dir := -1, -1
	defer func() {
		for _, fd := range []int{parent, dir} {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
		}
	}()
	dirRel, name := path.Dir(r), path.Base(r)
	same := func() error {
		return h.acc.retry(parent, dirRel, 0o500, func() error { return recheck(parent, name, it.ID) })
	}
	if r != "." {
		var err error
		if parent, err = h.acc.open(h.acc.root, ".", dirRel); err != nil {
			return fmt.Errorf("open the folder holding it: %w", err)
		}
		if err := same(); err != nil {
			if errors.Is(err, unix.ENOENT) && slices.ContainsFunc(h.renamed[it.ID], func(p string) bool {
				return strings.EqualFold(p, r)
			}) {
				return nil // this name, spelled another way, is already set aside
			}
			return err
		}
	}
	if len(it.InnerHEADs) > 0 {
		var err error
		if r == "." {
			dir, err = h.acc.open(h.acc.root, ".", ".")
		} else {
			dir, err = h.acc.open(parent, dirRel, name)
		}
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Fstat(dir, &st); err != nil {
			return err
		}
		if !sameStat(&st, it.ID) {
			return errHeldChanged
		}
	}
	for _, hd := range it.InnerHEADs {
		hr, err := filepath.Rel(it.Path, hd)
		if err != nil || hr == ".." || strings.HasPrefix(hr, "../") {
			hp, _ := h.rel(hd)
			h.warn(hp, errors.New("outside the item"))
			continue
		}
		if err := h.holdHEAD(dir, r, hr); err != nil {
			h.warn(path.Join(r, hr), err)
		}
	}
	if it.Kind == heldRepoLayout {
		return nil
	}
	if err := h.rename(parent, dirRel, name, same); err != nil {
		return err
	}
	h.renamed[it.ID] = append(h.renamed[it.ID], r)
	return nil
}

// holdHEAD sets aside the HEAD at hr under the item open at dir, found at r.
// The after-walk records no HEAD's identity, so the check is its type: a
// regular file or a symbolic link, the two git reads as HEAD.
func (h *holder) holdHEAD(dir int, r, hr string) error {
	atRel := path.Join(r, path.Dir(hr))
	at, err := h.acc.open(dir, r, path.Dir(hr))
	if err != nil {
		return fmt.Errorf("open the folder holding it: %w", err)
	}
	defer func() { _ = unix.Close(at) }()
	name := path.Base(hr)
	return h.rename(at, atRel, name, func() error {
		return h.acc.retry(at, atRel, 0o500, func() error {
			var st unix.Stat_t
			if err := unix.Fstatat(at, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
			if t := st.Mode & unix.S_IFMT; t != unix.S_IFREG && t != unix.S_IFLNK {
				return errHeldChanged
			}
			return nil
		})
	})
}

// rename sets aside name in the directory open at dir, found at dirRel, once
// check accepts what is there, and notes the rename.
func (h *holder) rename(dir int, dirRel, name string, check func() error) error {
	if err := check(); err != nil {
		return err
	}
	r := path.Join(dirRel, name)
	if heldGitBeforeRename != nil {
		heldGitBeforeRename(r)
	}
	var to string
	err := h.acc.retry(dir, dirRel, 0o300, func() (err error) {
		to, err = renameAside(dir, name, h.runID)
		return err
	})
	if err != nil {
		return err
	}
	h.notes = append(h.notes, "held back new git metadata: "+noteName(r)+" → "+
		noteName(path.Join(dirRel, to))+" (rename it back to restore)")
	return nil
}

// mergeHeldDirs returns items with each directory listed once. A directory
// cannot be hard-linked, so two items with its identity are one directory
// under two spellings: a case-insensitive filesystem's .git/hooks and
// .git/Hooks (or .git/hooKs, with a Kelvin sign, which sorts after it). The
// merged item takes the stronger kind (the lower held kind: entryKinds'
// precedence, .git entry first, layout last) and that kind's path, and every
// list of HEADs, respelled under that path. It goes after the last item at
// or under any of its spellings, so whatever the walk found inside it is held
// before it is renamed whole, in whichever order the spellings sort. Other
// entries are left alone: two names for one file are two hard links, each
// held.
func mergeHeldDirs(items []heldItem) []heldItem {
	merged := make(map[gitEntryID]heldItem)
	spellings := make(map[gitEntryID]int)
	byPath := make(map[string]gitEntryID) // each directory item's path
	for _, it := range items {
		if it.ID.Mode != fs.ModeDir {
			continue
		}
		byPath[it.Path] = it.ID
		spellings[it.ID]++
		keep, seen := merged[it.ID]
		if !seen {
			merged[it.ID] = it
			continue
		}
		other := it
		if other.Kind < keep.Kind {
			keep, other = other, keep
		}
		heads := slices.Clone(keep.InnerHEADs)
		for _, h := range other.InnerHEADs {
			if r, err := filepath.Rel(other.Path, h); err == nil {
				heads = append(heads, filepath.Join(keep.Path, r))
			}
		}
		slices.Sort(heads)
		keep.InnerHEADs = slices.Compact(heads)
		merged[it.ID] = keep
	}
	// last is, for each directory with several spellings, the index of the
	// last item at or under one of them; a walk up costs the path's depth.
	last := make(map[gitEntryID]int)
	for i, it := range items {
		for p := it.Path; ; {
			if id, ok := byPath[p]; ok && spellings[id] > 1 {
				last[id] = i
			}
			q := filepath.Dir(p)
			if q == p {
				break
			}
			p = q
		}
	}
	at := make(map[int][]gitEntryID)
	for id, i := range last {
		at[i] = append(at[i], id)
	}
	out := make([]heldItem, 0, len(items))
	for i, it := range items {
		if it.ID.Mode != fs.ModeDir || spellings[it.ID] < 2 {
			out = append(out, it)
		}
		ids := at[i]
		slices.SortFunc(ids, func(a, b gitEntryID) int { // inner first
			if d := len(merged[b].Path) - len(merged[a].Path); d != 0 {
				return d
			}
			return strings.Compare(merged[a].Path, merged[b].Path)
		})
		for _, id := range ids {
			out = append(out, merged[id])
		}
	}
	return out
}

// recheck returns errHeldChanged unless name in dir is still id: its device,
// inode and type by fstatat without following it, and a symbolic link's
// target by readlinkat, since a link recreated with another target can get
// its old inode number back. A failed lookup returns its error.
func recheck(dir int, name string, id gitEntryID) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if !sameStat(&st, id) {
		return errHeldChanged
	}
	if id.Mode&fs.ModeSymlink != 0 {
		buf := make([]byte, unix.PathMax+1)
		n, err := unix.Readlinkat(dir, name, buf)
		if err != nil {
			return err
		}
		if n > unix.PathMax || string(buf[:n]) != id.Target {
			return errHeldChanged
		}
	}
	return nil
}

// sameStat reports whether st has id's device, inode and type.
func sameStat(st *unix.Stat_t, id gitEntryID) bool {
	return uint64(st.Dev) == id.Dev && uint64(st.Ino) == id.Ino && statType(st.Mode) == id.Mode
}

// statType is st_mode's file type in fs.FileMode type bits, as os.Lstat
// spells it (gitEntryID.Mode).
func statType(m uint32) fs.FileMode {
	switch m & unix.S_IFMT {
	case unix.S_IFDIR:
		return fs.ModeDir
	case unix.S_IFLNK:
		return fs.ModeSymlink
	case unix.S_IFIFO:
		return fs.ModeNamedPipe
	case unix.S_IFSOCK:
		return fs.ModeSocket
	case unix.S_IFCHR:
		return fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		return fs.ModeDevice
	}
	return 0 // a regular file
}

// renameAside renames name in dir to name.golem-held-<runID>, or to the first
// free one of its -2, -3, ... forms (heldNameTries names in all), and returns
// the name used. Each try is promoteRename's renameat2(RENAME_NOREPLACE), so
// nothing is ever replaced; only a taken name (EEXIST) moves on to the next.
func renameAside(dir int, name, runID string) (string, error) {
	held := name + ".golem-held-" + runID
	for n := 1; n <= heldNameTries; n++ {
		to := held
		if n > 1 {
			to += "-" + strconv.Itoa(n)
		}
		err := promoteRename(dir, name, dir, to)
		if err == nil {
			return to, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", err
		}
	}
	return "", fmt.Errorf("%w (tried %d names)", unix.EEXIST, heldNameTries)
}

// noteName spells a path for a note: bare, or quoted (quoteArgForPreview)
// when it holds whitespace, a quote, a backslash, a non-graphic rune or
// invalid UTF-8, so a planted name cannot end a note line or forge another.
// A spelling longer than noteNameMax bytes is cut there, at a rune boundary,
// and ends in noteNameCut.
func noteName(p string) string {
	n := quoteArgForPreview(p)
	if !utf8.ValidString(p) {
		n = strconv.QuoteToGraphic(p)
	}
	if len(n) <= noteNameMax {
		return n
	}
	end := noteNameMax
	for !utf8.RuneStart(n[end]) {
		end--
	}
	return n[:end] + noteNameCut
}

// noteNameMax is the longest path spelling a note carries; the command
// chooses its names.
const noteNameMax = 200

// noteNameCut marks a path spelling noteName cut.
const noteNameCut = "…[cut]"
