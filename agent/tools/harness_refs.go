package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/kstruzzieri/go-llm/agent/interceptor"
)

// Bounds of one reference-set build (#784, spec D2). Crossing one makes the
// set incomplete; each configuration file is also capped at readFileMaxBytes,
// and Markdown has no per-file cap.
const (
	harnessDefaultMaxEntries = 100_000  // walk entries per build
	harnessMaxTotalBytes     = 32 << 20 // bytes read per build, Markdown included
	// harnessImportDepth is how many import hops are followed: spec D3 takes
	// the larger of Claude Code's four and Gemini CLI's maxDepth of 5.
	harnessImportDepth = 5
)

// buildOutputDirs are the folder names the reference walk skips besides
// ignoreDirs (spec D2, Open 3(b)): build and environment output, which a
// running build rewrites and which can hold more entries than the walk allows.
// A harness file inside one is not read, a documented residual. The list is
// fixed, never derived from what the workspace holds.
var buildOutputDirs = map[string]bool{
	"target": true, "build": true, "dist": true, "out": true, ".venv": true,
	"venv": true, "__pycache__": true, ".gradle": true, ".next": true, ".tox": true,
}

// harnessAnchorFolders are the harness folders whose parent is the anchor of
// a D4 file inside them; a file under none of them is its own anchor (spec D4).
var harnessAnchorFolders = []string{".claude", ".cursor", ".windsurf", ".gemini", ".codex", ".github", ".vscode"}

// harnessAncestorFiles are the instruction files read in every folder above
// the workspace root (spec D12). Every literal D4 file (commandFileKeys) is
// read there too, and harnessAncestorListings are listed once; nothing else
// above the root is read, and no folder there is walked.
var harnessAncestorFiles = []string{
	"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", "AGENTS.override.md", "GEMINI.md",
	".claude/CLAUDE.md", ".cursorrules", ".windsurfrules", ".clinerules",
}

// harnessAncestorListings are the folders listed, without recursion, in every
// folder above the root, with the extensions their reader takes. Any other
// entry there (a sub-folder, .DS_Store) is ignored, not a failure (revision 2,
// F6).
var harnessAncestorListings = []struct {
	dir  string
	exts []string
}{
	{".claude/rules", []string{".md", ".mdc"}},
	{".github/hooks", []string{".json"}},
	{".github/copilot", []string{".json"}},
	{".clinerules", []string{".md", ".mdc"}}, // Cline's folder form (ruling R16)
}

// harnessRefs is what the harness files a workspace can see load, run or link
// to (#784). exact and subtree map the HarnessKey of a workspace-relative path
// to the reason a write there prompts, such as "CLAUDE.md imports
// docs/policy.md"; a subtree entry also covers everything below it. patterns
// are OpenCode instruction globs, and virtual the targets of .github and
// .vscode folder links. An incomplete set covers everything; cause says why.
type harnessRefs struct {
	complete       bool
	cause          string
	entries        int // walk entries visited, for the benchmark
	exact, subtree map[string]string
	patterns       []harnessPattern
	virtual        []harnessVirtualLink
}

// harnessVirtualLink is a .github or .vscode link (spec D2) to a folder inside
// the workspace (ruling R20): target is the HarnessKey of that folder, vpath
// the link's classified path and link its workspace path, for the reason.
type harnessVirtualLink struct{ target, vpath, link string }

// harnessPattern is an OpenCode instructions glob over HarnessKey workspace
// paths (spec D5), with its reason. prefix is its literal folder, unescaped:
// the HarnessKey of the segments before the first holding *?[ ("" for the
// root), which patternLinks matches links against.
type harnessPattern struct{ key, prefix, reason string }

// covers reports whether a write to the workspace-relative path rel is a
// harness reference, and the first matching reason: an exact reference, then
// rel or an ancestor of it as a subtree (a dangling link's target may later be
// created as a file at exactly that path), then a pattern, then a path below a
// .github or .vscode link's target that is a harness path where the harness
// sees it, as a file created there would be (ruling R20).
func (r *harnessRefs) covers(rel string) (bool, string) {
	k := interceptor.HarnessKey(rel)
	if why, ok := r.exact[k]; ok {
		return true, why
	}
	for a := k; ; a = path.Dir(a) {
		if why, ok := r.subtree[a]; ok {
			return true, why
		}
		if a == "." || a == "/" {
			break
		}
	}
	for _, p := range r.patterns {
		if matchGlobBounded(p.key, k) {
			return true, p.reason
		}
	}
	for _, l := range r.virtual {
		rest, ok := strings.CutPrefix(k, l.target)
		if l.target == "." {
			rest, ok = "/"+k, true
		}
		if ok && strings.HasPrefix(rest, "/") && interceptor.IsHarnessPath(l.vpath+rest) {
			return true, fmt.Sprintf("%s links to %s", harnessQuote(l.link), harnessQuote(rel))
		}
	}
	return false, ""
}

// harnessFile is one file a reader reads. abs is where its bytes are and vabs
// where the harness sees it; they differ behind a link. v is the path that
// classifies it (the repository-prefixed workspace path, or the fixed name
// below an ancestor folder), and label names it in reasons.
type harnessFile struct{ abs, vabs, v, label string }

// harnessDir is a folder the walk lists: abs on disk, vrel where the harness
// sees it, link, the link whose target is being walked ("" if none), and
// chain, the real folders of the links that led here.
type harnessDir struct {
	abs, vrel, link string
	chain           []string
}

// harnessVisit is a link target walked: its real folder and the HarnessKey
// of the path the harness sees it at (ruling R18).
type harnessVisit struct{ real, vkey string }

// maxLinkHops bounds how many links a dangling link's lexical target is
// followed through (ruling R19), Linux's own limit.
const maxLinkHops = 40

// harnessLink is a link the walk met, kept for OpenCode globs. file reports
// a resolved non-directory target; unresolved targets may become folders.
type harnessLink struct {
	abs, vrel string
	file      bool
}

// refBuilder builds one harnessRefs. The first failure is the cause, and
// every step stops once there is one.
type refBuilder struct {
	ctx        context.Context
	w          *Workspace
	refs       *harnessRefs
	entries    int
	maxEntries int
	left       int64  // bytes still allowed
	rootKey    string // HarnessKey of the root, with a trailing "/"
	boundary   string // the repository folder (repositoryPrefix)
	home       string
	lookPath   func(name string) (string, bool)
	stack      []harnessDir
	visited    map[harnessVisit]bool
	readAt     map[[2]string]int // Markdown read, by abs and vabs: the shallowest depth
	dirLinks   []harnessLink
}

// harnessReferences builds the reference set of w from scratch (#784; spec
// D1: no cache). It walks the workspace with its own bounded walker, never
// Workspace.walk and never the scope guard (D2), reads every harness file with
// the reader its name selects (D3-D5) and follows links at harness paths (D6);
// then it reads the fixed paths in every folder above the root (D12) and
// matches OpenCode globs against the folder links seen (D5).
func (w *Workspace) harnessReferences(ctx context.Context) *harnessRefs {
	b := &refBuilder{
		ctx:        ctx,
		w:          w,
		refs:       &harnessRefs{complete: true, exact: map[string]string{}, subtree: map[string]string{}},
		maxEntries: cmp.Or(w.harnessMaxEntries, harnessDefaultMaxEntries),
		left:       harnessMaxTotalBytes,
		rootKey:    interceptor.HarnessKey(w.root) + "/",
		visited:    map[harnessVisit]bool{},
		readAt:     map[[2]string]int{},
	}
	_, b.boundary = repositoryPrefix(w.root)
	b.home, _ = os.UserHomeDir() // "" drops ~/ imports
	env, _ := buildExecEnv(os.LookupEnv)
	pathValue := pathFromEnv(env)
	b.lookPath = func(name string) (string, bool) {
		hit, _, err := customLookPath(pathValue, name)
		return hit, err == nil
	}
	b.walk()
	b.ancestors()
	b.patternLinks()
	b.refs.entries = b.entries
	return b.refs
}

// fail records the first cause, "<subject> <problem>", and makes the set
// incomplete.
func (b *refBuilder) fail(subject, problem string) {
	if b.refs.complete {
		b.refs.complete = false
		b.refs.cause = strings.TrimPrefix(subject+" "+problem, " ")
	}
}

func (b *refBuilder) failed() bool { return !b.refs.complete }

// harnessQuote renders a path or pattern for a reason: slashes (ruling R4),
// then quoteArgForPreview, so a name cannot forge a line.
func harnessQuote(p string) string { return quoteArgForPreview(filepath.ToSlash(p)) }

// harnessAbsent reports a path that is not there; ENOTDIR (a regular file
// where a folder is expected) counts.
func harnessAbsent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// walk lists every folder from the root, depth first.
func (b *refBuilder) walk() {
	b.stack = []harnessDir{{abs: b.w.root, vrel: "."}}
	for len(b.stack) > 0 && !b.failed() {
		d := b.stack[len(b.stack)-1]
		b.stack = b.stack[:len(b.stack)-1]
		b.list(d)
	}
}

// list reads d in batches, checking for cancellation before each. A vanished
// folder is skipped, and so is one this user cannot search, since no tool
// running as this user reaches inside it; one it can search but not list
// makes the set incomplete, since another tool can still open a known name
// there (spec D2).
func (b *refBuilder) list(d harnessDir) {
	if b.w.beforeHarnessReadDir != nil {
		b.w.beforeHarnessReadDir(d.vrel)
	}
	f, err := os.Open(d.abs)
	if errors.Is(err, fs.ErrPermission) {
		if _, err = os.Lstat(filepath.Join(d.abs, "CLAUDE.md")); errors.Is(err, fs.ErrPermission) {
			return // unsearchable
		}
		err = fs.ErrPermission
	}
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			b.fail(harnessQuote(d.vrel), "folder cannot be listed")
		}
		return
	}
	defer func() { _ = f.Close() }()
	for !b.failed() {
		if b.ctx.Err() != nil {
			b.fail("", "walk cancelled")
			return
		}
		entries, err := f.ReadDir(256)
		for _, e := range entries {
			if b.failed() {
				return
			}
			b.entry(d, e)
		}
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			b.fail(harnessQuote(d.vrel), "folder cannot be listed")
			return
		}
	}
}

// entry classifies one walk entry. Folders are pushed unless skipped (D2);
// links go to the link pass; a harness regular file, outside .agent, goes to
// its reader, and one reached under a link adds its real path as a reference.
func (b *refBuilder) entry(d harnessDir, e fs.DirEntry) {
	if b.entries++; b.entries > b.maxEntries {
		b.fail("", "walk stopped at the entry limit")
		return
	}
	name := e.Name()
	abs, vrel := filepath.Join(d.abs, name), filepath.Join(d.vrel, name)
	v := filepath.Join(b.w.harnessPrefix, vrel)
	harness := interceptor.IsHarnessPath(v) && !slices.Contains(strings.Split(interceptor.HarnessKey(vrel), "/"), ".agent")
	switch t := e.Type(); {
	case t&fs.ModeIrregular != 0 && !t.IsDir():
		// How Go reports a Windows junction or mount point: a link this walk
		// cannot follow. Another reparse point (a cloud placeholder folder)
		// also carries ModeDir and is walked as a folder (ruling R22).
		if (harness || harnessVirtualName(name)) && runtime.GOOS == "windows" {
			b.fail(harnessQuote(vrel), "link cannot be resolved")
		}
	case t.IsDir():
		if ignoreDirs[name] || buildOutputDirs[name] {
			return
		}
		if _, err := os.Lstat(filepath.Join(abs, ".git")); err == nil {
			return // a nested repository
		}
		b.stack = append(b.stack, harnessDir{abs: abs, vrel: vrel, link: d.link, chain: d.chain})
	case t&fs.ModeSymlink != 0:
		b.symlink(d, abs, vrel, v, harness)
	case t.IsRegular() && harness:
		if d.link != "" {
			if rel, ok := b.place(abs); ok {
				b.addExact(rel, fmt.Sprintf("%s links to %s", harnessQuote(d.link), harnessQuote(rel)))
			}
		}
		b.read(harnessFile{abs: abs, vabs: filepath.Join(b.w.root, vrel), v: v, label: vrel})
	}
}

// harnessVirtualName reports a link name walked as if its target sat there
// although the name alone is no harness path (spec D2): .github and .vscode.
func harnessVirtualName(name string) bool {
	k := interceptor.HarnessKey(name)
	return k == ".github" || k == ".vscode"
}

// symlink records a link for OpenCode globs, then follows a link
// at a harness path, or named .github or .vscode (spec D6, D2). A link that
// vanished meanwhile is skipped (spec D8). A dangling link's lexical target is
// followed through further links (ruling R19), and each hop inside the
// workspace is a subtree reference, since a file may be created there. A
// resolved harness target inside the workspace is a reference: a subtree for
// a folder, a file otherwise. A .github or .vscode link's folder target, or
// dangling hop, inside the workspace is kept for covers (ruling R20). A
// folder target is walked at the link's path,
// once per real folder and classified path, never into a folder already on
// the link chain (ruling R18), and not when it holds .git; a file target is
// read by the reader the link's path selects. Any other resolution failure
// makes the set incomplete.
func (b *refBuilder) symlink(d harnessDir, abs, vrel, v string, harness bool) {
	fi, statErr := os.Stat(abs)
	b.dirLinks = append(b.dirLinks, harnessLink{abs: abs, vrel: vrel, file: statErr == nil && !fi.IsDir()})
	named := harnessVirtualName(filepath.Base(vrel))
	if !harness && !named {
		return
	}
	target, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, fs.ErrNotExist) {
		hops, vanished, err := danglingTargets(abs)
		switch {
		case vanished:
		case err != nil:
			b.fail(harnessQuote(vrel), "link cannot be resolved")
		default:
			for _, h := range hops {
				rel, ok := b.place(h)
				switch {
				case ok && harness:
					b.add(b.refs.subtree, rel, fmt.Sprintf("%s links to %s", harnessQuote(vrel), harnessQuote(rel)))
				case ok:
					b.refs.virtual = append(b.refs.virtual, harnessVirtualLink{interceptor.HarnessKey(rel), v, vrel})
				}
			}
		}
		return
	}
	if err == nil {
		if fi, err = os.Stat(target); harnessAbsent(err) {
			return // vanished meanwhile
		}
	}
	if err != nil {
		b.fail(harnessQuote(vrel), "link cannot be resolved")
		return
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		if harness {
			b.fail(harnessQuote(vrel), "is not a regular file")
		}
		return
	}
	if rel, ok := b.place(target); ok {
		why := fmt.Sprintf("%s links to %s", harnessQuote(vrel), harnessQuote(rel))
		switch {
		case harness && fi.IsDir():
			b.add(b.refs.subtree, rel, why)
		case harness:
			b.addExact(rel, why)
		case fi.IsDir():
			b.refs.virtual = append(b.refs.virtual, harnessVirtualLink{interceptor.HarnessKey(rel), v, vrel})
		}
	}
	if !fi.IsDir() {
		if harness {
			b.read(harnessFile{abs: target, vabs: filepath.Join(b.w.root, vrel), v: v, label: vrel})
		}
		return
	}
	visit := harnessVisit{target, interceptor.HarnessKey(vrel)}
	if slices.Contains(d.chain, target) || b.visited[visit] {
		return // a cycle or a revisit is not a failure
	}
	b.visited[visit] = true
	if _, err := os.Lstat(filepath.Join(target, ".git")); err == nil {
		return
	}
	b.stack = append(b.stack, harnessDir{abs: target, vrel: vrel, link: vrel, chain: append(slices.Clip(d.chain), target)})
}

// danglingTargets returns the lexical target of the dangling link abs and,
// while a target is itself a link, of each further hop (ruling R19); the last
// names nothing yet. A relative target is joined both to the link's folder as
// written and to that folder with its links resolved, which is where the
// kernel resolves "..", and the chain goes on from the latter. vanished
// reports that abs itself is gone. A chain longer than maxLinkHops, or a hop
// that cannot be read, is an error.
func danglingTargets(abs string) (hops []string, vanished bool, err error) {
	for cur, n := abs, 1; ; n++ {
		t, err := os.Readlink(cur)
		if err != nil {
			return nil, cur == abs && errors.Is(err, fs.ErrNotExist), err
		}
		next := t
		if !filepath.IsAbs(t) {
			next = filepath.Dir(cur) + string(os.PathSeparator) + t
		}
		lexical := filepath.Clean(next)
		next = resolveExisting(next)
		if lexical != next {
			hops = append(hops, lexical)
		}
		hops = append(hops, next)
		if fi, err := os.Lstat(next); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			return hops, false, nil
		}
		if n == maxLinkHops {
			return nil, false, errLinkHops
		}
		cur = next
	}
}

// errLinkHops reports a dangling link chain longer than maxLinkHops.
var errLinkHops = errors.New("too many links")

// read sends f to the reader its classified name selects: Markdown imports
// (D3) or commands (D4, and D5 for OpenCode). Other names are not read.
func (b *refBuilder) read(f harnessFile) {
	if b.failed() {
		return
	}
	k := interceptor.HarnessKey(f.v)
	base := path.Base(k)
	switch ext := path.Ext(base); {
	case ext == ".md", ext == ".mdc", base == ".cursorrules", base == ".windsurfrules", base == ".clinerules":
		b.markdown(f, 0)
	default:
		if keys, ok := commandFileKeysFor(k); ok {
			b.config(f, keys)
		}
	}
}

// open opens f through openHarnessFile. An absent file is skipped with no
// cause; any other failure makes the set incomplete.
func (b *refBuilder) open(f harnessFile) *os.File {
	file, _, err := openHarnessFile(f.abs)
	switch {
	case err == nil:
		return file
	case harnessAbsent(err):
	case errors.Is(err, errNotRegular):
		b.fail(harnessQuote(f.label), "is not a regular file")
	default:
		b.fail(harnessQuote(f.label), "cannot be read")
	}
	return nil
}

// readFailed makes the set incomplete for a read error and reports whether
// there was one.
func (b *refBuilder) readFailed(f harnessFile, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errHarnessBudget):
		b.fail(harnessQuote(f.label), "is over the size limit")
	default:
		b.fail(harnessQuote(f.label), "cannot be read")
	}
	return true
}

// markdown reads f's imports at depth (0 for a harness file). A file already
// read at this depth or shallower from the same virtual location is skipped.
// Filesystem spellings must stay distinct even when their HarnessKeys match.
func (b *refBuilder) markdown(f harnessFile, depth int) {
	key := [2]string{f.abs, f.vabs}
	if d, ok := b.readAt[key]; ok && d <= depth {
		return
	}
	b.readAt[key] = depth
	file := b.open(f)
	if file == nil {
		return
	}
	defer func() { _ = file.Close() }()
	dirs := b.folders(f)
	err := scanMarkdownImports(budgetReader{file, &b.left}, func(tok string) {
		if !b.failed() {
			b.importToken(f.label, tok, dirs, depth)
		}
	})
	b.readFailed(f, err)
}

// importToken resolves one import of the file labelled label, read at depth
// (spec D3, revision 2 F8). A relative token resolves against each of dirs, a
// leading ~/ against the home folder; every candidate, absolute ones
// canonicalized first, that lands inside the workspace is a reference whether
// or not it exists. An existing regular file is read in turn, wherever it is,
// while depth+1 is under harnessImportDepth.
func (b *refBuilder) importToken(label, tok string, dirs []string, depth int) {
	if strings.IndexByte(tok, 0) >= 0 {
		return // no file can have that name
	}
	var cands []string
	switch rest, home := strings.CutPrefix(tok, "~/"); {
	case home && b.home == "":
		return
	case home:
		cands = harnessPathCandidates(b.home, rest)
	case filepath.IsAbs(tok):
		cands = harnessPathCandidates("", tok)
	default:
		for _, d := range dirs {
			cands = append(cands, harnessPathCandidates(d, tok)...)
		}
	}
	for _, c := range cands {
		name := c
		if rel, ok := b.place(c); ok {
			name = rel
			b.addExact(rel, fmt.Sprintf("%s imports %s", harnessQuote(label), harnessQuote(rel)))
		}
		if depth+1 >= harnessImportDepth || b.failed() {
			continue
		}
		// A name nobody can reach names nothing (isAbsentStatErr, per
		// platform), and so does one this user may not search (ruling R21,
		// spec D8); any other stat failure fails closed (M8). A file that
		// stats but cannot be read still fails closed, in markdown.
		fi, err := os.Stat(c)
		switch {
		case err == nil:
			if fi.Mode().IsRegular() {
				b.markdown(harnessFile{abs: c, vabs: c, v: c, label: name}, depth+1)
			}
		case isAbsentStatErr(err), errors.Is(err, fs.ErrPermission):
		default:
			b.fail(harnessQuote(name), "cannot be read")
		}
	}
}

// config reads a D4 configuration file (keys nil for .codex/config.toml),
// resolves its commands against the anchor and its folders, and adds what
// lands inside the workspace; an OpenCode file also goes to D5.
func (b *refBuilder) config(f harnessFile, keys []string) {
	file := b.open(f)
	if file == nil {
		return
	}
	data, err := io.ReadAll(io.LimitReader(budgetReader{file, &b.left}, readFileMaxBytes+1))
	_ = file.Close()
	if err == nil && len(data) > readFileMaxBytes {
		err = errHarnessBudget
	}
	if b.readFailed(f, err) {
		return
	}
	var cs commandSet
	var doc any
	if keys == nil {
		var strs []tomlString
		if strs, err = lexTOMLStrings(data); err == nil {
			cs = extractTOMLCommands(strs)
		}
	} else if doc, err = decodeJSONC(data); err == nil {
		cs = extractCommands(keys, doc)
	}
	if err != nil {
		b.fail(harnessQuote(f.label), "does not parse")
		return
	}
	// A user-level hook's project-folder variable means the session's project,
	// not the home folder (revision 2, F1).
	anchor := anchorOf(f.vabs, f.v)
	bases := append([]string{anchor}, b.folders(f)...)
	if a, ok := nestedAnchor(f.vabs, f.v); ok && anchor == filepath.Dir(f.vabs) {
		bases = append(bases, a) // ruling R23
	}
	bases = harnessUniq(bases)
	paths, complete := resolveCommandTokens(cs, bases, harnessUniq([]string{anchor, b.w.root, b.boundary}), b.lookPath)
	if !complete {
		b.fail(harnessQuote(f.label), "names too many paths") // ruling R15
		return
	}
	for _, p := range paths {
		if rel, ok := b.place(p); ok {
			b.addExact(rel, fmt.Sprintf("%s runs %s", harnessQuote(f.label), harnessQuote(rel)))
		}
	}
	if base := path.Base(interceptor.HarnessKey(f.v)); base == "opencode.json" || base == "opencode.jsonc" {
		b.opencode(f, doc)
	}
}

// opencode adds an OpenCode file's instructions (spec D5): paths, resolved
// against its folders, as references, and globs as patterns.
func (b *refBuilder) opencode(f harnessFile, doc any) {
	paths, patterns := opencodeInstructions(doc)
	for _, dir := range b.folders(f) {
		for _, p := range paths {
			for _, c := range harnessPathCandidates(dir, p) {
				if rel, ok := b.place(c); ok {
					b.addExact(rel, fmt.Sprintf("%s instructions name %s", harnessQuote(f.label), harnessQuote(rel)))
				}
			}
		}
		for _, p := range patterns {
			for _, hp := range b.patternKeys(dir, p) {
				hp.reason = fmt.Sprintf("%s instructions match %s", harnessQuote(f.label), harnessQuote(hp.key))
				b.refs.patterns = append(b.refs.patterns, hp)
			}
		}
	}
}

// patternKeys returns the glob p, written in folder dir or absolute, as
// HarnessKey globs over workspace paths, each with its literal prefix (no
// reason). Its literal folder (the glob's own segments before the first
// holding *?[, joined to dir) is placed like a path, and never read as glob
// syntax: dir, the root and the literal part may hold *?[ as plain
// characters, so the literal part is escaped in the key and kept unescaped as
// the prefix. A glob whose literal folder lies above the root (D12) is
// narrowed to what it matches below the root, so ../opencode.json's **/*.md
// still covers the workspace's Markdown; one that leaves the root matches
// nothing.
func (b *refBuilder) patternKeys(dir, p string) []harnessPattern {
	segs := strings.Split(filepath.ToSlash(p), "/")
	i := slices.IndexFunc(segs, func(s string) bool { return strings.ContainsAny(s, "*?[") })
	if i < 0 {
		return nil
	}
	base, rest := filepath.FromSlash(strings.Join(segs[:i], "/")), segs[i:]
	if filepath.IsAbs(p) {
		base += string(os.PathSeparator)
	}
	var keys []harnessPattern
	for _, candidate := range harnessPathCandidates(dir, base) {
		keys = append(keys, b.patternBase(candidate, rest)...)
	}
	return keys
}

// patternBase places one spelling of a glob's literal folder in the workspace.
func (b *refBuilder) patternBase(base string, rest []string) []harnessPattern {
	if rel, ok := b.place(base); ok {
		prefix := interceptor.HarnessKey(rel)
		if rel == "." {
			prefix = ""
		}
		key := path.Join(harnessGlobEscape.Replace(prefix), interceptor.HarnessKey(path.Join(rest...)))
		return []harnessPattern{{key: key, prefix: prefix}}
	}
	down, err := filepath.Rel(resolveExisting(base), b.w.root)
	if err != nil || down == ".." || strings.HasPrefix(down, ".."+string(os.PathSeparator)) {
		return nil
	}
	pats := [][]string{strings.Split(interceptor.HarnessKey(path.Join(rest...)), "/")}
	for _, seg := range strings.Split(interceptor.HarnessKey(down), "/") {
		var next [][]string
		for _, pat := range pats {
			for ; len(pat) > 0 && pat[0] == "**"; pat = pat[1:] {
				next = append(next, pat) // "**" takes seg and stays
			}
			if len(pat) > 1 {
				if ok, _ := path.Match(pat[0], seg); ok {
					next = append(next, pat[1:])
				}
			}
		}
		// Every entry is a suffix of the glob, so its length identifies it.
		slices.SortFunc(next, func(x, y []string) int { return len(x) - len(y) })
		pats = slices.CompactFunc(next, func(x, y []string) bool { return len(x) == len(y) })
	}
	keys := make([]harnessPattern, 0, len(pats))
	for _, pat := range pats {
		i := slices.IndexFunc(pat, func(s string) bool { return strings.ContainsAny(s, "*?[") })
		if i < 0 {
			i = len(pat)
		}
		keys = append(keys, harnessPattern{key: strings.Join(pat, "/"), prefix: strings.Join(pat[:i], "/")})
	}
	return keys
}

// harnessGlobEscape escapes the characters path.Match treats as syntax, for
// the literal folder of a glob key.
var harnessGlobEscape = strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`)

// patternLinks makes a matched file link's in-workspace target a reference,
// and every folder link a glob reaches a subtree reference (spec D5): a link
// at or below the glob's literal prefix covers its whole target, and a link
// above the prefix the part of its target the prefix names. A dangling link
// counts through its lexical targets (danglingTargets, as D6 does for harness
// paths), since a folder may be created there. A vanished link is skipped;
// any other resolution failure makes the set incomplete.
// Known over-coverage, kept on purpose: an empty prefix (**/*.md) takes every
// folder link's target.
func (b *refBuilder) patternLinks() {
	for _, p := range b.refs.patterns {
		for _, l := range b.dirLinks {
			if b.failed() {
				return
			}
			k, extra := interceptor.HarnessKey(l.vrel), ""
			if l.file && !matchGlobBounded(p.key, k) {
				continue
			}
			switch {
			case harnessUnder(k, p.prefix):
			case harnessUnder(p.prefix, k):
				extra = p.prefix[len(k)+1:]
			default:
				continue
			}
			b.addLinkTargets(l, extra)
		}
	}
	// A covered folder can contain links outside that subtree. Follow each
	// newly reached link once, with a separate bound on fixed-point work.
	seen := make(map[harnessLink]bool)
	left := b.maxEntries
	for changed := true; changed && !b.failed(); {
		changed = false
		for _, l := range b.dirLinks {
			if b.failed() {
				return
			}
			if seen[l] {
				continue
			}
			if b.ctx.Err() != nil {
				b.fail("", "link traversal cancelled")
				return
			}
			if left--; left < 0 {
				b.fail("", "link traversal stopped at the entry limit")
				return
			}
			for k := interceptor.HarnessKey(l.vrel); ; k = path.Dir(k) {
				if _, ok := b.refs.subtree[k]; ok {
					seen[l], changed = true, true
					b.addLinkTargets(l, "")
					break
				}
				if k == "." || k == "/" {
					break
				}
			}
		}
	}
}

// addLinkTargets protects a reached link's targets, optionally below extra.
func (b *refBuilder) addLinkTargets(l harnessLink, extra string) {
	t, err := filepath.EvalSymlinks(l.abs)
	targets := []string{t}
	if errors.Is(err, fs.ErrNotExist) {
		var vanished bool
		if targets, vanished, err = danglingTargets(l.abs); vanished {
			return // spec D8
		}
	}
	if err != nil {
		b.fail(harnessQuote(l.vrel), "link cannot be resolved")
		return
	}
	for _, t := range targets {
		if rel, ok := b.place(filepath.Join(t, filepath.FromSlash(extra))); ok {
			why := fmt.Sprintf("%s links to %s", harnessQuote(l.vrel), harnessQuote(rel))
			if l.file {
				b.addExact(rel, why)
			} else {
				b.add(b.refs.subtree, rel, why)
			}
		}
	}
}

// harnessUnder reports whether the key k is p or below it; every key is below
// the empty prefix.
func harnessUnder(k, p string) bool { return p == "" || k == p || strings.HasPrefix(k, p+"/") }

// ancestors reads the fixed paths in every folder above the root, up to and
// including the volume root or until harnessAncestorStop (spec D12). An absent
// path is normal; anything else that fails makes the set incomplete, and the
// cause names the absolute path.
func (b *refBuilder) ancestors() {
	stop := b.w.harnessAncestorStop
	if stop != "" {
		if s, err := filepath.EvalSymlinks(stop); err == nil {
			stop = s
		}
	}
	files := slices.Clone(harnessAncestorFiles)
	for _, f := range HarnessCommandFiles() {
		if path.Ext(f) != "" { // .github/hooks and .github/copilot are listings
			files = append(files, f)
		}
	}
	for d := b.w.root; filepath.Dir(d) != d && !b.failed(); {
		if d = filepath.Dir(d); d == stop {
			return
		}
		if b.ctx.Err() != nil {
			b.fail("", "walk cancelled")
			return
		}
		if b.w.onHarnessAncestor != nil {
			b.w.onHarnessAncestor(d)
		}
		// Each file is classified and anchored by its fixed name below d, not
		// by the folders above d (fix round 1, I4).
		for _, name := range files {
			abs := filepath.Join(d, filepath.FromSlash(name))
			if name == ".clinerules" {
				if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
					continue // a Cline rules folder is listed below (ruling R16)
				}
			}
			b.read(harnessFile{abs: abs, vabs: abs, v: name, label: abs})
		}
		for _, l := range harnessAncestorListings {
			b.listing(d, l.dir, l.exts)
		}
	}
}

// listing reads, from one listing of the folder name below the ancestor d,
// each regular file with one of exts, through links; folders and other names
// are ignored, and a name that is no folder counts as absent.
func (b *refBuilder) listing(d, name string, exts []string) {
	if b.failed() {
		return
	}
	dir := filepath.Join(d, filepath.FromSlash(name))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !harnessAbsent(err) {
			b.fail(harnessQuote(dir), "folder cannot be listed")
		}
		return
	}
	for _, e := range entries {
		if !slices.Contains(exts, strings.ToLower(filepath.Ext(e.Name()))) {
			continue
		}
		abs := filepath.Join(dir, e.Name())
		fi, err := os.Stat(abs)
		switch {
		case harnessAbsent(err), err == nil && fi.IsDir():
		case err != nil:
			b.fail(harnessQuote(abs), "cannot be read")
		default:
			b.read(harnessFile{abs: abs, vabs: abs, v: path.Join(name, e.Name()), label: abs})
		}
	}
}

// folders returns the folders f's relative references resolve against: its
// own, and where the harness sees it when that differs (spec D6).
func (b *refBuilder) folders(f harnessFile) []string {
	return harnessUniq([]string{filepath.Dir(f.abs), filepath.Dir(f.vabs)})
}

// anchorOf returns the anchor of the D4 file the harness sees at vabs, whose
// classified path is v (spec D4): the folder above the harness folder its D4
// entry starts with (.claude for .claude/settings.json, .github for
// .github/hooks/*.json), or vabs's own folder for an entry with none
// (.mcp.json). Only those trailing components count, so a harness folder name
// higher up the path (a root under ~/.codex/) does not move it (fix round 1,
// I4). The anchor may lie outside the workspace.
func anchorOf(vabs, v string) string {
	parts := strings.Split(interceptor.HarnessKey(v), "/")
	up := 1
	switch n := len(parts); {
	case n >= 2 && slices.Contains(harnessAnchorFolders, parts[n-2]):
		up = 2
	case n >= 3 && parts[n-3] == ".github" && (parts[n-2] == "hooks" || parts[n-2] == "copilot"):
		up = 3
	}
	for range up {
		vabs = filepath.Dir(vabs)
	}
	return vabs
}

// nestedAnchor returns the folder above the first harness folder in v, the
// classified path of the file the harness sees at vabs. A folderless D4 file
// under a harness folder (.claude/skills/foo/.mcp.json) keeps it as an extra
// base beside its own folder (ruling R23).
func nestedAnchor(vabs, v string) (string, bool) {
	parts := strings.Split(interceptor.HarnessKey(v), "/")
	i := slices.IndexFunc(parts[:len(parts)-1], func(c string) bool { return slices.Contains(harnessAnchorFolders, c) })
	if i < 0 {
		return "", false
	}
	for range len(parts) - i {
		vabs = filepath.Dir(vabs)
	}
	return vabs, true
}

// harnessUniq sorts xs and drops duplicates, in place.
func harnessUniq(xs []string) []string {
	slices.Sort(xs)
	return slices.Compact(xs)
}

// rel returns the workspace-relative path of the cleaned absolute path abs,
// if it lies inside the root.
func (b *refBuilder) rel(abs string) (string, bool) {
	if !b.w.underRoot(abs) {
		return "", false
	}
	rel, err := filepath.Rel(b.w.root, abs)
	return rel, err == nil
}

// place returns the workspace-relative path of abs if it lands inside the
// root: as written, or with the links in its longest existing ancestor
// resolved, or, when only a fold of the root's spelling keeps it outside, at
// its stored spelling (revision 2, F8).
func (b *refBuilder) place(abs string) (string, bool) {
	if rel, ok := b.rel(abs); ok {
		return rel, true
	}
	r := resolveExisting(abs)
	if rel, ok := b.rel(r); ok {
		return rel, true
	}
	if !strings.HasPrefix(interceptor.HarnessKey(r), b.rootKey) {
		return "", false
	}
	stored, _, err := canonicalFuturePathVerified(filepath.VolumeName(r)+string(os.PathSeparator), r)
	if err != nil {
		return "", false
	}
	return b.rel(stored)
}

// harnessPathCandidates keeps both the lexical path and the filesystem's
// resolution: cleaning a link/.. before resolving it can name a different file.
func harnessPathCandidates(base, p string) []string {
	if !filepath.IsAbs(p) {
		p = base + string(os.PathSeparator) + p
	}
	clean := filepath.Clean(p)
	// add resolves ordinary symlink aliases after command path deduplication.
	if p == clean || !slices.Contains(strings.Split(filepath.ToSlash(p), "/"), "..") {
		return []string{clean}
	}
	return harnessUniq([]string{clean, resolveExisting(p)})
}

// resolveExisting returns p with the links in its longest existing ancestor
// resolved and the rest rejoined.
func resolveExisting(p string) string {
	var rest []string
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			slices.Reverse(rest)
			return filepath.Join(append([]string{r}, rest...)...)
		}
		dir, leaf := filepath.Split(cur)
		parent := dir
		if len(parent) > 0 && os.IsPathSeparator(parent[len(parent)-1]) {
			parent = parent[:len(parent)-1]
		}
		if parent == filepath.VolumeName(cur) {
			parent = dir // keep the volume root absolute
		}
		if parent == cur || parent == "" {
			return p
		}
		rest = append(rest, leaf)
		cur = parent
	}
}

// addExact adds a file reference; the root itself is never a file target
// (ruling R15).
func (b *refBuilder) addExact(rel, why string) {
	if rel != "." {
		b.add(b.refs.exact, rel, why)
	}
}

// add stores the key of the workspace-relative rel in m with why, unless it is
// there already, and with it the key of its stored spelling (spec D7) and of
// its resolved spelling, links in its longest existing ancestor resolved and
// the rest rejoined, so an absent file behind a linked folder is covered where
// it would really be created (ruling R17).
func (b *refBuilder) add(m map[string]string, rel, why string) {
	k := interceptor.HarnessKey(rel)
	if _, ok := m[k]; !ok {
		m[k] = why
	}
	abs := filepath.Join(b.w.root, rel)
	if s, _, err := canonicalFuturePathVerified(b.w.root, abs); err == nil {
		b.alias(m, s, why)
	}
	b.alias(m, resolveExisting(abs), why)
}

// alias adds the key of abs, if it lies inside the root, to m with why.
func (b *refBuilder) alias(m map[string]string, abs, why string) {
	if rel, ok := b.rel(abs); ok {
		if k := interceptor.HarnessKey(rel); m[k] == "" {
			m[k] = why
		}
	}
}

// openHarnessFile opens abs, following links, for a reference reader. os.Stat
// must report a regular file before the open, and the descriptor the same
// regular file after it (errNotRegular, errFileChanged); on Unix the open does
// not block, so a FIFO swapped in between cannot hang Plan (spec D2).
func openHarnessFile(abs string) (*os.File, os.FileInfo, error) {
	before, err := os.Stat(abs)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, errNotRegular
	}
	f, err := os.OpenFile(abs, harnessOpenFlags, 0)
	if err != nil {
		return nil, nil, err
	}
	after, err := f.Stat()
	switch {
	case err != nil:
	case !after.Mode().IsRegular():
		err = errNotRegular
	case !os.SameFile(before, after):
		err = errFileChanged
	default:
		return f, after, nil
	}
	_ = f.Close()
	return nil, nil, err
}

// errHarnessBudget reports a read past harnessMaxTotalBytes.
var errHarnessBudget = errors.New("harness read budget exceeded")

// budgetReader charges what it reads to a build's byte budget; the read that
// takes the budget below zero fails with errHarnessBudget.
type budgetReader struct {
	r    io.Reader
	left *int64
}

func (r budgetReader) Read(p []byte) (int, error) {
	if *r.left < 0 {
		return 0, errHarnessBudget
	}
	if int64(len(p)) > *r.left+1 {
		p = p[:*r.left+1]
	}
	n, err := r.r.Read(p)
	if *r.left -= int64(n); *r.left < 0 {
		return n, errHarnessBudget
	}
	return n, err
}
