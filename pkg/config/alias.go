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
	// A canonical key may have more than one alias (two pre-beta names for one
	// path). The second alias must be judged against what the first one
	// adopted, not against the file -- koanf does not know the canonical field
	// was filled in memory, so without this the later alias would silently
	// overwrite the earlier one instead of the conflict being refused.
	adopted := map[string]string{}
	for _, key := range keys {
		hasCanonical, hasAlias := k.Exists(key.canonical), k.Exists(key.alias)
		if !hasAlias {
			continue
		}
		if first, ok := adopted[key.canonical]; ok {
			if !key.equal() {
				return fmt.Errorf("config: %s and %s both set to different values for %s; they are two spellings of one key, so keep one",
					first, key.alias, key.canonical)
			}
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
		adopted[key.canonical] = key.alias
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
			canonical: "storage.mail_driver",
			alias:     "storage.mailbox",
			adopt:     func() { s.MailDriver = s.MailboxAlias },
			equal:     func() bool { return s.MailDriver == s.MailboxAlias },
		},
		{
			canonical: "storage.mail_home",
			alias:     "storage.mail_home_template",
			adopt:     func() { s.MailHome = s.MailHomeTemplateAlias },
			equal:     func() bool { return s.MailHome == s.MailHomeTemplateAlias },
		},
		{
			canonical: "storage.mail_index_path",
			alias:     "storage.index_dir",
			adopt:     func() { s.MailIndexPath = s.IndexDirAlias },
			equal:     func() bool { return s.MailIndexPath == s.IndexDirAlias },
		},
		{
			canonical: "storage.mail_volatile_path",
			alias:     "storage.volatile_dir",
			adopt:     func() { s.MailVolatilePath = s.VolatileDirAlias },
			equal:     func() bool { return s.MailVolatilePath == s.VolatileDirAlias },
		},
		{
			canonical: "storage.mail_control_path",
			alias:     "storage.control_dir",
			adopt:     func() { s.MailControlPath = s.ControlDirAlias },
			equal:     func() bool { return s.MailControlPath == s.ControlDirAlias },
		},
		{
			canonical: "storage.mailbox_list_normalize_names_to_nfc",
			alias:     "storage.mailbox_list_normalize_to_nfc",
			adopt:     func() { s.MailboxListNormalizeNamesToNFC = s.MailboxListNormalizeToNFCAlias },
			equal: func() bool {
				return s.MailboxListNormalizeNamesToNFC == s.MailboxListNormalizeToNFCAlias
			},
		},
		// One path, two pre-beta names: alt_dir was the maildir spelling and
		// mdbox_alt_storage_path the mdbox one, for the same cold tier. Both
		// are aliases of the one canonical key, and each conflicts with it the
		// same way -- including the two of them against each other, which the
		// consolidation row below covers.
		{
			canonical: "storage.mail_alt_path",
			alias:     "storage.alt_dir",
			adopt:     func() { s.MailAltPath = s.AltDirAlias },
			equal:     func() bool { return s.MailAltPath == s.AltDirAlias },
		},
		{
			canonical: "storage.mail_alt_path",
			alias:     "storage.mdbox_alt_storage_path",
			adopt:     func() { s.MailAltPath = s.MdboxAltStoragePathAlias },
			equal:     func() bool { return s.MailAltPath == s.MdboxAltStoragePathAlias },
		},
		{
			canonical: "storage.mail_index_log_rotate_min_age",
			alias:     "storage.index_log_compact_min_age_secs",
			adopt:     func() { s.MailIndexLogRotateMinAge = s.IndexLogCompactMinAgeSecs },
			equal:     func() bool { return s.MailIndexLogRotateMinAge == s.IndexLogCompactMinAgeSecs },
		},
	}
}
