package config

import (
	"fmt"
	"log/slog"

	"github.com/knadh/koanf/v2"
)

// Config keys renamed to their reference spelling keep the old spelling working
// as an alias until beta ends. The rules are the same for every alias, so they
// live here rather than at each call site:
//
//   - the canonical key wins only when the two agree; both spellings set to
//     different values is a wiring error and refuses startup, because a rename
//     that silently picks a winner changes behaviour on a config nobody edited;
//   - the deprecation is logged once at load, naming both spellings, not on
//     every read;
//   - documentation and values.yaml carry the canonical spelling alone.
//
// Presence is asked of koanf rather than inferred from a zero value: a key set
// to 0 or "" is set, and adopting an alias only because the canonical field
// happens to be zero would make "both spellings present" undetectable.
type aliasedKey struct {
	canonical string
	alias     string
	// adopt copies the alias's value onto the canonical field. Called only when
	// the alias alone is set.
	adopt func()
	// equal reports whether both spellings carry the same value, and is only
	// consulted when both are set.
	equal func() bool
}

func applyAliases(k *koanf.Koanf, keys []aliasedKey) error {
	for _, key := range keys {
		hasCanonical, hasAlias := k.Exists(key.canonical), k.Exists(key.alias)
		if !hasAlias {
			continue
		}
		if hasCanonical {
			if !key.equal() {
				return fmt.Errorf("config: %s and its alias %s are both set to different values; keep the first and delete the second",
					key.canonical, key.alias)
			}
			continue
		}
		key.adopt()
		slog.Warn("config: deprecated key accepted as an alias, removed after beta",
			"key", key.alias, "use", key.canonical)
	}
	return nil
}

// storageAliases lists the storage-section renames. The rotation triple is the
// first of the family; the rest of the pre-beta names join it in the key review
// (#1234).
func storageAliases(cfg *Config) []aliasedKey {
	s := &cfg.Storage
	return []aliasedKey{
		{
			canonical: "storage.mail_index_log_rotate_min_size",
			alias:     "storage.index_log_compact_min_bytes",
			adopt:     func() { s.MailIndexLogRotateMinSizeRaw = s.IndexLogCompactMinBytesRaw },
			equal:     func() bool { return s.MailIndexLogRotateMinSizeRaw == s.IndexLogCompactMinBytesRaw },
		},
		{
			canonical: "storage.mail_index_log_rotate_max_size",
			alias:     "storage.index_log_compact_max_bytes",
			adopt:     func() { s.MailIndexLogRotateMaxSizeRaw = s.IndexLogCompactMaxBytesRaw },
			equal:     func() bool { return s.MailIndexLogRotateMaxSizeRaw == s.IndexLogCompactMaxBytesRaw },
		},
		{
			canonical: "storage.mail_index_log_rotate_min_age",
			alias:     "storage.index_log_compact_min_age_secs",
			adopt:     func() { s.MailIndexLogRotateMinAge = s.IndexLogCompactMinAgeSecs },
			equal:     func() bool { return s.MailIndexLogRotateMinAge == s.IndexLogCompactMinAgeSecs },
		},
	}
}
