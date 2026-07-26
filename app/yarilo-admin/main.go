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
//
// When YARILO_ADMIN_TYPE is set the plane is pre-selected and the first
// CLI argument is the service/command directly (no plane prefix needed):
//
//	YARILO_ADMIN_TYPE=backend yarilo-admin user info u1@example.com
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

var (
	apiURL   string
	apiToken string

	backendAPIURL   string
	backendAPIToken string
	backendAPIPort  int    // per-user routing: pod backend-api port (#792)
	routeFlag       string // "auto" | "true" | "false"
	routeByUser     bool   // resolved: route per-user backend ops via director LOOKUP

	outputFormat string
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	flag.StringVar(&apiURL, "url", envOr("YARILO_ADMIN_URL", "http://localhost:9103"), "Director API base URL (used by 'director' subcommand)")
	flag.StringVar(&apiToken, "token", envOr("YARILO_ADMIN_TOKEN", envOr("DIRECTOR_API_TOKEN", "")), "Director API bearer token")
	flag.StringVar(&backendAPIURL, "backend-url", envOr("YARILO_BACKEND_API_URL", "http://localhost:9105"), "yarilo-backend-api base URL (used by 'backend ...' subcommands)")
	flag.StringVar(&backendAPIToken, "backend-token", envOr("YARILO_BACKEND_API_TOKEN", envOr("BACKEND_API_TOKEN", "")), "yarilo-backend-api bearer token")
	flag.IntVar(&backendAPIPort, "backend-port", envOrInt("YARILO_BACKEND_API_PORT", 9105), "backend-api port on the resolved pod (per-user routing, #792)")
	flag.StringVar(&routeFlag, "route-by-user", envOr("YARILO_ADMIN_ROUTE_BY_USER", "auto"), "Route per-user backend ops to the user's pod via a director LOOKUP: auto (on when a director URL is configured) | true | false (escape hatch: talk to --backend-url directly)")
	flag.StringVar(&authAddr, "auth-addr", envOr("YARILO_AUTH_ADDR", "localhost:9102"), "yarilo-auth master socket (used by 'auth ...' subcommands)")
	flag.StringVar(&authCert, "auth-cert", envOr("YARILO_AUTH_CERT", ""), "mTLS client cert for auth-master socket")
	flag.StringVar(&authKey, "auth-key", envOr("YARILO_AUTH_KEY", ""), "mTLS client key for auth-master socket")
	flag.StringVar(&authCA, "auth-ca", envOr("YARILO_AUTH_CA", ""), "CA bundle that signs the auth-master server cert")
	flag.StringVar(&outputFormat, "O", "human", "Output format: human or json")
	flag.Parse()

	switch outputFormat {
	case "human", "json":
	default:
		fmt.Fprintf(os.Stderr, "yarilo-admin: unknown output format %q (valid: human, json)\n", outputFormat)
		os.Exit(1)
	}

	// YARILO_API_URL / YARILO_API_TOKEN override plane-specific vars when set.
	// They are injected by the Helm chart so every pod is pre-configured.
	if v := os.Getenv("YARILO_API_URL"); v != "" {
		switch os.Getenv("YARILO_ADMIN_TYPE") {
		case "backend":
			backendAPIURL = v
		case "director":
			apiURL = v
		}
	}
	if v := os.Getenv("YARILO_API_TOKEN"); v != "" {
		switch os.Getenv("YARILO_ADMIN_TYPE") {
		case "backend":
			backendAPIToken = v
		case "director":
			apiToken = v
		}
	}

	// Resolve per-user routing (#792). "auto" turns routing on when a director
	// URL was explicitly configured (env or --url flag) — the co-located
	// topology. A bare default director URL (no env, no flag) means standalone,
	// so routing stays off and the fixed --backend-url is used unchanged.
	switch strings.ToLower(routeFlag) {
	case "true", "on", "yes", "1":
		routeByUser = true
	case "false", "off", "no", "0":
		routeByUser = false
	default: // auto
		// Key ONLY on director-plane signals. YARILO_API_URL is plane-ambiguous
		// (adminBackendEnv sets it to the BACKEND API), so counting it here
		// false-positives inside a backend pod — routing on with no director to
		// LOOKUP against. A real director is signalled by YARILO_ADMIN_URL (the
		// adminDirectorEnv var) or an explicit --url.
		directorConfigured := os.Getenv("YARILO_ADMIN_URL") != "" || flagSet("url")
		routeByUser = directorConfigured
	}

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
	// When YARILO_ADMIN_TYPE is set the plane is implicit — first arg is the
	// service/command directly. Top-level "user" shorthand always delegates to
	// the backend plane regardless of YARILO_ADMIN_TYPE.
	adminType := os.Getenv("YARILO_ADMIN_TYPE")
	if adminType != "" {
		switch adminType {
		case "backend":
			return dispatchBackend(args)
		case "director":
			return dispatchDirector(args)
		case "auth":
			return dispatchAuth(args)
		default:
			return fmt.Errorf("unknown YARILO_ADMIN_TYPE %q — valid values: backend, director, auth", adminType)
		}
	}

	switch args[0] {
	case "director":
		return dispatchDirector(args[1:])
	case "backend":
		return dispatchBackend(args[1:])
	case "auth":
		return dispatchAuth(args[1:])
	case "user":
		// Top-level shorthand: yarilo-admin user <cmd> — always hits backend plane.
		return dispatchUser(args[1:])
	default:
		return fmt.Errorf("unknown plane %q — available: director, backend, auth\n(tip: set YARILO_ADMIN_TYPE=backend to skip the plane prefix)", args[0])
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
	case "quota":
		return dispatchQuota(args[1:])
	case "fts":
		return dispatchFTS(args[1:])
	default:
		return fmt.Errorf("unknown backend service %q — available: dict, folder, user, index, mdbox, subscriptions, specialuse, metadata, acl, who, sessions, quota, fts", args[0])
	}
}

func printUsage() {
	adminType := os.Getenv("YARILO_ADMIN_TYPE")
	if adminType != "" {
		fmt.Fprintf(os.Stderr, `yarilo-admin — yarilo operator CLI (plane: %s)

YARILO_ADMIN_TYPE=%s is set — plane prefix is implicit.

Usage:
  yarilo-admin <service> <command> [args...]

`, adminType, adminType)
		switch adminType {
		case "backend":
			printBackendUsage()
		case "director":
			fmt.Fprintln(os.Stderr, "Run 'yarilo-admin status|dump|map|backends|users|ring' directly.")
		}
		return
	}
	fmt.Fprintln(os.Stderr, `yarilo-admin — yarilo operator CLI

Usage:
  yarilo-admin [global-flags] <plane> <command> [args...]

Global flags:
  -O human|json    Output format (default: human); human renderers added per command over time
  --url            Director API base URL (env: YARILO_ADMIN_URL,  default: http://localhost:9103)
  --token          Director API bearer token (env: YARILO_ADMIN_TOKEN)
  --backend-url    Backend API base URL (env: YARILO_BACKEND_API_URL, default: http://localhost:9105)
  --backend-token  Backend API bearer token (env: YARILO_BACKEND_API_TOKEN)

Planes:
  director  — manage the yarilo-director cluster (ring, backends, users, peers)
  backend   — manage a backend's storage state (dict, acl, quota, folder, user, ...)
  auth      — query yarilo-auth userdb/passdb

Shorthand (no plane prefix):
  user      — alias for 'backend user' (mirrors doveadm user)

Tip: set YARILO_ADMIN_TYPE=backend|director|auth to skip the plane prefix entirely.

Run 'yarilo-admin <plane>' with no command for that plane's usage.`)
}

func printBackendUsage() {
	fmt.Fprintln(os.Stderr, `yarilo-admin backend <service> <command>

Services:
  dict          — pkg/dict KV-store ops (lookup, iterate, set, unset, atomic-inc, expire-scan, commit-batch, drivers, exists)
  acl           — RFC 4314 access control on mailboxes
  quota         — RFC 9208 quota counters (show, recalc, set)
  folder        — mailbox listing, GUID lookup, special-use queries
  user          — userdb queries
  index         — fileindex ops
  mdbox         — mdbox map ops
  subscriptions — IMAP SUBSCRIBE state
  specialuse    — special-use annotations
  metadata      — IMAP METADATA (RFC 5464)
  who           — active session listing
  sessions      — session management (kick)

Run 'yarilo-admin backend <service>' with no command for that service's usage.`)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// flagSet reports whether a flag was explicitly passed on the command line.
func flagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
