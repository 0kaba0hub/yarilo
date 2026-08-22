package authwait

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A process that dials auth at startup and exits on failure turns a few
// seconds of dependency downtime into a restart loop (#1369). The waiting
// variants exist so that cannot happen -- and this guard is what keeps the
// next startup site from being written with the plain Dial.
//
// Read from the source, because what it protects against is a call site nobody
// has written yet.
func TestStartupSitesDialWithAWait(t *testing.T) {
	root := repoRoot(t)

	// The request-path sites, named with the reason rather than silently
	// skipped. At startup there is nobody to tell, so waiting is right; on a
	// request there IS a client waiting for an answer, and a fast refusal
	// beats a hang -- for a dependency that can be down for minutes, waiting
	// inside the request is worse than the error.
	exempt := map[string]string{
		"app/yarilo-jmap/main.go": "per-request user resolver: refuse fast, do not hang the request",
		"app/yarilo-fts/main.go":  "per-request user resolver: refuse fast, do not hang the request",
		// A one-shot operator command, run by a person who is watching. Waiting
		// thirty seconds before saying "auth is down" helps nobody: the
		// operator can see it, fix it and run the command again.
		"app/yarilo-migrate/guidbackfill.go": "one-shot operator command: tell the operator now, do not wait",
	}

	var offenders []string
	err := filepath.WalkDir(filepath.Join(root, "app"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if _, ok := exempt[rel]; ok {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			// A plain Dial, not DialWaiting / DialContext-with-wait.
			if strings.Contains(trimmed, "authclient.Dial(") {
				offenders = append(offenders, rel+":"+itoa(i+1)+": "+trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("these dial auth without a bounded wait; at startup that is a restart loop when auth is briefly down:\n  %s\n\nUse DialWaiting, or add the site to the exemption list with the reason.",
			strings.Join(offenders, "\n  "))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the working directory")
		}
		dir = parent
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
