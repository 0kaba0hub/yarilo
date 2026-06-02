// yarilo-admin is the unified yarilo operator CLI. Every subsystem
// that needs an "ops surface" lives under a top-level plane:
//
//	director              — talks to yarilo-director's HTTP admin API
//	  status / dump / map / backends / users / ring
//
//	backend               — talks to yarilo-backend-api's HTTP admin API
//	  dict                — pkg/dict KV-store ops
//	  acl                 — RFC 4314 ACL (Phase ACL-1)
//	  quota               — RFC 9208 quota (Phase QUOTA-1)
//	  folder              — mailbox listing / GUID lookup (Phase later)
//	  user                — userdb queries (Phase later)
//	  mailbox             — per-folder operations (Phase later)
//
// Global flags pick the underlying HTTP endpoint per plane:
//
//	--url / --token              → director plane (default :9103)
//	--backend-url / --backend-token → backend plane (default :9105)
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

var (
	apiURL   string
	apiToken string

	backendAPIURL   string
	backendAPIToken string
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	flag.StringVar(&apiURL, "url", envOr("YARILO_ADMIN_URL", "http://localhost:9103"), "Director API base URL (used by 'director' subcommand)")
	flag.StringVar(&apiToken, "token", envOr("YARILO_ADMIN_TOKEN", envOr("DIRECTOR_API_TOKEN", "")), "Director API bearer token")
	flag.StringVar(&backendAPIURL, "backend-url", envOr("YARILO_BACKEND_API_URL", "http://localhost:9105"), "yarilo-backend-api base URL (used by 'backend ...' subcommands)")
	flag.StringVar(&backendAPIToken, "backend-token", envOr("YARILO_BACKEND_API_TOKEN", envOr("BACKEND_API_TOKEN", "")), "yarilo-backend-api bearer token")
	flag.StringVar(&authAddr, "auth-addr", envOr("YARILO_AUTH_ADDR", "localhost:9102"), "yarilo-auth master socket (used by 'auth ...' subcommands)")
	flag.StringVar(&authCert, "auth-cert", envOr("YARILO_AUTH_CERT", ""), "mTLS client cert for auth-master socket")
	flag.StringVar(&authKey, "auth-key", envOr("YARILO_AUTH_KEY", ""), "mTLS client key for auth-master socket")
	flag.StringVar(&authCA, "auth-ca", envOr("YARILO_AUTH_CA", ""), "CA bundle that signs the auth-master server cert")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	if err := dispatch(args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	switch args[0] {
	case "director":
		return dispatchDirector(args[1:])
	case "backend":
		return dispatchBackend(args[1:])
	case "auth":
		return dispatchAuth(args[1:])
	default:
		return fmt.Errorf("unknown plane %q — available: director, backend, auth", args[0])
	}
}

func dispatchBackend(args []string) error {
	if len(args) == 0 {
		printBackendUsage()
		return nil
	}
	switch args[0] {
	case "dict":
		return dispatchDict(args[1:])
	case "folder":
		return dispatchFolder(args[1:])
	case "user":
		return dispatchUser(args[1:])
	case "index":
		return dispatchIndex(args[1:])
	case "mdbox":
		return dispatchMdbox(args[1:])
	case "subscriptions", "subs":
		return dispatchSubs(args[1:])
	case "specialuse":
		return dispatchSpecialUse(args[1:])
	case "metadata":
		return dispatchMetadata(args[1:])
	case "acl":
		return dispatchACL(args[1:])
	case "who":
		return dispatchWho(args[1:])
	case "sessions":
		return dispatchSessions(args[1:])
	default:
		return fmt.Errorf("unknown backend service %q — available: dict, folder, user, index, mdbox, subscriptions, specialuse, metadata, acl, who, sessions (quota lands in a future phase)", args[0])
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `yarilo-admin — yarilo operator CLI

Usage:
  yarilo-admin [global-flags] <plane> <command> [args...]

Global flags:
  --url            Director API base URL (env: YARILO_ADMIN_URL,  default: http://localhost:9103)
  --token          Director API bearer token (env: YARILO_ADMIN_TOKEN)
  --backend-url    Backend API base URL (env: YARILO_BACKEND_API_URL, default: http://localhost:9105)
  --backend-token  Backend API bearer token (env: YARILO_BACKEND_API_TOKEN)

Planes:
  director  — manage the yarilo-director cluster (ring, backends, users, peers)
  backend   — manage a backend's storage state (dict; acl/quota/folder/user/mailbox in later phases)

Run 'yarilo-admin <plane>' with no command for that plane's usage.`)
}

func printBackendUsage() {
	fmt.Fprintln(os.Stderr, `yarilo-admin backend <service> <command>

Services:
  dict     — pkg/dict KV-store ops (lookup, iterate, set, unset, atomic-inc, expire-scan, commit-batch, drivers, exists)

Phase roadmap (services landing in future PRs):
  acl      — RFC 4314 access control on mailboxes
  quota    — RFC 9208 quotas
  folder   — mailbox listing, GUID lookup, special-use queries
  user     — userdb queries
  mailbox  — per-folder ops (delete, rename, recalc-index)

Run 'yarilo-admin backend <service>' with no command for that service's usage.`)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
