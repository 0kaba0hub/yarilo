package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
)

// humanBackends renders a {"backends":[...]} response as an aligned table.
func humanBackends(data []byte) error {
	var r struct {
		Backends []struct {
			IP     string `json:"ip"`
			Port   int    `json:"port"`
			Tag    string `json:"tag"`
			Up     bool   `json:"up"`
			Vhosts int    `json:"vhosts"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	if len(r.Backends) == 0 {
		fmt.Println("no backends")
		return nil
	}
	fmt.Printf("%-24s %-10s %-6s %s\n", "BACKEND", "TAG", "STATE", "VHOSTS")
	for _, b := range r.Backends {
		state := "down"
		if b.Up {
			state = "up"
		}
		fmt.Printf("%-24s %-10s %-6s %d\n", fmt.Sprintf("%s:%d", b.IP, b.Port), b.Tag, state, b.Vhosts)
	}
	return nil
}

// humanPeers renders a {"peers":[...]} response as one member per line.
func humanPeers(data []byte) error {
	var r struct {
		Peers []string `json:"peers"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	fmt.Printf("%d director%s:\n", len(r.Peers), plural(len(r.Peers)))
	for _, p := range r.Peers {
		fmt.Printf("  %s\n", p)
	}
	return nil
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
		user := fs.String("user", "", "username to look up")
		parseFlags(fs, args[1:]) //nolint:errcheck
		path := "/api/director/map"
		if *user != "" {
			path += "?user=" + url.QueryEscape(*user)
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
		return printOutput(data, err, humanPeers)
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

  ring status                         List director peers
  ring add ADDR                       Add peer (addr = ip:port)
  ring remove ADDR                    Remove peer`)
}
