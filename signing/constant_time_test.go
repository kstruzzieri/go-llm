package signing

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestHMACCompareIsConstantTime is a source-level gate, not a timing test. It
// pins one direct hmac.Equal inside HMACSigner.Verify. A timing assertion
// would be flaky and blind; scoping the AST gate to the secret comparison
// avoids banning legitimate equality checks elsewhere in the package.
func TestHMACCompareIsConstantTime(t *testing.T) {
	forbidden := map[string]bool{
		"bytes.Equal":       true,
		"bytes.Compare":     true,
		"reflect.DeepEqual": true,
		"slices.Equal":      true,
		"strings.EqualFold": true,
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "hmac.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var verify *ast.FuncDecl
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv != nil && fn.Name.Name == "Verify" {
			if verify != nil {
				t.Fatal("hmac.go has more than one Verify method")
			}
			verify = fn
		}
	}
	if verify == nil {
		t.Fatal("hmac.go has no Verify method")
	}
	hmacEqualCalls := 0
	ast.Inspect(verify.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			call := selectorName(x.Fun)
			if forbidden[call] {
				t.Errorf("%s: %s is forbidden for MAC comparison", fset.Position(x.Pos()), call)
			}
			if call == "hmac.Equal" {
				hmacEqualCalls++
			}
		case *ast.BinaryExpr:
			if (x.Op == token.EQL || x.Op == token.NEQ) && (isStringConversion(x.X) || isStringConversion(x.Y)) {
				t.Errorf("%s: string(...) equality is a variable-time compare", fset.Position(x.Pos()))
			}
		}
		return true
	})
	if hmacEqualCalls != 1 {
		t.Errorf("HMACSigner.Verify hmac.Equal calls = %d, want exactly 1", hmacEqualCalls)
	}
}

func selectorName(fun ast.Expr) string {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + sel.Sel.Name
}

func isStringConversion(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "string"
}
