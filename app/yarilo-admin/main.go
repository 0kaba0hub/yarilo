// yarilo-admin is a CLI tool for the yarilo-director HTTP admin API.
// Designed to run inside the director pod — no flags required in standard deployments.
//
// Environment variables (read automatically, no flags needed):
//
//	YARILO_ADMIN_URL   — API base URL (default: http://localhost:9103)
//	YARILO_ADMIN_TOKEN — Bearer token (fallback: DIRECTOR_API_TOKEN)
//
// Usage (override via flags if needed):
//
//	yarilo-admin [--url URL] [--token TOKEN] <resource> <action> [args...]
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
)

var (
	apiURL   string
	apiToken string
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	flag.StringVar(&apiURL, "url", envOr("YARILO_ADMIN_URL", "http://localhost:9103"), "API base URL")
	// Token: explicit env YARILO_ADMIN_TOKEN, then DIRECTOR_API_TOKEN (same secret as the server).
	flag.StringVar(&apiToken, "token", envOr("YARILO_ADMIN_TOKEN", envOr("DIRECTOR_API_TOKEN", "")), "Bearer token")
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
	if len(args) == 0 {
		return fmt.Errorf("no command given")
	}
	switch args[0] {
	case "director":
		return dispatchDirector(args[1:])
	default:
		return fmt.Errorf("unknown resource %q — available: director", args[0])
	}
}

func dispatchDirector(args []string) error {
	if len(args) == 0 {
		printDirectorUsage()
		return nil
	}
	switch args[0] {
	case "status":
		return printJSON(apiGet("/api/director/status"))
	case "dump":
		return printJSON(apiGet("/api/director/dump"))
	case "map":
		fs := flag.NewFlagSet("map", flag.ExitOnError)
		user := fs.String("user", "", "username to look up")
		fs.Parse(args[1:]) //nolint:errcheck
		path := "/api/director/map"
		if *user != "" {
			path += "?user=" + url.QueryEscape(*user)
		}
		return printJSON(apiGet(path))
	case "backends":
		return dispatchBackends(args[1:])
	case "users":
		return dispatchUsers(args[1:])
	case "ring":
		return dispatchRing(args[1:])
	default:
		return fmt.Errorf("unknown director command %q", args[0])
	}
}

func dispatchBackends(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: director backends <list|add|remove|update|up|down|flush>")
	}
	switch args[0] {
	case "list":
		return printJSON(apiGet("/api/director/backends"))
	case "add":
		fs := flag.NewFlagSet("backends add", flag.ExitOnError)
		port := fs.Int("port", 0, "backend port (required)")
		tag := fs.String("tag", "", "backend tag")
		vhosts := fs.Int("vhosts", 0, "virtual nodes (0 = default 100)")
		fs.Parse(args[1:]) //nolint:errcheck
		if fs.NArg() == 0 || *port == 0 {
			return fmt.Errorf("usage: director backends add <ip> --port PORT [--tag TAG] [--vhosts N]")
		}
		return printJSON(apiPost("/api/director/backends", map[string]any{
			"ip": fs.Arg(0), "port": *port, "tag": *tag, "vhosts": *vhosts,
		}))
	case "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: director backends remove <ip>")
		}
		return printJSON(apiDelete("/api/director/backends/" + args[1]))
	case "update":
		fs := flag.NewFlagSet("backends update", flag.ExitOnError)
		vhosts := fs.Int("vhosts", 0, "virtual nodes")
		fs.Parse(args[1:]) //nolint:errcheck
		if fs.NArg() == 0 {
			return fmt.Errorf("usage: director backends update <ip> --vhosts N")
		}
		return printJSON(apiPatch("/api/director/backends/"+fs.Arg(0), map[string]any{"vhosts": *vhosts}))
	case "up":
		if len(args) < 2 {
			return fmt.Errorf("usage: director backends up <ip>")
		}
		return printJSON(apiPost("/api/director/backends/"+args[1]+"/up", nil))
	case "down":
		if len(args) < 2 {
			return fmt.Errorf("usage: director backends down <ip>")
		}
		return printJSON(apiPost("/api/director/backends/"+args[1]+"/down", nil))
	case "flush":
		if len(args) < 2 {
			return fmt.Errorf("usage: director backends flush <ip|all>")
		}
		return printJSON(apiPost("/api/director/backends/"+args[1]+"/flush", nil))
	default:
		return fmt.Errorf("unknown backends command %q", args[0])
	}
}

func dispatchUsers(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: director users <move|kick>")
	}
	switch args[0] {
	case "move":
		fs := flag.NewFlagSet("users move", flag.ExitOnError)
		backend := fs.String("backend", "", "target backend as ip:port")
		backendIP := fs.String("ip", "", "target backend IP")
		backendPort := fs.Int("port", 0, "target backend port")
		fs.Parse(args[1:]) //nolint:errcheck
		if fs.NArg() == 0 {
			return fmt.Errorf("usage: director users move <user> --backend ip:port")
		}
		body := map[string]any{"backend": *backend, "ip": *backendIP, "port": *backendPort}
		return printJSON(apiPost("/api/director/users/"+url.PathEscape(fs.Arg(0))+"/move", body))
	case "kick":
		if len(args) < 2 {
			return fmt.Errorf("usage: director users kick <user>")
		}
		return printJSON(apiPost("/api/director/users/"+url.PathEscape(args[1])+"/kick", nil))
	default:
		return fmt.Errorf("unknown users command %q", args[0])
	}
}

func dispatchRing(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: director ring <status|add|remove>")
	}
	switch args[0] {
	case "status":
		return printJSON(apiGet("/api/director/ring"))
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: director ring add <addr>")
		}
		return printJSON(apiPost("/api/director/ring", map[string]any{"addr": args[1]}))
	case "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: director ring remove <addr>")
		}
		return printJSON(apiDelete("/api/director/ring?addr=" + url.QueryEscape(args[1])))
	default:
		return fmt.Errorf("unknown ring command %q", args[0])
	}
}

// --- HTTP helpers ---

func apiGet(path string) ([]byte, error) {
	return doRequest(http.MethodGet, path, nil)
}

func apiPost(path string, body any) ([]byte, error) {
	return doRequest(http.MethodPost, path, body)
}

func apiPatch(path string, body any) ([]byte, error) {
	return doRequest(http.MethodPatch, path, body)
}

func apiDelete(path string) ([]byte, error) {
	return doRequest(http.MethodDelete, path, nil)
}

func doRequest(method, path string, body any) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(apiURL, "/")+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func printJSON(data []byte, err error) error {
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if jsonErr := json.Indent(&buf, data, "", "  "); jsonErr != nil {
		fmt.Println(string(data))
		return nil
	}
	fmt.Println(buf.String())
	return nil
}

// --- usage ---

func printUsage() {
	fmt.Fprintln(os.Stderr, `yarilo-admin — yarilo-director HTTP admin CLI

Usage:
  yarilo-admin [--url URL] [--token TOKEN] <resource> <action> [args...]

Flags:
  --url    API base URL (env: YARILO_ADMIN_URL, default: http://localhost:9103)
  --token  Bearer token  (env: YARILO_ADMIN_TOKEN)

Resources:
  director — ring, backends, users, peers

Run 'yarilo-admin director' for director subcommands.`)
}

func printDirectorUsage() {
	fmt.Fprintln(os.Stderr, `yarilo-admin director <command>

Commands:
  status                              Overall status (backends + peers)
  dump                                Full state dump (backends, users, peers)
  map [--user USER]                   User→backend mapping; all if no --user

  backends list                       List backends
  backends add IP --port PORT         Add backend [--tag TAG] [--vhosts N]
  backends remove IP                  Remove backend from ring
  backends update IP --vhosts N       Update virtual node count
  backends up IP                      Mark backend up
  backends down IP                    Mark backend down (flush)
  backends flush IP|all               Flush backend or all backends

  users move USER --backend IP:PORT   Force-move user to backend
  users kick USER                     Kick user (disconnect all sessions)

  ring status                         List director peers
  ring add ADDR                       Add peer (addr = ip:port)
  ring remove ADDR                    Remove peer`)
}

// --- helpers ---

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
