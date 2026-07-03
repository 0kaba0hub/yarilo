package sieve

import (
	"fmt"

	gosieve "github.com/foxcpp/go-sieve"
)

// SupportedExtensions lists all Sieve extensions available in this build.
var SupportedExtensions = []string{
	"fileinto", "reject", "ereject", "envelope", "encoded-character",
	"variables", "relational", "copy", "subaddress", "environment",
	"body", "vacation", "vacation-seconds", "regex", "date", "index",
	"editheader", "mailbox", "duplicate", "ihave", "special-use",
	"imap4flags", "fcc", "include", "extlists", "enotify",
	"spamtest", "spamtestplus", "virustest", "mboxmetadata", "servermetadata",
	"vnd.yarilo.debug",
	"vnd.yarilo.environment",
	"vnd.yarilo.pipe",
}

// EffectiveExtensions returns the intersection of SupportedExtensions and allowed.
// When allowed is empty, SupportedExtensions is returned as-is.
func EffectiveExtensions(allowed []string) []string {
	if len(allowed) == 0 {
		return SupportedExtensions
	}
	set := make(map[string]struct{}, len(allowed))
	for _, e := range allowed {
		set[e] = struct{}{}
	}
	out := make([]string, 0, len(allowed))
	for _, e := range SupportedExtensions {
		if _, ok := set[e]; ok {
			out = append(out, e)
		}
	}
	return out
}

// CheckExtensions verifies that every extension required by script is present
// in the allowed list. Returns an error naming the first forbidden extension.
// When allowed is empty, all extensions are permitted.
func CheckExtensions(script *gosieve.Script, allowed []string) error {
	if len(allowed) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(allowed))
	for _, e := range allowed {
		set[e] = struct{}{}
	}
	for _, ext := range script.Extensions() {
		if _, ok := set[ext]; !ok {
			return fmt.Errorf("extension '%s' is not permitted", ext)
		}
	}
	return nil
}
