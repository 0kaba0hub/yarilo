package logrotate_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One key must not have two built-in values.
//
// Both logs -- the per-folder index log and the mdbox map log -- are
// configured by the same three settings, and each package used to carry its
// own copy of the defaults. They agreed, so nothing showed, until the fold age
// was lowered in one of them (#1460): an operator who left the key alone would
// then have got one cadence on one log and another on the other, with nothing
// anywhere saying so. Half a setting is harder to find than a missing one.
//
// So the numbers live in this package, and this row refuses a package that
// declares its own. Checked by source, because what it guards against is a
// constant nobody has written yet -- the same reason the control-root rule is
// checked this way.
func TestNobodyDeclaresTheirOwnRotationDefaults(t *testing.T) {
	// Names that mean "my own copy of a rotation threshold". A package is free
	// to name a variable after the value it read from this one.
	suspect := func(name string) bool {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "defaultlog") {
			return false
		}
		return strings.Contains(lower, "rotate") || strings.Contains(lower, "compact")
	}

	var checked int
	err := filepath.Walk("../..", func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr // a vanished entry is not this walk's problem
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil //nolint:nilerr // unparseable files are somebody else's failure
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			decl, ok := n.(*ast.GenDecl)
			if !ok || (decl.Tok != token.CONST && decl.Tok != token.VAR) {
				return true
			}
			for _, spec := range decl.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !suspect(name.Name) {
						continue
					}
					// Reading the shared value is the point; only a literal is
					// a second source of truth.
					if i < len(vs.Values) {
						if _, isLit := vs.Values[i].(*ast.BasicLit); !isLit {
							if _, isBinary := vs.Values[i].(*ast.BinaryExpr); !isBinary {
								continue
							}
						}
					}
					t.Errorf("%s: %s is a rotation threshold declared outside internal/storage/logrotate -- one key with two built-in values is a setting that half works, and the half that does not is silent",
						fset.Position(name.Pos()), name.Name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no source files were parsed; this guard proved nothing")
	}
}
