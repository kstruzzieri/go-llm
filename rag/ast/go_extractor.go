package ast

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	goast "go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	goLanguage                 = "go"
	goExtractorSignaturePrefix = "go-ast-extractor"
	goExtractorVersion         = "go-ast-v1"
)

// GoExtractor walks Go source trees with the standard library go/ast parser.
// The zero value is ready for use and safe for concurrent calls.
type GoExtractor struct{}

// Languages implements [Extractor].
func (GoExtractor) Languages() []string { return []string{goLanguage} }

// Extract implements [Extractor].
func (GoExtractor) Extract(ctx context.Context, scope string, root string, vectorSpaceID string) (SymbolGraph, error) {
	canonicalRoot := canonicalGraphPath(root)
	sources, sourceDigest, err := collectGoSources(ctx, root)
	if err != nil {
		return SymbolGraph{}, err
	}
	module := goModule{}
	if len(sources) > 0 {
		module, err = findGoModule(root)
		if err != nil {
			return SymbolGraph{}, err
		}
	}

	signature := encodeGoExtractionSignature(goExtractionSignature{
		Version:       goExtractorVersion,
		Scope:         scope,
		VectorSpaceID: vectorSpaceID,
		Root:          canonicalRoot,
		SourceDigest:  sourceDigest,
		ModuleRoot:    module.signatureRoot(),
		ModulePath:    module.path,
		ModuleDigest:  module.digest,
	})
	graph := SymbolGraph{
		Scope:               scope,
		VectorSpaceID:       vectorSpaceID,
		ExtractionSignature: signature,
		Root:                canonicalRoot,
	}
	if len(sources) == 0 {
		return graph, nil
	}

	nodes, calls, err := extractGoGraph(ctx, root, module, sources)
	if err != nil {
		return SymbolGraph{}, err
	}
	graph.Nodes = nodes
	graph.Calls = calls
	return graph, nil
}

// Stale implements [Extractor].
func (GoExtractor) Stale(ctx context.Context, scope string, root string, prevSignature string) (bool, error) {
	if prevSignature == "" {
		return true, nil
	}
	prev, ok := decodeGoExtractionSignature(prevSignature)
	if !ok {
		return true, nil
	}
	canonicalRoot := canonicalGraphPath(root)
	if prev.Version != goExtractorVersion || prev.Scope != scope || prev.Root != canonicalRoot {
		return true, nil
	}

	sources, sourceDigest, err := collectGoSources(ctx, root)
	if err != nil {
		return false, err
	}
	module := goModule{}
	if len(sources) > 0 {
		module, err = findGoModule(root)
		if err != nil {
			return false, err
		}
	}
	return prev.SourceDigest != sourceDigest ||
		prev.ModuleRoot != module.signatureRoot() ||
		prev.ModulePath != module.path ||
		prev.ModuleDigest != module.digest, nil
}

type goExtractionSignature struct {
	Version       string `json:"version"`
	Scope         string `json:"scope"`
	VectorSpaceID string `json:"vector_space_id"`
	Root          string `json:"root"`
	SourceDigest  string `json:"source_digest"`
	ModuleRoot    string `json:"module_root,omitempty"`
	ModulePath    string `json:"module_path,omitempty"`
	ModuleDigest  string `json:"module_digest,omitempty"`
}

func encodeGoExtractionSignature(sig goExtractionSignature) string {
	raw, err := json.Marshal(sig)
	if err != nil {
		return goExtractorSignaturePrefix + ":"
	}
	return goExtractorSignaturePrefix + ":" + base64.RawURLEncoding.EncodeToString(raw)
}

func decodeGoExtractionSignature(raw string) (goExtractionSignature, bool) {
	encoded, ok := strings.CutPrefix(raw, goExtractorSignaturePrefix+":")
	if !ok || encoded == "" {
		return goExtractionSignature{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return goExtractionSignature{}, false
	}
	var sig goExtractionSignature
	if err := json.Unmarshal(payload, &sig); err != nil {
		return goExtractionSignature{}, false
	}
	return sig, true
}

type goSourceFile struct {
	path    string
	absPath string
	rel     string
	data    []byte
}

func collectGoSources(ctx context.Context, root string) ([]goSourceFile, string, error) {
	nativeRoot := filepath.Clean(root)
	var sources []goSourceFile
	err := filepath.WalkDir(nativeRoot, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if filePath != nativeRoot && skipGoSourceDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || strings.ToLower(filepath.Ext(filePath)) != ".go" {
			return nil
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(nativeRoot, filePath)
		if err != nil {
			return err
		}
		absPath, err := filepath.Abs(filePath)
		if err != nil {
			return err
		}
		sources = append(sources, goSourceFile{
			path:    filePath,
			absPath: filepath.Clean(absPath),
			rel:     canonicalRelPath(rel),
			data:    data,
		})
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("rag/ast: walk Go sources in %q: %w", root, err)
	}

	sort.Slice(sources, func(i, j int) bool {
		return sources[i].rel < sources[j].rel
	})
	h := sha256.New()
	for _, source := range sources {
		_, _ = h.Write([]byte(source.rel))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(source.data)
		_, _ = h.Write([]byte{0})
	}
	return sources, fmt.Sprintf("%x", h.Sum(nil)), nil
}

func skipGoSourceDir(name string) bool {
	switch name {
	case ".git", "vendor", "node_modules", "dist", "__pycache__":
		return true
	default:
		return false
	}
}

func canonicalGraphPath(filePath string) string {
	return filepath.ToSlash(filepath.Clean(filePath))
}

func canonicalRelPath(filePath string) string {
	rel := filepath.ToSlash(filepath.Clean(filePath))
	return strings.TrimPrefix(rel, "./")
}

type goModule struct {
	found  bool
	root   string
	path   string
	digest string
}

func (m goModule) signatureRoot() string {
	if !m.found {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(m.root))
}

func findGoModule(root string) (goModule, error) {
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return goModule{}, fmt.Errorf("rag/ast: resolve root %q: %w", root, err)
	}
	for dir := absRoot; ; dir = filepath.Dir(dir) {
		modPath := filepath.Join(dir, "go.mod")
		data, err := os.ReadFile(modPath)
		if err == nil {
			modulePath := parseGoModulePath(data)
			if modulePath == "" {
				return goModule{}, fmt.Errorf("rag/ast: parse module path in %q", modPath)
			}
			sum := sha256.Sum256(data)
			return goModule{found: true, root: dir, path: modulePath, digest: fmt.Sprintf("%x", sum[:])}, nil
		}
		if !os.IsNotExist(err) {
			return goModule{}, fmt.Errorf("rag/ast: read %q: %w", modPath, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return goModule{}, nil
		}
	}
}

func parseGoModulePath(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		modulePath := fields[1]
		if unquoted, err := strconv.Unquote(modulePath); err == nil {
			return unquoted
		}
		return modulePath
	}
	return ""
}

type goPackage struct {
	name      string
	namespace string
	dir       string
	files     []*goParsedFile
}

type goParsedFile struct {
	path       string
	rel        string
	data       []byte
	file       *goast.File
	imports    map[string]string
	dotImports []string
}

func extractGoGraph(ctx context.Context, root string, module goModule, sources []goSourceFile) ([]SymbolNode, []CallEdge, error) {
	fset := token.NewFileSet()
	packages := make(map[string]*goPackage)
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		parsed, err := parser.ParseFile(fset, source.path, source.data, parser.ParseComments|parser.AllErrors)
		if err != nil {
			return nil, nil, fmt.Errorf("rag/ast: parse Go file %q: %w", source.rel, err)
		}
		dir := filepath.Dir(source.absPath)
		key := dir + "\x00" + parsed.Name.Name
		pkg := packages[key]
		if pkg == nil {
			pkg = &goPackage{
				name:      parsed.Name.Name,
				namespace: goNamespaceForPackage(root, dir, parsed.Name.Name, module),
				dir:       dir,
			}
			packages[key] = pkg
		}
		pkg.files = append(pkg.files, &goParsedFile{
			path: source.path,
			rel:  source.rel,
			data: source.data,
			file: parsed,
		})
	}

	pkgs := sortedGoPackages(packages)
	packageNames := make(map[string]string, len(pkgs))
	for _, pkg := range pkgs {
		packageNames[pkg.namespace] = pkg.name
	}
	for _, pkg := range pkgs {
		sort.Slice(pkg.files, func(i, j int) bool {
			return pkg.files[i].rel < pkg.files[j].rel
		})
		for _, file := range pkg.files {
			file.imports, file.dotImports = goFileImports(file.file, packageNames)
		}
	}

	index := newGoSymbolIndex()
	var (
		nodes  []SymbolNode
		funcs  []goFuncSymbol
		values []goValueSymbol
	)
	for _, pkg := range pkgs {
		for _, file := range pkg.files {
			for _, decl := range file.file.Decls {
				switch decl := decl.(type) {
				case *goast.FuncDecl:
					node := goFuncNode(fset, pkg, file, decl)
					nodes = append(nodes, node)
					index.addNode(node, goFuncReturnType(pkg, file, decl))
					funcs = append(funcs, goFuncSymbol{
						pkg:  pkg,
						file: file,
						decl: decl,
						id:   node.ID,
					})
				case *goast.GenDecl:
					genNodes, genValues := goGenDeclSymbols(fset, pkg, file, decl)
					for _, node := range genNodes {
						nodes = append(nodes, node)
						index.addNode(node, goTypeRef{})
					}
					values = append(values, genValues...)
				}
			}
		}
	}

	calls := extractGoCalls(fset, index, funcs, values)
	sortGoNodes(nodes)
	sortGoCalls(calls)
	return nodes, calls, nil
}

func sortedGoPackages(packages map[string]*goPackage) []*goPackage {
	out := make([]*goPackage, 0, len(packages))
	for _, pkg := range packages {
		out = append(out, pkg)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].namespace != out[j].namespace {
			return out[i].namespace < out[j].namespace
		}
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].dir < out[j].dir
	})
	return out
}

func goNamespaceForPackage(root string, dir string, packageName string, module goModule) string {
	base := ""
	absDir, err := filepath.Abs(dir)
	if err == nil && module.found {
		if rel, err := filepath.Rel(module.root, absDir); err == nil && relWithinRoot(rel) {
			rel = canonicalRelPath(rel)
			if rel == "." {
				base = module.path
			} else {
				base = path.Join(module.path, rel)
			}
		}
	}
	if base == "" {
		absRoot, rootErr := filepath.Abs(filepath.Clean(root))
		if err == nil && rootErr == nil {
			if rel, relErr := filepath.Rel(absRoot, absDir); relErr == nil && relWithinRoot(rel) {
				rel = canonicalRelPath(rel)
				if rel == "." {
					base = packageName
				} else {
					base = rel
				}
			}
		}
	}
	if base == "" {
		base = packageName
	}
	if strings.HasSuffix(packageName, "_test") && base != packageName {
		return base + "_test"
	}
	return base
}

func relWithinRoot(rel string) bool {
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func goFileImports(file *goast.File, packageNames map[string]string) (map[string]string, []string) {
	imports := make(map[string]string)
	var dotImports []string
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil || importPath == "" {
			continue
		}
		if spec.Name != nil {
			switch spec.Name.Name {
			case "_":
				continue
			case ".":
				dotImports = append(dotImports, importPath)
			default:
				imports[spec.Name.Name] = importPath
			}
			continue
		}
		alias := packageNames[importPath]
		if alias == "" {
			alias = path.Base(importPath)
		}
		if alias != "." && alias != "" {
			imports[alias] = importPath
		}
	}
	sort.Strings(dotImports)
	return imports, dotImports
}

func goFuncNode(fset *token.FileSet, pkg *goPackage, file *goParsedFile, decl *goast.FuncDecl) SymbolNode {
	kind := SymbolKindFunction
	receiver := ""
	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		kind = SymbolKindMethod
		receiver = goReceiverName(decl.Recv.List[0].Type)
	}
	id := SymbolID(SymbolKey{
		Language:  goLanguage,
		Kind:      kind,
		Namespace: pkg.namespace,
		Receiver:  receiver,
		Name:      decl.Name.Name,
	})
	return SymbolNode{
		ID:          id,
		Language:    goLanguage,
		Kind:        kind,
		Namespace:   pkg.namespace,
		Name:        decl.Name.Name,
		Receiver:    receiver,
		File:        file.rel,
		StartLine:   goLine(fset, decl.Pos()),
		EndLine:     goLine(fset, decl.End()),
		Declaration: goFuncDeclaration(fset, decl),
		Doc:         rawGoComment(fset, file.data, decl.Doc),
	}
}

func goGenDeclSymbols(fset *token.FileSet, pkg *goPackage, file *goParsedFile, decl *goast.GenDecl) ([]SymbolNode, []goValueSymbol) {
	switch decl.Tok {
	case token.TYPE:
		nodes := make([]SymbolNode, 0, len(decl.Specs))
		for _, spec := range decl.Specs {
			typeSpec, ok := spec.(*goast.TypeSpec)
			if !ok {
				continue
			}
			kind := goTypeSymbolKind(typeSpec)
			id := SymbolID(SymbolKey{
				Language:  goLanguage,
				Kind:      kind,
				Namespace: pkg.namespace,
				Name:      typeSpec.Name.Name,
			})
			doc := typeSpec.Doc
			if doc == nil {
				doc = decl.Doc
			}
			nodes = append(nodes, SymbolNode{
				ID:          id,
				Language:    goLanguage,
				Kind:        kind,
				Namespace:   pkg.namespace,
				Name:        typeSpec.Name.Name,
				File:        file.rel,
				StartLine:   goLine(fset, typeSpec.Pos()),
				EndLine:     goLine(fset, typeSpec.End()),
				Declaration: goSpecDeclaration(fset, token.TYPE, typeSpec),
				Doc:         rawGoComment(fset, file.data, doc),
			})
		}
		return nodes, nil
	case token.VAR, token.CONST:
		var nodes []SymbolNode
		var values []goValueSymbol
		kind := SymbolKindVar
		if decl.Tok == token.CONST {
			kind = SymbolKindConst
		}
		for _, spec := range decl.Specs {
			valueSpec, ok := spec.(*goast.ValueSpec)
			if !ok {
				continue
			}
			doc := valueSpec.Doc
			if doc == nil {
				doc = decl.Doc
			}
			for i, name := range valueSpec.Names {
				id := SymbolID(SymbolKey{
					Language:  goLanguage,
					Kind:      kind,
					Namespace: pkg.namespace,
					Name:      name.Name,
				})
				nodes = append(nodes, SymbolNode{
					ID:          id,
					Language:    goLanguage,
					Kind:        kind,
					Namespace:   pkg.namespace,
					Name:        name.Name,
					File:        file.rel,
					StartLine:   goLine(fset, valueSpec.Pos()),
					EndLine:     goLine(fset, valueSpec.End()),
					Declaration: goSpecDeclaration(fset, decl.Tok, valueSpec),
					Doc:         rawGoComment(fset, file.data, doc),
				})
				if initializer := goInitializerForName(valueSpec, i); initializer != nil {
					values = append(values, goValueSymbol{
						pkg:  pkg,
						file: file,
						id:   id,
						expr: initializer,
					})
				}
			}
		}
		return nodes, values
	default:
		return nil, nil
	}
}

func goInitializerForName(spec *goast.ValueSpec, nameIndex int) goast.Expr {
	if len(spec.Values) == 0 {
		return nil
	}
	if len(spec.Values) == 1 {
		return spec.Values[0]
	}
	if nameIndex < len(spec.Values) {
		return spec.Values[nameIndex]
	}
	return nil
}

func goTypeSymbolKind(spec *goast.TypeSpec) SymbolKind {
	switch spec.Type.(type) {
	case *goast.StructType:
		return SymbolKindStruct
	case *goast.InterfaceType:
		return SymbolKindInterface
	default:
		return SymbolKindType
	}
}

func goFuncDeclaration(fset *token.FileSet, decl *goast.FuncDecl) string {
	clone := *decl
	clone.Doc = nil
	clone.Body = nil
	return goFormatNode(fset, &clone)
}

func goSpecDeclaration(fset *token.FileSet, tok token.Token, spec goast.Spec) string {
	switch spec := spec.(type) {
	case *goast.TypeSpec:
		clone := *spec
		clone.Doc = nil
		clone.Comment = nil
		return goFormatNode(fset, &goast.GenDecl{Tok: tok, Specs: []goast.Spec{&clone}})
	case *goast.ValueSpec:
		clone := *spec
		clone.Doc = nil
		clone.Comment = nil
		return goFormatNode(fset, &goast.GenDecl{Tok: tok, Specs: []goast.Spec{&clone}})
	default:
		return ""
	}
}

func rawGoComment(fset *token.FileSet, source []byte, group *goast.CommentGroup) string {
	if group == nil {
		return ""
	}
	start := fset.PositionFor(group.Pos(), false).Offset
	end := fset.PositionFor(group.End(), false).Offset
	if start >= 0 && end >= start && end <= len(source) {
		return string(source[start:end])
	}
	return strings.TrimRight(group.Text(), "\n")
}

func goLine(fset *token.FileSet, pos token.Pos) int {
	if !pos.IsValid() {
		return 0
	}
	return fset.PositionFor(pos, false).Line
}

func goFormatNode(fset *token.FileSet, node any) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, node); err != nil {
		return ""
	}
	return strings.TrimSpace(buf.String())
}

type goFuncSymbol struct {
	pkg  *goPackage
	file *goParsedFile
	decl *goast.FuncDecl
	id   string
}

type goValueSymbol struct {
	pkg  *goPackage
	file *goParsedFile
	id   string
	expr goast.Expr
}

type goTypeRef struct {
	namespace     string
	name          string
	keyNamespace  string
	keyName       string
	elemNamespace string
	elemName      string
}

func (r goTypeRef) valid() bool {
	return r.namespace != "" && r.name != ""
}

func (r goTypeRef) usable() bool {
	return r.valid() || r.keyName != "" || r.elemName != ""
}

func (r goTypeRef) keyRef() goTypeRef {
	if r.keyName == "" {
		return goTypeRef{}
	}
	return goTypeRef{namespace: r.keyNamespace, name: r.keyName}
}

func (r goTypeRef) elemRef() goTypeRef {
	if r.elemName == "" {
		return goTypeRef{}
	}
	return goTypeRef{namespace: r.elemNamespace, name: r.elemName}
}

func collectionGoTypeRef(key goTypeRef, elem goTypeRef) goTypeRef {
	ref := goTypeRef{}
	if key.valid() {
		ref.keyNamespace = key.namespace
		ref.keyName = key.name
	}
	if elem.valid() {
		ref.elemNamespace = elem.namespace
		ref.elemName = elem.name
	}
	return ref
}

type goSymbolIndex struct {
	functions map[string]map[string]string
	methods   map[string]map[string]map[string]string
	types     map[string]map[string]string
	returns   map[string]goTypeRef
	imported  map[string]map[string]bool
}

func newGoSymbolIndex() *goSymbolIndex {
	return &goSymbolIndex{
		functions: make(map[string]map[string]string),
		methods:   make(map[string]map[string]map[string]string),
		types:     make(map[string]map[string]string),
		returns:   make(map[string]goTypeRef),
		imported:  make(map[string]map[string]bool),
	}
}

func (idx *goSymbolIndex) addNode(node SymbolNode, returnType goTypeRef) {
	switch node.Kind {
	case SymbolKindFunction:
		if idx.functions[node.Namespace] == nil {
			idx.functions[node.Namespace] = make(map[string]string)
		}
		idx.functions[node.Namespace][node.Name] = node.ID
	case SymbolKindMethod:
		if idx.methods[node.Namespace] == nil {
			idx.methods[node.Namespace] = make(map[string]map[string]string)
		}
		if idx.methods[node.Namespace][node.Receiver] == nil {
			idx.methods[node.Namespace][node.Receiver] = make(map[string]string)
		}
		idx.methods[node.Namespace][node.Receiver][node.Name] = node.ID
	case SymbolKindStruct, SymbolKindInterface, SymbolKindType:
		if idx.types[node.Namespace] == nil {
			idx.types[node.Namespace] = make(map[string]string)
		}
		idx.types[node.Namespace][node.Name] = node.ID
	}
	if returnType.valid() {
		idx.returns[node.ID] = returnType
	}
}

func (idx *goSymbolIndex) function(namespace string, name string) string {
	if byName := idx.functions[namespace]; byName != nil {
		return byName[name]
	}
	return ""
}

func (idx *goSymbolIndex) method(namespace string, receiver string, name string) string {
	if byReceiver := idx.methods[namespace]; byReceiver != nil {
		if byName := byReceiver[receiver]; byName != nil {
			return byName[name]
		}
	}
	return ""
}

func (idx *goSymbolIndex) hasType(ref goTypeRef) bool {
	if !ref.valid() {
		return false
	}
	if byName := idx.types[ref.namespace]; byName != nil {
		return byName[ref.name] != ""
	}
	return false
}

func (idx *goSymbolIndex) importedType(importPath string, name string) bool {
	byName, ok := idx.imported[importPath]
	if !ok {
		byName = make(map[string]bool)
		pkg, err := importer.Default().Import(importPath)
		if err == nil {
			for _, exported := range pkg.Scope().Names() {
				if _, ok := pkg.Scope().Lookup(exported).(*types.TypeName); ok {
					byName[exported] = true
				}
			}
		}
		idx.imported[importPath] = byName
	}
	return byName[name]
}

func extractGoCalls(fset *token.FileSet, index *goSymbolIndex, funcs []goFuncSymbol, values []goValueSymbol) []CallEdge {
	var calls []CallEdge
	for _, value := range values {
		collector := goCallCollector{
			fset:     fset,
			index:    index,
			pkg:      value.pkg,
			file:     value.file,
			callerID: value.id,
			scope:    newGoCallScope(),
		}
		collector.walkExpr(value.expr)
		calls = append(calls, collector.calls...)
	}
	for _, fn := range funcs {
		if fn.decl.Body == nil {
			continue
		}
		collector := goCallCollector{
			fset:     fset,
			index:    index,
			pkg:      fn.pkg,
			file:     fn.file,
			callerID: fn.id,
			scope:    initialGoCallScope(fn.pkg, fn.file, fn.decl),
		}
		collector.walkStmtList(fn.decl.Body.List)
		calls = append(calls, collector.calls...)
	}
	return calls
}

type goCallScope struct {
	vars  map[string]goTypeRef
	local map[string]bool
}

func newGoCallScope() goCallScope {
	return goCallScope{
		vars:  make(map[string]goTypeRef),
		local: make(map[string]bool),
	}
}

func (s goCallScope) clone() goCallScope {
	clone := newGoCallScope()
	for name, ref := range s.vars {
		clone.vars[name] = ref
	}
	return clone
}

func (s goCallScope) declare(name string, ref goTypeRef) {
	if name != "_" && ref.usable() {
		s.vars[name] = ref
		s.local[name] = true
	}
}

func initialGoCallScope(pkg *goPackage, file *goParsedFile, decl *goast.FuncDecl) goCallScope {
	scope := newGoCallScope()
	if decl.Recv != nil {
		addGoFieldList(scope, pkg, file, decl.Recv)
	}
	addGoFieldList(scope, pkg, file, decl.Type.Params)
	addGoFieldList(scope, pkg, file, decl.Type.Results)
	return scope
}

type goCallCollector struct {
	fset     *token.FileSet
	index    *goSymbolIndex
	pkg      *goPackage
	file     *goParsedFile
	callerID string
	scope    goCallScope
	calls    []CallEdge
}

func (c *goCallCollector) withScope(scope goCallScope) goCallCollector {
	child := *c
	child.scope = scope
	child.calls = nil
	return child
}

func (c *goCallCollector) appendFrom(child goCallCollector) {
	c.calls = append(c.calls, child.calls...)
}

func (c *goCallCollector) walkStmtList(stmts []goast.Stmt) {
	for _, stmt := range stmts {
		c.walkStmt(stmt)
	}
}

func (c *goCallCollector) walkStmt(stmt goast.Stmt) {
	switch stmt := stmt.(type) {
	case nil:
		return
	case *goast.DeclStmt:
		c.walkDecl(stmt.Decl)
	case *goast.AssignStmt:
		for _, expr := range stmt.Lhs {
			c.walkExpr(expr)
		}
		for _, expr := range stmt.Rhs {
			c.walkExpr(expr)
		}
		c.updateScopeFromAssign(stmt)
	case *goast.ExprStmt:
		c.walkExpr(stmt.X)
	case *goast.ReturnStmt:
		for _, expr := range stmt.Results {
			c.walkExpr(expr)
		}
	case *goast.GoStmt:
		c.walkExpr(stmt.Call)
	case *goast.DeferStmt:
		c.walkExpr(stmt.Call)
	case *goast.SendStmt:
		c.walkExpr(stmt.Chan)
		c.walkExpr(stmt.Value)
	case *goast.IncDecStmt:
		c.walkExpr(stmt.X)
	case *goast.BlockStmt:
		child := c.withScope(c.scope.clone())
		child.walkStmtList(stmt.List)
		c.appendFrom(child)
	case *goast.IfStmt:
		ifScope := c.scope.clone()
		child := c.withScope(ifScope)
		child.walkStmt(stmt.Init)
		child.walkExpr(stmt.Cond)
		body := child.withScope(child.scope.clone())
		body.walkStmtList(stmt.Body.List)
		child.appendFrom(body)
		if stmt.Else != nil {
			elseBranch := child.withScope(child.scope.clone())
			elseBranch.walkStmt(stmt.Else)
			child.appendFrom(elseBranch)
		}
		c.appendFrom(child)
	case *goast.ForStmt:
		child := c.withScope(c.scope.clone())
		child.walkStmt(stmt.Init)
		child.walkExpr(stmt.Cond)
		body := child.withScope(child.scope.clone())
		body.walkStmtList(stmt.Body.List)
		child.appendFrom(body)
		child.walkStmt(stmt.Post)
		c.appendFrom(child)
	case *goast.RangeStmt:
		child := c.withScope(c.scope.clone())
		child.walkExpr(stmt.X)
		body := child.withScope(child.scope.clone())
		body.bindRangeVars(stmt)
		body.walkStmtList(stmt.Body.List)
		child.appendFrom(body)
		c.appendFrom(child)
	case *goast.SwitchStmt:
		child := c.withScope(c.scope.clone())
		child.walkStmt(stmt.Init)
		child.walkExpr(stmt.Tag)
		for _, stmt := range stmt.Body.List {
			child.walkCaseClause(stmt)
		}
		c.appendFrom(child)
	case *goast.TypeSwitchStmt:
		child := c.withScope(c.scope.clone())
		child.walkStmt(stmt.Init)
		child.walkStmt(stmt.Assign)
		for _, stmt := range stmt.Body.List {
			child.walkCaseClause(stmt)
		}
		c.appendFrom(child)
	case *goast.SelectStmt:
		child := c.withScope(c.scope.clone())
		for _, stmt := range stmt.Body.List {
			child.walkCommClause(stmt)
		}
		c.appendFrom(child)
	case *goast.LabeledStmt:
		c.walkStmt(stmt.Stmt)
	}
}

func (c *goCallCollector) walkCaseClause(stmt goast.Stmt) {
	clause, ok := stmt.(*goast.CaseClause)
	if !ok {
		c.walkStmt(stmt)
		return
	}
	child := c.withScope(c.scope.clone())
	for _, expr := range clause.List {
		child.walkExpr(expr)
	}
	child.walkStmtList(clause.Body)
	c.appendFrom(child)
}

func (c *goCallCollector) walkCommClause(stmt goast.Stmt) {
	clause, ok := stmt.(*goast.CommClause)
	if !ok {
		c.walkStmt(stmt)
		return
	}
	child := c.withScope(c.scope.clone())
	child.walkStmt(clause.Comm)
	child.walkStmtList(clause.Body)
	c.appendFrom(child)
}

func (c *goCallCollector) walkDecl(decl goast.Decl) {
	gen, ok := decl.(*goast.GenDecl)
	if !ok {
		return
	}
	for _, spec := range gen.Specs {
		valueSpec, ok := spec.(*goast.ValueSpec)
		if !ok {
			continue
		}
		for _, expr := range valueSpec.Values {
			c.walkExpr(expr)
		}
		c.updateScopeFromValueSpec(valueSpec)
	}
}

func (c *goCallCollector) walkExpr(expr goast.Expr) {
	goast.Inspect(expr, func(node goast.Node) bool {
		switch node := node.(type) {
		case nil:
			return true
		case *goast.FuncLit:
			return false
		case *goast.CallExpr:
			c.recordCall(node)
		}
		return true
	})
}

func (c *goCallCollector) recordCall(call *goast.CallExpr) {
	calleeID, resolution, record := c.index.resolveCall(c.pkg, c.file, c.scope, call.Fun)
	if !record {
		return
	}
	c.calls = append(c.calls, CallEdge{
		CallerID:   c.callerID,
		CalleeID:   calleeID,
		CalleeRaw:  goFormatNode(c.fset, call.Fun),
		Resolution: resolution,
		File:       c.file.rel,
		Line:       goLine(c.fset, call.Fun.Pos()),
	})
}

func (c *goCallCollector) updateScopeFromValueSpec(spec *goast.ValueSpec) {
	for i, name := range spec.Names {
		ref := goTypeRef{}
		if spec.Type != nil {
			ref = goTypeRefFromExpr(c.pkg, c.file, spec.Type)
		} else if initializer := goInitializerForName(spec, i); initializer != nil {
			ref = goTypeRefFromValue(c.index, c.pkg, c.file, c.scope, initializer)
		}
		c.scope.declare(name.Name, ref)
	}
}

func (c *goCallCollector) updateScopeFromAssign(stmt *goast.AssignStmt) {
	if stmt.Tok != token.DEFINE {
		return
	}
	for i, lhs := range stmt.Lhs {
		name, ok := lhs.(*goast.Ident)
		if !ok || name.Name == "_" || i >= len(stmt.Rhs) {
			continue
		}
		if c.scope.local[name.Name] {
			continue
		}
		ref := goTypeRefFromValue(c.index, c.pkg, c.file, c.scope, stmt.Rhs[i])
		c.scope.declare(name.Name, ref)
	}
}

func (c *goCallCollector) bindRangeVars(stmt *goast.RangeStmt) {
	if stmt.Tok != token.DEFINE {
		return
	}
	key, elem := goRangeTypes(c.pkg, c.file, c.scope, stmt.X)
	if name, ok := stmt.Key.(*goast.Ident); ok {
		c.scope.declare(name.Name, key)
	}
	if name, ok := stmt.Value.(*goast.Ident); ok {
		c.scope.declare(name.Name, elem)
	}
}

func addGoFieldList(scope goCallScope, pkg *goPackage, file *goParsedFile, fields *goast.FieldList) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		ref := goTypeRefFromExpr(pkg, file, field.Type)
		if !ref.usable() {
			continue
		}
		for _, name := range field.Names {
			scope.declare(name.Name, ref)
		}
	}
}

func (idx *goSymbolIndex) resolveCall(pkg *goPackage, file *goParsedFile, scope goCallScope, fun goast.Expr) (string, CallResolution, bool) {
	base := unwrapGoInstantiation(fun)
	switch expr := base.(type) {
	case *goast.Ident:
		if goBuiltinCall(expr.Name) || goPredeclaredType(expr.Name) || idx.hasType(goTypeRef{namespace: pkg.namespace, name: expr.Name}) {
			return "", "", false
		}
		if calleeID := idx.function(pkg.namespace, expr.Name); calleeID != "" {
			return calleeID, CallResolutionResolved, true
		}
		for _, namespace := range file.dotImports {
			if calleeID := idx.function(namespace, expr.Name); calleeID != "" {
				return calleeID, CallResolutionResolved, true
			}
		}
		return "", CallResolutionUnresolved, true
	case *goast.SelectorExpr:
		if ident, ok := expr.X.(*goast.Ident); ok {
			if ref, ok := scope.vars[ident.Name]; ok && ref.valid() {
				if calleeID := idx.method(ref.namespace, ref.name, expr.Sel.Name); calleeID != "" {
					return calleeID, CallResolutionResolved, true
				}
				return "", CallResolutionUnresolved, true
			}
			if namespace, ok := file.imports[ident.Name]; ok {
				if idx.hasType(goTypeRef{namespace: namespace, name: expr.Sel.Name}) {
					return "", "", false
				}
				if idx.importedType(namespace, expr.Sel.Name) {
					return "", "", false
				}
				if calleeID := idx.function(namespace, expr.Sel.Name); calleeID != "" {
					return calleeID, CallResolutionResolved, true
				}
				return "", CallResolutionUnresolved, true
			}
		}
		if ref := goTypeRefFromValue(idx, pkg, file, scope, expr.X); ref.valid() {
			if calleeID := idx.method(ref.namespace, ref.name, expr.Sel.Name); calleeID != "" {
				return calleeID, CallResolutionResolved, true
			}
		}
		return "", CallResolutionUnresolved, true
	case *goast.FuncLit:
		return "", "", false
	default:
		return "", CallResolutionNotAttempted, true
	}
}

func goFuncReturnType(pkg *goPackage, file *goParsedFile, decl *goast.FuncDecl) goTypeRef {
	if decl.Type.Results == nil {
		return goTypeRef{}
	}
	for _, field := range decl.Type.Results.List {
		return goTypeRefFromExpr(pkg, file, field.Type)
	}
	return goTypeRef{}
}

func goTypeRefFromValue(index *goSymbolIndex, pkg *goPackage, file *goParsedFile, scope goCallScope, expr goast.Expr) goTypeRef {
	switch expr := expr.(type) {
	case *goast.CompositeLit:
		return goTypeRefFromExpr(pkg, file, expr.Type)
	case *goast.UnaryExpr:
		if expr.Op == token.AND {
			return goTypeRefFromValue(index, pkg, file, scope, expr.X)
		}
	case *goast.CallExpr:
		if ref := goTypeRefFromExpr(pkg, file, unwrapGoInstantiation(expr.Fun)); index.hasType(ref) {
			return ref
		}
		calleeID, resolution, record := index.resolveCall(pkg, file, scope, expr.Fun)
		if record && resolution == CallResolutionResolved {
			return index.returns[calleeID]
		}
	}
	return goTypeRef{}
}

func goTypeRefFromExpr(pkg *goPackage, file *goParsedFile, expr goast.Expr) goTypeRef {
	expr = unwrapGoInstantiation(expr)
	switch expr := expr.(type) {
	case *goast.Ident:
		if expr.Name == "" {
			return goTypeRef{}
		}
		return goTypeRef{namespace: pkg.namespace, name: expr.Name}
	case *goast.StarExpr:
		return goTypeRefFromExpr(pkg, file, expr.X)
	case *goast.ParenExpr:
		return goTypeRefFromExpr(pkg, file, expr.X)
	case *goast.SelectorExpr:
		ident, ok := expr.X.(*goast.Ident)
		if !ok {
			return goTypeRef{}
		}
		namespace := file.imports[ident.Name]
		if namespace == "" {
			return goTypeRef{}
		}
		return goTypeRef{namespace: namespace, name: expr.Sel.Name}
	case *goast.ArrayType:
		return collectionGoTypeRef(goTypeRef{}, goTypeRefFromExpr(pkg, file, expr.Elt))
	case *goast.MapType:
		return collectionGoTypeRef(
			goTypeRefFromExpr(pkg, file, expr.Key),
			goTypeRefFromExpr(pkg, file, expr.Value),
		)
	case *goast.ChanType:
		return collectionGoTypeRef(goTypeRef{}, goTypeRefFromExpr(pkg, file, expr.Value))
	default:
		return goTypeRef{}
	}
}

func goRangeTypes(pkg *goPackage, file *goParsedFile, scope goCallScope, expr goast.Expr) (goTypeRef, goTypeRef) {
	expr = unwrapGoInstantiation(expr)
	if ident, ok := expr.(*goast.Ident); ok {
		ref := scope.vars[ident.Name]
		return ref.keyRef(), ref.elemRef()
	}
	ref := goTypeRefFromExpr(pkg, file, expr)
	return ref.keyRef(), ref.elemRef()
}

func unwrapGoInstantiation(expr goast.Expr) goast.Expr {
	switch expr := expr.(type) {
	case *goast.IndexExpr:
		return unwrapGoInstantiation(expr.X)
	case *goast.IndexListExpr:
		return unwrapGoInstantiation(expr.X)
	case *goast.ParenExpr:
		return unwrapGoInstantiation(expr.X)
	default:
		return expr
	}
}

func goReceiverName(expr goast.Expr) string {
	ref := goReceiverBase(expr)
	if ref == "" {
		return goFormatNode(token.NewFileSet(), expr)
	}
	return ref
}

func goReceiverBase(expr goast.Expr) string {
	switch expr := unwrapGoInstantiation(expr).(type) {
	case *goast.Ident:
		return expr.Name
	case *goast.StarExpr:
		return goReceiverBase(expr.X)
	case *goast.ParenExpr:
		return goReceiverBase(expr.X)
	case *goast.SelectorExpr:
		return expr.Sel.Name
	default:
		return ""
	}
}

func goBuiltinCall(name string) bool {
	switch name {
	case "append", "cap", "clear", "close", "complex", "copy", "delete",
		"imag", "len", "make", "max", "min", "new", "panic", "print",
		"println", "real", "recover":
		return true
	default:
		return false
	}
}

func goPredeclaredType(name string) bool {
	switch name {
	case "any", "bool", "byte", "comparable", "complex64", "complex128",
		"error", "float32", "float64", "int", "int8", "int16", "int32",
		"int64", "rune", "string", "uint", "uint8", "uint16", "uint32",
		"uint64", "uintptr":
		return true
	default:
		return false
	}
}

func sortGoNodes(nodes []SymbolNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].File != nodes[j].File {
			return nodes[i].File < nodes[j].File
		}
		if nodes[i].StartLine != nodes[j].StartLine {
			return nodes[i].StartLine < nodes[j].StartLine
		}
		if nodes[i].EndLine != nodes[j].EndLine {
			return nodes[i].EndLine < nodes[j].EndLine
		}
		if nodes[i].Kind != nodes[j].Kind {
			return nodes[i].Kind < nodes[j].Kind
		}
		if nodes[i].Receiver != nodes[j].Receiver {
			return nodes[i].Receiver < nodes[j].Receiver
		}
		if nodes[i].Name != nodes[j].Name {
			return nodes[i].Name < nodes[j].Name
		}
		return nodes[i].ID < nodes[j].ID
	})
}

func sortGoCalls(calls []CallEdge) {
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].File != calls[j].File {
			return calls[i].File < calls[j].File
		}
		if calls[i].Line != calls[j].Line {
			return calls[i].Line < calls[j].Line
		}
		if calls[i].CallerID != calls[j].CallerID {
			return calls[i].CallerID < calls[j].CallerID
		}
		if calls[i].CalleeRaw != calls[j].CalleeRaw {
			return calls[i].CalleeRaw < calls[j].CalleeRaw
		}
		return calls[i].CalleeID < calls[j].CalleeID
	})
}
