package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
)

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
