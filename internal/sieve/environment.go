package sieve

import (
	"strings"

	"github.com/foxcpp/go-sieve/interp"
)

// yariloEnv implements interp.Env for the vnd.yarilo.environment extension.
//
// Exposed items:
//   - vnd.yarilo.username        — full login name (user@domain)
//   - vnd.yarilo.default-mailbox — always "INBOX"
//   - vnd.yarilo.config.<key>    — operator-defined key-value pairs from
//     yarilo.yaml sieve.sieve_environment
type yariloEnv struct {
	username    string
	configItems map[string]string
}

var _ interp.Env = (*yariloEnv)(nil)

func (e *yariloEnv) GetEnvironment(name string) (string, bool) {
	switch name {
	case "vnd.yarilo.username":
		return e.username, true
	case "vnd.yarilo.default-mailbox":
		return "INBOX", true
	}
	if key, ok := strings.CutPrefix(name, "vnd.yarilo.config."); ok {
		v, found := e.configItems[key]
		return v, found
	}
	return "", false
}
