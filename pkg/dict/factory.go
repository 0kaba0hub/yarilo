package dict

import "github.com/0kaba0hub/yarilo/pkg/dict/varexpand"

// Factory opens per-user Dict instances by varexpanding %u/%h/%n/%d in settings.
// Callers open a Dict for each user operation and close it when done.
//
// For the file driver, each user gets a separate JSON file; the path is
// derived by expanding %h in the "path" setting. For Redis, the prefix
// is derived by expanding %u in the "prefix" setting so that each user
// occupies a separate keyspace — configure prefix as "myapp:sieve:%u:".
type Factory struct {
	cfg   Config
	fixed Dict // non-nil only in unit tests (see FixedFactory)
}

// NewFactory creates a Factory from a driver config. Settings strings
// may contain %-variables (%u, %h, %n, %d, %%); they are expanded at
// Open time with the caller-supplied username and homeDir.
func NewFactory(cfg Config) *Factory { return &Factory{cfg: cfg} }

// FixedFactory returns a Factory that always yields the same pre-existing
// Dict. Close on the returned Dict is a no-op. Intended for unit tests.
func FixedFactory(d Dict) *Factory { return &Factory{fixed: d} }

// Open opens a Dict for the given user. The caller MUST Close the returned
// Dict when done; for the file driver this flushes writes, for Redis it
// releases the connection pool.
func (f *Factory) Open(username, homeDir string) (Dict, error) {
	if f.fixed != nil {
		return &nopCloseDict{f.fixed}, nil
	}
	vars := varexpand.Vars{Username: username, HomeDir: homeDir}
	expanded := Config{
		Driver:   f.cfg.Driver,
		Settings: expandSettings(f.cfg.Settings, vars),
	}
	return Open(expanded)
}

func expandSettings(s map[string]any, vars varexpand.Vars) map[string]any {
	out := make(map[string]any, len(s))
	for k, v := range s {
		if str, ok := v.(string); ok {
			out[k] = varexpand.Expand(str, vars)
		} else {
			out[k] = v
		}
	}
	return out
}

// nopCloseDict wraps a Dict and makes Close a no-op.
type nopCloseDict struct{ Dict }

func (d *nopCloseDict) Close() error { return nil }
