// yarilo-admin is the unified yarilo operator CLI. Every subsystem
// that needs an "ops surface" gets a subcommand here.
//
// Subsystems currently wired:
//
//	director  — HTTP client for the yarilo-director admin API
//	dict      — local KV-store ops (pkg/dict) for metadata/quota/ACL/...
//
// Subsystems are dispatched on the FIRST positional arg, so global flags
// (--url / --token) come before the subsystem name and apply to whichever
// subsystem actually uses them (director). Dict subcommands carry their
// own --config / --dict / --driver / --setting flags after the subsystem.
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
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	flag.StringVar(&apiURL, "url", envOr("YARILO_ADMIN_URL", "http://localhost:9103"), "Director API base URL (used by 'director' subcommand)")
	flag.StringVar(&apiToken, "token", envOr("YARILO_ADMIN_TOKEN", envOr("DIRECTOR_API_TOKEN", "")), "Director API bearer token")
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
	case "dict":
		return dispatchDict(args[1:])
	default:
		return fmt.Errorf("unknown subsystem %q — available: director, dict", args[0])
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `yarilo-admin — yarilo operator CLI

Usage:
  yarilo-admin [global-flags] <subsystem> <command> [args...]

Global flags (director only):
  --url    Director API base URL (env: YARILO_ADMIN_URL, default: http://localhost:9103)
  --token  Director API bearer token (env: YARILO_ADMIN_TOKEN)

Subsystems:
  director  — manage the yarilo-director cluster (ring, backends, users, peers)
  dict      — manage pkg/dict KV stores (lookup, iterate, set, unset, atomic-inc, expire-scan)

Run 'yarilo-admin <subsystem>' with no command for that subsystem's usage.`)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
