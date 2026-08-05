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

// The guard in inbox_guard_test.go reads one function at a time, and the defect
// in #1070 was assembled from two: checkFolder searched ALL and expunged, while
// the name "INBOX" was supplied by its caller. Neither function carried all
// three signals, so nothing fired — and the smoke run emptied a live mailbox on
// every pass for as long as that arrangement stood.
//
// These read the call, not the function body: what a caller hands to a
// destructive helper is the thing that decides whether it is destructive.

// rootLiteral reports whether e is a string literal naming the mailbox root.
func rootLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v := strings.Trim(lit.Value, "`\"")
	return v == "" || strings.EqualFold(v, "INBOX")
}

func eachCall(t *testing.T, fn func(pos token.Position, name string, args []ast.Expr)) int {
	t.Helper()
	fset := token.NewFileSet()
	var files int
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Errorf("%s: %v", path, perr)
			return nil
		}
		files++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.Ident:
				fn(fset.Position(call.Pos()), f.Name, call.Args)
			case *ast.SelectorExpr:
				fn(fset.Position(call.Pos()), f.Sel.Name, call.Args)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if files < 3 {
		t.Errorf("parsed only %d source files; the search is no longer finding the package", files)
	}
	return files
}

// A check against the mailbox root has to name the messages it injected. Its
// cleanup can then remove those and leave the rest, which is the difference
// between tidying up after a test and emptying somebody's mail.
func TestChecksAgainstTheRootNameWhatTheySeeded(t *testing.T) {
	eachCall(t, func(pos token.Position, name string, args []ast.Expr) {
		if name != "checkFolder" || len(args) < 3 {
			return
		}
		if !rootLiteral(args[2]) {
			return
		}
		if len(args) < 4 {
			t.Errorf("%s: checkFolder names the mailbox root with nothing to scope its cleanup by — "+
				"pass the Message-ID this check injected, or the cleanup empties the account", pos)
		}
	})
}

// Nothing may ask the server to delete the mailbox itself. The server refuses
// since 2.3.52, but a suite that asks is a defect on its own: the refusal
// arrives in a server log, while this fires in the run that wrote the call.
func TestNothingAsksToDeleteTheMailboxRoot(t *testing.T) {
	eachCall(t, func(pos token.Position, name string, args []ast.Expr) {
		if name != "deleteFolder" || len(args) != 1 {
			return
		}
		if rootLiteral(args[0]) {
			t.Errorf("%s: deleteFolder names the mailbox root — on maildir that is the account, not a folder in it", pos)
		}
	})
}

// The result of a cleanup has to be looked at. Discarding it is what let a
// cleanup that deleted the wrong thing keep reporting success, and it is why
// the destruction in #1063 took a manual isolation to attribute.
func TestCleanupResultsAreNotDiscarded(t *testing.T) {
	fset := token.NewFileSet()
	var checked int
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			stmt, ok := n.(*ast.ExprStmt)
			if !ok {
				return true
			}
			call, ok := stmt.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			checked++
			switch sel.Sel.Name {
			case "deleteFolder", "deleteUIDs", "removeSeeded":
				t.Errorf("%s: %s is called as a statement, so its error goes nowhere",
					fset.Position(call.Pos()), sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if checked < 20 {
		t.Errorf("inspected only %d discarded calls; the search is no longer finding the source", checked)
	}
}
