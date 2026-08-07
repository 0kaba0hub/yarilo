package mailbox

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The path builders must carry no normalisation flag. NFC is applied once, at
// the name-entry boundary (NormalizeName); a bool parameter here is an
// invitation to normalise again, which is the two-owner arrangement whose order
// #1078, #1092 and #1113 each got wrong in turn. The signature is the guardrail:
// there must be nothing to pass.
func TestFolderSubpathBuildersTakeNoNormalisationFlag(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "folderpath.go", nil, 0)
	if err != nil {
		t.Fatalf("parse folderpath.go: %v", err)
	}

	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			return true
		}
		if fn.Name.Name == "FolderSubpathForm" {
			t.Errorf("%s reintroduced: the NFC-carrying builder is exactly the second owner this file removed", fn.Name.Name)
		}
		for _, p := range fn.Type.Params.List {
			id, ok := p.Type.(*ast.Ident)
			if !ok || id.Name != "bool" {
				continue
			}
			for _, name := range p.Names {
				t.Errorf("%s takes a bool %q — a path builder must not decide the name form (#1113)", fn.Name.Name, name.Name)
			}
		}
		return true
	})
}
