package imap

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plumbing are the session methods that answer no client request: they carry
// no command a client could have issued, so timing them would invent one.
var plumbing = map[string]bool{
	"SessionID":              true,
	"Close":                  true,
	"AuthenticateMechanisms": true,
}

// Every command a client can issue must be timed. The wrapper does not embed
// the session precisely so a forgotten method breaks the build -- but a method
// added to the wrapper without its timer would compile and silently go
// unmeasured, and this is the row that catches that.
//
// It reads the package rather than a list: a list is a second place to
// remember, and the whole point of measuring at one seam is that there is only
// one place to look.
func TestEveryCommandIsTimed(t *testing.T) {
	commands := sessionMethods(t)
	timed := timedMethods(t)

	for name := range commands {
		if plumbing[name] {
			continue
		}
		if !timed[name] {
			t.Errorf("session.%s is a command with no timing: it will run and never be measured", name)
		}
	}
	for name := range timed {
		if !commands[name] {
			t.Errorf("timedSession.%s times a method the session no longer has", name)
		}
	}
}

// sessionMethods are the exported methods on *session, across the package.
func sessionMethods(t *testing.T) map[string]bool {
	t.Helper()
	return methodsOn(t, "session", func(string) bool { return true })
}

// timedMethods are the wrapper's methods that actually record a sample.
func timedMethods(t *testing.T) map[string]bool {
	t.Helper()
	return methodsOn(t, "timedSession", func(body string) bool {
		return strings.Contains(body, "t.observe(")
	})
}

func methodsOn(t *testing.T, receiver string, keep func(body string) bool) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		f, err := parser.ParseFile(fset, e.Name(), src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || !fn.Name.IsExported() {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			ident, ok := star.X.(*ast.Ident)
			if !ok || ident.Name != receiver {
				continue
			}
			// Offsets within this file, not positions in the file set: the
			// two differ once more than one file has been parsed.
			start := fset.Position(fn.Pos()).Offset
			end := fset.Position(fn.End()).Offset
			body := string(src[start:end])
			if keep(body) {
				out[fn.Name.Name] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("found no methods on *%s — the reader is broken, not the code", receiver)
	}
	return out
}
