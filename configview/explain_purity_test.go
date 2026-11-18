package configview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// TestExplain_Purity fails if a change makes ExplainCapabilities or
// ExplainListing, or any module code they reach through static calls, function
// literals or named functions used as values, do any of these: call a function
// or method declared outside the module that is not on a fixed list of ones
// that do no I/O; call an interface method other than ReadSnapshot on a
// fingerprint.EvidenceReader or Err and Done on a context.Context; hold more
// than one ReadSnapshot call site, or hold it inside a loop, a helper or a
// function literal; call code through a function-typed variable, parameter,
// field, element or result; start a goroutine, print with the builtins, or use
// unsafe or //go:linkname; or, at run time, call any store method other than
// one ReadSnapshot, dial through http.DefaultTransport or a transport cloned
// from it, or create a file under the directories GO_LLM_CONFIG,
// XDG_CONFIG_HOME and HOME name. It checks the files built for the host it runs
// on, with no build tags. It does not follow methods the standard library calls
// on values the readers pass to it (Error, String, MarshalJSON, Is, Unwrap), and
// it is not a defence against code written to evade it. Package initialization
// and code outside the module are out of scope. Two subtests, Signature and
// NewFiles, are convenience checks with clearer messages: Allow and FuncValue
// already fail any use of a banned parameter.
func TestExplain_Purity(t *testing.T) {
	// Load runs whatever -run selects: every other subtest needs it. A failure
	// is reported once, by the Load subtest, or here when -run skipped it.
	u, timing, err := purityLoad()
	loadRan := false
	t.Run("Load", func(t *testing.T) {
		loadRan = true
		if err != nil {
			t.Fatal(err)
		}
		t.Log(timing)
	})
	if err != nil {
		if !loadRan {
			t.Fatalf("Load: %v", err)
		}
		t.FailNow()
	}
	readers := u.analyze(t, []string{purityModule + "/configview.ExplainCapabilities", purityModule + "/configview.ExplainListing"}, nil)
	for _, f := range u.newFiles(t) {
		readers.newFile(f.file, f.pkg)
	}
	got := readers.byRule()

	t.Run(ruleWalk, func(t *testing.T) {
		u.runRows(t, ruleWalk)
		purityReport(t, got[ruleWalk])
		walked := map[string]bool{}
		for _, name := range readers.walked {
			walked[name] = true
		}
		for _, name := range purityMustWalk {
			if !walked[name] {
				t.Errorf("the walk never reached %s, which the readers call", name)
			}
		}
		external := make([]string, 0, len(readers.reached))
		for name := range readers.reached {
			external = append(external, name)
		}
		sort.Strings(external)
		t.Logf("readers: %d module functions walked; %d functions outside the module reached: %s", len(readers.walked), len(external), strings.Join(external, ", "))
	})
	for _, rule := range []string{ruleAllow, ruleFuncValue, ruleReject} {
		t.Run(rule, func(t *testing.T) {
			u.runRows(t, rule)
			purityReport(t, got[rule])
		})
	}
	t.Run("Runtime", purityRuntime)
	t.Run(ruleSignature, func(t *testing.T) {
		u.runRows(t, ruleSignature)
		var sig, files []purityViolation
		for _, v := range got[ruleSignature] {
			if strings.HasPrefix(v.msg, "NewFiles: ") {
				files = append(files, v)
			} else {
				sig = append(sig, v)
			}
		}
		purityReport(t, sig)
		t.Run("NewFiles", func(t *testing.T) { purityReport(t, files) })
	})
}

const (
	purityModule = "github.com/kstruzzieri/go-llm"
	// purityRowPkg is a fixture row's package path. It counts as module code.
	purityRowPkg = "example.com/purityrow"
	// puritySnapshot is the one store read, by receiver type and method.
	puritySnapshot = purityModule + "/fingerprint.EvidenceReader.ReadSnapshot"
	// purityListTimeout bounds Load's go list run.
	purityListTimeout = 3 * time.Minute
)

// The rules. Each is a subtest; NewFiles reports under Signature.
const (
	ruleWalk      = "Walk" // a module function the walk cannot find a declaration for
	ruleAllow     = "Allow"
	ruleFuncValue = "FuncValue"
	ruleReject    = "Reject"
	ruleSignature = "Signature"
)

var purityRules = []string{ruleWalk, ruleAllow, ruleFuncValue, ruleReject, ruleSignature}

// purityAllow lists, by (*types.Func).FullName, the functions and methods
// declared outside the module that walked code may call or use as a value. A
// name is added only when it does no I/O and calls no function value or
// interface method its caller supplies, and each addition says why in the
// change that adds it. slices.SortFunc is the one function-value exception:
// its comparator is walked. fmt.Errorf, fmt.Sprintf and errors.Is do call
// methods of the values they are given (Error, String, Format, Is, Unwrap); they
// are listed only under the doc comment's stated limit, and no further function
// that calls methods of an interface value it is given may be added. If the walk
// reaches time.Time's Format or String methods, or anything in sync, reflect or
// runtime, stop and ask the maintainer instead of adding it.
var purityAllow = map[string]bool{
	"time.Now":                     true,
	"(time.Time).After":            true,
	"(time.Time).Before":           true,
	"(time.Time).Equal":            true,
	"(time.Time).IsZero":           true,
	"(time.Time).Add":              true,
	"strings.Contains":             true,
	"strings.HasPrefix":            true,
	"strings.HasSuffix":            true,
	"strings.TrimPrefix":           true,
	"strings.TrimSuffix":           true,
	"strings.LastIndex":            true,
	"strings.ToLower":              true,
	"strings.TrimRight":            true,
	"strings.Split":                true,
	"strings.SplitSeq":             true,
	"strings.Join":                 true,
	"strings.Compare":              true,
	"cmp.Compare":                  true,
	"(*regexp.Regexp).MatchString": true,
	"slices.Sort":                  true,
	"slices.SortFunc":              true,
	"slices.Contains":              true,
	"slices.Clone":                 true,
	"slices.Compact":               true,
	"sort.Strings":                 true,
	"errors.New":                   true,
	"errors.Is":                    true,
	"fmt.Errorf":                   true,
	"fmt.Sprintf":                  true,
	"net/url.ParseRequestURI":      true,
	"(*net/url.URL).Hostname":      true,
	"(*net/url.URL).Port":          true,
	"(*net/url.URL).EscapedPath":   true,
	"net.ParseIP":                  true,
	"(net.IP).String":              true,
	"(net.IP).IsLoopback":          true,
}

// purityBuiltins are the builtins walked code may call. print and println are
// Reject's, and package unsafe's builtins are Reject's through the reference.
var purityBuiltins = map[string]bool{
	"len": true, "cap": true, "append": true, "make": true, "new": true, "copy": true,
	"delete": true, "clear": true, "min": true, "max": true, "panic": true,
}

// purityBannedParams are the parameter types no reader may take; a function
// type is banned too.
var purityBannedParams = map[string]bool{
	purityModule + "/provider.Provider":            true,
	"*" + purityModule + "/provider.ModelRegistry": true,
	"*net/http.Client":                             true,
	"net/http.RoundTripper":                        true,
}

// purityNewFiles are the files #736 adds that must declare no init function and
// no package-level variable initialized by a call: package path, file name.
var purityNewFiles = [][2]string{
	{purityModule + "/config", "capabilities.go"},
	{purityModule + "/configview", "explain.go"},
	{purityModule + "/provider", "explain_evidence.go"},
}

// purityMustWalk are module functions the readers call; the walk must walk
// each body, so a walk that silently stops early fails Walk.
var purityMustWalk = []string{
	purityModule + "/provider.EvidenceBindingFor",
	purityModule + "/provider.EvidenceModelNames",
	purityModule + "/provider.ExplainEvidence",
	"(*" + purityModule + "/provider.Catalog).ProfileFor",
	"(*" + purityModule + "/config.Config).CapabilityPolicy",
	purityModule + "/fingerprint.ReadCapabilities",
	purityModule + "/internal/evidencesel.Select",
}

// purityIsModule reports whether path is module code: a package of this module,
// or a fixture row's.
func purityIsModule(path string) bool {
	return path == purityModule || strings.HasPrefix(path, purityModule+"/") || path == purityRowPkg
}

// purityPkg is one type-checked package.
type purityPkg struct {
	types *types.Package
	info  *types.Info
	files []*ast.File
}

// purityFunc is one function or method declaration.
type purityFunc struct {
	decl *ast.FuncDecl
	file *ast.File
	pkg  *purityPkg
}

// purityUniverse is every module package configview depends on, type-checked
// from source, with every other import read from the export data go list built.
type purityUniverse struct {
	root  string
	fset  *token.FileSet
	imp   types.Importer
	pkgs  map[string]*purityPkg
	funcs map[string]purityFunc // by (*types.Func).FullName
}

type purityImporter func(path string) (*types.Package, error)

func (f purityImporter) Import(path string) (*types.Package, error) { return f(path) }

// purityLoad runs go list once and type-checks the module packages it names. It
// returns a line of timings for the Load subtest to log.
func purityLoad() (*purityUniverse, string, error) {
	start := time.Now()
	root, err := purityModuleRoot()
	if err != nil {
		return nil, "", err
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		return nil, "", fmt.Errorf("find the go tool: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), purityListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, goTool, "list", "-deps", "-export", "-json", purityModule+"/configview")
	cmd.Dir = root
	// GOFLAGS replaces any inherited value, so no build tags leak in; GOPROXY=off
	// fails fast instead of fetching a module the cache lacks.
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=readonly", "GOPROXY=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, "", fmt.Errorf("%s list -deps -export did not finish within %v; a cold build cache compiles every dependency", goTool, purityListTimeout)
	}
	if err != nil {
		return nil, "", fmt.Errorf("%s list -deps -export (GOFLAGS=-mod=readonly, GOPROXY=off; a module missing from the cache needs go mod download first): %w\n%s", goTool, err, stderr.Bytes())
	}
	listed := time.Since(start)

	type listedPkg struct {
		ImportPath string
		Dir        string
		Export     string
		GoFiles    []string
		Error      *struct{ Err string }
	}
	exports := map[string]string{}
	var module []listedPkg
	for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); {
		var p listedPkg
		if err := dec.Decode(&p); err != nil {
			return nil, "", fmt.Errorf("decode go list output: %w", err)
		}
		if p.Error != nil {
			return nil, "", fmt.Errorf("go list: %s: %s", p.ImportPath, p.Error.Err)
		}
		if purityIsModule(p.ImportPath) {
			module = append(module, p) // go list -deps lists dependencies first
		} else if p.Export != "" {
			exports[p.ImportPath] = p.Export
		}
	}

	u := &purityUniverse{root: root, fset: token.NewFileSet(), pkgs: map[string]*purityPkg{}, funcs: map[string]purityFunc{}}
	gc := importer.ForCompiler(u.fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("go list built no export data for %q", path)
		}
		return os.Open(file)
	})
	u.imp = purityImporter(func(path string) (*types.Package, error) {
		if p, ok := u.pkgs[path]; ok {
			return p.types, nil
		}
		if purityIsModule(path) {
			return nil, fmt.Errorf("module package %q is not in configview's dependency closure", path)
		}
		return gc.Import(path)
	})
	for _, p := range module {
		files := make([]*ast.File, 0, len(p.GoFiles))
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(u.fset, filepath.Join(p.Dir, name), nil, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				return nil, "", fmt.Errorf("parse %s: %w", name, err)
			}
			files = append(files, f)
		}
		pkg, err := u.check(p.ImportPath, files)
		if err != nil {
			return nil, "", fmt.Errorf("type-check %s: %w", p.ImportPath, err)
		}
		u.pkgs[p.ImportPath] = pkg
		pkg.declare(u.funcs)
	}
	took := time.Since(start)
	return u, fmt.Sprintf("Load: %v wall (go list -deps -export %v, type-check %v); %d module packages from source, %d export files; go tool %s",
		took.Round(time.Millisecond), listed.Round(time.Millisecond), (took - listed).Round(time.Millisecond), len(module), len(exports), goTool), nil
}

// purityModuleRoot is the directory holding go.mod, walking up from the test's
// working directory.
func purityModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// check type-checks one package with full types.Info and reports every error.
func (u *purityUniverse) check(path string, files []*ast.File) (*purityPkg, error) {
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Instances:  map[*ast.Ident]types.Instance{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Implicits:  map[ast.Node]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Scopes:     map[ast.Node]*types.Scope{},
	}
	var errs []error
	conf := types.Config{
		Importer: u.imp,
		Sizes:    types.SizesFor("gc", runtime.GOARCH),
		Error:    func(err error) { errs = append(errs, err) },
	}
	pkg, _ := conf.Check(path, u.fset, files, info)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &purityPkg{types: pkg, info: info, files: files}, nil
}

// declare indexes the package's function and method declarations by full name.
func (p *purityPkg) declare(into map[string]purityFunc) {
	for _, f := range p.files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name == "init" || fd.Name.Name == "_" {
				continue
			}
			if fn, ok := p.info.Defs[fd.Name].(*types.Func); ok {
				into[fn.FullName()] = purityFunc{decl: fd, file: f, pkg: p}
			}
		}
	}
}

// purityFile is a file NewFiles checks.
type purityFile struct {
	file *ast.File
	pkg  *purityPkg
}

// newFiles finds the files #736 adds; a missing one fails the test, so a rename
// cannot quietly empty NewFiles.
func (u *purityUniverse) newFiles(t *testing.T) []purityFile {
	var out []purityFile
	for _, nf := range purityNewFiles {
		path, base := nf[0], nf[1]
		p := u.pkgs[path]
		if p == nil {
			t.Fatalf("NewFiles: package %s not loaded", path)
		}
		found := false
		for _, f := range p.files {
			if filepath.Base(u.fset.Position(f.Package).Filename) == base {
				out = append(out, purityFile{file: f, pkg: p})
				found = true
			}
		}
		if !found {
			t.Fatalf("NewFiles: %s has no file %s", path, base)
		}
	}
	return out
}

// purityViolation is one rule's finding at one source position.
type purityViolation struct {
	rule string
	pos  string
	msg  string
}

func (v purityViolation) String() string { return v.rule + ": " + v.pos + ": " + v.msg }

func purityReport(t *testing.T, vs []purityViolation) {
	t.Helper()
	for _, v := range vs {
		t.Error(v)
	}
}

// purityAnalysis is one walk from a set of roots.
type purityAnalysis struct {
	u       *purityUniverse
	row     map[string]purityFunc // a fixture row's functions; nil for the readers
	roots   map[string]bool
	seen    map[string]bool
	queue   []string
	files   map[*ast.File]bool
	walked  []string
	parent  map[string]string // who first reached each walked function
	reached map[string]bool   // functions outside the module reached, by full name
	out     []purityViolation
}

// analyze walks everything roots reach and checks the roots' signatures. row is
// the fixture row's package, or nil for the readers.
func (u *purityUniverse) analyze(t *testing.T, roots []string, row *purityPkg) *purityAnalysis {
	t.Helper()
	a := &purityAnalysis{u: u, roots: map[string]bool{}, seen: map[string]bool{}, files: map[*ast.File]bool{}, parent: map[string]string{}, reached: map[string]bool{}}
	if row != nil {
		a.row = map[string]purityFunc{}
		row.declare(a.row)
	}
	for _, name := range roots {
		f, ok := a.fn(name)
		if !ok {
			t.Fatalf("no declaration for root %s", name)
		}
		a.roots[name], a.seen[name] = true, true
		a.queue = append(a.queue, name)
		a.signature(f, name)
	}
	for len(a.queue) > 0 {
		name := a.queue[0]
		a.queue = a.queue[1:]
		f, _ := a.fn(name)
		a.walked = append(a.walked, name)
		a.file(f.file, name)
		if f.decl.Body == nil {
			continue // a call to it is Reject's
		}
		w := &purityBody{a: a, fn: name, info: f.pkg.info, root: a.roots[name], done: map[ast.Expr]bool{}, sel: map[*ast.Ident]bool{}, rangeX: map[*ast.CallExpr]bool{}}
		w.walk(f.decl.Body)
	}
	return a
}

func (a *purityAnalysis) fn(name string) (purityFunc, bool) {
	if f, ok := a.row[name]; ok {
		return f, true
	}
	f, ok := a.u.funcs[name]
	return f, ok
}

// report records a violation at at. fn is the walked function holding it, ""
// for none; the message then says how the walk reached fn.
func (a *purityAnalysis) report(rule, fn string, at ast.Node, format string, args ...any) {
	p := a.u.fset.Position(at.Pos())
	if rel, err := filepath.Rel(a.u.root, p.Filename); err == nil && !strings.HasPrefix(rel, "..") {
		p.Filename = rel
	}
	msg := fmt.Sprintf(format, args...)
	if fn != "" {
		msg += " (reached via " + a.path(fn) + ")"
	}
	a.out = append(a.out, purityViolation{rule: rule, pos: p.String(), msg: msg})
}

// path is the chain of calls or references by which the walk first reached fn.
// Parents form a tree: each is set once, to a function queued earlier.
func (a *purityAnalysis) path(fn string) string {
	var chain []string
	for name := fn; name != ""; name = a.parent[name] {
		chain = append(chain, strings.NewReplacer(purityModule+"/", "", purityRowPkg+".", "").Replace(name))
	}
	slices.Reverse(chain)
	return strings.Join(chain, " -> ")
}

func (a *purityAnalysis) byRule() map[string][]purityViolation {
	m := map[string][]purityViolation{}
	for _, v := range a.out {
		m[v.rule] = append(m[v.rule], v)
	}
	return m
}

// file checks, once per file holding a walked function (fn, the first walked
// there), for an import of unsafe and for //go:linkname directives.
func (a *purityAnalysis) file(f *ast.File, fn string) {
	if a.files[f] {
		return
	}
	a.files[f] = true
	for _, imp := range f.Imports {
		if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == "unsafe" {
			a.report(ruleReject, fn, imp, "import of unsafe in a file holding a walked function")
		}
	}
	for _, g := range f.Comments {
		for _, c := range g.List {
			if strings.HasPrefix(c.Text, "//go:linkname") {
				a.report(ruleReject, fn, c, "//go:linkname directive in a file holding a walked function")
			}
		}
	}
}

// module records a reach of module function fn from the walked function from,
// as a call or as a value, and queues its body.
func (a *purityAnalysis) module(fn *types.Func, from string, at ast.Node, called bool) {
	name := fn.Origin().FullName()
	if a.roots[name] {
		a.report(ruleAllow, from, at, "reader %s reached from walked code", name)
	}
	f, ok := a.fn(name)
	if !ok {
		a.report(ruleWalk, from, at, "no declaration found for module function %s", name)
		return
	}
	if called && f.decl.Body == nil {
		a.report(ruleReject, from, at, "call of %s, declared without a body", name)
	}
	if !a.seen[name] {
		a.seen[name] = true
		a.parent[name] = from
		a.queue = append(a.queue, name)
	}
}

// external records a reach of fn, declared outside the module, from the walked
// function from, as a call or as a value.
func (a *purityAnalysis) external(fn *types.Func, from string, at ast.Node) {
	name := fn.Origin().FullName()
	a.reached[name] = true
	if !purityAllow[name] {
		a.report(ruleAllow, from, at, "%s is declared outside the module and not on the list of functions that do no I/O; add it to purityAllow only if it does no I/O and calls nothing its caller supplies, and say why in the change that adds it", name)
	}
}

// signature checks root name's parameters, aliases resolved.
func (a *purityAnalysis) signature(f purityFunc, name string) {
	fn, _ := f.pkg.info.Defs[f.decl.Name].(*types.Func)
	if fn == nil {
		return
	}
	sig := fn.Type().(*types.Signature)
	params := sig.Params()
	for i := range params.Len() {
		p := params.At(i)
		typ := p.Type()
		if sig.Variadic() && i == params.Len()-1 {
			typ = typ.(*types.Slice).Elem()
		}
		typ = types.Unalias(typ)
		if ptr, ok := typ.(*types.Pointer); ok {
			typ = types.NewPointer(types.Unalias(ptr.Elem()))
		}
		switch {
		case purityBannedParams[types.TypeString(typ, nil)]:
			a.report(ruleSignature, name, f.decl.Name, "reader %s takes parameter %s of type %s", fn.Name(), p.Name(), types.TypeString(typ, nil))
		case isPuritySignature(typ):
			a.report(ruleSignature, name, f.decl.Name, "reader %s takes parameter %s of function type %s", fn.Name(), p.Name(), types.TypeString(typ, nil))
		}
	}
}

func isPuritySignature(t types.Type) bool {
	_, ok := t.Underlying().(*types.Signature)
	return ok
}

// newFile checks one of #736's files for an init function and for a
// package-level variable initialized by a call.
func (a *purityAnalysis) newFile(f *ast.File, p *purityPkg) {
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == "init" {
				a.report(ruleSignature, "", d.Name, "NewFiles: init function")
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, s := range d.Specs {
				vs := s.(*ast.ValueSpec)
				for i, v := range vs.Values {
					if purityHasCall(p.info, v) {
						name := vs.Names[min(i, len(vs.Names)-1)]
						a.report(ruleSignature, "", name, "NewFiles: package-level variable %s initialized by a call", name.Name)
					}
				}
			}
		}
	}
}

// purityHasCall reports whether e calls a function, outside any function
// literal; conversions and builtins are not calls here.
func purityHasCall(info *types.Info, e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			tv := info.Types[ast.Unparen(n.Fun)]
			if !tv.IsType() && !tv.IsBuiltin() {
				found = true
			}
		}
		return !found
	})
	return found
}

// purityBody walks one function body, function literals included.
type purityBody struct {
	a      *purityAnalysis
	fn     string // the walked function's full name
	info   *types.Info
	root   bool                   // the body is a reader's own
	sites  int                    // ReadSnapshot call sites accepted in this reader
	done   map[ast.Expr]bool      // judged as part of their parent: callees, selector names
	sel    map[*ast.Ident]bool    // selector names (x.Sel)
	rangeX map[*ast.CallExpr]bool // calls written directly as a range expression
}

func (w *purityBody) walk(body *ast.BlockStmt) {
	var stack []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		switch n := n.(type) {
		case *ast.GoStmt:
			w.a.report(ruleReject, w.fn, n, "go statement")
		case *ast.RangeStmt:
			if c, ok := ast.Unparen(n.X).(*ast.CallExpr); ok {
				w.rangeX[c] = true
			}
		case *ast.SelectorExpr:
			w.sel[n.Sel], w.done[n.Sel] = true, true
		case *ast.CallExpr:
			w.call(n, slices.ContainsFunc(stack, func(s ast.Node) bool {
				switch s.(type) {
				case *ast.ForStmt, *ast.RangeStmt, *ast.FuncLit:
					return true
				}
				return false
			}))
		case *ast.Ident:
			w.unsafeRef(n)
		}
		if e, ok := n.(ast.Expr); ok {
			w.value(e)
		}
		stack = append(stack, n)
		return true
	})
}

// call classifies one call: (i) conversion, (ii) builtin, (iii) static module
// call, (iv) other static call, (v) dynamic call, (vi) a function literal called
// where it appears; anything else is a call through a function value.
func (w *purityBody) call(c *ast.CallExpr, enclosed bool) {
	fun := ast.Unparen(c.Fun)
	if w.info.Types[fun].IsType() {
		return // (i)
	}
	w.done[fun] = true
	if x := purityInstantiated(w.info, fun); x != nil {
		fun = x
		w.done[fun] = true
	}
	if w.info.Types[fun].IsBuiltin() {
		w.builtin(c, fun) // (ii)
		return
	}
	if _, ok := fun.(*ast.FuncLit); ok {
		return // (vi): its body is walked where it appears
	}
	fn, recv := w.resolve(fun)
	switch {
	case fn == nil:
		w.a.report(ruleFuncValue, w.fn, c, "call through the function value %s", types.ExprString(fun))
	case recv != nil:
		w.dynamic(c, fn, recv, enclosed) // (v)
	case purityIsModule(fn.Pkg().Path()):
		w.a.module(fn, w.fn, c, true) // (iii)
	default:
		w.a.external(fn, w.fn, c) // (iv)
	}
}

// purityInstantiated returns the generic function an explicit instantiation
// e (f[T] or f[T, U]) names, or nil.
func purityInstantiated(info *types.Info, e ast.Expr) ast.Expr {
	var x ast.Expr
	switch e := e.(type) {
	case *ast.IndexExpr:
		x = e.X
	case *ast.IndexListExpr:
		x = e.X
	default:
		return nil
	}
	x = ast.Unparen(x)
	var id *ast.Ident
	switch v := x.(type) {
	case *ast.Ident:
		id = v
	case *ast.SelectorExpr:
		id = v.Sel
	default:
		return nil
	}
	if _, ok := info.Instances[id]; !ok {
		return nil
	}
	if _, ok := info.Uses[id].(*types.Func); !ok {
		return nil
	}
	return x
}

// resolve returns the function or method e names, and for a dynamic call (an
// interface or type-parameter receiver) the receiver's static type. It returns
// nil for anything that is not a named function or method.
func (w *purityBody) resolve(e ast.Expr) (*types.Func, types.Type) {
	switch e := e.(type) {
	case *ast.Ident:
		fn, _ := w.info.Uses[e].(*types.Func)
		return fn, nil
	case *ast.SelectorExpr:
		if s, ok := w.info.Selections[e]; ok {
			fn, ok := s.Obj().(*types.Func)
			if !ok {
				return nil, nil // a field
			}
			if purityDynamic(s.Recv(), fn) {
				return fn, s.Recv()
			}
			return fn, nil
		}
		fn, _ := w.info.Uses[e.Sel].(*types.Func)
		return fn, nil
	}
	return nil, nil
}

func purityDynamic(recv types.Type, fn *types.Func) bool {
	if p, ok := types.Unalias(recv).(*types.Pointer); ok {
		recv = p.Elem()
	}
	if _, ok := types.Unalias(recv).(*types.TypeParam); ok {
		return true
	}
	r := fn.Type().(*types.Signature).Recv()
	return r != nil && types.IsInterface(r.Type())
}

// dynamic judges a call of an interface method by its receiver's static type.
func (w *purityBody) dynamic(c *ast.CallExpr, fn *types.Func, recv types.Type, enclosed bool) {
	name := types.TypeString(recv, nil) + "." + fn.Name()
	switch name {
	case puritySnapshot:
		switch {
		case !w.root:
			w.a.report(ruleAllow, w.fn, c, "ReadSnapshot call site outside a reader's own body")
		case enclosed:
			w.a.report(ruleAllow, w.fn, c, "ReadSnapshot call site inside a loop or function literal")
		case w.sites > 0:
			w.a.report(ruleAllow, w.fn, c, "second ReadSnapshot call site in this reader")
		default:
			w.sites++
		}
	case "context.Context.Err", "context.Context.Done":
	default:
		w.a.report(ruleAllow, w.fn, c, "call of interface method %s, not ReadSnapshot on fingerprint.EvidenceReader or Err or Done on context.Context", name)
	}
}

func (w *purityBody) builtin(c *ast.CallExpr, fun ast.Expr) {
	var b *types.Builtin
	switch f := fun.(type) {
	case *ast.Ident:
		b, _ = w.info.Uses[f].(*types.Builtin)
	case *ast.SelectorExpr:
		b, _ = w.info.Uses[f.Sel].(*types.Builtin)
	}
	switch {
	case b == nil:
		w.a.report(ruleWalk, w.fn, c, "unresolved builtin call %s", types.ExprString(fun))
	case b.Pkg() != nil:
		// package unsafe's: Reject reports the reference
	case b.Name() == "print" || b.Name() == "println":
		w.a.report(ruleReject, w.fn, c, "call of builtin %s", b.Name())
	case !purityBuiltins[b.Name()]:
		w.a.report(ruleAllow, w.fn, c, "call of builtin %s, not on the list", b.Name())
	}
}

// unsafeRef reports a reference to package unsafe.
func (w *purityBody) unsafeRef(id *ast.Ident) {
	if w.sel[id] {
		return // x.Sel: x is judged
	}
	switch obj := w.info.Uses[id].(type) {
	case *types.PkgName:
		if obj.Imported().Path() == "unsafe" {
			w.a.report(ruleReject, w.fn, id, "reference to package unsafe")
		}
	case types.Object:
		if obj.Pkg() != nil && obj.Pkg().Path() == "unsafe" {
			w.a.report(ruleReject, w.fn, id, "reference to package unsafe")
		}
	}
}

// value judges a function-typed expression outside callee position: a function
// literal, a named function or concrete method (walked, or on Allow's list),
// and a listed iterator call written directly as a range expression pass;
// everything else is FuncValue's.
func (w *purityBody) value(e ast.Expr) {
	if w.done[e] {
		return
	}
	switch e.(type) {
	case *ast.ParenExpr, *ast.FuncLit:
		return
	}
	if x := purityInstantiated(w.info, e); x != nil {
		w.done[x] = true
		w.named(x)
		return
	}
	if w.named(e) {
		return
	}
	tv, ok := w.info.Types[e]
	if !ok || !tv.IsValue() || tv.IsNil() || !isPuritySignature(tv.Type) {
		return
	}
	if c, ok := e.(*ast.CallExpr); ok && w.rangeX[c] && w.listedIterator(c) {
		return
	}
	w.a.report(ruleFuncValue, w.fn, e, "function value %s used other than as a named function, a literal or a listed iterator", types.ExprString(e))
}

// named handles e when it names a function or method used as a value.
func (w *purityBody) named(e ast.Expr) bool {
	switch e.(type) {
	case *ast.Ident, *ast.SelectorExpr:
	default:
		return false
	}
	fn, recv := w.resolve(e)
	switch {
	case fn == nil:
		return false
	case recv != nil:
		w.a.report(ruleFuncValue, w.fn, e, "interface method %s used as a value", types.ExprString(e))
	case purityIsModule(fn.Pkg().Path()):
		w.a.module(fn, w.fn, e, false)
	default:
		w.a.external(fn, w.fn, e)
	}
	return true
}

// listedIterator reports whether c calls a function declared outside the module
// that is on Allow's list (strings.SplitSeq).
func (w *purityBody) listedIterator(c *ast.CallExpr) bool {
	fun := ast.Unparen(c.Fun)
	if x := purityInstantiated(w.info, fun); x != nil {
		fun = x
	}
	fn, recv := w.resolve(fun)
	return fn != nil && recv == nil && !purityIsModule(fn.Pkg().Path()) && purityAllow[fn.Origin().FullName()]
}

// purityRow is a fixture: a few lines of Go, type-checked in memory and never
// compiled. Its functions named Reader... are the roots. It passes when rule
// reports exactly want violations, one of them containing msg, and no other
// rule reports any.
type purityRow struct {
	name string
	sub  string // the subtest that runs it
	rule string
	want int
	msg  string // when want > 0, some report of rule contains it
	src  string // the file after its package clause
}

var purityRows = []purityRow{
	// Walk: each hides one strings.Repeat call reachable only through the
	// named path.
	{"deferred_literal", ruleWalk, ruleAllow, 1, "strings.Repeat", `import "strings"
func Reader() { defer func() { _ = strings.Repeat("a", 2) }() }`},
	{"immediate_literal", ruleWalk, ruleAllow, 1, "strings.Repeat", `import "strings"
func Reader() { func() { _ = strings.Repeat("a", 2) }() }`},
	{"explicit_instantiation", ruleWalk, ruleAllow, 1, "strings.Repeat", `import "strings"
func id[T any](x T) T { _ = strings.Repeat("a", 2); return x }
func Reader() int { return id[int](1) }`},
	{"parenthesized_callee", ruleWalk, ruleAllow, 1, "strings.Repeat", `import "strings"
func hidden() { _ = strings.Repeat("a", 2) }
func Reader() { (hidden)() }`},
	{"method_expression_call", ruleWalk, ruleAllow, 1, "strings.Repeat", `import "strings"
type T struct{}
func (T) M() { _ = strings.Repeat("a", 2) }
func Reader() { T.M(T{}) }`},
	{"recursion", ruleWalk, ruleAllow, 1, "strings.Repeat", `import "strings"
func rec(n int) {
	if n > 0 {
		rec(n - 1)
	}
	_ = strings.Repeat("a", 2)
}
func Reader() { rec(1) }`},
	{"named_comparator", ruleWalk, ruleAllow, 1, "strings.Repeat", `import ("slices"; "strings")
func cmpInts(a, b int) int { _ = strings.Repeat("a", 2); return a - b }
func Reader(xs []int) { slices.SortFunc(xs, cmpInts) }`},
	{"literal_argument_hidden", ruleWalk, ruleAllow, 1, "strings.Repeat", `import ("slices"; "strings")
func Reader(xs []int) { slices.SortFunc(xs, func(a, b int) int { _ = strings.Repeat("a", 2); return a - b }) }`},
	{"method_value_comparator", ruleWalk, ruleAllow, 1, "strings.Repeat", `import ("slices"; "strings")
type T struct{}
func (T) cmp(a, b int) int { _ = strings.Repeat("a", 2); return a - b }
func Reader(xs []int) { slices.SortFunc(xs, T{}.cmp) }`},
	{"two_type_arguments", ruleWalk, ruleAllow, 1, "strings.Repeat", `import "strings"
func f[A, B any](a A, _ B) A { _ = strings.Repeat("a", 2); return a }
func Reader() int { return f[int, string](1, "") }`},
	{"string_conversion", ruleWalk, ruleWalk, 0, "", `func Reader(b []byte) string { return string(b) }`},
	{"builtin_len", ruleWalk, ruleWalk, 0, "", `func Reader(s string) int { return len(s) }`},

	// Allow.
	{"os_readfile", ruleAllow, ruleAllow, 1, "os.ReadFile", `import "os"
func Reader() { _, _ = os.ReadFile("x") }`},
	{"unlisted_pure", ruleAllow, ruleAllow, 1, "strings.Repeat", `import "strings"
func Reader() { _ = strings.Repeat("a", 2) }`},
	{"constraint_method", ruleAllow, ruleAllow, 1, "interface method T.M", `type impl struct{}
func (impl) M() {}
func f[T interface{ M() }](t T) { t.M() }
func Reader() { f(impl{}) }`},
	{"embedded_interface_method", ruleAllow, ruleAllow, 1, "purityrow.S.Read", `import "io"
type S struct{ io.Reader }
func Reader(s S) { _, _ = s.Read(nil) }`},
	{"other_store_method", ruleAllow, ruleAllow, 1, "EvidenceReader.EvidenceRows", `import ("context"; "github.com/kstruzzieri/go-llm/fingerprint")
func Reader(ctx context.Context, store fingerprint.EvidenceReader) { _, _ = store.EvidenceRows(ctx, "e", "m") }`},
	{"set_finalizer", ruleAllow, ruleAllow, 1, "runtime.SetFinalizer", `import "runtime"
func Reader() { x := new(int); runtime.SetFinalizer(x, nil) }`},
	{"func_value_off_list", ruleAllow, ruleAllow, 1, "os.LookupEnv", `import "os"
func Reader() { _ = os.LookupEnv }`},
	{"err_on_other_interface", ruleAllow, ruleAllow, 1, "purityrow.rows.Err", `type rows interface{ Err() error }
func Reader(r rows) { _ = r.Err() }`},
	{"builtin_off_list", ruleAllow, ruleAllow, 1, "builtin recover", `func Reader() { _ = recover() }`},
	{"second_snapshot_site", ruleAllow, ruleAllow, 1, "second ReadSnapshot call site", `import ("context"; "time"; "github.com/kstruzzieri/go-llm/fingerprint")
func Reader(ctx context.Context, store fingerprint.EvidenceReader) {
	now := time.Now()
	_, _ = store.ReadSnapshot(ctx, "e", "a", nil, "", nil, now)
	_, _ = store.ReadSnapshot(ctx, "e", "a", nil, "", nil, now)
}`},
	{"snapshot_in_loop", ruleAllow, ruleAllow, 1, "ReadSnapshot call site inside a loop", `import ("context"; "time"; "github.com/kstruzzieri/go-llm/fingerprint")
func Reader(ctx context.Context, store fingerprint.EvidenceReader) {
	for i := 0; i < 2; i++ {
		_, _ = store.ReadSnapshot(ctx, "e", "a", nil, "", nil, time.Time{})
	}
}`},
	{"snapshot_in_helper", ruleAllow, ruleAllow, 1, "ReadSnapshot call site outside a reader's own body", `import ("context"; "time"; "github.com/kstruzzieri/go-llm/fingerprint")
func read(ctx context.Context, store fingerprint.EvidenceReader) {
	_, _ = store.ReadSnapshot(ctx, "e", "a", nil, "", nil, time.Time{})
}
func Reader(ctx context.Context, store fingerprint.EvidenceReader) { read(ctx, store) }`},
	{"snapshot_in_range", ruleAllow, ruleAllow, 1, "ReadSnapshot call site inside a loop", `import ("context"; "time"; "github.com/kstruzzieri/go-llm/fingerprint")
func Reader(ctx context.Context, store fingerprint.EvidenceReader) {
	for range 2 {
		_, _ = store.ReadSnapshot(ctx, "e", "a", nil, "", nil, time.Time{})
	}
}`},
	{"snapshot_in_literal", ruleAllow, ruleAllow, 1, "ReadSnapshot call site inside a loop or function literal", `import ("context"; "time"; "github.com/kstruzzieri/go-llm/fingerprint")
func Reader(ctx context.Context, store fingerprint.EvidenceReader) {
	func() { _, _ = store.ReadSnapshot(ctx, "e", "a", nil, "", nil, time.Time{}) }()
}`},
	{"reader_recursion", ruleAllow, ruleAllow, 1, "reader example.com/purityrow.Reader reached from walked code", `func Reader() { helper() }
func helper() { Reader() }`},
	{"reader_calls_reader", ruleAllow, ruleAllow, 1, "reader example.com/purityrow.ReaderB reached from walked code", `func ReaderA() { ReaderB() }
func ReaderB() {}`},

	// FuncValue.
	{"param_call", ruleFuncValue, ruleFuncValue, 1, "function value f", `func helper(f func()) { f() }
func Reader() { helper(func() {}) }`},
	{"field_call", ruleFuncValue, ruleFuncValue, 1, "function value h.f", `func Reader() { h := struct{ f func() }{f: func() {}}; h.f() }`},
	{"result_call", ruleFuncValue, ruleFuncValue, 1, "function value g()", `func g() func() { return func() {} }
func Reader() { g()() }`},
	{"global_func_var", ruleFuncValue, ruleFuncValue, 1, "function value hook", `var hook = func() {}
func Reader() { hook() }`},
	{"interface_method_value", ruleFuncValue, ruleFuncValue, 1, "interface method err.Error", `func Reader(err error) { _ = err.Error }`},
	{"range_module_iterator", ruleFuncValue, ruleFuncValue, 1, "function value seq()", `import "iter"
func seq() iter.Seq[int] { return nil }
func Reader() {
	for x := range seq() {
		_ = x
	}
}`},
	{"var_as_argument", ruleFuncValue, ruleFuncValue, 1, "function value cmp", `import "slices"
func sortWith(xs []int, cmp func(a, b int) int) { slices.SortFunc(xs, cmp) }
func Reader(xs []int) { sortWith(xs, func(a, b int) int { return a - b }) }`},
	{"deferred_var", ruleFuncValue, ruleFuncValue, 1, "function value f", `func Reader() { f := func() {}; defer f() }`},
	{"interface_method_expression", ruleFuncValue, ruleFuncValue, 1, "interface method error.Error", `func Reader() { _ = error.Error }`},
	{"comparator_literal", ruleFuncValue, ruleFuncValue, 0, "", `import ("cmp"; "slices")
func Reader(xs []int) { slices.SortFunc(xs, func(a, b int) int { return cmp.Compare(a, b) }) }`},
	{"splitseq_range", ruleFuncValue, ruleFuncValue, 0, "", `import "strings"
func Reader(s string) {
	for p := range strings.SplitSeq(s, ",") {
		_ = p
	}
}`},
	{"deferred_literal_clean", ruleFuncValue, ruleFuncValue, 0, "", `func Reader() { defer func() {}() }`},

	// Reject.
	{"go_stmt", ruleReject, ruleReject, 1, "go statement", `func Reader() { go func() {}() }`},
	{"bodyless_call", ruleReject, ruleReject, 1, "purityrow.ext, declared without a body", `func ext()
func Reader() { ext() }`},
	{"linkname_only", ruleReject, ruleReject, 1, "//go:linkname", `//go:linkname nanotime runtime.nanotime
func nanotime() int64
func Reader() {}`},
	{"print_println", ruleReject, ruleReject, 2, "builtin println", `func Reader() { print("a"); println("b") }`},
	{"unsafe_sizeof", ruleReject, ruleReject, 2, "reference to package unsafe", `import "unsafe"
func Reader() uintptr { return unsafe.Sizeof(0) }`},
	{"unsafe_aliased", ruleReject, ruleReject, 2, "reference to package unsafe", `import u "unsafe"
func Reader(p *int) { _ = u.Pointer(p) }`},
	{"blank_unsafe_import", ruleReject, ruleReject, 1, "import of unsafe", `import _ "unsafe"
func Reader() {}`},

	// Signature, with NewFiles.
	{"provider_param", ruleSignature, ruleSignature, 1, "provider.Provider", `import "github.com/kstruzzieri/go-llm/provider"
func Reader(p provider.Provider) {}`},
	{"registry_param", ruleSignature, ruleSignature, 1, "provider.ModelRegistry", `import "github.com/kstruzzieri/go-llm/provider"
func Reader(r *provider.ModelRegistry) {}`},
	{"client_param", ruleSignature, ruleSignature, 1, "*net/http.Client", `import "net/http"
func Reader(c *http.Client) {}`},
	{"round_tripper_param", ruleSignature, ruleSignature, 1, "net/http.RoundTripper", `import "net/http"
func Reader(rt http.RoundTripper) {}`},
	{"func_param", ruleSignature, ruleSignature, 1, "function type func()", `func Reader(f func()) {}`},
	{"variadic_provider", ruleSignature, ruleSignature, 1, "parameter ps of type github.com/kstruzzieri/go-llm/provider.Provider", `import "github.com/kstruzzieri/go-llm/provider"
func Reader(ps ...provider.Provider) {}`},
	{"alias_client_param", ruleSignature, ruleSignature, 1, "parameter c of type *net/http.Client", `import "net/http"
type C = *http.Client
func Reader(c C) {}`},
	{"init_func", ruleSignature, ruleSignature, 1, "init function", `func init() {}
func Reader() {}`},
	{"var_from_call", ruleSignature, ruleSignature, 1, "variable x initialized by a call", `func one() int { return 1 }
var x = one()
func Reader() int { return x }`},
}

// runRows runs sub's fixture rows.
func (u *purityUniverse) runRows(t *testing.T, sub string) {
	t.Helper()
	ran := 0
	for _, r := range purityRows {
		if r.sub != sub {
			continue
		}
		ran++
		t.Run(r.name, func(t *testing.T) {
			f, err := parser.ParseFile(u.fset, r.name+".go", "package purityrow\n\n"+r.src, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse row: %v", err)
			}
			p, err := u.check(purityRowPkg, []*ast.File{f})
			if err != nil {
				t.Fatalf("type-check row: %v", err)
			}
			var roots []string
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && strings.HasPrefix(fd.Name.Name, "Reader") {
					roots = append(roots, p.info.Defs[fd.Name].(*types.Func).FullName())
				}
			}
			if len(roots) == 0 {
				t.Fatal("row has no Reader function")
			}
			if r.want > 0 && r.msg == "" {
				t.Fatal("a row that wants a report needs msg")
			}
			a := u.analyze(t, roots, p)
			a.newFile(f, p)
			for _, v := range a.out {
				t.Log(v)
			}
			got := a.byRule()
			for _, rule := range purityRules {
				want := 0
				if rule == r.rule {
					want = r.want
				}
				if len(got[rule]) != want {
					t.Errorf("%s reported %d, want %d: %v", rule, len(got[rule]), want, got[rule])
				}
			}
			if r.want > 0 && !slices.ContainsFunc(got[r.rule], func(v purityViolation) bool { return strings.Contains(v.msg, r.msg) }) {
				t.Errorf("no %s report contains %q: %v", r.rule, r.msg, got[r.rule])
			}
		})
	}
	if ran == 0 {
		t.Fatalf("no fixture rows for %s", sub)
	}
}

// errPurityDial is what the network trap returns.
var errPurityDial = errors.New("purity: dial refused by the test's trap")

// purityRuntime runs each reader against a recording store (a good snapshot, a
// read error, a snapshot without the model) and with no store, for an Ollama
// tag the catalog knows, an Ollama model read under two spellings and an
// openai-compat model. http.DefaultTransport is a recording trap, and the
// config, data, cache and state directories (and GO_LLM_CONFIG's file) are
// redirected into an empty temp dir. After every run the trap must have seen
// only the self-check's dial and the temp dir must be empty.
func purityRuntime(t *testing.T) {
	catalog, err := provider.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	keys := []provider.ModelKey{
		{Provider: "ol", Model: "qwen3:8b"},     // a catalog profile
		{Provider: "ol", Model: "qwen3:latest"}, // read under qwen3 and qwen3:latest
		{Provider: "lc", Model: "m"},            // openai-compat
	}
	if catalog.ProfileFor(keys[0].Model, "") == nil {
		t.Fatalf("catalog has no profile for %s; the fixture needs one", keys[0].Model)
	}
	if names := provider.EvidenceModelNames(fingerprint.AdapterOllama, keys[1].Model); len(names) != 2 {
		t.Fatalf("EvidenceModelNames(%s) = %v; the fixture needs two spellings", keys[1].Model, names)
	}
	cfg := exConfig("sk-purity", map[string]config.ModelConfig{
		"tagged": {Provider: "ol", Name: keys[0].Model, Type: "dense"},
		"latest": {Provider: "ol", Name: keys[1].Model, Type: "dense"},
		"remote": {Provider: "lc", Name: keys[2].Model, Type: "dense"},
	})
	at := time.Now().Add(-time.Hour)
	// snapshot has a listing whose one member is member, and a row and an
	// access record for each spelling of model.
	snapshot := func(b provider.EvidenceBinding, model, member string) fingerprint.EvidenceSnapshot {
		snap := fingerprint.EvidenceSnapshot{
			Generation: 1,
			Listing:    &fingerprint.ListingSnapshot{Endpoint: b.Endpoint, StartedAt: at, CommittedAt: at, Members: []fingerprint.ListingMember{{Model: member, Digest: "sha256:d"}}},
			Named:      []fingerprint.NamedModel{{Model: model, AddedAt: at}},
			Access:     map[string]*fingerprint.ModelAccess{},
		}
		for _, n := range provider.EvidenceModelNames(b.Adapter, model) {
			snap.Rows = append(snap.Rows, exRow(b, n, "sha256:d", "chat", fingerprint.CapProbeYes, at))
			snap.Access[n] = &fingerprint.ModelAccess{Model: n, State: fingerprint.AccessAnswered, ObservedAt: at}
		}
		return snap
	}

	// The trap is an *http.Transport, so a client that clones
	// http.DefaultTransport (GuardHTTPClient) keeps its DialContext.
	var mu sync.Mutex
	var dials []string
	orig := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		Proxy: nil,
		DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			dials = append(dials, network+" "+addr)
			return nil, errPurityDial
		},
	}
	t.Cleanup(func() { http.DefaultTransport = orig })
	dialed := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(dials)
	}
	// GO_LLM_CONFIG names a file, so it points inside the dir: an atomic
	// writer's temp file then lands in the checked dir too.
	dir := t.TempDir()
	env := map[string]string{"GO_LLM_CONFIG": filepath.Join(dir, "models.json")}
	for _, v := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"} {
		env[v] = dir
	}
	for v, val := range env {
		t.Setenv(v, val)
	}

	// Self-checks: each guard is in place before the readers run.
	resp, err := http.DefaultClient.Get("http://127.0.0.1:1/")
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, errPurityDial) || len(dialed()) != 1 {
		t.Fatalf("self-check: http.DefaultClient.Get = %v with dials %v; want the trap's error and one dial", err, dialed())
	}
	for v, val := range env {
		if got := os.Getenv(v); got != val {
			t.Fatalf("self-check: %s = %q, want %q", v, got, val)
		}
	}

	// after checks one run: no dial beyond the self-check's, nothing created.
	// What it reports is removed, so the next run starts clean.
	seen := 1
	after := func(run string) {
		t.Helper()
		if got := dialed(); len(got) > seen {
			t.Errorf("%s: dialed %v through http.DefaultTransport", run, got[seen:])
			seen = len(got)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			t.Errorf("%s: created %s under the redirected directories", run, e.Name())
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	// returned checks a reader's error: nil, or ErrEvidenceUnreadable after a
	// failed read.
	returned := func(run string, err error, failedRead bool) {
		t.Helper()
		switch {
		case failedRead && !errors.Is(err, provider.ErrEvidenceUnreadable):
			t.Errorf("%s: error %v, want ErrEvidenceUnreadable", run, err)
		case !failedRead && err != nil:
			t.Errorf("%s: error %v, want nil", run, err)
		}
	}
	// reads checks that a recording store saw exactly one ReadSnapshot.
	reads := func(run string, r *recordingReader) {
		t.Helper()
		if got := r.callNames(); !slices.Equal(got, []string{"ReadSnapshot"}) {
			t.Errorf("%s: called store methods %v, want exactly one ReadSnapshot", run, got)
		}
	}

	readErr := errors.New("store: disk on fire")
	runs := []struct {
		name   string
		member func(model string) string // nil: the read fails
	}{
		{"good", func(model string) string { return model }},
		{"read_error", nil},
		{"no_member", func(string) string { return "other:1b" }},
	}
	for _, key := range keys {
		b := exBinding(t, cfg, key.Provider)
		for _, run := range runs {
			label := fmt.Sprintf("ExplainCapabilities(%s/%s) %s", key.Provider, key.Model, run.name)
			r := &recordingReader{err: readErr}
			if run.member != nil {
				r = &recordingReader{snap: snapshot(b, key.Model, run.member(key.Model))}
			}
			_, err := ExplainCapabilities(t.Context(), cfg, r, catalog, key)
			returned(label, err, run.member == nil)
			reads(label, r)
			after(label)
		}
		label := fmt.Sprintf("ExplainCapabilities(%s/%s) nil_store", key.Provider, key.Model)
		_, err := ExplainCapabilities(t.Context(), cfg, nil, catalog, key)
		returned(label, err, false)
		after(label)
	}
	for _, key := range []provider.ModelKey{keys[0], keys[2]} {
		b := exBinding(t, cfg, key.Provider)
		for _, run := range runs {
			label := fmt.Sprintf("ExplainListing(%s) %s", key.Provider, run.name)
			r := &recordingReader{err: readErr}
			if run.member != nil {
				r = &recordingReader{snap: snapshot(b, key.Model, run.member(key.Model))}
			}
			_, err := ExplainListing(t.Context(), cfg, r, key.Provider)
			returned(label, err, run.member == nil)
			reads(label, r)
			after(label)
		}
		label := fmt.Sprintf("ExplainListing(%s) nil_store", key.Provider)
		_, err := ExplainListing(t.Context(), cfg, nil, key.Provider)
		returned(label, err, false)
		after(label)
	}
}
