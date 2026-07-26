package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

// dispatchWho is the `backend who` subtree. Default output is a
// human-readable table — JSON is opt-in via --output json.
//
// Subcommands:
//
//	yarilo-admin backend who [list] [--protocol ...] [--user U] [--output table|json]
//	yarilo-admin backend who count [proto] [--user U] [--by user|protocol] [--output table|json]
func dispatchWho(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "count":
			return whoCount(args[1:])
		case "list":
			return whoList(args[1:])
		}
	}
	return whoList(args)
}

func whoList(args []string) error {
	fs := flag.NewFlagSet("backend who", flag.ContinueOnError)
	service := fs.String("protocol", "", "filter by service (imap | pop3 | submission | lmtp); empty = all")
	user := fs.String("user", "", "filter by user")
	all := fs.Bool("all", false, "cluster-wide view (all backends) with a BACKEND column; default shows only THIS backend's sessions")
	output := fs.String("output", "table", "table | json")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: yarilo-admin backend who [list] [--protocol IMAP] [--user U] [--all] [--output table|json]")
	}
	// Server always returns the JSON shape; CLI decides how to render.
	// Request flat sessions when rendering a table — easier to print.
	groupBy := "user"
	if *output == "table" {
		groupBy = "none"
	}
	body, err := backendAPIPost("/api/backend/who", map[string]any{
		"service":  *service,
		"user":     *user,
		"group_by": groupBy,
		"all":      *all,
	})
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(body, nil)
	}
	return renderWhoTable(os.Stdout, body, *all)
}

func whoCount(args []string) error {
	fs := flag.NewFlagSet("backend who count", flag.ContinueOnError)
	user := fs.String("user", "", "filter by user")
	by := fs.String("by", "", `breakdown dimension: "" (single total), "protocol", or "user"`)
	all := fs.Bool("all", false, "count across all backends; default counts only THIS backend's sessions")
	output := fs.String("output", "table", "table | json")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	service := ""
	if fs.NArg() > 0 {
		service = fs.Arg(0)
	}
	body, err := backendAPIPost("/api/backend/who/count", map[string]any{
		"service": service,
		"user":    *user,
		"by":      *by,
		"all":     *all,
	})
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(body, nil)
	}
	return renderCountTable(os.Stdout, body, *by)
}

// ---- table renderers ------------------------------------------------------

type whoSession struct {
	ID          string `json:"id"`
	User        string `json:"user"`
	IP          string `json:"ip"`
	Service     string `json:"service"`
	ConnectedAt string `json:"connected_at"`
	Folder      string `json:"folder,omitempty"`
	Backend     string `json:"backend,omitempty"`
}

// userGroup aggregates every active session belonging to one user
// into the per-user row shown by `yarilo-admin backend who`.
type userGroup struct {
	user      string
	count     int
	protocols []string
	ips       []string
	folders   []string
	backends  []string
	since     string
}

// renderWhoTable prints the per-user table. showBackend adds a BACKEND column
// (the --all / cluster-wide view, #814); the default local-backend view omits
// it since every row is on this same backend.
func renderWhoTable(w io.Writer, body []byte, showBackend bool) error {
	var resp struct {
		Total    int          `json:"total"`
		Sessions []whoSession `json:"sessions"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse who response: %w", err)
	}
	if resp.Total == 0 {
		fmt.Fprintln(w, "(no active sessions)")
		return nil
	}
	groups := map[string]*userGroup{}
	for _, s := range resp.Sessions {
		g, ok := groups[s.User]
		if !ok {
			g = &userGroup{user: s.User, since: s.ConnectedAt}
			groups[s.User] = g
		}
		g.count++
		g.protocols = append(g.protocols, s.Service)
		g.ips = append(g.ips, s.IP)
		if s.Folder != "" {
			g.folders = append(g.folders, s.Folder)
		}
		if s.Backend != "" {
			g.backends = append(g.backends, s.Backend)
		}
		if s.ConnectedAt < g.since {
			g.since = s.ConnectedAt
		}
	}
	users := make([]string, 0, len(groups))
	for u := range groups {
		users = append(users, u)
	}
	sort.Strings(users)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if showBackend {
		fmt.Fprintln(tw, "USER\tCOUNT\tPROTOCOLS\tIPS\tBACKEND\tFOLDERS\tSINCE")
	} else {
		fmt.Fprintln(tw, "USER\tCOUNT\tPROTOCOLS\tIPS\tFOLDERS\tSINCE")
	}
	for _, u := range users {
		g := groups[u]
		if showBackend {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
				g.user, g.count, joinUnique(g.protocols), joinUnique(g.ips), joinUnique(g.backends), joinUnique(g.folders), g.since)
		} else {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n",
				g.user, g.count, joinUnique(g.protocols), joinUnique(g.ips), joinUnique(g.folders), g.since)
		}
	}
	tw.Flush()
	fmt.Fprintf(w, "\nTotal: %d sessions, %d users\n", resp.Total, len(groups))
	return nil
}

// joinUnique renders a list of strings as a deduped, alpha-sorted
// summary. Wraps the joined result in parentheses when ≥2 distinct
// values are present so the reader can tell a single-value cell
// from a list-cell at a glance. Multiplicity (how many sessions
// share the same value) is intentionally omitted — the per-user
// COUNT column already carries the totals and the per-value count
// added too much visual noise.
//
// Examples:
//
//	["imap", "imap"]                        → "imap"
//	["imap", "submission"]                  → "(imap,submission)"
//	["imap", "imap", "submission"]          → "(imap,submission)"
//	["10.0.0.15", "10.0.0.15", "10.0.0.15"] → "10.0.0.15"
func joinUnique(items []string) string {
	seen := map[string]struct{}{}
	for _, item := range items {
		seen[item] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	joined := strings.Join(keys, ",")
	if len(keys) >= 2 {
		return "(" + joined + ")"
	}
	return joined
}

func renderCountTable(w io.Writer, body []byte, by string) error {
	var resp struct {
		Total      int            `json:"total"`
		Service    string         `json:"service"`
		User       string         `json:"user"`
		ByProtocol map[string]int `json:"by_protocol"`
		ByUser     map[string]int `json:"by_user"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse count response: %w", err)
	}
	// Plain `count` — just the number, scriptable with $(...).
	if by == "" && len(resp.ByProtocol) == 0 && len(resp.ByUser) == 0 {
		fmt.Fprintln(w, resp.Total)
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	switch {
	case len(resp.ByProtocol) > 0:
		fmt.Fprintln(tw, "PROTOCOL\tCOUNT")
		keys := make([]string, 0, len(resp.ByProtocol))
		for k := range resp.ByProtocol {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(tw, "%s\t%d\n", k, resp.ByProtocol[k])
		}
		fmt.Fprintf(tw, "TOTAL\t%d\n", resp.Total)
	case len(resp.ByUser) > 0:
		fmt.Fprintln(tw, "USER\tCOUNT")
		keys := make([]string, 0, len(resp.ByUser))
		for k := range resp.ByUser {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(tw, "%s\t%d\n", k, resp.ByUser[k])
		}
		fmt.Fprintf(tw, "TOTAL\t%d\n", resp.Total)
	}
	tw.Flush()
	return nil
}
