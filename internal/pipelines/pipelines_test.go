package pipelines

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The master and develop lines must run the same gate on the same diff.
//
// A PR into develop used to run nothing at all, and was green because nothing
// had run. Adding the trigger fixes that once; keeping the two path lists equal
// is what stops them drifting afterwards, and drift here is invisible: both
// pipelines stay green while answering differently about the same change.
//
// The lists are compared, not the workflows: everything else about the two is
// meant to differ -- develop publishes no git tag, no release and nothing to
// Docker Hub.
func TestBothLinesGateTheSameChanges(t *testing.T) {
	master := prTrigger(t, "ci.yml")
	develop := prTrigger(t, "ci-develop.yml")

	if !reflect.DeepEqual(master.Paths, develop.Paths) {
		t.Errorf("the two lines gate different paths, so the same diff gets a check on one and not the other\n  ci.yml:         %v\n  ci-develop.yml: %v",
			master.Paths, develop.Paths)
	}
	if len(master.Paths) == 0 {
		t.Error("ci.yml declares no pull_request paths; this guard is reading a file that changed shape")
	}
	if len(develop.Branches) != 1 || develop.Branches[0] != "develop" {
		t.Errorf("ci-develop.yml gates pull requests into %v, want [develop]", develop.Branches)
	}
}

type trigger struct {
	Branches []string `yaml:"branches"`
	Paths    []string `yaml:"paths"`
}

func prTrigger(t *testing.T, name string) trigger {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	// "on" is parsed by YAML 1.1 rules as the boolean true, which is why the
	// key is read from a map of any rather than a struct tag.
	var doc map[any]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	on, ok := doc[true].(map[string]any)
	if !ok {
		if on, ok = doc["on"].(map[string]any); !ok {
			t.Fatalf("%s has no on: block this guard can read", name)
		}
	}
	pr, ok := on["pull_request"]
	if !ok {
		t.Fatalf("%s has no pull_request trigger, so a PR against it runs nothing and is green because nothing ran", name)
	}
	raw, err := yaml.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	var out trigger
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
