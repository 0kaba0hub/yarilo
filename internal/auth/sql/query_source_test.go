package sql_test

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	authsql "github.com/yarilomail/yarilo/internal/auth/sql"
)

// An absent password query does not mean "no query": it means a query against
// OUR schema, which for a deployment with its own tables authenticates nobody.
// The first sign used to be users unable to log in, with nothing in the log to
// say which query ran (#1299).
//
// Both directions, because a line that always fires says as little as one that
// never does.
func TestPasswordQuerySourceIsLogged(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "users.db")

	tests := []struct {
		name    string
		query   string
		wantIn  string
		absent  string
		wantLvl string
	}{
		{
			name:    "no query falls back to the built-in schema",
			query:   "",
			wantIn:  "using the built-in yarilo_users query",
			absent:  "using the configured password query",
			wantLvl: "WARN",
		},
		{
			name:    "a configured query is used and said so",
			query:   "SELECT password FROM my_users WHERE email = %u",
			wantIn:  "using the configured password query",
			absent:  "built-in",
			wantLvl: "INFO",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(restore)

			p, err := authsql.New(authsql.Config{
				Driver:        "sqlite",
				DSN:           dsn,
				PasswordQuery: tc.query,
			})
			if err != nil {
				t.Fatalf("open passdb: %v", err)
			}
			defer p.Close() //nolint:errcheck

			out := buf.String()
			if !strings.Contains(out, tc.wantIn) {
				t.Errorf("log does not say %q:\n%s", tc.wantIn, out)
			}
			if strings.Contains(out, tc.absent) {
				t.Errorf("log also says %q, which is the other case:\n%s", tc.absent, out)
			}
			if !strings.Contains(out, tc.wantLvl) {
				t.Errorf("log level is not %s:\n%s", tc.wantLvl, out)
			}
		})
	}
}
