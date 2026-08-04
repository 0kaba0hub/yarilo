package helmchart

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// A key declared in values.yaml and never rendered into the ConfigMap is not a
// setting. It is documentation of a setting that does not exist: an operator
// sets it, the deploy is green, the pod's yarilo.yaml has no such key, and the
// binary uses its built-in default. Nothing reports that the value was ignored.
//
// fts_flatcurve_prefix_search shipped that way in #1054 and was found only
// because a live gate produced identical results with the setting at two
// different values — which reads as a setting that does not affect search
// rather than one that never arrives (#1059).
//
// The comparison is textual rather than a chart render: it needs no helm
// binary, so it runs wherever the unit tests do rather than only where the
// lint job does.
func TestEveryConfigKeyIsRendered(t *testing.T) {
	root := filepath.Join("..", "..")
	values := readValues(t, filepath.Join(root, "helm", "values.yaml"))
	// Every template, not only the ConfigMap: a few settings reach the pod
	// through a Secret or an env var instead, and a guard that looked at one
	// file would call those unrendered.
	templates := readTemplates(t, filepath.Join(root, "helm", "templates"))

	var missing []string
	var checked int
	for _, key := range configKeys(values) {
		checked++
		if !strings.Contains(templates, key.leaf) {
			missing = append(missing, key.path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("declared in values.yaml and never rendered into the ConfigMap:\n    %s\n"+
			"Each is a key an operator can set with no effect and no warning.",
			strings.Join(missing, "\n    "))
	}

	// A guard that finds nothing to guard has stopped guarding: rename a file
	// or move the chart and this would pass over an empty set.
	if checked < 100 {
		t.Errorf("checked only %d keys; the search is no longer finding the chart's settings", checked)
	}
}

type configKey struct {
	// path is section.key, for the error message.
	path string
	// leaf is the name as it appears in the rendered file.
	leaf string
}

// configKeys returns the leaf keys that look like yarilo config settings:
// snake_case names carrying their section's prefix, which is the convention
// this repo uses for everything that reaches yarilo.yaml.
//
// Kubernetes plumbing — image, resources, replicas, nodeSelector — is
// camelCase or unprefixed and is not config, so the shape of the name is what
// separates the two.
func configKeys(values map[string]any) []configKey {
	var out []configKey
	snake := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)+$`)

	var walk func(prefix string, node any)
	walk = func(prefix string, node any) {
		m, ok := node.(map[string]any)
		if !ok {
			return
		}
		for name, child := range m {
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			if sub, ok := child.(map[string]any); ok {
				walk(path, sub)
				continue
			}
			if snake.MatchString(name) {
				out = append(out, configKey{path: path, leaf: name})
			}
		}
	}
	walk("", values)
	return out
}

// readTemplates concatenates every template so a key rendered anywhere counts.
func readTemplates(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b.WriteString(readFile(t, path))
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return b.String()
}

func readValues(t *testing.T, path string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
