package main

import "fmt"

func dispatchUser(args []string) error {
	if len(args) == 0 {
		printUserUsage()
		return nil
	}
	switch args[0] {
	case "info":
		return userInfo(args[1:])
	case "usage":
		return userUsage(args[1:])
	case "iterate", "list":
		return userIterate(args[1:])
	default:
		return fmt.Errorf("unknown user command %q — available: info, usage, iterate", args[0])
	}
}

func printUserUsage() {
	fmt.Println(`yarilo-admin backend user <command>

Commands:
  info     <user>  — username, resolved home, configured namespaces,
                     and (when yarilo-auth is wired into
                     backend_api.auth_master_addr) the userdb block
                     plus userdb_status ("ok" / "not_found" / "error").
  usage    <user>  — per-folder message + byte totals across every namespace.
  iterate          — every username yarilo-auth's userdb backend can
                     enumerate. Requires backend_api.auth_master_addr
                     to be configured; 503 otherwise.`)
}

func userInfo(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: yarilo-admin backend user info <user>")
	}
	return printJSON(backendAPIPost("/api/backend/user/info", map[string]any{"user": args[0]}))
}

func userUsage(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: yarilo-admin backend user usage <user>")
	}
	return printJSON(backendAPIPost("/api/backend/user/usage", map[string]any{"user": args[0]}))
}

func userIterate(_ []string) error {
	return printJSON(backendAPIPost("/api/backend/user/iterate", map[string]any{}))
}
