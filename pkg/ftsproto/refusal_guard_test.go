package ftsproto

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Once a code is read by position, a refusal written by hand is not a style
// problem: a tab inside its text shifts the fields, and arbitrary error text
// becomes a code or swallows the message after it. So every NO is built in one
// place, and this fails the build if a second one appears.
//
// The same shape as the escaping guard on the ring events (#1392): the rule is
// not "remember to escape" -- it is "there is one writer, and it escapes".
func TestEveryRefusalGoesThroughTheBuilder(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			// The builder itself is where the reply is assembled.
			if f == "server.go" && strings.HasPrefix(trimmed, "return replyNO + ") {
				continue
			}
			if strings.Contains(trimmed, `"NO\t`) || strings.Contains(trimmed, "replyNO + ") {
				t.Errorf("%s:%d builds a refusal outside the builder: %s\n"+
					"use no() or noFor(), which flatten the text so a tab cannot become a code", f, i+1, trimmed)
			}
		}
	}
}
