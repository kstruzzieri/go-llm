package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const goLLM = "github.com/kstruzzieri/go-llm/"

// opsScanGlobs are the files scanned as ops code, relative to cmd/golem.
// Build-tagged files are parsed whatever the host OS.
var opsScanGlobs = []string{"ops*.go", "../../internal/opsbackend/*.go", "../../internal/opsview/*.go"}

// opsGuardedRefs are the package-qualified names that build an HTTP client
// or transport, issue a request, open or listen on a socket, resolve names,
// start a process or make a raw system call.
var opsGuardedRefs = map[string][]string{
	"net/http": {"Client", "Transport", "DefaultClient", "DefaultTransport", "Get", "Head", "Post", "PostForm",
		"NewRequest", "NewRequestWithContext", "NewFileTransport", "NewFileTransportFS"},
	"net": {"Dial", "DialTimeout", "DialIP", "DialTCP", "DialUDP", "DialUnix", "Dialer",
		"Listen", "ListenConfig", "ListenIP", "ListenMulticastUDP", "ListenPacket", "ListenTCP", "ListenUDP",
		"ListenUnix", "ListenUnixgram", "FileConn", "FileListener", "FilePacketConn",
		"LookupAddr", "LookupCNAME", "LookupHost", "LookupIP", "LookupMX", "LookupNS", "LookupPort", "LookupSRV",
		"LookupTXT", "Resolver", "DefaultResolver"},
	"crypto/tls": {"Dial", "DialWithDialer", "Dialer"},
	"os":         {"StartProcess"},
	"syscall": {"Socket", "Socketpair", "Connect", "Bind", "Sendto", "Sendmsg", "SendmsgN", "ForkExec", "Exec",
		"StartProcess", "Syscall", "Syscall6", "Syscall9", "RawSyscall", "RawSyscall6"},
	goLLM + "provider": {"GuardHTTPClient"},
}

// opsPermittedRefs is every guarded reference ops code may make, keyed by
// file, declaration and name, with its exact count: the one guarded and
// allowlisted construction (spec §4.2) and getsid. A reference anywhere
// else, a changed count, or an entry no longer present fails.
var opsPermittedRefs = map[string]int{
	"internal/opsbackend/client.go client net/http.Client":                          1, // the hc field
	"internal/opsbackend/client.go newClient net/http.Transport":                    2, // fallback literal, DefaultTransport assertion
	"internal/opsbackend/client.go newClient net/http.DefaultTransport":             1, // cloned, never used itself
	"internal/opsbackend/client.go newClient net/http.Client":                       2, // stock client for the guard, allowlist client
	"internal/opsbackend/client.go newClient " + goLLM + "provider.GuardHTTPClient": 1,
	"internal/opsbackend/client.go client.get net/http.NewRequestWithContext":       1,
	"cmd/golem/ops_watch_linux.go getsid syscall.RawSyscall":                        1, // SYS_GETSID; syscall has no Getsid on Linux
}

// opsClosedNames lists the only names ops code may use from packages that
// also reach backends (provider builds routers, slot and warmth probes;
// agent runs models) and from golem's own package-level declarations
// outside ops*.go ("main", where model admission, discovery and routers
// live; checked for ops files, while the helpers they reach are scanned
// instead). Every entry must be in use.
var opsClosedNames = map[string][]string{
	goLLM + "provider": {"Capability", "CapEmbed", "CapGenerate", "CapInsert",
		"Destination", "DestinationEdge", "DestinationGate", "DestinationPolicy",
		"DestinationPurposeDiscovery", "DestinationPurposeHealth", "ErrDestinationDenied",
		"NewDestination", "NewDestinationGate", "NewDestinationManifest"},
	goLLM + "agent": {"ModelCallCapabilities"},
	"main":          {"loadDocumentFor", "modelsJSONInput", "parseQuietly", "realTermOps", "sanitizeApprovalPreview", "termOps"},
}

// opsAllowedModuleImports are the only non-stdlib packages ops code and the
// golem helpers it reaches may use; any other (ollama, openaicompat,
// configio, providerbootstrap, x/net, ...) reaches the network outside the
// guard. Every entry must be in use.
var opsAllowedModuleImports = []string{
	goLLM + "agent", goLLM + "config", goLLM + "configview", goLLM + "provider",
	goLLM + "internal/opsbackend", goLLM + "internal/opsview", goLLM + "internal/promptfence",
	"golang.org/x/term",
}

// opsAllowedStdImports closes the standard library to what ops code and the
// golem helpers it reaches use today. Every entry must be in use.
var opsAllowedStdImports = []string{"bytes", "cmp", "context", "encoding/json", "errors", "flag", "fmt", "io",
	"maps", "math", "net", "net/http", "net/url", "os", "os/signal", "runtime", "slices", "strconv", "strings",
	"sync", "sync/atomic", "syscall", "text/tabwriter", "time", "unicode", "unicode/utf8"}

// opsForbiddenImports may never be allowed: they reach the network, run
// programs, or defeat this scan (cgo, unsafe go:linkname, reflection,
// plugins).
var opsForbiddenImports = []string{"C", "unsafe", "reflect", "plugin", "os/exec",
	"net/http/httputil", "net/http/httptest", "net/rpc"}

// opsForbiddenIdent are the probe and bootstrap entry points spec §7.2 names.
var opsForbiddenIdent = []string{"NewOpenAICompatSlotSource", "NewOllamaWarmthSource", "ProbeToolCall",
	"RefreshInventory", "providerbootstrap", "admitForSubcommand"}

// TestOpsCodeUsesOnlyGuardedClients scans golem ops code for HTTP clients,
// sockets, processes or probe entry points that bypass the allowlist. It
// also scans, transitively, every golem declaration ops code reaches in
// other cmd/golem files, with every method of each type reached. Calls into
// other packages are bounded by the import and closed-name lists, not
// followed. It supplements the dispatch canary, which proves only exercised
// paths.
func TestOpsCodeUsesOnlyGuardedClients(t *testing.T) {
	g := &opsGuard{t: t, fset: token.NewFileSet(), sites: map[string][]string{}, used: map[string]bool{},
		golem: map[string][]opsDecl{}, methods: map[string][]opsDecl{}, reached: map[ast.Node]bool{}}
	var opsDecls []opsDecl
	opsTypes := map[string]bool{}
	for _, pat := range opsScanGlobs {
		m, err := filepath.Glob(pat)
		if err != nil || len(m) == 0 {
			t.Fatalf("glob %s: %v %v", pat, m, err)
		}
		for _, path := range m {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
			main := !strings.Contains(rel, "/")
			if main {
				rel = "cmd/golem/" + rel
			}
			f, of := g.parse(path, rel, main, true)
			g.checkImports(f)
			for _, d := range f.Decls {
				eachDecl(d, func(name string, n ast.Node) {
					opsDecls = append(opsDecls, opsDecl{of, name, n})
					if ts, ok := n.(*ast.TypeSpec); ok && main {
						opsTypes[ts.Name.Name] = true
					}
				})
			}
		}
	}
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range all {
		if strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "ops") {
			continue
		}
		f, of := g.parse(path, "cmd/golem/"+path, true, false)
		for _, d := range f.Decls {
			eachDecl(d, func(name string, n ast.Node) {
				if fd, ok := n.(*ast.FuncDecl); ok && fd.Recv != nil {
					recv := strings.SplitN(name, ".", 2)[0]
					if opsTypes[recv] {
						t.Errorf("%s: method %s is declared outside ops*.go on an ops type: move it into an ops file", g.fset.Position(fd.Pos()), name)
					}
					g.methods[recv] = append(g.methods[recv], opsDecl{of, name, n})
					return
				}
				for _, id := range boundNames(n) {
					if id != "_" {
						g.golem[id] = append(g.golem[id], opsDecl{of, name, n})
					}
				}
			})
		}
	}
	for _, d := range opsDecls {
		g.scan(d)
	}
	for len(g.queue) > 0 {
		d := g.queue[0]
		g.queue = g.queue[1:]
		g.scan(d)
	}
	g.finish()
}

// opsFile is one parsed file and how its declarations are checked.
type opsFile struct {
	rel     string
	imports map[string]string // local name -> import path
	main    bool              // package main (cmd/golem)
	ops     bool              // ops code itself, not a golem helper it reaches
}

// opsDecl is one top-level declaration and its label.
type opsDecl struct {
	file *opsFile
	name string
	node ast.Node
}

// opsGuard accumulates findings across every scanned declaration.
type opsGuard struct {
	t       *testing.T
	fset    *token.FileSet
	sites   map[string][]string  // guarded references by "file decl pkg.Name"
	used    map[string]bool      // closed names and allowed imports in use
	golem   map[string][]opsDecl // golem declarations outside ops*.go, by name
	methods map[string][]opsDecl // golem methods outside ops*.go, by receiver type
	reached map[ast.Node]bool
	queue   []opsDecl
}

func (g *opsGuard) parse(path, rel string, main, ops bool) (*ast.File, *opsFile) {
	f, err := parser.ParseFile(g.fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		g.t.Fatal(err)
	}
	of := &opsFile{rel: rel, imports: map[string]string{}, main: main, ops: ops}
	for _, is := range f.Imports {
		p, _ := strconv.Unquote(is.Path.Value)
		name := p[strings.LastIndex(p, "/")+1:]
		if is.Name != nil {
			name = is.Name.Name
		}
		of.imports[name] = p
	}
	return f, of
}

// checkImports refuses dot imports and any import outside the allowlists,
// blank ones included.
func (g *opsGuard) checkImports(f *ast.File) {
	for _, is := range f.Imports {
		p, _ := strconv.Unquote(is.Path.Value)
		if is.Name != nil && is.Name.Name == "." {
			g.t.Errorf("%s: dot import of %s hides what ops code calls", g.fset.Position(is.Pos()), p)
			continue
		}
		if why := g.importRefusal(p); why != "" {
			g.t.Errorf("%s: ops code may not import %s (%s)", g.fset.Position(is.Pos()), p, why)
		}
	}
}

// importRefusal reports why p is not allowed, or "" and marks it used.
func (g *opsGuard) importRefusal(p string) string {
	switch {
	case slices.Contains(opsForbiddenImports, p):
		return "in opsForbiddenImports"
	case !strings.Contains(strings.SplitN(p, "/", 2)[0], "."):
		if !slices.Contains(opsAllowedStdImports, p) {
			return "not in opsAllowedStdImports"
		}
	case !slices.Contains(opsAllowedModuleImports, p):
		if strings.HasPrefix(p, goLLM+"internal/") {
			return "not in opsAllowedModuleImports; an internal package allowed there must also join opsScanGlobs"
		}
		return "not in opsAllowedModuleImports"
	}
	g.used[p] = true
	return ""
}

// scan applies every rule to one declaration and queues the golem
// declarations it reaches.
func (g *opsGuard) scan(d opsDecl) {
	g.reached[d.node] = true
	if fd, ok := d.node.(*ast.FuncDecl); ok && fd.Body == nil {
		g.t.Errorf("%s: %s has no body (assembly or go:linkname)", g.fset.Position(fd.Pos()), d.name)
	}
	// Names the declaration binds itself, and identifiers that name a
	// field, method or struct key rather than a package-level declaration.
	// ponytail: scope by name, not by block. A declaration that binds a
	// local x and also refers to golem's package-level x passes; go/types
	// would close that at the cost of loading all of package main.
	locals, notRef := map[string]bool{}, map[*ast.Ident]bool{}
	ast.Inspect(d.node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, e := range x.Lhs {
					if id, ok := e.(*ast.Ident); ok {
						locals[id.Name] = true
					}
				}
			}
		case *ast.RangeStmt:
			if x.Tok == token.DEFINE {
				for _, e := range []ast.Expr{x.Key, x.Value} {
					if id, ok := e.(*ast.Ident); ok {
						locals[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, id := range x.Names {
				locals[id.Name] = true
			}
		case *ast.FuncType:
			for _, l := range []*ast.FieldList{x.TypeParams, x.Params, x.Results} {
				bindFields(locals, l)
			}
		case *ast.Field: // struct fields, methods and parameters
			for _, id := range x.Names {
				notRef[id] = true
			}
		case *ast.TypeSpec:
			locals[x.Name.Name] = true
		case *ast.FuncDecl:
			notRef[x.Name] = true
			bindFields(locals, x.Recv)
		case *ast.CompositeLit:
			// Keys name struct fields, except in a map literal, where
			// they are values. A named map type is not recognized, so
			// its keys are skipped too.
			if _, isMap := x.Type.(*ast.MapType); !isMap {
				for _, e := range x.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							notRef[id] = true
						}
					}
				}
			}
		case *ast.SelectorExpr:
			notRef[x.Sel] = true
		}
		return true
	})
	ast.Inspect(d.node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				if pkg, ok := d.file.imports[id.Name]; ok {
					notRef[id] = true
					g.ref(d, pkg, x.Sel.Name, x.Pos())
				}
			}
		case *ast.Ident:
			if slices.Contains(opsForbiddenIdent, x.Name) {
				g.t.Errorf("%s: forbidden reference %s (opsForbiddenIdent)", g.fset.Position(x.Pos()), x.Name)
			}
			if !d.file.main || locals[x.Name] || notRef[x] || len(g.golem[x.Name]) == 0 {
				return true
			}
			if d.file.ops {
				g.closed(d, "main", x.Name, x.Pos())
			}
			for _, r := range g.golem[x.Name] {
				g.enqueue(r)
				if _, ok := r.node.(*ast.TypeSpec); ok {
					for _, m := range g.methods[x.Name] {
						g.enqueue(m)
					}
				}
			}
		}
		return true
	})
}

func (g *opsGuard) enqueue(d opsDecl) {
	if !g.reached[d.node] {
		g.reached[d.node] = true
		g.queue = append(g.queue, d)
	}
}

// ref checks one package-qualified reference.
func (g *opsGuard) ref(d opsDecl, pkg, name string, pos token.Pos) {
	if !d.file.ops { // a helper's file imports are not ops code's; check each use
		if why := g.importRefusal(pkg); why != "" {
			g.t.Errorf("%s: %s, reached from ops code, uses %s.%s (%s)", g.fset.Position(pos), d.name, pkg, name, why)
		}
	}
	if slices.Contains(opsGuardedRefs[pkg], name) {
		key := d.file.rel + " " + d.name + " " + pkg + "." + name
		g.sites[key] = append(g.sites[key], g.fset.Position(pos).String())
		return
	}
	g.closed(d, pkg, name, pos)
}

func (g *opsGuard) closed(d opsDecl, pkg, name string, pos token.Pos) {
	allowed, closed := opsClosedNames[pkg]
	if !closed {
		return
	}
	if !slices.Contains(allowed, name) {
		g.t.Errorf("%s: %s uses %s.%s, not in opsClosedNames[%q]", g.fset.Position(pos), d.name, pkg, name, pkg)
	}
	g.used[pkg+"."+name] = true
}

// finish compares guarded references with opsPermittedRefs and reports
// allowances no longer in use.
func (g *opsGuard) finish() {
	keys := slices.Collect(maps.Keys(g.sites))
	for k := range opsPermittedRefs {
		if _, ok := g.sites[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		if len(g.sites[key]) == opsPermittedRefs[key] {
			continue
		}
		ref := key[strings.LastIndex(key, " ")+1:]
		var where []string
		for k := range opsPermittedRefs {
			if strings.HasSuffix(k, " "+ref) {
				where = append(where, strings.TrimSuffix(k, " "+ref))
			}
		}
		slices.Sort(where)
		only := "nowhere"
		if len(where) > 0 {
			only = "only in " + strings.Join(where, ", ")
		}
		g.t.Errorf("guarded reference %s: found %d, permitted %d by opsPermittedRefs, at %v (%s is permitted %s)",
			key, len(g.sites[key]), opsPermittedRefs[key], g.sites[key], ref, only)
	}
	for _, pkg := range slices.Sorted(maps.Keys(opsClosedNames)) {
		for _, name := range opsClosedNames[pkg] {
			if !g.used[pkg+"."+name] {
				g.t.Errorf("opsClosedNames[%q] permits %s, which ops code no longer uses: remove it", pkg, name)
			}
		}
	}
	for list, paths := range map[string][]string{"opsAllowedStdImports": opsAllowedStdImports, "opsAllowedModuleImports": opsAllowedModuleImports} {
		for _, p := range paths {
			if !g.used[p] {
				g.t.Errorf("%s permits %s, which ops code no longer uses: remove it", list, p)
			}
		}
	}
}

// eachDecl calls fn for each top-level declaration in d with its label: a
// function (Type.Method for a method), or each type or value spec.
func eachDecl(d ast.Decl, fn func(name string, n ast.Node)) {
	switch d := d.(type) {
	case *ast.FuncDecl:
		name := d.Name.Name
		if d.Recv != nil && len(d.Recv.List) == 1 {
			t := d.Recv.List[0].Type
			if s, ok := t.(*ast.StarExpr); ok {
				t = s.X
			}
			if ix, ok := t.(*ast.IndexExpr); ok { // generic receiver
				t = ix.X
			}
			if id, ok := t.(*ast.Ident); ok {
				name = id.Name + "." + name
			}
		}
		fn(name, d)
	case *ast.GenDecl:
		for _, s := range d.Specs {
			switch s := s.(type) {
			case *ast.TypeSpec:
				fn(s.Name.Name, s)
			case *ast.ValueSpec:
				fn(s.Names[0].Name, s)
			}
		}
	}
}

// boundNames returns the package-level names a declaration binds.
func boundNames(n ast.Node) []string {
	switch n := n.(type) {
	case *ast.FuncDecl:
		return []string{n.Name.Name}
	case *ast.TypeSpec:
		return []string{n.Name.Name}
	case *ast.ValueSpec:
		var out []string
		for _, id := range n.Names {
			out = append(out, id.Name)
		}
		return out
	}
	return nil
}

// bindFields records the names a parameter, result or receiver list binds.
func bindFields(locals map[string]bool, l *ast.FieldList) {
	if l == nil {
		return
	}
	for _, f := range l.List {
		for _, id := range f.Names {
			locals[id.Name] = true
		}
	}
}
