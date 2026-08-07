package imap

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// dispatch is the one resolver that turns a wire folder name into the rel every
// tree is addressed by, so it is the one place NFC has to be applied. This
// asserts the resolver still normalises, reading the function rather than the
// behaviour: a body that stopped calling the normaliser would compile and pass
// every functional test whose fixture happened not to use a decomposed name
// (#1113).
func TestDispatchNormalisesTheName(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "dispatch.go", nil, 0)
	if err != nil {
		t.Fatalf("parse dispatch.go: %v", err)
	}
	var found, normalises bool
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "dispatch" || fn.Recv == nil {
			return true
		}
		found = true
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			if call, ok := m.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok &&
					(sel.Sel.Name == "normaliseName" || sel.Sel.Name == "NormalizeName") {
					normalises = true
				}
			}
			return true
		})
		return false
	})
	if !found {
		t.Fatal("dispatch resolver not found — the guard must move with it")
	}
	if !normalises {
		t.Error("dispatch no longer normalises the folder name (#1113)")
	}
}
