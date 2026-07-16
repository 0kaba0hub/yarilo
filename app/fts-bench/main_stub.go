//go:build !flatcurve

package main

import (
	"fmt"
	"os"
)

// fts-bench links libxapian (flatcurve engine); the default static build
// carries a stub so `go build ./...` stays cgo-free. Build the real tool with
// -tags flatcurve (the fts image does this).
func main() {
	fmt.Fprintln(os.Stderr, "fts-bench: built without the flatcurve engine — rebuild with -tags flatcurve")
	os.Exit(1)
}
