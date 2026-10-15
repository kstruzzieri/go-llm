package containmentinv

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// markerPrefix starts a marker comment; the rest of the comment is the row ID.
const markerPrefix = "// containment: "

var layers = []string{"aggregate", "focused", "scenario", "model"}

// site is one marker position: a row ID inside a top-level function of a file.
type site struct{ id, file, fn string }

type marker struct {
	site
	pos string // file:line
}

// source is what the lint needs from the scanned test files.
type source struct {
	funcs   map[string]map[string]bool // slash path -> top-level function names
	sites   map[site]bool
	markers []marker
}

// Lint checks inv against the *_test.go files under repoRoot and the
// registry's scenario declarations, and returns every violation:
//   - each ref names an existing function whose body holds the row's marker;
//   - each marker names a row and sits in a function one of that row's refs cites;
//   - scenario-backed statuses are derived from the registry, never contradicted;
//   - a row cites TestContainment/<X> for each executable scenario it lists, and only those;
//   - pending and out-of-scope rows carry no refs; pending and uncontained need an owner;
//   - model rows list artifacts, rows with refs list known mandatory profiles.
func Lint(inv Inventory, repoRoot string, scenarios []ScenarioDecl) []error {
	src, errs := scanTests(repoRoot)
	decls := make(map[string]ScenarioDecl, len(scenarios))
	for _, s := range scenarios {
		decls[s.ID] = s
	}
	profiles := map[string]bool{}
	for _, p := range inv.Profiles {
		profiles[p.Name] = true
	}
	rows := map[string]Row{}
	cited := map[site]bool{}
	for _, r := range inv.Rows {
		if _, dup := rows[r.ID]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate row ID", r.ID))
		}
		rows[r.ID] = r
		errs = append(errs, lintRow(r, decls, profiles)...)
		for _, ref := range r.Refs {
			cited[site{r.ID, ref.File, ref.Func}] = true
			errs = append(errs, lintRef(r.ID, ref, src)...)
		}
	}
	errs = append(errs, lintScenarioReqs(rows, scenarios)...)
	errs = append(errs, lintProfiles(inv)...)
	for _, m := range src.markers {
		switch {
		case !hasRow(rows, m.id):
			errs = append(errs, fmt.Errorf("%s: marker %q names no inventory row", m.pos, m.id))
		case m.fn == "":
			errs = append(errs, fmt.Errorf("%s: %s marker sits outside any function", m.pos, m.id))
		case !cited[m.site]:
			errs = append(errs, fmt.Errorf("%s: %s marker in %s is not cited by any %s ref", m.pos, m.id, m.fn, m.id))
		}
	}
	return errs
}

func hasRow(rows map[string]Row, id string) bool {
	_, ok := rows[id]
	return ok
}

func lintRow(r Row, decls map[string]ScenarioDecl, profiles map[string]bool) []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{r.ID}, args...)...))
	}
	if familyOf(r.ID) == "" {
		add("ID belongs to no inventory family")
	}
	if !slices.Contains(layers, r.Layer) {
		add("layer %q, want one of %s", r.Layer, strings.Join(layers, ", "))
	}
	status, err := rowStatus(r, decls)
	if err != nil {
		errs = append(errs, err)
	}
	kind, arg := splitStatus(status)
	if len(r.Scenarios) > 0 && r.Owner != "" {
		add("scenario-backed row stores owner %q; the scenario registry owns it", r.Owner)
	}
	if len(r.Scenarios) == 0 && (kind == "pending" || kind == "uncontained") {
		if r.Owner == "" {
			add("%s status needs an owner", kind)
		} else if arg != r.Owner {
			add("status %q does not name the row owner %q", status, r.Owner)
		}
	}
	if (kind == "pending" || kind == "out-of-scope") && len(r.Refs) > 0 {
		add("%s row must not carry refs", kind)
	}
	if r.Layer == "model" && len(r.Artifacts) == 0 {
		add("model row lists no artifacts")
	}
	if (kind == "verified" || kind == "uncontained") && len(r.Refs) == 0 && len(r.Artifacts) == 0 {
		add("%s row has no refs and no artifacts", kind)
	}
	if len(r.Refs) > 0 && len(r.Profiles) == 0 {
		add("row has refs but lists no mandatory profile")
	}
	for _, p := range r.Profiles {
		if !profiles[p] {
			add("unknown profile %q", p)
		}
	}
	for _, ref := range r.Refs {
		if id, ok := strings.CutPrefix(ref.Test, "TestContainment/"); ok && !slices.Contains(r.Scenarios, id) {
			add("ref %s cites scenario %s, which this row does not list", ref.Test, id)
		}
	}
	for _, id := range r.Scenarios {
		d, ok := decls[id]
		if ok && !slices.Contains(d.Reqs, r.ID) {
			add("scenario %s does not list this row in its reqs", id)
		}
		if d.Declared == "contained" || d.Declared == "uncontained" {
			if !slices.ContainsFunc(r.Refs, func(ref Ref) bool { return ref.Test == "TestContainment/"+id }) {
				add("executable scenario %s has no TestContainment/%s ref", id, id)
			}
		}
	}
	return errs
}

// rowStatus is the row's effective status: derived from its scenarios when it
// has any (a stored status must then agree), otherwise the stored status.
func rowStatus(r Row, decls map[string]ScenarioDecl) (string, error) {
	if len(r.Scenarios) == 0 {
		if kind, _ := splitStatus(r.Status); kind == "" {
			return "", fmt.Errorf("%s: status %q, want verified, uncontained(<owner>), pending(<owner>), unobserved(<profile>) or out-of-scope(<reason>)", r.ID, r.Status)
		}
		return r.Status, nil
	}
	derived, err := deriveStatus(r.ID, r.Scenarios, decls)
	if err != nil {
		return "", err
	}
	if r.Status != "" && r.Status != derived {
		return derived, fmt.Errorf("%s: stored status %q disagrees with %q derived from scenarios %s", r.ID, r.Status, derived, strings.Join(r.Scenarios, ", "))
	}
	return derived, nil
}

// deriveStatus: any uncontained scenario makes the row uncontained(<owners>);
// otherwise every scenario must share one outcome. A mix without an
// uncontained scenario has no honest single status: split the row.
func deriveStatus(rowID string, ids []string, decls map[string]ScenarioDecl) (string, error) {
	labels := map[string][]string{} // outcome -> owners or reasons, first-seen order
	for _, id := range ids {
		d, ok := decls[id]
		if !ok {
			return "", fmt.Errorf("%s: scenario %s is not declared in containment-scenarios.json", rowID, id)
		}
		label := d.Owner
		switch d.Declared {
		case "contained":
		case "uncontained", "pending":
		case "out-of-scope":
			label = d.Reason
		default:
			return "", fmt.Errorf("%s: scenario %s declares unknown outcome %q", rowID, id, d.Declared)
		}
		if d.Declared != "contained" && label == "" {
			return "", fmt.Errorf("%s: scenario %s is %s without an owner or reason", rowID, id, d.Declared)
		}
		if !slices.Contains(labels[d.Declared], label) {
			labels[d.Declared] = append(labels[d.Declared], label)
		}
	}
	if owners, ok := labels["uncontained"]; ok {
		return "uncontained(" + strings.Join(owners, ", ") + ")", nil
	}
	if len(labels) != 1 {
		return "", fmt.Errorf("%s: scenarios %s mix outcomes without an uncontained one; split the row", rowID, strings.Join(ids, ", "))
	}
	for _, outcome := range []string{"pending", "out-of-scope"} {
		if values, ok := labels[outcome]; ok {
			return outcome + "(" + strings.Join(values, ", ") + ")", nil
		}
	}
	return "verified", nil // the single outcome is contained
}

// splitStatus parses a status into its kind and parenthesised argument; an
// invalid status yields an empty kind.
func splitStatus(s string) (kind, arg string) {
	if s == "verified" {
		return s, ""
	}
	open := strings.Index(s, "(")
	if open <= 0 || !strings.HasSuffix(s, ")") || open+2 > len(s)-1 {
		return "", ""
	}
	kind, arg = s[:open], s[open+1:len(s)-1]
	if !slices.Contains([]string{"uncontained", "pending", "unobserved", "out-of-scope"}, kind) {
		return "", ""
	}
	return kind, arg
}

func lintRef(id string, ref Ref, src source) []error {
	if ref.Package == "" || ref.Test == "" || ref.File == "" || ref.Func == "" {
		return []error{fmt.Errorf("%s: ref %+v needs package, test, file and func", id, ref)}
	}
	var errs []error
	if want := "./" + path.Dir(ref.File); ref.Package != want {
		errs = append(errs, fmt.Errorf("%s: ref %s: package %s, want %s (the directory of %s)", id, ref.Test, ref.Package, want, ref.File))
	}
	fns, ok := src.funcs[ref.File]
	if !ok {
		return append(errs, fmt.Errorf("%s: ref %s: %s is not a scanned _test.go file", id, ref.Test, ref.File))
	}
	if !fns[ref.Func] {
		return append(errs, fmt.Errorf("%s: ref %s: function %s not found in %s", id, ref.Test, ref.Func, ref.File))
	}
	if top, _, _ := strings.Cut(ref.Test, "/"); !src.hasFunc(path.Dir(ref.File), top) {
		errs = append(errs, fmt.Errorf("%s: ref %s: top-level test %s not found in package %s", id, ref.Test, top, ref.Package))
	}
	if !src.sites[site{id, ref.File, ref.Func}] {
		errs = append(errs, fmt.Errorf("%s: no `%s%s` marker inside %s (%s) for %s", id, markerPrefix, id, ref.Func, ref.File, ref.Test))
	}
	return errs
}

func (s source) hasFunc(dir, name string) bool {
	for file, fns := range s.funcs {
		if path.Dir(file) == dir && fns[name] {
			return true
		}
	}
	return false
}

// lintScenarioReqs: every requirement a scenario claims is a row listing it.
func lintScenarioReqs(rows map[string]Row, scenarios []ScenarioDecl) []error {
	var errs []error
	for _, s := range scenarios {
		for _, req := range s.Reqs {
			r, ok := rows[req]
			if !ok {
				errs = append(errs, fmt.Errorf("scenario %s claims %s, which has no inventory row", s.ID, req))
			} else if !slices.Contains(r.Scenarios, s.ID) {
				errs = append(errs, fmt.Errorf("%s: row does not list scenario %s, which claims it", req, s.ID))
			}
		}
	}
	return errs
}

// lintProfiles: names are unique and every allowed skip is `<leaf>=<reason>`
// for a leaf some row mandatory on that profile cites.
func lintProfiles(inv Inventory) []error {
	var errs []error
	seen := map[string]bool{}
	for _, p := range inv.Profiles {
		if p.Name == "" || seen[p.Name] {
			errs = append(errs, fmt.Errorf("profile %q: empty or duplicate name", p.Name))
		}
		seen[p.Name] = true
		for _, skip := range p.AllowedSkips {
			leaf, reason, ok := strings.Cut(skip, "=")
			if !ok || leaf == "" || reason == "" {
				errs = append(errs, fmt.Errorf("profile %s: allowed skip %q, want <leaf>=<reason>", p.Name, skip))
				continue
			}
			if !mandatoryLeaf(inv.Rows, p.Name, leaf) {
				errs = append(errs, fmt.Errorf("profile %s: allowed skip for %s, which no row mandatory on %s cites", p.Name, leaf, p.Name))
			}
		}
	}
	return errs
}

func mandatoryLeaf(rows []Row, profile, leaf string) bool {
	for _, r := range rows {
		if slices.Contains(r.Profiles, profile) && slices.ContainsFunc(r.Refs, func(ref Ref) bool { return ref.Test == leaf }) {
			return true
		}
	}
	return false
}

// scanTests parses every *_test.go under root, skipping dot-directories,
// testdata, docs and vendor, and records top-level functions and markers.
func scanTests(root string) (source, []error) {
	src := source{funcs: map[string]map[string]bool{}, sites: map[site]bool{}}
	var errs []error
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "docs" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			errs = append(errs, fmt.Errorf("parse %s: %w", rel, err))
			return nil
		}
		src.addFile(fset, rel, f)
		return nil
	})
	if walkErr != nil {
		errs = append(errs, fmt.Errorf("scan %s: %w", root, walkErr))
	}
	return src, errs
}

func (s *source) addFile(fset *token.FileSet, rel string, f *ast.File) {
	fns := map[string]bool{}
	var decls []*ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok {
			fns[funcName(fn)] = true
			decls = append(decls, fn)
		}
	}
	s.funcs[rel] = fns
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			id, ok := strings.CutPrefix(c.Text, markerPrefix)
			if !ok {
				continue
			}
			m := marker{site{id, rel, ""}, fmt.Sprintf("%s:%d", rel, fset.Position(c.Pos()).Line)}
			for _, fn := range decls {
				if fn.Pos() <= c.Pos() && c.Pos() < fn.End() {
					m.fn = funcName(fn)
				}
			}
			s.markers = append(s.markers, m)
			s.sites[m.site] = true
		}
	}
}

// funcName is a function's name, or Type.Method for a method.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch x := t.(type) {
	case *ast.IndexExpr:
		t = x.X
	case *ast.IndexListExpr:
		t = x.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}
