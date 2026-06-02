// Package authclient is the Go client for the yarilo-auth master
// protocol. It opens a single mTLS TCP connection to the master
// listener (the second socket yarilo-auth exposes — see
// internal/auth/protocol/master.go), drains the handshake, and
// provides typed methods for the wire commands the protocol
// surfaces today:
//
//   - [Client.Userdb]        — password-less user lookup (USER)
//   - [Client.IterateUsers]  — enumerate every known user (LIST)
//   - [Client.PassdbLookup]  — placeholder; returns
//     [ErrNotImplemented] until Phase
//     AUTH-2 ships Passdb.LookupCredentials
//
// One Client owns one TCP conn. Commands within a Client serialise
// behind a mutex — multiple goroutines may call methods concurrently
// and the wire-side ordering is enforced internally. Callers that
// need higher throughput open additional Clients; this package does
// not ship a pool today.
//
// Consumers wire authclient at startup with the configured master
// address and the internal mTLS material. The current production
// consumer is yarilo-backend-api (Phase AUTH-1 follow-up); future
// consumers — yarilo-admin's userdb subcommand, the standalone
// smoketest — will Dial the same address.
package authclient
