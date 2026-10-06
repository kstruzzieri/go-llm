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

// opsGuardedRefs are the package-qualified names that build an HTTP client
// or transport, issue a request or open a connection.
var opsGuardedRefs = map[string][]string{
	"net/http": {"Client", "Transport", "DefaultClient", "DefaultTransport", "Get", "Head", "Post", "PostForm",
		"NewRequest", "NewRequestWithContext", "NewFileTransport", "NewFileTransportFS"},
	"net":              {"Dial", "DialTimeout", "DialIP", "DialTCP", "DialUDP", "DialUnix", "Dialer"},
	"crypto/tls":       {"Dial", "DialWithDialer", "Dialer"},
	"syscall":          {"Socket", "Connect"},
	goLLM + "provider": {"GuardHTTPClient"},
}

// opsPermittedRefs is every guarded reference ops code may make, keyed by
// file, declaration and name, with its exact count: the one guarded and
// allowlisted construction (spec §4.2). A reference anywhere else, a changed
// count, or an entry no longer present fails.
var opsPermittedRefs = map[string]int{
	"internal/opsbackend/client.go client net/http.Client":                          1, // the hc field
	"internal/opsbackend/client.go newClient net/http.Transport":                    2, // fallback literal, DefaultTransport assertion
	"internal/opsbackend/client.go newClient net/http.DefaultTransport":             1, // cloned, never used itself
	"internal/opsbackend/client.go newClient net/http.Client":                       2, // stock client for the guard, allowlist client
	"internal/opsbackend/client.go newClient " + goLLM + "provider.GuardHTTPClient": 1,
	"internal/opsbackend/client.go client.get net/http.NewRequestWithContext":       1,
}

// opsClosedNames lists the only names ops code may use from provider (which
// also builds routers, slot and warmth probes) and from golem's own
// package-level declarations outside ops*.go ("main", where model
// admission, discovery and routers live). Every entry must be in use.
var opsClosedNames = map[string][]string{
	goLLM + "provider": {"Destination", "DestinationEdge", "DestinationGate", "DestinationPolicy",
		"DestinationPurposeDiscovery", "DestinationPurposeHealth", "ErrDestinationDenied",
		"NewDestination", "NewDestinationGate", "NewDestinationManifest"},
	"main": {"loadDocumentFor", "modelsJSONInput", "parseQuietly", "realTermOps", "sanitizeApprovalPreview", "termOps"},
}

// opsAllowedModuleImports are the only non-stdlib imports ops code may use;
// any other (ollama, openaicompat, configio, providerbootstrap, x/net, ...)
// reaches the network outside the guard.
var opsAllowedModuleImports = []string{
	goLLM + "config", goLLM + "configview", goLLM + "provider",
	goLLM + "internal/opsbackend", goLLM + "internal/opsview", goLLM + "internal/promptfence",
}

// opsForbiddenStdImports issue requests, or run programs that can.
var opsForbiddenStdImports = []string{"net/http/httputil", "net/http/httptest", "net/rpc", "os/exec"}

// TestOpsCodeUsesOnlyGuardedClients scans golem ops code for HTTP clients or
// probe entry points that bypass the allowlist. It supplements the dispatch
// canary, which proves only exercised paths.
func TestOpsCodeUsesOnlyGuardedClients(t *testing.T) {
	var files []string
	for _, pat := range []string{"ops*.go", "../../internal/opsbackend/*.go", "../../internal/opsview/*.go"} {
		m, err := filepath.Glob(pat)
		if err != nil || len(m) == 0 {
			t.Fatalf("glob %s: %v %v", pat, m, err)
		}
		files = append(files, m...)
	}
	forbiddenIdent := map[string]bool{
		"NewOpenAICompatSlotSource": true, "NewOllamaWarmthSource": true, "ProbeToolCall": true,
		"RefreshInventory": true, "providerbootstrap": true, "admitForSubcommand": true,
	}
	mainNames := golemNamesOutsideOps(t)
	fset := token.NewFileSet()
	sites := map[string][]string{}
	used := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		isMain := !strings.Contains(rel, "/")
		if isMain {
			rel = "cmd/golem/" + rel
		}
		imports := map[string]string{}
		for _, is := range f.Imports {
			p, _ := strconv.Unquote(is.Path.Value)
			name := p[strings.LastIndex(p, "/")+1:]
			if is.Name != nil {
				name = is.Name.Name
			}
			nonStd := strings.Contains(strings.SplitN(p, "/", 2)[0], ".")
			switch {
			case name == ".":
				t.Errorf("%s: dot import of %s hides what ops code calls", fset.Position(is.Pos()), p)
			case slices.Contains(opsForbiddenStdImports, p) || nonStd && !slices.Contains(opsAllowedModuleImports, p):
				t.Errorf("%s: ops code may not import %s", fset.Position(is.Pos()), p)
			}
			imports[name] = p
		}
		ref := func(decl, pkg, name string, pos token.Pos) {
			if slices.Contains(opsGuardedRefs[pkg], name) {
				key := rel + " " + decl + " " + pkg + "." + name
				sites[key] = append(sites[key], fset.Position(pos).String())
				return
			}
			if allowed, closed := opsClosedNames[pkg]; closed {
				if !slices.Contains(allowed, name) {
					t.Errorf("%s: forbidden reference %s.%s (not in opsClosedNames)", fset.Position(pos), pkg, name)
				}
				used[pkg+"."+name] = true
			}
		}
		for _, d := range f.Decls {
			eachDecl(d, func(decl string, node ast.Node) {
				// Names the declaration binds itself, and identifiers that
				// name a field, method or key rather than a package-level
				// declaration.
				// ponytail: scope by name, not by block. A declaration that
				// binds a local x and also refers to golem's package-level x
				// passes; go/types would close that at the cost of loading
				// all of package main.
				locals, notRef := map[string]bool{}, map[*ast.Ident]bool{}
				ast.Inspect(node, func(n ast.Node) bool {
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
					case *ast.KeyValueExpr:
						if id, ok := x.Key.(*ast.Ident); ok {
							notRef[id] = true
						}
					case *ast.SelectorExpr:
						notRef[x.Sel] = true
					}
					return true
				})
				ast.Inspect(node, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.SelectorExpr:
						if id, ok := x.X.(*ast.Ident); ok {
							if pkg, ok := imports[id.Name]; ok {
								notRef[id] = true
								ref(decl, pkg, x.Sel.Name, x.Pos())
							}
						}
					case *ast.Ident:
						if forbiddenIdent[x.Name] {
							t.Errorf("%s: forbidden reference %s", fset.Position(x.Pos()), x.Name)
						}
						if isMain && mainNames[x.Name] && !locals[x.Name] && !notRef[x] {
							ref(decl, "main", x.Name, x.Pos())
						}
					}
					return true
				})
			})
		}
	}
	keys := slices.Collect(maps.Keys(sites))
	for k := range opsPermittedRefs {
		if _, ok := sites[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		if len(sites[key]) != opsPermittedRefs[key] {
			t.Errorf("guarded reference %s: found %d, permitted %d at %v (spec §4.2: clients, transports and requests are built only by opsbackend/client.go's guarded construction)",
				key, len(sites[key]), opsPermittedRefs[key], sites[key])
		}
	}
	for pkg, names := range opsClosedNames {
		for _, name := range names {
			if !used[pkg+"."+name] {
				t.Errorf("opsClosedNames permits %s.%s, which ops code no longer uses: remove it", pkg, name)
			}
		}
	}
}

// eachDecl calls fn for each top-level declaration in d with its name: a
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

// golemNamesOutsideOps returns golem's package-level names declared in
// non-test files other than ops*.go.
func golemNamesOutsideOps(t *testing.T) map[string]bool {
	t.Helper()
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	fset := token.NewFileSet()
	for _, path := range all {
		if strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "ops") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					names[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						names[s.Name.Name] = true
					case *ast.ValueSpec:
						for _, id := range s.Names {
							names[id.Name] = true
						}
					}
				}
			}
		}
	}
	delete(names, "_")
	return names
}
