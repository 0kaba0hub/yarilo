package toolchain

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// go.mod and the base image must name one toolchain: the scan reads go.mod,
// the image ships the base. They disagreed silently, and both numbers were
// right about different toolchains (#1497).
func TestGoModAndDockerfileNameTheSameToolchain(t *testing.T) {
	root := filepath.Join("..", "..")

	gomod := readFile(t, filepath.Join(root, "go.mod"))
	m := regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)$`).FindStringSubmatch(gomod)
	if m == nil {
		t.Fatal("go.mod has no `go X.Y.Z` line; an unpinned patch version is what this guards against")
	}
	want := m[1]

	stages := golangStages(t, readFile(t, filepath.Join(root, "docker", "Dockerfile")))
	if len(stages) == 0 {
		t.Fatal("no golang base image found in docker/Dockerfile; this guard is watching a file that moved")
	}
	for _, st := range stages {
		if version := strings.TrimSuffix(st.tag, "-alpine"); version != want {
			t.Errorf("stage %q builds on golang:%s, go.mod says %s", st.name, st.tag, want)
		}
		// Per stage, because ENV does not cross a FROM: without this the fts
		// stage -- which builds a binary we ship -- could lose the line while
		// the builder stage kept it, and the check would still pass.
		if !strings.Contains(st.body, "ENV GOTOOLCHAIN=local") {
			t.Errorf("stage %q does not set GOTOOLCHAIN=local, so a newer go.mod fetches a toolchain instead of failing the build", st.name)
		}
	}
}

type dockerStage struct {
	name string
	tag  string
	body string
}

// golangStages splits the Dockerfile at each FROM and returns the golang ones,
// each with the text that belongs to it alone.
func golangStages(t *testing.T, dockerfile string) []dockerStage {
	t.Helper()
	from := regexp.MustCompile(`(?m)^FROM (\S+)(?: AS (\S+))?`)
	locs := from.FindAllStringSubmatchIndex(dockerfile, -1)
	var out []dockerStage
	for i, loc := range locs {
		end := len(dockerfile)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		image := dockerfile[loc[2]:loc[3]]
		if !strings.HasPrefix(image, "golang:") {
			continue
		}
		name := image
		if loc[4] >= 0 {
			name = dockerfile[loc[4]:loc[5]]
		}
		out = append(out, dockerStage{
			name: name,
			tag:  strings.TrimPrefix(image, "golang:"),
			body: dockerfile[loc[0]:end],
		})
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}
