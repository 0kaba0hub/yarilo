package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The smoke test runs against a live deployment, from inside the cluster, with
// the image that was just rolled out. Nothing in its flags or its documentation
// says the account must be disposable — so it must not empty a mailbox it did
// not fill.
//
// It did: clearInbox selected INBOX, searched for everything, and expunged it,
// twenty-five times per run. Three messages seeded into u51 for an unrelated
// measurement were gone after one run, and the loss was silent — the delete was
// //nolint:errcheck and the function returned nothing on any failure path
// (#1056).
//
// This reads the tree rather than trusting review because the destructive
// version compiled, passed, and read as housekeeping. Its comment described the
// problem it solved and not the one it caused.
func TestNothingEmptiesTheInbox(t *testing.T) {
	fset := token.NewFileSet()
	var checked int

	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Errorf("%s: %v", path, perr)
			return nil
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			checked++
			var namesInbox, searchesEverything, deletes bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.BasicLit:
					if v.Kind == token.STRING && strings.Contains(v.Value, "INBOX") {
						namesInbox = true
					}
				case *ast.CallExpr:
					sel, ok := v.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					switch sel.Sel.Name {
					case "deleteUIDs":
						deletes = true
					case "uidSearch":
						// An unbounded search: everything in the mailbox,
						// whoever put it there. A search for the check's own
						// subject or marker is the correct shape and stays
						// allowed — the fault is the breadth, not the delete.
						if len(v.Args) == 1 {
							if lit, ok := v.Args[0].(*ast.BasicLit); ok &&
								lit.Kind == token.STRING && strings.Contains(lit.Value, "ALL") {
								searchesEverything = true
							}
						}
					}
				}
				return true
			})
			if namesInbox && searchesEverything && deletes {
				t.Errorf("%s: %s selects INBOX, searches ALL and deletes what it finds — the "+
					"smoke test must not empty a mailbox it did not fill. Identify its own "+
					"message by the unique subject or marker it already sends.",
					fset.Position(fn.Pos()), fn.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// A guard that finds nothing to guard has stopped guarding: rename the
	// helper or move the package and this would keep passing over an empty set.
	if checked < 20 {
		t.Errorf("inspected only %d functions; the search is no longer finding the source", checked)
	}
}

// Deleting inside a folder the check created is the correct shape and must stay
// allowed, or the guard above would push the next author towards leaving state
// behind instead.
func TestDeletingInsideAnOwnedFolderIsAllowed(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sieve.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "checkFolder" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "deleteUIDs" {
					found = true
				}
			}
			return true
		})
	}
	if !found {
		t.Skip("checkFolder no longer deletes; the guard's boundary has moved and wants rereading")
	}
}
