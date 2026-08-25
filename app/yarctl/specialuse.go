package main

import "fmt"

func dispatchSpecialUse(args []string) error {
	if len(args) == 0 {
		printSpecialUseUsage()
		return nil
	}
	switch args[0] {
	case "list":
		return specialUseList(args[1:])
	case "get":
		return specialUseGet(args[1:])
	case "set":
		return specialUseSet(args[1:])
	case "delete", "del":
		return specialUseDelete(args[1:])
	default:
		return fmt.Errorf("unknown specialuse command %q — available: list, get, set, delete", args[0])
	}
}

func printSpecialUseUsage() {
	fmt.Println(`yarctl backend specialuse <command>

Commands:
  list   <user>                       — overrides + configured defaults
  get    <user> <folder>              — resolved attr + source (override/default/none)
  set    <user> <folder> <attr>       — record special-use attr (e.g. "\Sent")
  delete <user> <folder>  (alias: del) — drop override; default (if any) re-applies

Only the personal namespace carries special-use overrides — RFC 6154
\Sent / \Drafts / etc. semantics do not extend to shared or public.`)
}

func specialUseList(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: yarctl backend specialuse list <user>")
	}
	return printJSON(backendAPIPost("/api/backend/specialuse/list", map[string]any{
		"user": args[0],
	}))
}

func specialUseGet(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: yarctl backend specialuse get <user> <folder>")
	}
	return printJSON(backendAPIPost("/api/backend/specialuse/get", map[string]any{
		"user":   args[0],
		"folder": args[1],
	}))
}

func specialUseSet(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf(`usage: yarctl backend specialuse set <user> <folder> <attr>  (e.g. "\Sent")`)
	}
	return printJSON(backendAPIPost("/api/backend/specialuse/set", map[string]any{
		"user":   args[0],
		"folder": args[1],
		"attr":   args[2],
	}))
}

func specialUseDelete(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: yarctl backend specialuse delete <user> <folder>")
	}
	return printJSON(backendAPIPost("/api/backend/specialuse/delete", map[string]any{
		"user":   args[0],
		"folder": args[1],
	}))
}
