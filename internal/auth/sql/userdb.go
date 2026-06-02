package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
)

// Userdb is the SQL-backed implementation of protocol.Userdb. It
// shares Config + DSN with the existing Passdb (operators typically
// have one DB serving both roles) but is exposed as a separate type
// so a deployment can chain heterogeneous backends — e.g. SQL
// passdb + LDAP userdb.
//
// The default lookup query selects the columns shipped in the
// built-in yarilo_users schema (home, mail). Operators with richer
// schemas point Config.UserQuery at their own SELECT that returns
// the column names listed in [defaultUserdbColumns] — see the
// rowScanner below for the exact mapping from column name →
// UserInfo field. Any column the row carries that does not match a
// known typed field lands in UserInfo.Extra so forward-compatibility
// is preserved without requiring driver changes for every new field.
type Userdb struct {
	db           *sql.DB
	driver       string
	userQuery    string
	iterateQuery string
}

// NewUserdb opens an SQL userdb. The Config is shared with Passdb's
// New so callers can construct both from one block; the userdb
// honours Config.UserQuery and Config.IterateQuery, defaulting to
// the same yarilo_users columns the Passdb assumes when the operator
// has not overridden them.
func NewUserdb(c Config) (*Userdb, error) {
	drv, ok := sqlDriverName(c.Driver)
	if !ok {
		return nil, fmt.Errorf("auth/sql: unsupported driver %q (want sqlite|mysql|postgres)", c.Driver)
	}
	db, err := sql.Open(drv, c.DSN)
	if err != nil {
		return nil, fmt.Errorf("auth/sql: userdb open %s: %w", c.Driver, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("auth/sql: userdb ping %s: %w", c.Driver, err)
	}
	// The userdb does NOT auto-create the schema — Passdb.New is
	// the canonical schema owner. If callers opt to construct a
	// userdb without a passdb (e.g. LDAP passdb + SQL userdb),
	// they must point UserQuery at a query that works against
	// their existing schema.
	uq := c.UserQuery
	if uq == "" {
		uq = defaultUserdbQuery
	}
	return &Userdb{
		db:           db,
		driver:       c.Driver,
		userQuery:    uq,
		iterateQuery: c.IterateQuery,
	}, nil
}

// defaultUserdbQuery selects every column the built-in yarilo_users
// schema declares. Operators with richer schemas override via
// Config.UserQuery — they SELECT any subset of [defaultUserdbColumns]
// (or new columns landing in Extra). Column order does not matter;
// the row scanner uses sql.Rows.Columns() metadata to map names →
// fields.
const defaultUserdbQuery = `SELECT username, home, mail FROM yarilo_users WHERE username = %u AND enabled = 1`

// Lookup implements protocol.Userdb. Returns (nil, nil) when the
// query returns no rows so a UserdbChain falls through to the next
// backend; returns (nil, err) on driver / scan / parse errors so
// the chain short-circuits with the operator-visible failure.
func (u *Userdb) Lookup(username string) (*protocol.UserInfo, error) {
	query, args := substituteVars(u.driver, u.userQuery, username)

	rows, err := u.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, fmt.Errorf("auth/sql: userdb query: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("auth/sql: userdb scan: %w", err)
		}
		return nil, nil
	}

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("auth/sql: userdb columns: %w", err)
	}
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range cols {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, fmt.Errorf("auth/sql: userdb row scan: %w", err)
	}

	info := &protocol.UserInfo{Username: username}
	for i, col := range cols {
		val := stringify(values[i])
		if val == "" {
			continue
		}
		if err := assignField(info, col, val); err != nil {
			return nil, fmt.Errorf("auth/sql: userdb column %q: %w", col, err)
		}
	}
	// Default to the SQL-supplied username if the query returned one;
	// otherwise the caller-supplied username (assigned above) stays.
	if info.Username == "" {
		info.Username = username
	}
	return info, nil
}

// Iterate implements protocol.UserdbIterator when an iterate_query
// is configured. Returns an error if no query was set so callers
// can distinguish "no enumeration support" from "empty result".
func (u *Userdb) Iterate() ([]string, error) {
	if u.iterateQuery == "" {
		return nil, errors.New("auth/sql: iterate_query not configured")
	}
	rows, err := u.db.QueryContext(context.Background(), u.iterateQuery)
	if err != nil {
		return nil, fmt.Errorf("auth/sql: userdb iterate: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("auth/sql: userdb iterate scan: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Close releases the underlying database connection.
func (u *Userdb) Close() error { return u.db.Close() }

// stringify normalises any sql-row value into a string. Numeric
// columns are decimal-rendered; NULLs and unknown types return "".
// The userdb wire is text-only, so the unified string form is the
// simplest contract for downstream assignment.
func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	case uint64:
		return strconv.FormatUint(x, 10)
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}

// assignField populates the matching typed UserInfo field for known
// column names, mirroring Dovecot's reserved auth-fields names
// (auth-request.c / userdb-passwd.c). Unknown columns spill into
// UserInfo.Extra so a richer operator schema does not lose data.
// Lists (groups, allow_nets, quota_rule) accept comma-separated
// values; quota_rule may also be passed once per row by repeating
// the column under different aliases — both are merged here.
//
//nolint:gocyclo // long but each branch is one line of assignment
func assignField(info *protocol.UserInfo, col, val string) error {
	switch strings.ToLower(col) {
	case "username", "user":
		info.Username = val
	case "original_user":
		info.OriginalUser = val
	case "master_user":
		info.MasterUser = val
	case "login_user":
		info.LoginUser = val
	case "uid":
		n, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return err
		}
		info.UID = uint32(n)
	case "gid":
		n, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return err
		}
		info.GID = uint32(n)
	case "home":
		info.Home = val
	case "chroot":
		info.Chroot = val
	case "system_groups_user":
		info.SystemGroupsUser = val
	case "groups":
		info.Groups = splitCSV(val)
	case "client_cert_present":
		info.ClientCertPresent = isTruthy(val)
	case "mail", "mail_location":
		info.MailLocation = val
	case "mail_uid":
		n, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return err
		}
		info.MailUID = uint32(n)
	case "mail_gid":
		n, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return err
		}
		info.MailGID = uint32(n)
	case "mailbox_format":
		info.MailboxFormat = val
	case "mail_attribute_dict":
		info.MailAttributeDict = val
	case "quota_rule":
		info.QuotaRules = append(info.QuotaRules, splitCSV(val)...)
	case "quota_over_flag":
		info.QuotaOverFlag = val
	case "allow_nets":
		info.AllowNets = splitCSV(val)
	case "nologin":
		info.NoLogin = isTruthy(val)
	case "nodelay":
		info.NoDelay = isTruthy(val)
	case "noauthenticate":
		info.NoAuthenticate = isTruthy(val)
	case "pass_expired":
		info.PassExpired = isTruthy(val)
	case "nopassword":
		info.NoPassword = isTruthy(val)
	case "proxy":
		info.Proxy = isTruthy(val)
	case "proxy_maybe":
		info.ProxyMaybe = isTruthy(val)
	case "host":
		info.Host = val
	case "port":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		info.Port = n
	case "destuser":
		info.DestUser = val
	case "proxy_mech":
		info.ProxyMech = val
	case "proxy_timeout":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		info.ProxyTimeout = n
	case "proxy_redirect_reauth":
		info.ProxyRedirectReauth = isTruthy(val)
	case "proxy_nopipelining":
		info.ProxyNoPipelining = isTruthy(val)
	case "ssl":
		info.SSL = val
	case "starttls":
		info.StartTLS = isTruthy(val)
	case "mail_max_userip_connections":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		info.MailMaxUserIPConnections = n
	case "mail_max_user_connections":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		info.MailMaxUserConnections = n
	case "service":
		info.Service = val
	case "local_name":
		info.LocalName = val
	case "password":
		info.Password = val
	case "enabled":
		// Filter column from default query — semantic is `WHERE
		// enabled = 1` clause, not a UserInfo field.
	default:
		if strings.HasPrefix(strings.ToLower(col), "forward_") {
			if info.Forward == nil {
				info.Forward = map[string]string{}
			}
			info.Forward[strings.TrimPrefix(strings.ToLower(col), "forward_")] = val
			return nil
		}
		if info.Extra == nil {
			info.Extra = map[string]string{}
		}
		info.Extra[col] = val
	}
	return nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "yes", "y", "true", "t", "on":
		return true
	}
	return false
}
