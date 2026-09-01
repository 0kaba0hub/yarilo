// commentcensus lists the multi-line comment blocks in the tree and measures
// what share of each package is comment.
//
// It is a work list and a measurement, nothing else: it never edits a file and
// never says what a block should become. That judgment is the pass's, one block
// at a time, and a tool that guessed would be exactly the mechanical stripping
// that was considered and rejected (#1620).
//
// What it will not put on the list, because those blocks are not prose:
// build and generate directives, lint pragmas, the cgo preamble that must sit
// against import "C", and a licence header. Listing them would send a reader to
// look at something they must not touch.
//
// Usage:
//
//	go run ./hack/commentcensus              # per-package ratios, worst first
//	go run ./hack/commentcensus -blocks      # the work list, every block of 3+
//	go run ./hack/commentcensus -json        # the same numbers for a gate check
//	go run ./hack/commentcensus -pkg internal/storage/...
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Block is one comment block the pass has to read.
type Block struct {
	Package string `json:"package"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Lines   int    `json:"lines"`
	First   string `json:"first"`
}

// PackageCensus is one package's measurement.
type PackageCensus struct {
	Package      string  `json:"package"`
	GoLines      int     `json:"go_lines"`
	CommentLines int     `json:"comment_lines"`
	Ratio        float64 `json:"ratio"`
	Blocks       int     `json:"blocks"`
	BlockLines   int     `json:"block_lines"`
}

func main() {
	var (
		root      = flag.String("root", ".", "tree to walk")
		pkgGlob   = flag.String("pkg", "", "only packages whose path matches this prefix")
		minLines  = flag.Int("min", 3, "a block is this many lines or more")
		showBlock = flag.Bool("blocks", false, "print the work list instead of the summary")
		asJSON    = flag.Bool("json", false, "print the summary as JSON")
		check     = flag.Bool("check", false, "fail when a package is over what it is allowed")
		update    = flag.Bool("update-baseline", false, "write the baseline from the tree as it stands")
		maxRatio  = flag.Float64("max", 0.10, "the share a swept package may not exceed")
		baseline  = flag.String("baseline", "hack/commentcensus/baseline.json", "packages not swept yet, and what they hold today")
	)
	flag.Parse()

	census, blocks, err := walk(*root, *pkgGlob, *minLines)
	if err != nil {
		fmt.Fprintln(os.Stderr, "commentcensus:", err)
		os.Exit(1)
	}

	switch {
	case *update:
		if err := writeBaseline(*baseline, census, *maxRatio); err != nil {
			fmt.Fprintln(os.Stderr, "commentcensus:", err)
			os.Exit(1)
		}
	case *check:
		if !runCheck(*baseline, census, *maxRatio) {
			os.Exit(1)
		}
	case *showBlock:
		printBlocks(blocks)
	case *asJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(census); err != nil {
			fmt.Fprintln(os.Stderr, "commentcensus:", err)
			os.Exit(1)
		}
	default:
		printSummary(census)
	}
}

func walk(root, pkgPrefix string, minLines int) ([]PackageCensus, []Block, error) {
	byPkg := map[string]*PackageCensus{}
	var blocks []Block

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", ".git", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		pkg := filepath.ToSlash(filepath.Dir(path))
		pkg = strings.TrimPrefix(pkg, "./")
		if pkgPrefix != "" && !strings.HasPrefix(pkg, strings.TrimSuffix(pkgPrefix, "...")) {
			return nil
		}
		fileBlocks, goLines, commentLines, ferr := readFile(path, minLines)
		if ferr != nil {
			return ferr
		}
		c := byPkg[pkg]
		if c == nil {
			c = &PackageCensus{Package: pkg}
			byPkg[pkg] = c
		}
		c.GoLines += goLines
		c.CommentLines += commentLines
		for _, b := range fileBlocks {
			b.Package = pkg
			blocks = append(blocks, b)
			c.Blocks++
			c.BlockLines += b.Lines
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	out := make([]PackageCensus, 0, len(byPkg))
	for _, c := range byPkg {
		if c.GoLines > 0 {
			c.Ratio = float64(c.CommentLines) / float64(c.GoLines)
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ratio != out[j].Ratio {
			return out[i].Ratio > out[j].Ratio
		}
		return out[i].Package < out[j].Package
	})
	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].Lines != blocks[j].Lines {
			return blocks[i].Lines > blocks[j].Lines
		}
		if blocks[i].File != blocks[j].File {
			return blocks[i].File < blocks[j].File
		}
		return blocks[i].Line < blocks[j].Line
	})
	return out, blocks, nil
}

func readFile(path string, minLines int) ([]Block, int, int, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, 0, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		// A file this parser cannot read is counted for its lines and left off
		// the work list: the census must not stop at one bad file, and a block
		// nobody can locate is not work anybody can do.
		return nil, countLines(src), 0, nil
	}
	goLines := countLines(src)

	cgoPreamble := cgoPreambleGroup(f)
	commentLines := 0
	var blocks []Block
	for _, g := range f.Comments {
		n := groupLines(fset, g)
		commentLines += n
		if n < minLines || untouchable(g, cgoPreamble) {
			continue
		}
		pos := fset.Position(g.Pos())
		blocks = append(blocks, Block{
			File:  filepath.ToSlash(path),
			Line:  pos.Line,
			Lines: n,
			First: firstLine(g),
		})
	}
	return blocks, goLines, commentLines, nil
}

// groupLines counts the physical lines a comment group occupies, so a /* */
// block counts what it actually costs a reader.
func groupLines(fset *token.FileSet, g *ast.CommentGroup) int {
	return fset.Position(g.End()).Line - fset.Position(g.Pos()).Line + 1
}

// cgoPreambleGroup returns the comment group that must stay attached to
// import "C", or nil. Moving or trimming it changes what cgo compiles.
func cgoPreambleGroup(f *ast.File) *ast.CommentGroup {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		for _, spec := range gd.Specs {
			is, ok := spec.(*ast.ImportSpec)
			if !ok || is.Path == nil || is.Path.Value != `"C"` {
				continue
			}
			if is.Doc != nil {
				return is.Doc
			}
			if gd.Doc != nil {
				return gd.Doc
			}
		}
	}
	return nil
}

// untouchable reports whether a block is one the pass must not touch: it is
// machinery or licence, not prose about the code.
func untouchable(g, cgoPreamble *ast.CommentGroup) bool {
	if g == cgoPreamble {
		return true
	}
	for _, c := range g.List {
		t := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
		switch {
		case strings.HasPrefix(t, "go:"), // build, generate, embed, linkname
			strings.HasPrefix(t, "nolint"),
			strings.HasPrefix(t, "export "),
			strings.HasPrefix(t, "+build"),
			strings.HasPrefix(t, "Code generated "):
			return true
		}
		lower := strings.ToLower(t)
		if strings.HasPrefix(lower, "copyright") || strings.HasPrefix(lower, "spdx-license-identifier") {
			return true
		}
	}
	return false
}

func firstLine(g *ast.CommentGroup) string {
	if len(g.List) == 0 {
		return ""
	}
	t := g.List[0].Text
	t = strings.TrimPrefix(t, "//")
	t = strings.TrimPrefix(t, "/*")
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

func countLines(src []byte) int {
	if len(src) == 0 {
		return 0
	}
	n := strings.Count(string(src), "\n")
	if !strings.HasSuffix(string(src), "\n") {
		n++
	}
	return n
}

func printSummary(census []PackageCensus) {
	var goLines, comment, blocks, blockLines int
	fmt.Printf("%-58s %8s %8s %6s %7s %8s\n", "package", "go", "comment", "share", "blocks", "in blocks")
	for _, c := range census {
		goLines += c.GoLines
		comment += c.CommentLines
		blocks += c.Blocks
		blockLines += c.BlockLines
		fmt.Printf("%-58s %8d %8d %5.1f%% %7d %8d\n",
			c.Package, c.GoLines, c.CommentLines, c.Ratio*100, c.Blocks, c.BlockLines)
	}
	share := 0.0
	if goLines > 0 {
		share = float64(comment) / float64(goLines) * 100
	}
	fmt.Printf("\n%-58s %8d %8d %5.1f%% %7d %8d\n", "TOTAL", goLines, comment, share, blocks, blockLines)
}

func printBlocks(blocks []Block) {
	for _, b := range blocks {
		fmt.Printf("%s:%d\t%d\t%s\n", b.File, b.Line, b.Lines, b.First)
	}
}

// The guard, and why it needs a baseline at all.
//
// The tree is at 22% today and the target is 10%, so a bare threshold would
// fail every package at once and be switched off within the hour. The baseline
// records what each unswept package holds now: a package may not exceed what it
// already had, and once a batch brings it to the threshold it leaves the
// baseline and the threshold alone holds it. So the number can only go down,
// a new essay fails immediately in a swept package, and in an unswept one it
// fails as soon as it makes that package worse than the day the guard landed
// (#1620).

// runCheck reports whether every package is within what it is allowed.
func runCheck(path string, census []PackageCensus, maxRatio float64) bool {
	allowed, err := readBaseline(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "commentcensus:", err)
		return false
	}
	ok := true
	for _, c := range census {
		if c.GoLines == 0 {
			continue
		}
		limit := maxRatio
		note := ""
		if b, found := allowed[c.Package]; found {
			limit = b
			note = " (not swept yet; it may not grow)"
		}
		if c.Ratio > limit+1e-9 {
			fmt.Printf("%s: %.1f%% of %d lines is comment, over %.1f%%%s\n",
				c.Package, c.Ratio*100, c.GoLines, limit*100, note)
			ok = false
		}
	}
	if !ok {
		fmt.Println("\nA comment block that is not an invariant belongs in the issue it came from.")
		fmt.Println("If a package was swept, drop its line from the baseline instead of raising it.")
	}
	return ok
}

func readBaseline(path string) (map[string]float64, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]float64{}, nil
		}
		return nil, err
	}
	var out map[string]float64
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("baseline %s: %w", path, err)
	}
	return out, nil
}

// writeBaseline records every package still over the threshold. A package at or
// under it is left out on purpose: from then on the threshold is what holds it.
func writeBaseline(path string, census []PackageCensus, maxRatio float64) error {
	out := map[string]float64{}
	for _, c := range census {
		if c.GoLines > 0 && c.Ratio > maxRatio {
			out[c.Package] = roundUp(c.Ratio)
		}
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644) //nolint:gosec // a checked-in baseline
}

// roundUp keeps the baseline readable and gives a package a hair of room, so a
// one-line change to a small package is not a gate failure by itself.
func roundUp(r float64) float64 {
	return float64(int(r*1000)+1) / 1000
}
