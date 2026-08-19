package locks_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every long-running component must dial the lock service through
// NewClientWaiting, because a component that starts before it is ordinary and
// exiting on that costs a restart -- and spends the RESTARTS counter a rollout
// window is judged by (#1350).
//
// This is a structural check because the defect was structural: the first
// attempt wired three binaries under app/ and missed internal/backend, from
// which the session processes start. Nothing failed; the components that were
// missed simply kept the old behaviour, and QA found it in a window (#1353).
// A test that walks the tree fails on the next file instead.
func TestEveryComponentWaitsForTheLockService(t *testing.T) {
	// Run from pkg/locks, so the repository root is two levels up.
	root := filepath.Join("..", "..")

	// yarilo-migrate is a one-shot operator command, not a component: an
	// operator running it while the lock service is down wants to be told so
	// immediately, not to have their terminal wait half a minute.
	allowed := map[string]bool{
		filepath.Join("app", "yarilo-migrate", "guidbackfill.go"): true,
	}

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		// The implementation itself is where NewClient lives.
		if strings.HasPrefix(rel, filepath.Join("pkg", "locks")) || allowed[rel] {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(body), "locks.NewClient(") {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("these dial the lock service without waiting for it, so they exit when it is a moment late:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
