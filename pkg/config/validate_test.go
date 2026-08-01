package config

import "testing"

// TestValidateBackendOrDirector (#735) proves a login/proxy component must
// have at least one of backend_addr/director_addr set — the guard that
// replaced the old silent fall-back to this process's own in-process
// director bind address (dialing localhost where no director runs).
func TestValidateBackendOrDirector(t *testing.T) {
	tests := []struct {
		name         string
		backendAddr  string
		directorAddr string
		wantErr      bool
	}{
		{"both empty is an error", "", "", true},
		{"backend_addr only (standalone)", "yarilo-imap:143", "", false},
		{"director_addr only (director mode)", "", "yarilo-director:9102", false},
		{"both set is not an error (backend_addr wins at dial time)", "yarilo-imap:143", "yarilo-director:9102", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBackendOrDirector("imap_login_service", tc.backendAddr, tc.directorAddr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateBackendOrDirector(%q, %q) err = %v, wantErr %v",
					tc.backendAddr, tc.directorAddr, err, tc.wantErr)
			}
			if err != nil && tc.name == "both empty is an error" {
				want := "imap_login_service: set either backend_addr (standalone) or director_addr (director mode)"
				if err.Error() != want {
					t.Fatalf("error message = %q, want %q", err.Error(), want)
				}
			}
		})
	}
}

// TestCacheSizeResolution proves auth.cache.cache_size is parsed to bytes at
// load (human-readable units) and that a malformed value fails loudly rather
// than silently disabling the cache (#950 follow-on).
func TestCacheSizeResolution(t *testing.T) {
	tests := []struct {
		in       string
		wantByte int64
		wantErr  bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"100M", 100 * 1024 * 1024, false},
		{"512k", 512 * 1024, false},
		{"104857600", 104857600, false},
		{"bogus", 0, true},
		{"100MB", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			cfg := &Config{}
			cfg.Auth.Cache.CacheSize = tc.in
			err := cfg.validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("cache_size=%q: want error, got none (bytes=%d)", tc.in, cfg.Auth.Cache.CacheSizeBytes())
				}
				return
			}
			if err != nil {
				t.Fatalf("cache_size=%q: unexpected error: %v", tc.in, err)
			}
			if got := cfg.Auth.Cache.CacheSizeBytes(); got != tc.wantByte {
				t.Fatalf("cache_size=%q resolved to %d, want %d", tc.in, got, tc.wantByte)
			}
		})
	}
}
