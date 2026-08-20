package imagebuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up until it finds go.mod, so the guard does not depend on
// where the test binary is run from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's working directory")
		}
		dir = parent
	}
}

// spawnSites finds the packages that run external programs. They are the reason
// PID 1 has to reap: such a program can leave a grandchild behind, the orphan
// is reparented to PID 1, and Go reaps only its own children.
func spawnSites(t *testing.T, root string) []string {
	t.Helper()
	var sites []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(body), "exec.Command") {
			rel, _ := filepath.Rel(root, path)
			sites = append(sites, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return sites
}

// TestImageRunsAnInitAsPID1 ties the image's entrypoint to the reason it needs
// one. yarilo's binaries run external programs -- the director's flush hook,
// sieve pipe/filter/execute, the quota-warning script -- and a killed one
// leaves an orphaned grandchild. With a Go binary as PID 1 nothing reaps it:
// each one stayed as a zombie holding an entry against the pod's PID limit,
// behind a tidy "hook failed" log line (#1373).
//
// An in-process reaper is not the alternative. wait4(-1) races cmd.Wait for
// the status of a child it is waiting on, and losing that race turns a timeout
// into "no child processes" -- which would break the flush hook's own timeout
// classification (#1368).
//
// The guard reads both halves so removing the init while the spawn sites exist
// fails here, with the sites named.
func TestImageRunsAnInitAsPID1(t *testing.T) {
	root := repoRoot(t)
	sites := spawnSites(t, root)
	if len(sites) == 0 {
		t.Skip("nothing runs external programs any more; PID 1 has nothing to reap")
	}

	body, err := os.ReadFile(filepath.Join(root, "docker", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	src := string(body)

	var entrypoint string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ENTRYPOINT") {
			entrypoint = strings.TrimSpace(line)
		}
	}
	if entrypoint == "" {
		t.Fatal("the image has no ENTRYPOINT")
	}
	if !strings.Contains(entrypoint, "tini") {
		t.Errorf("PID 1 is not an init, but these packages run external programs whose orphans it must reap:\n  %s\nENTRYPOINT is %s",
			strings.Join(sites, "\n  "), entrypoint)
	}
	// Named in the ENTRYPOINT is not the same as present in the image: the
	// package has to be installed, or the container fails to start at all.
	installed := false
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "apk add") && strings.Contains(line, " tini") {
			installed = true
			break
		}
	}
	if !installed {
		t.Error("tini is in the ENTRYPOINT but no apk add installs it: the image would not start")
	}
}
