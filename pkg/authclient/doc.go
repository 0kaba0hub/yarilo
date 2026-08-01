// Package authclient is the Go client for the yarilo-auth master protocol. It
// opens a single mTLS TCP connection to the master listener (see
// internal/auth/protocol/master.go), drains the handshake, and provides typed
// methods for the wire commands:
//
//   - [Client.Userdb]        password-less user lookup (USER)
//   - [Client.IterateUsers]  enumerate every known user (LIST)
//   - [Client.PassdbLookup]  returns [ErrNotImplemented] until Passdb.LookupCredentials ships
//
// One Client owns one TCP conn. Commands serialise behind a mutex, so multiple
// goroutines may call methods concurrently; wire-side ordering is enforced
// internally. For higher throughput, open additional Clients; there is no pool.
package authclient
