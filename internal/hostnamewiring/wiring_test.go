package hostnamewiring

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// submission's hostname overrides submission and nothing else.
//
// It used to leak: the backend's LMTP server, the director's LMTP proxy and
// yarilo-lmtp-login all took protocol.submission.hostname, so a deployment that
// named its submission service renamed its LHLO banner, its Received header and
// the domain part of every Message-ID it wrote (#1506). Nothing in the config
// said those were one setting, and nothing in the output said they had moved.
//
// A source guard because the wiring is what is being asserted: every spelling
// compiles, every one produces a name, and only the name differs.
func TestOnlySubmissionTakesSubmissionsHostname(t *testing.T) {
	root := filepath.Join("..", "..")
	// Where an LMTP server or proxy is constructed. Each must name the
	// installation.
	lmtpSites := []string{
		filepath.Join(root, "internal", "backend", "backend.go"),
		filepath.Join(root, "app", "yarilo-director", "main.go"),
		filepath.Join(root, "app", "yarilo-lmtp-login", "main.go"),
	}
	optsHostname := regexp.MustCompile(`Hostname:\s*(cfg\.[A-Za-z.()]+)`)

	var checked int
	for _, path := range lmtpSites {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range optsHostname.FindAllStringSubmatch(string(src), -1) {
			checked++
			if strings.Contains(m[1], "Submission") {
				t.Errorf("%s takes %s for an LMTP server: submission's key would rename the banner, "+
					"the Received header and every synthesised Message-ID", filepath.Base(path), m[1])
			}
		}
	}
	// A guard that finds nothing to guard has stopped guarding.
	if checked < 2 {
		t.Errorf("found %d Hostname assignments across the LMTP sites; this guard is reading files that moved", checked)
	}
}
