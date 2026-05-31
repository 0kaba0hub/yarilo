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
	default:
		return fmt.Errorf("unknown user command %q — available: info, usage", args[0])
	}
}

func printUserUsage() {
	fmt.Println(`yarilo-admin backend user <command>

Commands:
  info   <user>   — username, resolved home, configured namespaces
  usage  <user>   — per-folder message + byte totals across every namespace`)
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
