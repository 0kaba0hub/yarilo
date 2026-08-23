package backendapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A control-file path resolved by hand is how two services come to write in
// different places, and the failure is silent: one protocol shows a
// subscription list the other does not (#1437).
//
// The rule lives in mailbox.ControlRoot. This fails the build if a package
// spells it out again -- checked by source, because what it guards against is
// code nobody has written yet.
func TestNobodyResolvesTheControlRootByHand(t *testing.T) {
	roots := map[string]string{
		"backendapi": ".",
		"imap":       "../imap",
		"jmap":       "../jmap",
		"lmtp":       "../lmtp",
		"userstate":  "../userstate",
	}
	for name, root := range roots {
		t.Run(name, func(t *testing.T) {
			err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
				if err != nil || fi.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return nil
				}
				body, rerr := os.ReadFile(path)
				if rerr != nil {
					return rerr
				}
				lines := strings.Split(string(body), "\n")
				for i, line := range lines {
					if !strings.Contains(line, "ControlDir != \"\"") {
						continue
					}
					// Assigning ControlDir from a userdb answer is a different
					// operation: it fills the field, it does not resolve a root.
					window := strings.Join(lines[i:min(i+4, len(lines))], "\n")
					if strings.Contains(window, "ui.ControlDir =") || strings.Contains(window, "ControlDir = expand") {
						continue
					}
					t.Errorf("%s:%d resolves the control root by hand: %s\n"+
						"call mailbox.ControlRoot -- two spellings of a path rule drift, and the symptom is one service not seeing what another wrote",
						path, i+1, strings.TrimSpace(line))
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
