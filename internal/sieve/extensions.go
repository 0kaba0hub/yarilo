package sieve

// SupportedExtensions lists all Sieve extensions available in this build.
var SupportedExtensions = []string{
	"fileinto", "reject", "ereject", "envelope", "encoded-character",
	"variables", "relational", "copy", "subaddress", "environment",
	"body", "vacation", "vacation-seconds", "regex", "date", "index",
	"editheader", "mailbox", "duplicate", "ihave", "special-use",
	"imap4flags", "fcc", "include", "extlists", "enotify",
	"spamtest", "spamtestplus", "virustest", "mboxmetadata", "servermetadata",
}
