package passwdfile

import "strings"

// user is one parsed passwd-file record.
type user struct {
	name     string
	password string
	home     string
	// extra holds every space-separated key=value from the extra-fields
	// column, verbatim (the userdb_ prefix is preserved and stripped at
	// userdb-lookup time). Passdb-side fields (allow_nets, nologin, ...) and
	// userdb-only fields (userdb_mail, ...) both live here.
	extra map[string]string
}

// parse reads the passwd-file body and returns username → record. The format
// is colon-separated, mirroring the classic /etc/passwd layout:
//
//	user:password:uid:gid:gecos:home:shell:extra_fields
//
// Only user (col 0) is required. password is col 1; home is col 5; every
// column from 7 onward is rejoined with ":" as the extra-fields blob and split
// on spaces into key=value pairs. uid/gid/gecos/shell are parsed positionally
// for layout compatibility but not retained — yarilo derives system identity
// from config and storage paths from the home template, not per-user uid/gid.
//
// Lines that are empty or begin with '#' or ':' are skipped. A later duplicate
// username overrides an earlier one.
func parse(body string) map[string]*user {
	out := make(map[string]*user)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || line[0] == '#' || line[0] == ':' {
			continue
		}
		cols := strings.Split(line, ":")
		u := &user{name: cols[0], extra: map[string]string{}}
		if len(cols) > 1 {
			u.password = cols[1]
		}
		if len(cols) > 5 {
			u.home = cols[5]
		}
		if len(cols) > 7 {
			extraBlob := strings.Join(cols[7:], ":")
			for _, tok := range strings.Fields(extraBlob) {
				if eq := strings.IndexByte(tok, '='); eq > 0 {
					u.extra[tok[:eq]] = tok[eq+1:]
				}
			}
		}
		out[u.name] = u
	}
	return out
}
