package config

import (
	"reflect"
	"strings"
	"testing"
)

// Every field named *Alias exists to receive a pre-beta spelling, and is useless
// unless some alias set adopts it. Two defects in package 2a were exactly that
// shape: a field renamed by accident with no adopter, and a per-service ssl
// override whose aliases the fixed general.ssl paths never reached. In both
// cases the value looked set and did nothing — the class this whole mechanism
// exists to prevent.
//
// So the coverage is asserted mechanically rather than by review: collect every
// *Alias field by reflection, collect every alias the sets know how to adopt,
// and refuse a field that nobody claims.
func TestEveryAliasFieldHasAnAdopter(t *testing.T) {
	// A config with every optional block present, so per-block alias sets
	// (services.*.ssl) are built rather than skipped.
	cfg := &Config{}
	cfg.Services = ServicesConfig{
		IMAP: &ServiceConfig{SSL: &SSLConfig{}}, IMAPS: &ServiceConfig{SSL: &SSLConfig{}},
		Submission: &ServiceConfig{SSL: &SSLConfig{}}, Submissions: &ServiceConfig{SSL: &SSLConfig{}},
		POP3: &ServiceConfig{SSL: &SSLConfig{}}, POP3S: &ServiceConfig{SSL: &SSLConfig{}},
		LMTP: &ServiceConfig{SSL: &SSLConfig{}}, ManageSieve: &ServiceConfig{SSL: &SSLConfig{}},
		ManageSieveBE: &ServiceConfig{SSL: &SSLConfig{}}, JMAP: &ServiceConfig{SSL: &SSLConfig{}},
		JMAPBE: &ServiceConfig{SSL: &SSLConfig{}},
	}

	adopted := map[string]bool{}
	for _, set := range allAliasSets(cfg) {
		for _, key := range set {
			// The last path element is the key name; a field may be adopted
			// under several paths (one per service block).
			adopted[lastSegment(key.alias)] = true
		}
	}

	var orphans []string
	walkAliasFields(reflect.TypeOf(Config{}), "", func(path, koanfTag string) {
		if !adopted[koanfTag] {
			orphans = append(orphans, path+" (koanf:\""+koanfTag+"\")")
		}
	})
	if len(orphans) > 0 {
		t.Errorf("alias fields nobody adopts — a value written under these keys is read into a field and then ignored:\n  %s",
			strings.Join(orphans, "\n  "))
	}
}

// The counterpart: an alias set naming a key no field carries would adopt
// nothing, which is the same silence seen from the other side.
func TestEveryAdoptedAliasHasAField(t *testing.T) {
	cfg := &Config{Services: ServicesConfig{IMAP: &ServiceConfig{SSL: &SSLConfig{}}}}

	fields := map[string]bool{}
	walkAliasFields(reflect.TypeOf(Config{}), "", func(_, koanfTag string) { fields[koanfTag] = true })
	for _, set := range allAliasSets(cfg) {
		for _, key := range set {
			name := lastSegment(key.alias)
			if !fields[name] {
				t.Errorf("alias set adopts %q, but no *Alias field carries that key", key.alias)
			}
		}
	}
}

func allAliasSets(cfg *Config) [][]aliasedKey {
	return [][]aliasedKey{
		storageAliases(cfg), generalAliases(cfg), aclAliases(cfg),
		authAliases(cfg), serviceSSLAliases(cfg),
	}
}

func lastSegment(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

// walkAliasFields visits every field whose name ends in "Alias", anywhere in
// the config tree, and reports its dotted Go path and koanf tag.
func walkAliasFields(t reflect.Type, path string, visit func(path, koanfTag string)) {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		here := f.Name
		if path != "" {
			here = path + "." + f.Name
		}
		if strings.HasSuffix(f.Name, "Alias") {
			tag := f.Tag.Get("koanf")
			if tag != "" && tag != "-" {
				visit(here, tag)
			}
			continue
		}
		walkAliasFields(f.Type, here, visit)
	}
}
