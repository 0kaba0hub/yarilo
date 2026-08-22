package ftsservice

import (
	"os"
	"strings"
	"testing"
)

// Every caller of handle() must release it: the idle sweeper closes a handle
// only at zero in-use, so one missing release pins a user's write lock open
// for the life of the process -- which is the defect the sweeper exists to fix
// (#1396), reintroduced one call site at a time.
//
// Read from the source because what this guards is a call site nobody has
// written yet.
func TestEveryHandleCallerReleasesIt(t *testing.T) {
	body, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	lines := strings.Split(string(body), "\n")

	var offenders []string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "s.handle(") || strings.HasPrefix(trimmed, "//") ||
			strings.HasPrefix(trimmed, "func ") {
			continue
		}
		// The release may be a few lines down, past the error check.
		released := false
		for _, next := range lines[i:min(i+10, len(lines))] {
			if strings.Contains(next, "s.release(h)") {
				released = true
				break
			}
		}
		if !released {
			offenders = append(offenders, trimmed)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("these take a user handle and never release it, so its write lock is never freed:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
