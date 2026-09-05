package imap

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Every log line on the reactive-heal path names the user, asserted over the
// source: the next line added would join the ones that did not (#1683).
func TestEveryHealLogLineNamesTheUser(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "dbox_rebuild.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "slog" {
			return true
		}
		seen++
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value == `"user"` {
				return true
			}
		}
		msg := ""
		if lit, ok := call.Args[0].(*ast.BasicLit); ok {
			msg = lit.Value
		}
		t.Errorf("%s at %s carries no user: a per-user diagnosis has to be made from "+
			"neighbouring sources", msg, fset.Position(call.Pos()))
		return true
	})
	// A parse that found nothing would pass silently.
	if seen < 8 {
		t.Fatalf("found %d slog calls in dbox_rebuild.go, want at least 8: the scan is not "+
			"reaching them", seen)
	}
}
