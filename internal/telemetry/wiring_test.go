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

// site is one telemetry.Options literal found in the tree.
type site struct {
	path string
	line int
	lit  *ast.CompositeLit
	// addrIdents are the identifiers in the same file that were assigned from
	// telemetry.Addr(...), so `Addr: telemAddr` can be told apart from
	// `Addr: cfg.Listen`.
	addrIdents map[string]bool
}

// field returns the value assigned to a named field, and whether it was set.
func (s site) field(name string) (ast.Expr, bool) {
	for _, elt := range s.lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == name {
			return kv.Value, true
		}
	}
	return nil, false
}

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
	sites := telemetryOptionSites(t)
	assertEnoughSites(t, len(sites))

	for _, s := range sites {
		if _, ok := s.field("Pprof"); !ok {
			t.Errorf("%s:%d: telemetry.Options without Pprof — this component cannot be profiled, and nothing at the call site says so",
				s.path, s.line)
		}
	}
}

// Every component must resolve its telemetry address through telemetry.Addr,
// which is what honours TELEMETRY_LISTEN — the mechanism the chart uses to give
// each container in the co-located pod a distinct telemetry port, and the one
// at the centre of #1008.
//
// Eight components used to pass the configured value straight through and drop
// the variable on the floor (#1019). What makes that worth a guard and not only
// a fix is its shape: the bypass compiles, reads naturally, and is silent — an
// operator who sets the port sees it in the manifest and never learns that the
// binary did not listen.
func TestEveryComponentResolvesTheTelemetryAddress(t *testing.T) {
	sites := telemetryOptionSites(t)
	assertEnoughSites(t, len(sites))

	for _, s := range sites {
		addr, ok := s.field("Addr")
		if !ok {
			t.Errorf("%s:%d: telemetry.Options with no Addr", s.path, s.line)
			continue
		}
		if !resolvesThroughAddr(addr, s.addrIdents) {
			t.Errorf("%s:%d: Addr does not come from telemetry.Addr — TELEMETRY_LISTEN is silently ignored here",
				s.path, s.line)
		}
	}
}

// resolvesThroughAddr accepts telemetry.Addr(...) written in place, or an
// identifier this file assigned from it.
//
// Identifiers are followed rather than trusted. Accepting any identifier would
// have let `Addr: addr` through, which is exactly the form the eight broken
// components used — a guard that passes on the defect it exists for is worse
// than no guard, because it is also a claim that the defect cannot recur.
func resolvesThroughAddr(v ast.Expr, addrIdents map[string]bool) bool {
	switch e := v.(type) {
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "telemetry" && sel.Sel.Name == "Addr"
	case *ast.Ident:
		return addrIdents[e.Name]
	}
	return false
}

// telemetryOptionSites parses app/ and internal/ and returns every
// telemetry.Options literal built outside this package.
func telemetryOptionSites(t *testing.T) []site {
	t.Helper()

	var out []site
	for _, root := range []string{filepath.Join("..", "..", "app"), filepath.Join("..", "..", "internal")} {
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
			idents := addrDerivedIdents(file)
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isTelemetryOptions(lit.Type) {
					return true
				}
				out = append(out, site{
					path:       path,
					line:       fset.Position(lit.Pos()).Line,
					lit:        lit,
					addrIdents: idents,
				})
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return out
}

// addrDerivedIdents collects the names this file assigned from telemetry.Addr,
// including through a later reassignment (`if x == "" { x = ":8080" }` keeps the
// name derived; the fallback is still a resolved address).
func addrDerivedIdents(file *ast.File) map[string]bool {
	out := map[string]bool{}
	record := func(lhs []ast.Expr, rhs []ast.Expr) {
		for i, r := range rhs {
			if i >= len(lhs) || !resolvesThroughAddr(r, nil) {
				continue
			}
			if id, ok := lhs[i].(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			record(stmt.Lhs, stmt.Rhs)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, 0, len(stmt.Names))
			for _, name := range stmt.Names {
				lhs = append(lhs, name)
			}
			record(lhs, stmt.Values)
		}
		return true
	})
	return out
}

// assertEnoughSites fails when the search stops finding the call sites. A guard
// that finds nothing to guard is a guard that has stopped working: rename the
// type or move the components and these tests would keep passing while checking
// an empty set.
func assertEnoughSites(t *testing.T, n int) {
	t.Helper()
	if n < 10 {
		t.Fatalf("found only %d telemetry.Options literals across app/ and internal/; the search is no longer finding the call sites", n)
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
