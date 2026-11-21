package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// launchAPIs are the package-qualified names that start a process, keyed by
// the import path (golang.org/x/sys/... packages share one entry). exec.Cmd
// is handled separately: only a composite literal of it is a launch.
var launchAPIs = map[string][]string{
	"os/exec": {"Command", "CommandContext"},
	"os":      {"StartProcess", "ProcAttr"},
	"syscall": {"Exec", "ForkExec", "StartProcess"},
	"x/sys":   {"Exec", "ForkExec", "StartProcess"},
}

// allowedLaunches is every process launch cmd/golem may contain, keyed by
// enclosing function and API with its exact count. A new launch must be
// classified here, which means routing it through hostGitCommand or
// reviewing it as deliberately non-git.
var allowedLaunches = map[string]int{
	"hostGitCommand exec.CommandContext":   1,
	"hostGitCommand Path assignment":       1, // pins the resolved git path
	"requireHostGit exec.CommandContext":   1, // the --version probe
	"runEditorProcess exec.CommandContext": 1,
}

// canonPkg names each API group in records, whatever the local import alias.
var canonPkg = map[string]string{"os/exec": "exec", "os": "os", "syscall": "syscall", "x/sys": "sys"}

// launchRecord is one launch reference and where it is.
type launchRecord struct{ key, pos string }

// collectLaunches parses src as one file of package main and returns every
// process-launch reference and every refusal (dot imports). A dot import of
// a launch package fails because it would hide what is called.
func collectLaunches(fset *token.FileSet, name string, src any) ([]launchRecord, []string) {
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var recs []launchRecord
	var refusals []string
	pkgOf := map[string]string{} // local name -> launchAPIs key
	for _, is := range f.Imports {
		p, _ := strconv.Unquote(is.Path.Value)
		api := p
		if strings.HasPrefix(p, "golang.org/x/sys/") {
			api = "x/sys"
		} else if _, ok := launchAPIs[p]; !ok {
			continue
		}
		local := p[strings.LastIndex(p, "/")+1:]
		if is.Name != nil {
			local = is.Name.Name
		}
		switch local {
		case ".":
			refusals = append(refusals, fmt.Sprintf("%s: dot import of %s hides process launches", fset.Position(is.Pos()), p))
		case "_":
		default:
			pkgOf[local] = api
		}
	}
	for _, d := range f.Decls {
		fn := name + ":<package-level>"
		if fd, ok := d.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) > 0 {
				t := fd.Recv.List[0].Type
				if s, ok := t.(*ast.StarExpr); ok {
					t = s.X
				}
				if ix, ok := t.(*ast.IndexExpr); ok {
					t = ix.X
				}
				if id, ok := t.(*ast.Ident); ok {
					fn = id.Name + "." + fn
				}
			}
		}
		var dr []launchRecord
		var paths []launchRecord
		add := func(to *[]launchRecord, api string, pos token.Pos) {
			*to = append(*to, launchRecord{fn + " " + api, fset.Position(pos).String()})
		}
		qualified := func(e ast.Expr) (string, string, bool) {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				return "", "", false
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || pkgOf[id.Name] == "" {
				return "", "", false
			}
			return id.Name, sel.Sel.Name, true
		}
		ast.Inspect(d, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if l, sel, ok := qualified(x.Type); ok && pkgOf[l] == "os/exec" && sel == "Cmd" {
					add(&dr, "exec.Cmd literal", x.Pos())
				}
			case *ast.SelectorExpr:
				if l, sel, ok := qualified(x); ok && slices.Contains(launchAPIs[pkgOf[l]], sel) {
					add(&dr, canonPkg[pkgOf[l]]+"."+sel, x.Pos())
				}
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if s, ok := lhs.(*ast.SelectorExpr); ok && (s.Sel.Name == "Path" || s.Sel.Name == "Args") {
						add(&paths, s.Sel.Name+" assignment", s.Pos())
					}
				}
			}
			return true
		})
		recs = append(recs, dr...)
		if len(dr) > 0 { // ponytail: any .Path/.Args write beside a launch counts
			recs = append(recs, paths...)
		}
	}
	return recs, refusals
}

// TestHostProcessLaunchGuardCatchesForms pins the collector against the ways
// a launch can be written, so the production scan cannot silently go blind.
func TestHostProcessLaunchGuardCatchesForms(t *testing.T) {
	tests := []struct {
		name, src string
		want      []string
		refuse    bool
	}{
		{"second Command in allowed func", "package main\nimport \"os/exec\"\nfunc hostGitCommand() { exec.CommandContext(nil, \"a\"); exec.Command(\"b\") }",
			[]string{"hostGitCommand exec.CommandContext", "hostGitCommand exec.Command"}, false},
		{"function value", "package main\nimport \"os/exec\"\nfunc f() { g := exec.Command; _ = g }",
			[]string{"f exec.Command"}, false},
		{"Cmd literal", "package main\nimport \"os/exec\"\nfunc f(p string) { _ = &exec.Cmd{Path: p} }",
			[]string{"f exec.Cmd literal"}, false},
		{"Cmd type is not a launch", "package main\nimport \"os/exec\"\nfunc f() *exec.Cmd { var c *exec.Cmd; return c }",
			nil, false},
		{"Path assignment", "package main\nimport \"os/exec\"\nfunc f(p string) { c := exec.CommandContext(nil, \"a\"); c.Path = p }",
			[]string{"f exec.CommandContext", "f Path assignment"}, false},
		{"syscall.Exec", "package main\nimport \"syscall\"\nfunc f() { syscall.Exec(\"a\", nil, nil) }",
			[]string{"f syscall.Exec"}, false},
		{"aliased import", "package main\nimport x \"os/exec\"\nfunc f() { x.Command(\"a\") }",
			[]string{"f exec.Command"}, false},
		{"method and package level", "package main\nimport \"os\"\ntype T struct{}\nfunc (t *T) m() { os.StartProcess(\"a\", nil, nil) }\nvar v = os.ProcAttr{}",
			[]string{"T.m os.StartProcess", "x.go:<package-level> os.ProcAttr"}, false},
		{"x/sys", "package main\nimport \"golang.org/x/sys/unix\"\nfunc f() { unix.ForkExec(\"a\", nil, nil) }",
			[]string{"f sys.ForkExec"}, false},
		{"dot import", "package main\nimport . \"os/exec\"\nfunc f() { Command(\"a\") }",
			nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recs, refusals := collectLaunches(token.NewFileSet(), "x.go", tt.src)
			var got []string
			for _, r := range recs {
				got = append(got, r.key)
			}
			slices.Sort(got)
			want := slices.Clone(tt.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("records = %q, want %q", got, want)
			}
			if tt.refuse != (len(refusals) > 0) {
				t.Errorf("refusals = %q, want refusal %v", refusals, tt.refuse)
			}
		})
	}
}

// TestHostProcessLaunchesAreClassified fails when cmd/golem gains, loses or
// moves a process launch. Every .go file in the directory is parsed whatever
// its build tags, so platform-specific launches are seen too.
func TestHostProcessLaunchesAreClassified(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v %v", files, err)
	}
	// ponytail: top level only; the only subdirectory is testdata, which
	// holds no package main of this binary.
	fset := token.NewFileSet()
	found := map[string][]string{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		recs, refusals := collectLaunches(fset, path, src)
		for _, r := range refusals {
			t.Error(r)
		}
		for _, r := range recs {
			found[r.key] = append(found[r.key], r.pos)
		}
	}
	for key, poss := range found {
		if len(poss) != allowedLaunches[key] {
			t.Errorf("unexpected launch %q x%d (allowed %d) at %s: route it through hostGitCommand or classify it in allowedLaunches", key, len(poss), allowedLaunches[key], strings.Join(poss, ", "))
		}
	}
	for key, n := range allowedLaunches {
		if _, ok := found[key]; !ok {
			t.Errorf("missing launch %q (allowed x%d): remove it from allowedLaunches", key, n)
		}
	}
}
