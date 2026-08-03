package telemetry

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every component must pass Pprof when it builds a telemetry server, or
// profiling is available on some pods and silently missing on others.
//
// The cost of unevenness is not the missing feature. It is somebody diagnosing
// yarilo-director in six months, reaching for /debug/pprof, getting a 404, and
// spending the afternoon working out whether the switch is off or the endpoint
// was never wired — a question this test makes impossible to ask.
//
// It reads the tree rather than trusting review because the omission compiles:
// a telemetry.Options literal without the field is valid Go with a zero value
// that means "off forever".
func TestEveryComponentWiresPprof(t *testing.T) {
	roots := []string{filepath.Join("..", "..", "app"), filepath.Join("..", "..", "internal")}

	var checked int
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			// This package builds the Options; it does not consume them.
			if filepath.Dir(path) == filepath.Join("..", "..", "internal", "telemetry") {
				return nil
			}

			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Errorf("%s: %v", path, perr)
				return nil
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isTelemetryOptions(lit.Type) {
					return true
				}
				checked++
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Pprof" {
						return true
					}
				}
				t.Errorf("%s:%d: telemetry.Options without Pprof — this component cannot be profiled, and nothing at the call site says so",
					path, fset.Position(lit.Pos()).Line)
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	// A guard that finds nothing to guard is a guard that has stopped working:
	// rename the type or move the components and this test would keep passing
	// while checking an empty set.
	if checked < 10 {
		t.Errorf("found only %d telemetry.Options literals across app/ and internal/; the search is no longer finding the call sites", checked)
	}
}

// isTelemetryOptions matches `telemetry.Options` composite literals.
func isTelemetryOptions(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Options" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "telemetry"
}
