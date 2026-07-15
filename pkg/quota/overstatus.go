package quota

import "strings"

// OverStatusPolicy carries the quota_over_status tunables: a login-time hook
// that keeps an external "over quota" flag in sync. On session start the actual
// over-quota state is compared to the flag carried in userdb; on a mismatch an
// external program updates the flag so an MTA can reject mail to over-quota
// users without querying the mail server.
type OverStatusPolicy struct {
	// Mask is the wildcard pattern the userdb over-flag is matched against to
	// decide whether the user is currently flagged as over. Empty disables the
	// check.
	Mask string
	// LazyCheck defers the check from login to the first quota operation.
	LazyCheck bool
	// Execute is the program (+ args) run from the warning bin dir on a mismatch.
	Execute string
}

// IsOverAny reports whether usage meets or exceeds any set limit (storage or
// message), matching the reference `value >= limit` over-quota test.
func IsOverAny(u Usage, limits Limits) bool {
	if limits.StorageBytes > 0 && u.StorageBytes >= limits.StorageBytes {
		return true
	}
	if limits.Messages > 0 && u.Messages >= limits.Messages {
		return true
	}
	return false
}

// WildcardMatchIcase reports whether s matches pattern, case-insensitively, with
// glob wildcards `*` (any run) and `?` (any single character). Mirrors the
// reference wildcard_match_icase used for quota_over_status_mask.
func WildcardMatchIcase(s, pattern string) bool {
	return wildcard(strings.ToLower(s), strings.ToLower(pattern))
}

func wildcard(s, p string) bool {
	// Iterative glob with backtracking on '*'.
	var si, pi int
	star, mark := -1, 0
	for si < len(s) {
		if pi < len(p) && (p[pi] == '?' || p[pi] == s[si]) {
			si++
			pi++
		} else if pi < len(p) && p[pi] == '*' {
			star, mark = pi, si
			pi++
		} else if star >= 0 {
			pi = star + 1
			mark++
			si = mark
		} else {
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
