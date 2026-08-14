package config

import (
	"reflect"
	"strconv"
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
	// One entry in each list-shaped section, so the per-entry alias sets are
	// built rather than skipped: a chain nobody declared covers nothing.
	cfg.Auth.Passdb = []PassdbEntry{{}}
	cfg.Auth.MasterUsers.Masterdb = []PassdbEntry{{}}
	cfg.Auth.OAuth2 = []OAuth2Entry{{}}

	// Matched by FULL koanf path, not by the key's last segment. The same tail
	// lives in several sections -- client_workarounds is an imap key, an lmtp
	// key and a submission key -- so one adopted tail would vouch for orphans
	// elsewhere, which is the very thing this check exists to refuse.
	adopted := map[string]bool{}
	for _, set := range allAliasSets(cfg) {
		for _, key := range set {
			adopted[normalisePath(key.alias)] = true
		}
	}

	var orphans []string
	walkAliasPaths(reflect.TypeOf(Config{}), "", "", func(goPath, koanfPath string) {
		if !adopted[normalisePath(koanfPath)] {
			orphans = append(orphans, goPath+" (koanf path "+koanfPath+")")
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
	cfg.Auth.Passdb = []PassdbEntry{{}}
	cfg.Auth.MasterUsers.Masterdb = []PassdbEntry{{}}
	cfg.Auth.OAuth2 = []OAuth2Entry{{}}

	fields := map[string]bool{}
	walkAliasPaths(reflect.TypeOf(Config{}), "", "", func(_, koanfPath string) {
		fields[normalisePath(koanfPath)] = true
	})
	for _, set := range allAliasSets(cfg) {
		for _, key := range set {
			if !fields[normalisePath(key.alias)] {
				t.Errorf("alias set adopts %q, but no *Alias field lives at that path", key.alias)
			}
		}
	}
}

func allAliasSets(cfg *Config) [][]aliasedKey {
	return [][]aliasedKey{
		storageAliases(cfg), generalAliases(cfg), aclAliases(cfg),
		authAliases(cfg), serviceSSLAliases(cfg), protocolAliases(cfg),
	}
}

// normalisePath folds the two path shapes whose leaf is repeated: a service
// block (same type under many names) and a list entry (same type at many
// indices). Both build one alias entry per occurrence, so comparing by name or
// index would make coverage depend on what a test config happens to declare.
func normalisePath(path string) string {
	out := make([]string, 0, 8)
	for _, seg := range strings.Split(normaliseServicePath(path), ".") {
		if _, err := strconv.Atoi(seg); err == nil {
			continue // a list index is not part of the key's identity
		}
		out = append(out, seg)
	}
	return strings.Join(out, ".")
}

// normaliseServicePath folds every services.<name>.ssl path onto one, because
// the blocks are the same type and the alias set builds one entry per deployed
// service: comparing them by name would make coverage depend on which services
// a test config happens to declare.
func normaliseServicePath(path string) string {
	if !strings.HasPrefix(path, "services.") {
		return path
	}
	rest := strings.SplitN(strings.TrimPrefix(path, "services."), ".", 2)
	if len(rest) != 2 {
		return path
	}
	return "services.<service>." + rest[1]
}

// walkAliasPaths visits every field whose name ends in "Alias", anywhere in the
// config tree, and reports its Go path and its full koanf path — the section
// chain the loader reads it under, which is what makes two same-named keys in
// different sections distinguishable.
func walkAliasPaths(t reflect.Type, goPath, koanfPath string, visit func(goPath, koanfPath string)) {
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
		tag := f.Tag.Get("koanf")
		if tag == "-" {
			continue
		}
		goHere := f.Name
		if goPath != "" {
			goHere = goPath + "." + f.Name
		}
		koanfHere := tag
		if koanfPath != "" && tag != "" {
			koanfHere = koanfPath + "." + tag
		} else if tag == "" {
			koanfHere = koanfPath
		}
		if strings.HasSuffix(f.Name, "Alias") {
			if tag != "" {
				visit(goHere, koanfHere)
			}
			continue
		}
		walkAliasPaths(f.Type, goHere, koanfHere, visit)
	}
}

// The reason the match is by full path: the same key name lives in several
// sections. If coverage were judged by the last segment, one adopted
// client_workarounds would vouch for the two that nobody adopts — and package
// 2b introduces exactly that shape in imap, lmtp and submission at once.
func TestCoverageIsJudgedByPathNotByKeyName(t *testing.T) {
	type inner struct {
		SameNameAlias string `koanf:"same_name"`
	}
	type outer struct {
		A inner `koanf:"a"`
		B inner `koanf:"b"`
	}

	var paths []string
	walkAliasPaths(reflect.TypeOf(outer{}), "", "", func(_, koanfPath string) {
		paths = append(paths, koanfPath)
	})
	if len(paths) != 2 {
		t.Fatalf("walked %v, want both sections", paths)
	}
	if paths[0] == paths[1] {
		t.Fatalf("both sections walked to %q: two different keys are indistinguishable", paths[0])
	}
	for _, want := range []string{"a.same_name", "b.same_name"} {
		var found bool
		for _, p := range paths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("path %q not walked; got %v", want, paths)
		}
	}
}
