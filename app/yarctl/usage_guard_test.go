package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The usage text a command prints is what an operator reads when they type the
// command with no arguments. It is therefore a contract -- and until this row
// existed, nothing kept it equal to the dispatcher.
//
// It had drifted: `yarctl backend acl` never mentioned rebuild, materialise or
// registry, which exist and work; `yarctl backend` never listed the mailbox
// service at all. The public command reference was written FROM that output,
// so the gap propagated: a source checked against itself proves nothing
// (yarilomail/yarilo#1468).
//
// Both directions, as everywhere else. Every dispatcher branch must appear in
// the text; every command the text offers must exist as a branch. One
// direction catches a command nobody documented, the other catches a command
// that was removed and still advertised.
func TestUsageTextMatchesTheDispatcher(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(file)
		if rerr != nil {
			t.Fatal(rerr)
		}
		cases := dispatchCases(t, file, string(src))
		usage := usageCommands(string(src))
		switch {
		case len(cases) == 0 && len(usage) == 0:
			continue // neither half: not a command file
		case len(usage) == 0:
			// A dispatcher whose usage text lives elsewhere is normal; say so
			// rather than deciding silently, so a text that was deleted is
			// visible in the run.
			t.Logf("%s: dispatch branches with no usage text in this file", file)
			continue
		case len(cases) == 0:
			// The dangerous half: a usage text whose dispatcher this guard
			// could not find -- a renamed dispatch function takes its file out
			// of scope, and the check would go quiet rather than fail.
			t.Errorf("%s: a usage text with no dispatch* function in this file -- if the dispatcher was renamed, this file just left the guard's scope",
				file)
			continue
		}
		checked++

		for name := range cases {
			if !usage[name] {
				t.Errorf("%s: the dispatcher accepts %q and the usage text never mentions it -- an operator who types the command with no arguments is told it does not exist",
					file, name)
			}
		}
		for name := range usage {
			if !cases[name] {
				t.Errorf("%s: the usage text offers %q and the dispatcher has no such branch -- it will answer 'unknown command'",
					file, name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no file carried both a dispatcher and a usage text; this guard proved nothing")
	}
}

// dispatchCases collects the command names a file's switch statements accept.
// Read from the syntax tree rather than by pattern, so a string that merely
// looks like a case does not count as one.
func dispatchCases(t *testing.T, file, src string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	// Output formats and flag values are switched on too; they are not
	// commands and belong to no usage list of commands.
	notACommand := map[string]bool{
		"human": true, "json": true, "table": true, "yaml": true,
		"true": true, "false": true, "on": true, "off": true,
		"yes": true, "no": true, "1": true, "0": true,
		"sha1": true, "sha256": true,
	}
	out := map[string]bool{}
	// Only the command dispatchers. Other switches in these files decide
	// output formats, SCRAM mechanisms and status strings -- reading those as
	// commands is how a checker starts crying wolf, and a checker nobody
	// believes is worse than none.
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fn.Name.Name, "dispatch") {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				name := strings.Trim(lit.Value, `"`)
				if name == "" || notACommand[name] || !commandName.MatchString(name) {
					continue
				}
				out[name] = true
			}
			return true
		})
	}
	return out
}

var commandName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// usageCommands reads the command names out of a file's usage text: the first
// word of each indented line under a "Commands:" or "Services:" heading.
func usageCommands(src string) map[string]bool {
	out := map[string]bool{}
	for _, block := range regexp.MustCompile("`([^`]*)`").FindAllStringSubmatch(src, -1) {
		var inList bool
		for _, line := range strings.Split(block[1], "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case listHeading(trimmed):
				inList = true
				continue
			case trimmed == "":
				continue
			case !strings.HasPrefix(line, "  "):
				inList = false
				continue
			}
			if !inList {
				continue
			}
			// A command entry sits at the list's own indent and its name is
			// followed by two or more spaces (or nothing). A wrapped
			// description line is indented further, and reading its first word
			// as a command is how this check produced twenty findings and no
			// truth on its first run.
			if !entryLine.MatchString(line) {
				continue
			}
			// An example invocation is not an entry. Recognised by shape --
			// it starts with the binary's own name -- rather than by
			// excluding an "Examples:" heading, which would be one more
			// section name to remember.
			if strings.HasPrefix(trimmed, "yarctl ") {
				continue
			}
			// An entry starts with the command's own name, so a line that
			// opens with punctuation is prose -- a parenthesised note under
			// "Usage:", for instance. Shape again, not another exclusion list.
			if !startsWithCommandWord.MatchString(trimmed) {
				continue
			}
			// Every leading command word, not just the first: an entry may
			// name a subtree ("backends add IP --port PORT"), and both words
			// are branches the dispatcher takes.
			for _, word := range strings.Fields(trimmed) {
				if strings.HasPrefix(word, "<") || strings.HasPrefix(word, "[") ||
					strings.HasPrefix(word, "-") || !commandName.MatchString(strings.Trim(word, "(),|/")) {
					break
				}
				for _, name := range strings.FieldsFunc(word, func(r rune) bool {
					return r == '(' || r == ')' || r == ',' || r == '|' || r == '/'
				}) {
					if commandName.MatchString(name) {
						out[name] = true
					}
				}
			}
			if alias := aliasIn(trimmed); alias != "" {
				out[alias] = true
			}
		}
	}
	return out
}

// entryLine matches a top-level command entry by its indent. A wrapped
// description line is indented past the name column, which is what separates
// the two -- and reading a continuation as a command is how this check
// produced twenty findings and no truth on its first run.
var entryLine = regexp.MustCompile(`^ {2,3}\S`)

// listHeading recognises the headings a usage text puts its command list
// under. Commands and services are the common two; the top-level text also
// groups by plane and by shorthand, and a heading this function does not know
// is a list the guard silently skips -- so it is written to be extended when a
// new one appears rather than to guess.
// A heading is any capitalised phrase ending in a colon -- Commands:,
// Services:, Planes:, "Shorthand (no plane prefix):". Recognised by shape
// rather than by a list of known ones: a list is a place somebody has to
// remember to extend, and the section that gets added without being added
// here is precisely the one nobody would look at.
func listHeading(line string) bool {
	return headingShape.MatchString(line)
}

var startsWithCommandWord = regexp.MustCompile(`^[a-z]`)

var headingShape = regexp.MustCompile(`^[A-Z][A-Za-z]+[^:]*:$`)

var aliasPattern = regexp.MustCompile(`alias:\s*([a-z][a-z0-9-]*)`)

func aliasIn(line string) string {
	if m := aliasPattern.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}
