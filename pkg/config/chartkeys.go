package config

// notRenderedByTheChart is every config key the binary reads that
// configmap.yaml does not write as a line of its own.
//
// It exists so that a key added to the config without a line in the chart
// fails a test instead of defaulting silently on somebody's cluster -- which
// is what #1528 was about. The list is the inventory taken when that guard was
// written; nothing may be added to it without the reason being one of the two
// below.
var notRenderedByTheChart = map[string]string{}

// throughAnOperatorBlock: the key lives inside a structure the chart passes
// through with toYaml -- passdb and userdb driver arguments, OAuth provider
// entries, Sieve lists. The chart cannot enumerate them because their shape is
// the operator's, and values.yaml carries them.
var throughAnOperatorBlock = []string{
	"acl_shared_dict", "add_received_header", "api_header", "audience", "cache_size", "check_after",
	"check_before", "client_id", "client_secret", "client_workarounds", "command_timeout", "conn_max_idle_time",
	"conn_max_lifetime", "connect_timeout", "dbox_reactive_rebuild", "default_pass_scheme", "extra_fields", "failure_delay",
	"fts_language_tokenizer_address_token_maxlen", "fts_language_tokenizer_generic_algorithm", "fts_language_tokenizer_generic_explicit_prefix", "fts_language_tokenizer_generic_token_maxlen", "fts_language_tokenizer_generic_wb5a", "fts_search_timeout_secs",
	"globals_only", "hash_mech", "hash_nonce", "hash_truncate_bits", "hdr_delivery_address", "host",
	"identifier", "introspection_mode", "introspection_url", "issuer_url", "issuers", "iterate_query",
	"jwks_url", "log_only", "mail_inbox_path", "mail_path", "maildir_sync_on_select", "max_idle_conns",
	"max_message_size", "max_open_conns", "max_recipients", "negative_ttl_seconds", "passwd_file", "password",
	"password_query", "per_recipient_burst", "per_recipient_window_seconds", "quota_grace", "quota_warning_name", "reject_on_fail",
	"report_after", "save_to_detail_mailbox", "scopes", "sieve_global_after", "sieve_global_before", "ssl_verify",
	"trusted", "type", "user", "user_concurrency_limit", "user_query", "username_attribute",
	"username_validation_format", "verbose_replies",
}

// defaultOnly: the chart never writes the key and the binary's default is the
// deployed value. Some are aliases kept for configs written by hand.
var defaultOnly = []string{
	"active_attribute", "active_value", "disable_plaintext_auth", "fail_open", "hidden", "home_dir",
	"http_timeout_ms", "lmtp_backend_port", "max_attempts", "oauth2_mode", "prefer_introspection", "prefer_server_ciphers",
	"proxy", "socket", "ssl_prefer_server_ciphers", "ssl_server_alt_cert_file", "ssl_server_alt_key_file", "submission_client_workarounds",
	"tls_alt_cert", "tls_alt_key", "tls_cert", "tls_key", "tls_min_version", "token_expire_grace_seconds",
	"tokeninfo_url", "url", "username",
}

func init() {
	for _, k := range throughAnOperatorBlock {
		notRenderedByTheChart[k] = "passed through as an operator-authored block"
	}
	for _, k := range defaultOnly {
		notRenderedByTheChart[k] = "read with a default the chart never writes"
	}
}
