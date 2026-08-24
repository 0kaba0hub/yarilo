package lmtp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every answer a recipient gets is timed, and this is the row that makes that
// a rule rather than a habit.
//
// The behavioural rows next door count observations on the paths that exist
// today. A seventh exit added tomorrow with a bare status.SetStatus compiles,
// passes all of them, and puts a delivery back outside the graph -- silently,
// which is the shape this whole change exists to remove.
//
// So the assertion is on the source: SetStatus may be called from the two
// helpers in metrics.go and nowhere else. One of them times; the other names
// the seam that cannot be timed per recipient and says why. Adding a third
// exception means writing one there, deliberately.
func TestEveryRecipientStatusGoesThroughAHelper(t *testing.T) {
	const (
		timed   = "setStatus"
		proxied = "setProxyStatus"
	)
	allowed := map[string]bool{timed: true, proxied: true}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		checked++

		var enclosing string
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				enclosing = node.Name.Name
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "SetStatus" {
					return true
				}
				if allowed[enclosing] {
					return true
				}
				t.Errorf("%s: %s calls SetStatus directly -- a recipient answered outside %s is a delivery the graph never sees; route it through %s, or add a named helper here that says why it cannot be timed",
					fset.Position(node.Pos()), enclosing, timed, timed)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no source files were parsed; this guard proved nothing")
	}
}
