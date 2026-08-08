package config

import "testing"

// The vocabulary is closed: a typo must fail startup, not pick a durability
// policy. Unset resolves to the value that does the EXTRA work, so an
// unconfigured deployment is the safe one.
func TestNormalizeFTSStorageType(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", FTSStorageLocal, true},
		{"local", FTSStorageLocal, true},
		{"nfs", FTSStorageNFS, true},
		{"NFS", FTSStorageNFS, true},
		{"  nfs  ", FTSStorageNFS, true},
		// Plausible-but-wrong spellings a reader might expect to work.
		{"posix", "", false},
		{"nfs4", "", false},
		{"true", "", false},
		{"remote", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeFTSStorageType(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeFTSStorageType(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}
