package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"
	"time"
)

// humanBackends renders a {"backends":[...]} response as an aligned table.
func humanBackends(data []byte) error {
	var r struct {
		Backends []struct {
			IP       string `json:"ip"`
			Port     int    `json:"port"`
			Tag      string `json:"tag"`
			Up       bool   `json:"up"`
			Vhosts   int    `json:"vhosts"`
			Sessions int    `json:"sessions"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	if len(r.Backends) == 0 {
		fmt.Println("no backends")
		return nil
	}
	fmt.Printf("%-24s %-10s %-6s %-7s %s\n", "BACKEND", "TAG", "STATE", "VHOSTS", "SESSIONS")
	for _, b := range r.Backends {
		state := "down"
		if b.Up {
			state = "up"
		}
		// A co-located backend registers a nominal port 0 (the pod IP serves
		// many protocol ports; the login proxy picks the port). Show just the
		// IP in that case; static / admin-added backends keep ip:port.
		addr := b.IP
		if b.Port != 0 {
			addr = fmt.Sprintf("%s:%d", b.IP, b.Port)
		}
		fmt.Printf("%-24s %-10s %-6s %-7d %d\n", addr, b.Tag, state, b.Vhosts, b.Sessions)
	}
	return nil
}

// humanRingStatus renders the rich ring-status object (#833) as an aligned
// per-member table: this replica's computed neighbors, its live edges (link
// role/state/uptime), and the sparse dedup watermark — plus any tombstones.
func humanRingStatus(data []byte) error {
	var r struct {
		Self    string `json:"self"`
		Size    int    `json:"size"`
		Members []struct {
			Addr  string  `json:"addr"`
			Index int     `json:"index"`
			Self  bool    `json:"self"`
			Left  *string `json:"left"`
			Right *string `json:"right"`
			Seq   *uint64 `json:"seq"`
			Link  *struct {
				Role  string  `json:"role"`
				State string  `json:"state"`
				Since *string `json:"since"`
			} `json:"link"`
		} `json:"members"`
		Tombstones []struct {
			Addr string `json:"addr"`
			Age  string `json:"age"`
		} `json:"tombstones"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}

	fmt.Printf("ring status: %d director%s (self %s)\n", r.Size, plural(r.Size), r.Self)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "IDX\tADDR\tLEFT | RIGHT\tLINK\tSEQ")
	for _, m := range r.Members {
		marker := m.Addr
		if m.Self {
			marker = "* " + m.Addr
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			m.Index, marker, neighborsCol(m.Left, m.Right), linkCol(m.Self, m.Link), seqCol(m.Seq))
	}
	tw.Flush()

	if len(r.Tombstones) > 0 {
		fmt.Printf("tombstones (%d):\n", len(r.Tombstones))
		for _, t := range r.Tombstones {
			fmt.Printf("  %s (age %s)\n", t.Addr, t.Age)
		}
	}
	return nil
}

func neighborsCol(left, right *string) string {
	l, rt := "-", "-"
	if left != nil {
		l = *left
	}
	if right != nil {
		rt = *right
	}
	return l + " | " + rt
}

func linkCol(self bool, link *struct {
	Role  string  `json:"role"`
	State string  `json:"state"`
	Since *string `json:"since"`
}) string {
	if self {
		return "(self)"
	}
	if link == nil {
		return "-" // not a direct neighbor of this replica
	}
	if link.State == "connected" && link.Since != nil {
		if t, err := time.Parse(time.RFC3339, *link.Since); err == nil {
			return fmt.Sprintf("%s connected %s", link.Role, time.Since(t).Round(time.Second))
		}
		return link.Role + " connected"
	}
	return link.Role + " " + link.State
}

func seqCol(seq *uint64) string {
	if seq == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *seq)
}

// humanStatus renders {"status":"ok"} action replies as a single line.
func humanStatus(data []byte) error {
	var r struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.Status == "" {
		fmt.Println("ok")
		return nil
	}
	fmt.Println(r.Status)
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func dispatchDirector(args []string) error {
	if len(args) == 0 {
		printDirectorUsage()
		return nil
	}
	switch args[0] {
	case "status":
		data, err := apiGet("/api/director/status")
		return printOutput(data, err, humanBackends)
	case "dump":
		data, err := apiGet("/api/director/dump")
		return printOutput(data, err, nil) // full dump stays JSON
	case "map":
		fs := flag.NewFlagSet("map", flag.ExitOnError)
		user := fs.String("user", "", "username to look up (introspects the stored pin, no side effect)")
		parseFlags(fs, args[1:]) //nolint:errcheck
		path := "/api/director/map"
		if *user != "" {
			// peek: pure introspection (#813) — reports the stored pin (or
			// "pinned": false) without resolving/assigning. The unpeeked
			// ?user= endpoint is the routing resolver used internally by
			// per-user backend ops and must not be used for inspection.
			path += "?user=" + url.QueryEscape(*user) + "&peek=1"
		}
		data, err := apiGet(path)
		return printOutput(data, err, nil)
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
		data, err := apiGet("/api/director/backends")
		return printOutput(data, err, humanBackends)
	case "add":
		fs := flag.NewFlagSet("backends add", flag.ExitOnError)
		port := fs.Int("port", 0, "backend port (required)")
		tag := fs.String("tag", "", "backend tag")
		vhosts := fs.Int("vhosts", 0, "virtual nodes (0 = default 100)")
		parseFlags(fs, args[1:]) //nolint:errcheck
		if fs.NArg() == 0 || *port == 0 {
			return fmt.Errorf("usage: director backends add <ip> --port PORT [--tag TAG] [--vhosts N]")
		}
		data, err := apiPost("/api/director/backends", map[string]any{
			"ip": fs.Arg(0), "port": *port, "tag": *tag, "vhosts": *vhosts,
		})
		return printOutput(data, err, humanStatus)
	case "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: director backends remove <ip>")
		}
		data, err := apiDelete("/api/director/backends/" + args[1])
		return printOutput(data, err, humanStatus)
	case "update":
		fs := flag.NewFlagSet("backends update", flag.ExitOnError)
		vhosts := fs.Int("vhosts", 0, "virtual nodes")
		parseFlags(fs, args[1:]) //nolint:errcheck
		if fs.NArg() == 0 {
			return fmt.Errorf("usage: director backends update <ip> --vhosts N")
		}
		data, err := apiPatch("/api/director/backends/"+fs.Arg(0), map[string]any{"vhosts": *vhosts})
		return printOutput(data, err, humanStatus)
	case "up":
		if len(args) < 2 {
			return fmt.Errorf("usage: director backends up <ip>")
		}
		data, err := apiPost("/api/director/backends/"+args[1]+"/up", nil)
		return printOutput(data, err, humanStatus)
	case "down":
		if len(args) < 2 {
			return fmt.Errorf("usage: director backends down <ip>")
		}
		data, err := apiPost("/api/director/backends/"+args[1]+"/down", nil)
		return printOutput(data, err, humanStatus)
	case "flush":
		if len(args) < 2 {
			return fmt.Errorf("usage: director backends flush <ip|all>")
		}
		data, err := apiPost("/api/director/backends/"+args[1]+"/flush", nil)
		return printOutput(data, err, humanStatus)
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
		parseFlags(fs, args[1:]) //nolint:errcheck
		if fs.NArg() == 0 {
			return fmt.Errorf("usage: director users move <user> --backend ip:port")
		}
		body := map[string]any{"backend": *backend, "ip": *backendIP, "port": *backendPort}
		data, err := apiPost("/api/director/users/"+url.PathEscape(fs.Arg(0))+"/move", body)
		return printOutput(data, err, humanStatus)
	case "kick":
		if len(args) < 2 {
			return fmt.Errorf("usage: director users kick <user>")
		}
		data, err := apiPost("/api/director/users/"+url.PathEscape(args[1])+"/kick", nil)
		return printOutput(data, err, humanStatus)
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
		data, err := apiGet("/api/director/ring")
		return printOutput(data, err, humanRingStatus)
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: director ring add <addr>")
		}
		data, err := apiPost("/api/director/ring", map[string]any{"addr": args[1]})
		return printOutput(data, err, humanStatus)
	case "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: director ring remove <addr>")
		}
		data, err := apiDelete("/api/director/ring?addr=" + url.QueryEscape(args[1]))
		return printOutput(data, err, humanStatus)
	default:
		return fmt.Errorf("unknown ring command %q", args[0])
	}
}

func printDirectorUsage() {
	fmt.Fprintln(os.Stderr, `yarilo-admin director <command>

Commands:
  status                              Backend routing status (use 'ring status' for directors)
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

  ring status                         Ring topology of the queried replica: neighbors, link state, seq
  ring add ADDR                       Add peer (addr = ip:port)
  ring remove ADDR                    Remove peer`)
}
