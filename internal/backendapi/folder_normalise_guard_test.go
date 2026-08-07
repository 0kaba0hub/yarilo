package backendapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// backendapi is not one resolver: each request type is decoded and resolved on
// its own, so folder-name normalisation is placed by hand. A new decode path
// that reads a folder name and forgets to normalise it would fail silently -- a
// decomposed name would just build a divergent tree, the exact defect #1113
// closes, walking in a door #1113 did not shut.
//
// The guard scopes to the decode boundary: a function that calls decodeJSON
// owns a raw wire name, so any folder field it reads must pass through
// mailbox.NormalizeName in that same function. A handler that instead receives
// its request from a resolver (openSpecialUseStoreReq, ...) does not decode and
// is not the owner -- the resolver it called is, and the resolver is checked.
//
// This turns the several placements into one thing that fails, the way
// root_cleanup_guard_test.go reads a call rather than a behaviour.
func TestFolderNamesAreNormalisedAtEveryDecodeBoundary(t *testing.T) {
	folderFields := map[string]bool{"Folder": true, "OldFolder": true, "NewFolder": true}

	fset := token.NewFileSet()
	var checked int
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("%s: %v", path, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}

			var decodes bool
			reads := map[string]bool{}
			normalised := map[string]bool{}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				switch e := m.(type) {
				case *ast.CallExpr:
					if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "decodeJSON" {
						decodes = true
					}
					// NormalizeName(req.Field, ...) — any expression, not only
					// an assignment, so the return-value form counts too.
					if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NormalizeName" {
						for _, a := range e.Args {
							if f := folderFieldOf(a, folderFields); f != "" {
								normalised[f] = true
							}
						}
					}
				case *ast.SelectorExpr:
					if f := folderFieldOf(e, folderFields); f != "" {
						reads[f] = true
					}
				}
				return true
			})
			if !decodes || len(reads) == 0 {
				return true
			}
			for field := range reads {
				checked++
				if !normalised[field] {
					t.Errorf("%s decodes a request and reads req.%s but never passes it through mailbox.NormalizeName — a decomposed name would build a divergent tree (#1113)",
						fn.Name.Name, field)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if checked == 0 {
		t.Fatal("the guard matched no decode boundary; its shape no longer fits the code and it is asserting nothing")
	}
}

// folderFieldOf returns the folder field name when e is req.<Folder|OldFolder|
// NewFolder>, else "".
func folderFieldOf(e ast.Expr, fields map[string]bool) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != "req" || !fields[sel.Sel.Name] {
		return ""
	}
	return sel.Sel.Name
}
