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
	// present overrides how set-ness is decided. koanf addresses a list as one
	// opaque value -- "auth.passdb" is a key, "auth.passdb.0.password_query" is
	// not -- so an entry inside a list has to be asked of the parsed maps
	// instead. Nil means the ordinary path lookup.
	present func(k *koanf.Koanf) (canonical, alias bool)
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
		if key.present != nil {
			hasCanonical, hasAlias = key.present(k)
		}
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

// sieveAliases folds the duplicate managesieve limit onto the sieve key it
// duplicates (#1286, package 4).
func sieveAliases(cfg *Config) []aliasedKey {
	return []aliasedKey{
		intAlias("sieve.sieve_max_script_size", "protocol.managesieve.max_script_size",
			&cfg.Sieve.MaxScriptSize, &cfg.Protocol.ManageSieve.MaxScriptSizeAlias),
	}
}

// protocolAliases lists the package-2b renames: the protocol and provider
// sections take the reference's flat prefixed spellings.
func protocolAliases(cfg *Config) []aliasedKey {
	l := &cfg.Protocol.LMTP
	im := &cfg.Protocol.IMAP
	sub := &cfg.Protocol.Submission
	rel := &sub.Relay
	q := &cfg.Quota
	f := &cfg.FTS

	out := []aliasedKey{
		boolAlias("protocol.lmtp.lmtp_add_received_header", "protocol.lmtp.add_received_header", &l.AddReceivedHeader, &l.AddReceivedHeaderAlias),
		boolAlias("protocol.lmtp.lmtp_save_to_detail_mailbox", "protocol.lmtp.save_to_detail_mailbox", &l.SaveToDetailMailbox, &l.SaveToDetailMailboxAlias),
		strAlias("protocol.lmtp.lmtp_hdr_delivery_address", "protocol.lmtp.hdr_delivery_address", &l.HdrDeliveryAddress, &l.HdrDeliveryAddressAlias),
		boolAlias("protocol.lmtp.lmtp_verbose_replies", "protocol.lmtp.verbose_replies", &l.VerboseReplies, &l.VerboseRepliesAlias),
		intAlias("protocol.lmtp.lmtp_user_concurrency_limit", "protocol.lmtp.user_concurrency_limit", &l.UserConcurrencyLimit, &l.UserConcurrencyLimitAlias),
		listAlias("protocol.lmtp.lmtp_client_workarounds", "protocol.lmtp.client_workarounds", &l.ClientWorkarounds, &l.ClientWorkaroundsAlias),

		listAlias("protocol.imap.imap_client_workarounds", "protocol.imap.client_workarounds", &im.ClientWorkarounds, &im.ClientWorkaroundsAlias),

		strAlias("protocol.submission.submission_max_mail_size", "protocol.submission.max_message_size", &sub.MaxMsgSizeRaw, &sub.MaxMsgSizeRawAlias),
		intAlias("protocol.submission.submission_max_recipients", "protocol.submission.max_recipients", &sub.MaxRecipients, &sub.MaxRecipientsAlias),
		listAlias("protocol.submission.submission_client_workarounds", "protocol.submission.client_workarounds", &sub.Workarounds, &sub.WorkaroundsAlias),

		strAlias("protocol.submission.relay.submission_relay_host", "protocol.submission.relay.host", &rel.Host, &rel.HostAlias),
		intAlias("protocol.submission.relay.submission_relay_port", "protocol.submission.relay.port", &rel.Port, &rel.PortAlias),
		strAlias("protocol.submission.relay.submission_relay_user", "protocol.submission.relay.user", &rel.User, &rel.UserAlias),
		strAlias("protocol.submission.relay.submission_relay_password", "protocol.submission.relay.password", &rel.Password, &rel.PasswordAlias),
		strAlias("protocol.submission.relay.submission_relay_ssl", "protocol.submission.relay.ssl", &rel.SSL, &rel.SSLAlias),
		boolAlias("protocol.submission.relay.submission_relay_ssl_verify", "protocol.submission.relay.ssl_verify", &rel.SSLVerify, &rel.SSLVerifyAlias),
		boolAlias("protocol.submission.relay.submission_relay_trusted", "protocol.submission.relay.trusted", &rel.Trusted, &rel.TrustedAlias),
		intAlias("protocol.submission.relay.submission_relay_connect_timeout", "protocol.submission.relay.connect_timeout", &rel.ConnectTimeout, &rel.ConnectTimeoutAlias),
		intAlias("protocol.submission.relay.submission_relay_command_timeout", "protocol.submission.relay.command_timeout", &rel.CommandTimeout, &rel.CommandTimeoutAlias),

		// The value is a size, resolved once at load. Adoption happens before
		// that pass, so the canonical field is what gets parsed -- an alias
		// adopted afterwards would leave it holding an unparsed string and the
		// grace would read as zero.
		strAlias("quota.quota_storage_grace", "quota.quota_grace", &q.Grace, &q.GraceAlias),

		intAlias("fts.fts_search_timeout", "fts.fts_search_timeout_secs", &f.SearchTimeoutSecs, &f.SearchTimeoutSecsAlias),
		intAlias("fts.language_tokenizer_generic_token_maxlen", "fts.fts_language_tokenizer_generic_token_maxlen", &f.LanguageTokenMaxLen, &f.LanguageTokenMaxLenAlias),
		intAlias("fts.language_tokenizer_address_token_maxlen", "fts.fts_language_tokenizer_address_token_maxlen", &f.LanguageAddressMaxLen, &f.LanguageAddressMaxLenAlias),
		strAlias("fts.language_tokenizer_generic_algorithm", "fts.fts_language_tokenizer_generic_algorithm", &f.LanguageTokenizerAlgorithm, &f.LanguageTokenizerAlgorithmAlias),
		boolAlias("fts.language_tokenizer_generic_wb5a", "fts.fts_language_tokenizer_generic_wb5a", &f.LanguageTokenizerWB5A, &f.LanguageTokenizerWB5AAlias),
		boolAlias("fts.language_tokenizer_generic_explicit_prefix", "fts.fts_language_tokenizer_generic_explicit_prefix", &f.LanguageTokenizerExplicitPrefix, &f.LanguageTokenizerExplicitPrefixAlias),
	}

	// List entries: the path carries the index, since that is what koanf reads
	// them under. A chain of three passdbs is three sets of pairs, not one.
	for i := range cfg.Auth.Passdb {
		out = append(out, passdbAliases("auth.passdb", i, &cfg.Auth.Passdb[i])...)
	}
	for i := range cfg.Auth.MasterUsers.Masterdb {
		out = append(out, passdbAliases("auth.master_users.masterdb", i, &cfg.Auth.MasterUsers.Masterdb[i])...)
	}
	for i := range cfg.Auth.OAuth2 {
		out = append(out, oauth2Aliases("auth.oauth2", i, &cfg.Auth.OAuth2[i])...)
	}
	return out
}

// listEntryPresence answers set-ness for a key inside a list entry, reading the
// parsed value rather than the key index: koanf holds the whole list under one
// key, so a path with an index in it never "exists".
func listEntryPresence(listPath string, idx int, canonical, alias string) func(*koanf.Koanf) (bool, bool) {
	return func(k *koanf.Koanf) (bool, bool) {
		entries, ok := k.Get(listPath).([]any)
		if !ok || idx >= len(entries) {
			return false, false
		}
		m, ok := entries[idx].(map[string]any)
		if !ok {
			return false, false
		}
		_, hasCanonical := m[canonical]
		_, hasAlias := m[alias]
		return hasCanonical, hasAlias
	}
}

func passdbAliases(listPath string, idx int, e *PassdbEntry) []aliasedKey {
	path := fmt.Sprintf("%s.%d", listPath, idx)
	with := func(k aliasedKey, canonical, alias string) aliasedKey {
		k.present = listEntryPresence(listPath, idx, canonical, alias)
		return k
	}
	return []aliasedKey{
		with(strAlias(path+".passdb_sql_query", path+".password_query", &e.PasswordQuery, &e.PasswordQueryAlias), "passdb_sql_query", "password_query"),
		with(strAlias(path+".userdb_sql_query", path+".user_query", &e.UserQuery, &e.UserQueryAlias), "userdb_sql_query", "user_query"),
		with(strAlias(path+".userdb_sql_iterate_query", path+".iterate_query", &e.IterateQuery, &e.IterateQueryAlias), "userdb_sql_iterate_query", "iterate_query"),
		with(strAlias(path+".passdb_default_password_scheme", path+".default_pass_scheme", &e.DefaultPassScheme, &e.DefaultPassSchemeAlias), "passdb_default_password_scheme", "default_pass_scheme"),
		// No passdb_ prefix on this one: that is how the reference spells it
		// (2.4.4 source, package 3), and the pattern does not outrank the source.
		with(strAlias(path+".passwd_file_path", path+".passwd_file", &e.PasswdFile, &e.PasswdFileAlias), "passwd_file_path", "passwd_file"),
	}
}

func oauth2Aliases(listPath string, idx int, e *OAuth2Entry) []aliasedKey {
	path := fmt.Sprintf("%s.%d", listPath, idx)
	with := func(k aliasedKey, canonical, alias string) aliasedKey {
		k.present = listEntryPresence(listPath, idx, canonical, alias)
		return k
	}
	return []aliasedKey{
		with(strAlias(path+".oauth2_jwks_url", path+".jwks_url", &e.JWKSURL, &e.JWKSURLAlias), "oauth2_jwks_url", "jwks_url"),
		with(strAlias(path+".oauth2_introspection_url", path+".introspection_url", &e.IntrospectionURL, &e.IntrospectionURLAlias), "oauth2_introspection_url", "introspection_url"),
		with(strAlias(path+".oauth2_tokeninfo_url", path+".tokeninfo_url", &e.TokeninfoURL, &e.TokeninfoURLAlias), "oauth2_tokeninfo_url", "tokeninfo_url"),
		with(strAlias(path+".oauth2_issuer_url", path+".issuer_url", &e.IssuerURL, &e.IssuerURLAlias), "oauth2_issuer_url", "issuer_url"),
		with(strAlias(path+".oauth2_introspection_mode", path+".introspection_mode", &e.IntrospectionMode, &e.IntrospectionModeAlias), "oauth2_introspection_mode", "introspection_mode"),
		with(boolAlias(path+".oauth2_prefer_introspection", path+".prefer_introspection", &e.PreferIntrospection, &e.PreferIntrospectionAlias), "oauth2_prefer_introspection", "prefer_introspection"),
		with(strAlias(path+".oauth2_client_id", path+".client_id", &e.ClientID, &e.ClientIDAlias), "oauth2_client_id", "client_id"),
		with(strAlias(path+".oauth2_client_secret", path+".client_secret", &e.ClientSecret, &e.ClientSecretAlias), "oauth2_client_secret", "client_secret"),
		with(listAlias(path+".oauth2_issuers", path+".issuers", &e.Issuers, &e.IssuersAlias), "oauth2_issuers", "issuers"),
		with(strAlias(path+".oauth2_mode", path+".mode", (*string)(&e.Mode), &e.ModeAlias), "oauth2_mode", "mode"),
		with(strAlias(path+".oauth2_audience", path+".audience", &e.Audience, &e.AudienceAlias), "oauth2_audience", "audience"),
		with(listAlias(path+".oauth2_scope", path+".scopes", &e.Scopes, &e.ScopesAlias), "oauth2_scope", "scopes"),
		with(strAlias(path+".oauth2_username_attribute", path+".username_attribute", &e.UsernameAttribute, &e.UsernameAttributeAlias), "oauth2_username_attribute", "username_attribute"),
		with(strAlias(path+".oauth2_username_validation_format", path+".username_validation_format", &e.UsernameValidationFormat, &e.UsernameValidationFormatAlias), "oauth2_username_validation_format", "username_validation_format"),
		with(strAlias(path+".oauth2_active_attribute", path+".active_attribute", &e.ActiveAttribute, &e.ActiveAttributeAlias), "oauth2_active_attribute", "active_attribute"),
		with(strAlias(path+".oauth2_active_value", path+".active_value", &e.ActiveValue, &e.ActiveValueAlias), "oauth2_active_value", "active_value"),
		with(listAlias(path+".oauth2_fields", path+".extra_fields", &e.ExtraFields, &e.ExtraFieldsAlias), "oauth2_fields", "extra_fields"),
		with(intAlias(path+".oauth2_token_expire_grace_seconds", path+".token_expire_grace_seconds", &e.TokenExpireGraceSeconds, &e.TokenExpireGraceSecondsAlias), "oauth2_token_expire_grace_seconds", "token_expire_grace_seconds"),
		with(intAlias(path+".oauth2_http_timeout_ms", path+".http_timeout_ms", &e.HTTPTimeoutMs, &e.HTTPTimeoutMsAlias), "oauth2_http_timeout_ms", "http_timeout_ms"),
	}
}

func strAlias(canonical, alias string, c, a *string) aliasedKey {
	return aliasedKey{canonical: canonical, alias: alias,
		adopt: func() { *c = *a }, equal: func() bool { return *c == *a }}
}

func intAlias(canonical, alias string, c, a *int) aliasedKey {
	return aliasedKey{canonical: canonical, alias: alias,
		adopt: func() { *c = *a }, equal: func() bool { return *c == *a }}
}

func boolAlias(canonical, alias string, c, a *bool) aliasedKey {
	return aliasedKey{canonical: canonical, alias: alias,
		adopt: func() { *c = *a }, equal: func() bool { return *c == *a }}
}

func listAlias(canonical, alias string, c, a *[]string) aliasedKey {
	return aliasedKey{canonical: canonical, alias: alias,
		adopt: func() { *c = *a }, equal: func() bool { return equalStrings(*c, *a) }}
}

// invertedPairs are the renames whose sense is flipped. They are NOT aliases
// and are deliberately kept out of applyAliases: copying a value across an
// inversion would turn an operator's security setting into its opposite on a
// config nobody edited.
//
// The rule is stricter than for an alias, and stricter on purpose: both
// spellings present is refused EVEN WHEN THEY AGREE. An operator carrying both
// is one edit away from meaning the opposite of what they wrote, and the pair
// reads as if the two lines reinforce each other when they cancel.
func refuseInvertedPairs(cfg *Config) error {
	for name, svc := range servicesByName(cfg) {
		if svc == nil {
			continue
		}
		if svc.AllowCleartext != nil && svc.DisablePlainAuth != nil {
			return fmt.Errorf("config: services.%s sets both auth_allow_cleartext and disable_plaintext_auth; "+
				"they are one setting with opposite senses (auth_allow_cleartext=false is disable_plaintext_auth=true), "+
				"so keep auth_allow_cleartext and delete the other", name)
		}
		if svc.DisablePlainAuth != nil {
			slog.Warn("config: deprecated key accepted, removed after beta",
				"key", "services."+name+".disable_plaintext_auth",
				"use", "services."+name+".auth_allow_cleartext",
				"note", "the sense is inverted: disable_plaintext_auth=true is auth_allow_cleartext=false")
		}
	}
	return nil
}
