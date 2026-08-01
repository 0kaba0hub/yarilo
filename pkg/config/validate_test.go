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

// TestSizeFieldsResolve proves every human-readable size field is parsed to
// bytes at load, and a malformed value anywhere fails startup loudly.
func TestSizeFieldsResolve(t *testing.T) {
	cfg := &Config{}
	cfg.Protocol.Submission.MaxMsgSizeRaw = "40M"
	cfg.FTS.MessageMaxSizeRaw = "1G"
	cfg.FTS.DecoderMaxSizeRaw = "10M"
	cfg.FTS.DetectionSampleBytesRaw = "8k"
	cfg.Storage.IndexLogCompactMinBytesRaw = "32k"
	cfg.Storage.IndexLogCompactMaxBytesRaw = "1M"
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	checks := []struct {
		name string
		got  int64
		want int64
	}{
		{"max_message_size", cfg.Protocol.Submission.MaxMsgSize, 40 * 1024 * 1024},
		{"fts_message_max_size", cfg.FTS.MessageMaxSize, 1024 * 1024 * 1024},
		{"fts_decoder_max_size", cfg.FTS.DecoderMaxSize, 10 * 1024 * 1024},
		{"fts_detection_sample_bytes", int64(cfg.FTS.DetectionSampleBytes), 8 * 1024},
		{"index_log_compact_min_bytes", cfg.Storage.IndexLogCompactMinBytes, 32 * 1024},
		{"index_log_compact_max_bytes", cfg.Storage.IndexLogCompactMaxBytes, 1024 * 1024},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s resolved to %d, want %d", c.name, c.got, c.want)
		}
	}

	bad := &Config{}
	bad.FTS.DecoderMaxSizeRaw = "10MB" // "MB" is not a valid suffix
	if err := bad.validate(); err == nil {
		t.Fatal("malformed fts_decoder_max_size should fail validate")
	}
}
