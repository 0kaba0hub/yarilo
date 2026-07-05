package sql

import (
	"fmt"
	"strings"
)

// substituteVars rewrites %u/%n/%d in query into positional placeholders
// (driver-specific: ? for sqlite/mysql, $N for postgres) and returns the
// rewritten query plus the corresponding args slice. This keeps user input
// out of the SQL string — no injection surface.
//
//	%u → full username (alice@example.com)
//	%n → local part   (alice)
//	%d → domain       (example.com)
//
// Other % sequences are left as-is.
func substituteVars(driver, query, username string) (string, []any) {
	local, domain := splitUser(username)
	vars := map[byte]string{'u': username, 'n': local, 'd': domain}

	var sb strings.Builder
	sb.Grow(len(query))
	var args []any
	pos := 1
	inQuote := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			inQuote = !inQuote
			sb.WriteByte(ch)
			continue
		}
		if !inQuote && ch == '%' && i+1 < len(query) {
			if val, ok := vars[query[i+1]]; ok {
				args = append(args, val)
				if driver == "postgres" {
					sb.WriteString(fmt.Sprintf("$%d", pos))
					pos++
				} else {
					sb.WriteByte('?')
				}
				i++
				continue
			}
		}
		sb.WriteByte(ch)
	}
	return sb.String(), args
}

func splitUser(u string) (local, domain string) {
	if i := strings.IndexByte(u, '@'); i >= 0 {
		return u[:i], u[i+1:]
	}
	return u, ""
}
