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
			adopt:     func() { s.MailIndexLogRotateMinSizeRaw = s.IndexLogCompactMinBytesAlias },
			equal:     func() bool { return s.MailIndexLogRotateMinSizeRaw == s.IndexLogCompactMinBytesAlias },
		},
		{
			canonical: "storage.mail_index_log_rotate_max_size",
			alias:     "storage.index_log_compact_max_bytes",
			adopt:     func() { s.MailIndexLogRotateMaxSizeRaw = s.IndexLogCompactMaxBytesAlias },
			equal:     func() bool { return s.MailIndexLogRotateMaxSizeRaw == s.IndexLogCompactMaxBytesAlias },
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
			adopt:     func() { s.MailIndexLogRotateMinAge = s.IndexLogCompactMinAgeSecsAlias },
			equal:     func() bool { return s.MailIndexLogRotateMinAge == s.IndexLogCompactMinAgeSecsAlias },
		},
	}
}

// generalAliases lists the general-section renames of package 2 (#1286).
func generalAliases(cfg *Config) []aliasedKey {
	hap := &cfg.General.HAProxy
	out := sslAliasesFor("general.ssl", &cfg.General.SSL)
	return append(out, aliasedKey{
		canonical: "general.haproxy.haproxy_trusted_networks",
		alias:     "general.haproxy.trusted_nets",
		adopt:     func() { hap.HAProxyTrustedNetworks = hap.TrustedNetsAlias },
		equal:     func() bool { return equalStrings(hap.HAProxyTrustedNetworks, hap.TrustedNetsAlias) },
	})
}

// aclAliases lists the acl-section renames of package 2.
func aclAliases(cfg *Config) []aliasedKey {
	a := &cfg.ACL
	return []aliasedKey{
		{
			canonical: "acl.acl_globals_only",
			alias:     "acl.globals_only",
			adopt:     func() { a.GlobalsOnly = a.GlobalsOnlyAlias },
			equal:     func() bool { return a.GlobalsOnly == a.GlobalsOnlyAlias },
		},
		{
			canonical: "acl.acl_sharing_map",
			alias:     "acl.acl_shared_dict",
			adopt:     func() { a.SharedDict = a.SharedDictAlias },
			equal:     func() bool { return a.SharedDict == a.SharedDictAlias },
		},
	}
}

// authAliases lists the auth-section renames of package 2.
func authAliases(cfg *Config) []aliasedKey {
	au := &cfg.Auth
	c := &cfg.Auth.Cache
	p := &cfg.Auth.Policy
	m := &cfg.Auth.MasterUsers
	boolKey := func(canonical, alias string, cv, av *bool) aliasedKey {
		return aliasedKey{canonical: canonical, alias: alias,
			adopt: func() { *cv = *av }, equal: func() bool { return *cv == *av }}
	}
	strKey := func(canonical, alias string, cv, av *string) aliasedKey {
		return aliasedKey{canonical: canonical, alias: alias,
			adopt: func() { *cv = *av }, equal: func() bool { return *cv == *av }}
	}
	intKey := func(canonical, alias string, cv, av *int) aliasedKey {
		return aliasedKey{canonical: canonical, alias: alias,
			adopt: func() { *cv = *av }, equal: func() bool { return *cv == *av }}
	}
	return []aliasedKey{
		strKey("auth.cache.auth_cache_size", "auth.cache.cache_size", &c.CacheSize, &c.CacheSizeAlias),
		intKey("auth.cache.auth_cache_ttl", "auth.cache.ttl_seconds", &c.TTLSeconds, &c.TTLSecondsAlias),
		intKey("auth.cache.auth_cache_negative_ttl", "auth.cache.negative_ttl_seconds", &c.NegativeTTLSeconds, &c.NegativeTTLSecondsAlias),
		intKey("auth.auth_failure_delay", "auth.failure_delay", &au.FailureDelaySeconds, &au.FailureDelaySecondsAlias),
		strKey("auth.master_users.auth_master_user_separator", "auth.master_users.separator", &m.Separator, &m.SeparatorAlias),
		strKey("auth.policy.auth_policy_server_url", "auth.policy.url", &p.URL, &p.URLAlias),
		strKey("auth.policy.auth_policy_server_api_header", "auth.policy.api_header", &p.APIHeader, &p.APIHeaderAlias),
		strKey("auth.policy.auth_policy_hash_mech", "auth.policy.hash_mech", &p.HashMech, &p.HashMechAlias),
		strKey("auth.policy.auth_policy_hash_nonce", "auth.policy.hash_nonce", &p.HashNonce, &p.HashNonceAlias),
		{
			canonical: "auth.policy.auth_policy_hash_truncate",
			alias:     "auth.policy.hash_truncate_bits",
			adopt:     func() { p.HashTruncateBits = p.HashTruncateBitsAlias },
			equal:     func() bool { return p.HashTruncateBits == p.HashTruncateBitsAlias },
		},
		boolKey("auth.policy.auth_policy_reject_on_fail", "auth.policy.reject_on_fail", &p.RejectOnFail, &p.RejectOnFailAlias),
		boolKey("auth.policy.auth_policy_log_only", "auth.policy.log_only", &p.LogOnly, &p.LogOnlyAlias),
		boolKey("auth.policy.auth_policy_check_before_auth", "auth.policy.check_before", &p.CheckBefore, &p.CheckBeforeAlias),
		boolKey("auth.policy.auth_policy_check_after_auth", "auth.policy.check_after", &p.CheckAfter, &p.CheckAfterAlias),
		boolKey("auth.policy.auth_policy_report_after_auth", "auth.policy.report_after", &p.ReportAfter, &p.ReportAfterAlias),
	}
}

// equalStrings compares two string lists as values, for aliases whose knob is a
// list rather than a scalar.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// serviceSSLAliases lists the per-service ssl override blocks. A service block
// is the same SSLConfig type, so it carries the same pre-beta spellings -- and
// the fixed general.ssl paths do not reach it. Without this, an override
// written the old way filled an alias field nobody adopted and the service lost
// its certificate: a value that looks set and does nothing.
func serviceSSLAliases(cfg *Config) []aliasedKey {
	var out []aliasedKey
	for name, svc := range servicesByName(cfg) {
		if svc == nil || svc.SSL == nil {
			continue
		}
		out = append(out, sslAliasesFor("services."+name+".ssl", svc.SSL)...)
	}
	return out
}

// sslAliasesFor builds the ssl pairs for one block, wherever that block lives.
func sslAliasesFor(path string, ssl *SSLConfig) []aliasedKey {
	str := func(canonical, alias string, c, a *string) aliasedKey {
		return aliasedKey{canonical: path + "." + canonical, alias: path + "." + alias,
			adopt: func() { *c = *a }, equal: func() bool { return *c == *a }}
	}
	return []aliasedKey{
		str("ssl_server_cert_file", "tls_cert", &ssl.SSLServerCert, &ssl.TLSCertAlias),
		str("ssl_server_key_file", "tls_key", &ssl.SSLServerKey, &ssl.TLSKeyAlias),
		str("ssl_server_alt_cert_file", "tls_alt_cert", &ssl.SSLServerAltCert, &ssl.TLSAltCertAlias),
		str("ssl_server_alt_key_file", "tls_alt_key", &ssl.SSLServerAltKey, &ssl.TLSAltKeyAlias),
		str("ssl_min_protocol", "tls_min_version", &ssl.SSLMinProtocol, &ssl.TLSMinVersionAlias),
		{
			canonical: path + ".ssl_prefer_server_ciphers",
			alias:     path + ".prefer_server_ciphers",
			adopt:     func() { ssl.SSLPreferCiphers = ssl.PreferServerAlias },
			equal:     func() bool { return ssl.SSLPreferCiphers == ssl.PreferServerAlias },
		},
	}
}

// servicesByName maps the service blocks to the koanf names they live under, so
// an alias path can be built for a block that is one of many rather than at a
// fixed location.
func servicesByName(cfg *Config) map[string]*ServiceConfig {
	s := &cfg.Services
	return map[string]*ServiceConfig{
		"imap": s.IMAP, "imaps": s.IMAPS,
		"submission": s.Submission, "submissions": s.Submissions,
		"pop3": s.POP3, "pop3s": s.POP3S,
		"lmtp": s.LMTP, "managesieve": s.ManageSieve, "managesieve_be": s.ManageSieveBE,
		"jmap": s.JMAP, "jmap_be": s.JMAPBE,
	}
}
