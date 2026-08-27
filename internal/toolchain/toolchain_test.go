package toolchain

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// go.mod and the Dockerfile must name the same Go patch version.
//
// They did not: go.mod said 1.26.2 while the Dockerfile built from
// golang:1.26-alpine, which floats. So govulncheck, which reads go.mod,
// reported 15 called standard-library vulnerabilities while the shipped binary
// was built by go1.26.7 and had every one of them fixed (#1497). Neither number
// was wrong; they were about different toolchains, and nothing said so.
//
// A scan is only a statement about what we ship while these two agree, and they
// agree by accident unless something checks.
func TestGoModAndDockerfileNameTheSameToolchain(t *testing.T) {
	root := filepath.Join("..", "..")

	gomod := readFile(t, filepath.Join(root, "go.mod"))
	m := regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)$`).FindStringSubmatch(gomod)
	if m == nil {
		t.Fatal("go.mod has no `go X.Y.Z` line; an unpinned patch version is what this guards against")
	}
	want := m[1]

	dockerfile := readFile(t, filepath.Join(root, "docker", "Dockerfile"))
	bases := regexp.MustCompile(`(?m)^FROM golang:([^ ]+) AS (\S+)`).FindAllStringSubmatch(dockerfile, -1)
	if len(bases) == 0 {
		t.Fatal("no golang base image found in docker/Dockerfile; this guard is watching a file that moved")
	}
	for _, b := range bases {
		tag, stage := b[1], b[2]
		version := strings.TrimSuffix(tag, "-alpine")
		if version != want {
			t.Errorf("stage %q builds on golang:%s, go.mod says %s — a scan that reads go.mod would describe a toolchain we do not ship",
				stage, tag, want)
		}
	}

	// The pin is only real while nothing is allowed to download past it.
	for _, b := range bases {
		if !strings.Contains(dockerfile, "ENV GOTOOLCHAIN=local") {
			t.Errorf("stage %q is pinned but GOTOOLCHAIN is not local, so a newer toolchain can be fetched mid-build", b[2])
			break
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}
