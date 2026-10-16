package containmentinv

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// markerPrefix starts a marker comment; the rest of the comment is the row ID.
const markerPrefix = "// containment: "

var layers = []string{"aggregate", "focused", "scenario", "model"}

// site is one marker position: a row ID inside a top-level function of a file.
type site struct{ id, file, fn string }

type marker struct {
	site
	pos    string    // file:line
	offset token.Pos // position in the scan's file set
}

type functionKey struct{ dir, pkg, name string }

// source is what the lint needs from the scanned test files.
type source struct {
	funcs        map[string]map[string]bool    // slash path -> top-level function names
	packages     map[string]string             // slash path -> Go package name
	dependencies map[functionKey][]functionKey // calls and registered function values
	module       string
	sites        map[site]bool
	markers      []marker
	// lastAttack is, per slash path and function, the position of the last
	// assignment to the observation's AttackEffect field: a `.AttackEffect =`
	// statement or an `observation{AttackEffect: ...}` literal. Reads and the
	// other fields do not count, so a deleted probe cannot leave the marker
	// bound to a control write while AttackEffect defaults to contained.
	lastAttack map[string]map[string]token.Pos
	// registrars is, per slash path and scenario ID, the functions bound to
	// the scenario's registration (registrationFuncs).
	registrars map[string]map[string][]string
}

// scenarioFile is the only file whose registrations a TestContainment ref may
// cite, and registryRoots are its registry functions.
const scenarioFile = "cmd/golem/containment_scenarios_test.go"

var registryRoots = []string{"containmentScenarios", "executableScenarios", "pendingScenarios"}

// Lint checks inv against the *_test.go files under repoRoot and the
// registry's scenario declarations, and returns every violation:
//   - each ref names an existing function whose body holds the row's marker;
//   - non-scenario refs reach that function through static function references;
//   - each marker names a row and sits in a function one of that row's refs cites;
//   - scenario-backed statuses are derived from the registry, never contradicted;
//   - a row cites TestContainment/<X> for each executable scenario it lists, and only those;
//   - pending and out-of-scope rows carry no refs; pending and uncontained need an owner;
//   - model rows list artifacts, rows with refs list known mandatory profiles;
//   - a TestContainment/<X> ref cites a function bound to scenario X's
//     registration, and its marker precedes an AttackEffect assignment there;
//   - a verified or uncontained row outside the model layer cites at least one
//     entry of the mutation ledger, every cited entry exists in ledger and
//     lists the row in its Affected refs, and every row an entry lists cites
//     it; only an administrative META- row may carry an Exempt reason instead;
//   - the linux-host profile allows no skip, every other allowed skip is
//     capability:<cap> for a cited leaf, a scenario leaf's cap is the first of
//     its caps the profile lacks, and a row lists a profile only if one of its
//     refs is not an allowed skip there.
func Lint(inv Inventory, repoRoot string, scenarios []ScenarioDecl, ledger []LedgerEntry) []error {
	src, errs := scanTests(repoRoot)
	decls := make(map[string]ScenarioDecl, len(scenarios))
	for _, s := range scenarios {
		decls[s.ID] = s
	}
	entries := make(map[string]bool, len(ledger))
	for _, e := range ledger {
		entries[e.ID] = true
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
		errs = append(errs, lintRow(r, decls, profiles, entries)...)
		for _, ref := range r.Refs {
			cited[site{r.ID, ref.File, ref.Func}] = true
			errs = append(errs, lintRef(r.ID, ref, src)...)
		}
	}
	errs = append(errs, lintScenarioReqs(rows, scenarios)...)
	errs = append(errs, lintLedger(rows, ledger)...)
	errs = append(errs, lintProfiles(inv, decls)...)
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

// lintLedger: a row's cited entries list it in their Affected refs, and every
// row an entry lists cites the entry.
func lintLedger(rows map[string]Row, ledger []LedgerEntry) []error {
	var errs []error
	listed := map[string][]string{}
	for _, e := range ledger {
		listed[e.ID] = e.Rows
		if len(e.Rows) == 0 {
			errs = append(errs, fmt.Errorf("ledger %s lists no inventory row in its Affected refs", e.ID))
		}
		for _, id := range e.Rows {
			r, ok := rows[id]
			switch {
			case !ok:
				errs = append(errs, fmt.Errorf("ledger %s lists %s, which has no inventory row", e.ID, id))
			case !slices.Contains(r.Mutations, e.ID):
				errs = append(errs, fmt.Errorf("%s: ledger %s lists this row, which does not cite it", id, e.ID))
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		for _, m := range rows[id].Mutations {
			if rs, ok := listed[m]; ok && !slices.Contains(rs, id) {
				errs = append(errs, fmt.Errorf("%s: cites %s, whose Affected refs do not list this row", id, m))
			}
		}
	}
	return errs
}

func hasRow(rows map[string]Row, id string) bool {
	_, ok := rows[id]
	return ok
}

func lintRow(r Row, decls map[string]ScenarioDecl, profiles, ledger map[string]bool) []error {
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
	// Spec §6.4: model rows are evidenced by artifacts, and pending,
	// unobserved, out-of-scope and administrative rows by their Exempt reason.
	// A verified or uncontained row may claim an exemption only when it is
	// administrative (META-); any other such row cites the ledger.
	if kind == "verified" || kind == "uncontained" {
		switch {
		case r.Exempt != "" && !strings.HasPrefix(r.ID, "META-"):
			add("%s row carries an exempt reason; only an administrative META- row may, any other cites a mutation ledger entry", kind)
		case r.Layer != "model" && r.Exempt == "" && len(r.Mutations) == 0:
			add("%s row cites no mutation ledger entry and has no exempt reason", kind)
		}
	}
	for _, m := range r.Mutations {
		if !ledger[m] {
			add("mutation %s is not an entry of the mutation ledger", m)
		}
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
		return append(errs, fmt.Errorf("%s: no `%s%s` marker inside %s (%s) for %s", id, markerPrefix, id, ref.Func, ref.File, ref.Test))
	}
	scenario, ok := strings.CutPrefix(ref.Test, "TestContainment/")
	if !ok {
		top, _, _ := strings.Cut(ref.Test, "/")
		if !src.reaches(path.Dir(ref.File), top, functionKey{path.Dir(ref.File), src.packages[ref.File], ref.Func}) {
			errs = append(errs, fmt.Errorf("%s: ref %s: marked function %s (%s) is not reachable from test %s", id, ref.Test, ref.Func, ref.File, top))
		}
		return errs
	}
	scenario, _, _ = strings.Cut(scenario, "/")
	if ref.File != scenarioFile {
		return append(errs, fmt.Errorf("%s: ref %s: file %s, want %s, the scenario registry", id, ref.Test, ref.File, scenarioFile))
	}
	switch fns := src.registrars[ref.File][scenario]; {
	case len(fns) == 0:
		errs = append(errs, fmt.Errorf("%s: ref %s: no function in %s registers scenario %s", id, ref.Test, ref.File, scenario))
	case !slices.Contains(fns, ref.Func):
		errs = append(errs, fmt.Errorf("%s: ref %s: %s is not bound to scenario %s's registration (%s)", id, ref.Test, ref.Func, scenario, strings.Join(fns, ", ")))
	}
	if !slices.ContainsFunc(src.markers, func(m marker) bool {
		return m.site == site{id, ref.File, ref.Func} && m.offset < src.lastAttack[ref.File][ref.Func]
	}) {
		errs = append(errs, fmt.Errorf("%s: ref %s: no `%s%s` marker in %s precedes an AttackEffect assignment", id, ref.Test, markerPrefix, id, ref.Func))
	}
	return errs
}

// reaches checks structural references, including table-driven callbacks and
// external-test bridges. It does not prove control flow inside a test body.
func (s source) reaches(dir, test string, target functionKey) bool {
	var queue []functionKey
	for fn := range s.dependencies {
		if fn.dir == dir && fn.name == test {
			queue = append(queue, fn)
		}
	}
	seen := map[functionKey]bool{}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if seen[fn] {
			continue
		}
		seen[fn] = true
		if fn == target {
			return true
		}
		queue = append(queue, s.dependencies[fn]...)
	}
	return false
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

// capabilitySkip is the only skip reason a profile may allow: the
// missingCapabilityReason form t.Skip logs.
var capabilitySkip = regexp.MustCompile(`^capability:[a-z]+$`)

// lintProfiles: names are unique and linux-host allows no skip. Every allowed
// skip is `<leaf>=capability:<cap>` for a leaf some row cites; for a
// TestContainment/<X> leaf, cap is the first of X's caps the profile lacks,
// the only reason X can report there. A row lists a profile only when one of
// its refs is not an allowed skip there: a row whose every ref may skip is
// skip-allowed (the checker still runs it), not mandatory.
func lintProfiles(inv Inventory, decls map[string]ScenarioDecl) []error {
	var errs []error
	seen := map[string]bool{}
	for _, p := range inv.Profiles {
		if p.Name == "" || seen[p.Name] {
			errs = append(errs, fmt.Errorf("profile %q: empty or duplicate name", p.Name))
		}
		seen[p.Name] = true
		if p.Name == "linux-host" && len(p.AllowedSkips) > 0 {
			errs = append(errs, fmt.Errorf("profile linux-host must allow no skip, allows %s", strings.Join(p.AllowedSkips, ", ")))
		}
		for _, skip := range p.AllowedSkips {
			leaf, reason, ok := strings.Cut(skip, "=")
			if !ok || leaf == "" || reason == "" {
				errs = append(errs, fmt.Errorf("profile %s: allowed skip %q, want <leaf>=<reason>", p.Name, skip))
				continue
			}
			if !capabilitySkip.MatchString(reason) {
				errs = append(errs, fmt.Errorf("profile %s: allowed skip reason %q, want capability:<cap>", p.Name, reason))
			} else if id, ok := strings.CutPrefix(leaf, "TestContainment/"); ok {
				errs = append(errs, lintScenarioSkip(p, id, strings.TrimPrefix(reason, "capability:"), decls)...)
			}
			if !citedLeaf(inv.Rows, leaf) {
				errs = append(errs, fmt.Errorf("profile %s: allowed skip for %s, which no row cites", p.Name, leaf))
			}
		}
		for _, r := range inv.Rows {
			if len(r.Refs) > 0 && slices.Contains(r.Profiles, p.Name) && !slices.ContainsFunc(r.Refs, func(ref Ref) bool { return !skipAllowed(p, ref.Test) }) {
				errs = append(errs, fmt.Errorf("%s: lists profile %s, but every ref is an allowed skip there; the row is skip-allowed, not mandatory", r.ID, p.Name))
			}
		}
	}
	return errs
}

// lintScenarioSkip: missingCapabilityReason reports the first of a scenario's
// caps the host lacks, so the profile may allow exactly that one.
func lintScenarioSkip(p Profile, id, c string, decls map[string]ScenarioDecl) []error {
	d, ok := decls[id]
	if !ok {
		return []error{fmt.Errorf("profile %s: allowed skip for scenario %s, which is not declared", p.Name, id)}
	}
	if !slices.Contains(d.Caps, c) {
		return []error{fmt.Errorf("profile %s: allowed skip for %s names capability %s, which the scenario does not require (caps %s)", p.Name, id, c, strings.Join(d.Caps, ", "))}
	}
	if slices.Contains(p.Caps, c) {
		return []error{fmt.Errorf("profile %s: allowed skip for %s names capability %s, which the profile provides", p.Name, id, c)}
	}
	if first := d.Caps[slices.IndexFunc(d.Caps, func(x string) bool { return !slices.Contains(p.Caps, x) })]; c != first {
		return []error{fmt.Errorf("profile %s: allowed skip for %s names capability %s, but the scenario reports the first cap the profile lacks, %s", p.Name, id, c, first)}
	}
	return nil
}

// skipAllowed reports whether leaf has an allowed skip on p.
func skipAllowed(p Profile, leaf string) bool {
	return slices.ContainsFunc(p.AllowedSkips, func(s string) bool { return strings.HasPrefix(s, leaf+"=") })
}

func citedLeaf(rows []Row, leaf string) bool {
	return slices.ContainsFunc(rows, func(r Row) bool {
		return slices.ContainsFunc(r.Refs, func(ref Ref) bool { return ref.Test == leaf })
	})
}

// scanTests parses every *_test.go under root, skipping dot- and
// underscore-prefixed directories, testdata, docs, vendor and nested modules
// (a directory with its own go.mod), and records top-level functions, markers,
// observation statements and scenario registrations.
func scanTests(root string) (source, []error) {
	src := source{funcs: map[string]map[string]bool{}, sites: map[site]bool{},
		packages: map[string]string{}, dependencies: map[functionKey][]functionKey{},
		lastAttack: map[string]map[string]token.Pos{}, registrars: map[string]map[string][]string{}}
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "module" {
				src.module = fields[1]
				if unquoted, err := strconv.Unquote(src.module); err == nil {
					src.module = unquoted
				}
				break
			}
		}
	}
	var errs []error
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p == root {
				return nil
			}
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "docs" || name == "vendor" {
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(p, "go.mod")); err == nil {
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
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			errs = append(errs, fmt.Errorf("containmentinv: parse %s: %w", rel, err))
			return nil
		}
		src.addFile(fset, rel, f)
		return nil
	})
	if walkErr != nil {
		errs = append(errs, fmt.Errorf("containmentinv: scan %s: %w", root, walkErr))
	}
	return src, errs
}

func (s *source) addFile(fset *token.FileSet, rel string, f *ast.File) {
	s.packages[rel] = f.Name.Name
	localImports := map[string]bool{}
	for _, imp := range f.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil || s.module == "" || importPath != path.Join(s.module, path.Dir(rel)) {
			continue
		}
		alias := path.Base(importPath)
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		localImports[alias] = true
	}
	fns := map[string]bool{}
	var decls []*ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok {
			fns[funcName(fn)] = true
			decls = append(decls, fn)
			key := functionKey{path.Dir(rel), f.Name.Name, funcName(fn)}
			s.dependencies[key] = nil
			var inspect func(ast.Node) bool
			inspect = func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if pkg, ok := x.X.(*ast.Ident); ok && pkg.Obj == nil && localImports[pkg.Name] {
						s.dependencies[key] = append(s.dependencies[key], functionKey{key.dir, strings.TrimSuffix(key.pkg, "_test"), x.Sel.Name})
					}
					if _, name := x.X.(*ast.Ident); !name {
						ast.Inspect(x.X, inspect)
					}
					return false
				case *ast.KeyValueExpr:
					// A bare key can name a struct field, never a function value.
					if _, name := x.Key.(*ast.Ident); !name {
						ast.Inspect(x.Key, inspect)
					}
					ast.Inspect(x.Value, inspect)
					return false
				case *ast.Ident:
					if x.Obj == nil || x.Obj.Kind == ast.Fun {
						s.dependencies[key] = append(s.dependencies[key], functionKey{key.dir, key.pkg, x.Name})
					}
				}
				return true
			}
			ast.Inspect(fn.Body, inspect)
		}
	}
	s.funcs[rel] = fns
	s.lastAttack[rel] = map[string]token.Pos{}
	s.registrars[rel] = map[string][]string{}
	regs := map[string]map[string][]string{} // function -> scenario ID -> bound functions
	callees := map[string][]string{}
	for _, fn := range decls {
		regs[funcName(fn)], callees[funcName(fn)] = s.scanFunc(rel, fn, fns)
	}
	// Only registry entries count: registrations in functions the registry
	// roots reach through this file's calls, never a test's own literal.
	reached := map[string]bool{}
	queue := slices.Clone(registryRoots)
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if reached[fn] || !fns[fn] {
			continue
		}
		reached[fn] = true
		queue = append(queue, callees[fn]...)
	}
	for _, fn := range slices.Sorted(maps.Keys(reached)) {
		for id, bound := range regs[fn] {
			for _, b := range bound {
				if !slices.Contains(s.registrars[rel][id], b) {
					s.registrars[rel][id] = append(s.registrars[rel][id], b)
				}
			}
		}
	}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			id, ok := strings.CutPrefix(c.Text, markerPrefix)
			if !ok {
				continue
			}
			m := marker{site{id, rel, ""}, fmt.Sprintf("%s:%d", rel, fset.Position(c.Pos()).Line), c.Pos()}
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

// scanFunc records fn's last observation statement and returns the
// scenarios it registers and the functions of this file it calls. A scenario
// is registered by a `scenario` composite literal that sets `ID: "<X>"` and
// `run:`, or by a call to a function of this file whose first string literal
// argument is "<X>" (wScenario, pendingRow). The functions bound to the
// registration are fn itself, the called helper, and every function of this
// file called by name inside the literal or call (d1's readExport).
func (s *source) scanFunc(rel string, fn *ast.FuncDecl, fns map[string]bool) (map[string][]string, []string) {
	name := funcName(fn)
	regs := map[string][]string{}
	var callees []string
	register := func(id string, node ast.Node, helper string) {
		bound := []string{name}
		if helper != "" {
			bound = append(bound, helper)
		}
		ast.Inspect(node, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if callee, ok := call.Fun.(*ast.Ident); ok && fns[callee.Name] {
					bound = append(bound, callee.Name)
				}
			}
			return true
		})
		for _, b := range bound {
			if !slices.Contains(regs[id], b) {
				regs[id] = append(regs[id], b)
			}
		}
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "AttackEffect" {
					s.lastAttack[rel][name] = max(s.lastAttack[rel][name], x.Pos())
				}
			}
		case *ast.CompositeLit:
			t, _ := x.Type.(*ast.Ident)
			if t != nil && t.Name == "observation" {
				for _, elt := range x.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if key, _ := kv.Key.(*ast.Ident); key != nil && key.Name == "AttackEffect" {
							s.lastAttack[rel][name] = max(s.lastAttack[rel][name], x.Pos())
						}
					}
				}
			}
			if t == nil || t.Name != "scenario" {
				break
			}
			var id string
			var hasID, hasRun bool
			for _, elt := range x.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				switch key, _ := kv.Key.(*ast.Ident); {
				case key == nil:
				case key.Name == "ID":
					id, hasID = stringLit(kv.Value)
				case key.Name == "run":
					hasRun = true
				}
			}
			if hasID && hasRun {
				register(id, x, "")
			}
		case *ast.CallExpr:
			if callee, ok := x.Fun.(*ast.Ident); ok && fns[callee.Name] {
				callees = append(callees, callee.Name)
				for _, arg := range x.Args {
					if id, ok := stringLit(arg); ok {
						register(id, x, callee.Name)
						break
					}
				}
			}
		}
		return true
	})
	return regs, callees
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
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
