package jmapcore

import (
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// modulePath is this repository's module. Nothing under it may be imported
// here.
const modulePath = "github.com/yarilomail/yarilo"

// This package is meant to be extracted as a standalone library. One import of
// a yarilo package turns that split from a move into a refactor, which is why
// the rule is a test and not a convention.
func TestNoYariloImports(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			for _, imp := range f.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("%s: bad import %s", name, imp.Path.Value)
				}
				if path == modulePath || strings.HasPrefix(path, modulePath+"/") {
					t.Errorf("%s imports %s; jmapcore must stay free of yarilo", name, path)
				}
			}
		}
	}
}
