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

// skip_schema and an absent password query contradict each other: one says the
// tables are the operator's, the other asks for ours. A warning would be a
// config that declares one thing and does another, so the combination is
// refused — while every neighbouring combination still starts, which is what
// makes the refusal about the contradiction rather than about either setting.
func TestSkipSchemaWithoutQueryIsRefused(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "users.db")

	tests := []struct {
		name       string
		skipSchema bool
		query      string
		wantErr    bool
		errNames   []string
	}{
		{
			name:       "skip_schema and no query contradict",
			skipSchema: true,
			wantErr:    true,
			errNames:   []string{"skip_schema", "passdb_sql_query"},
		},
		{
			name:       "skip_schema with a query is a complete statement",
			skipSchema: true,
			query:      "SELECT password FROM my_users WHERE email = %u",
		},
		{
			// The built-in schema against an external database is what the
			// default exists for; refusing it would make the schema unusable
			// with any database but our own.
			name:  "no query and no skip_schema is the out-of-the-box case",
			query: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := authsql.New(authsql.Config{
				Driver:        "sqlite",
				DSN:           dsn,
				PasswordQuery: tc.query,
				SkipSchema:    tc.skipSchema,
			})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("refused a legitimate combination: %v", err)
				}
				p.Close() //nolint:errcheck
				return
			}
			if err == nil {
				p.Close() //nolint:errcheck
				t.Fatal("the contradictory combination opened anyway")
			}
			for _, want := range tc.errNames {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err, want)
				}
			}
		})
	}
}
