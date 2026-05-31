package main

import (
	"flag"
	"fmt"
)

func dispatchSubs(args []string) error {
	if len(args) == 0 {
		printSubsUsage()
		return nil
	}
	switch args[0] {
	case "list":
		return subsList(args[1:])
	case "add":
		return subsAdd(args[1:])
	case "remove", "rm":
		return subsRemove(args[1:])
	default:
		return fmt.Errorf("unknown subscriptions command %q — available: list, add, remove", args[0])
	}
}

func printSubsUsage() {
	fmt.Println(`yarilo-admin backend subscriptions <command>

Commands:
  list   <user> [--namespace NS]                — every subscribed folder
  add    <user> <folder> [--namespace NS]       — record SUBSCRIBE
  remove <user> <folder> [--namespace NS]       — drop SUBSCRIBE

Reuses the same on-disk format + lock key as IMAP SUBSCRIBE so
concurrent sessions see admin writes immediately.`)
}

func subsList(args []string) error {
	fs := flag.NewFlagSet("subs list", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarilo-admin backend subscriptions list <user> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/subscriptions/list", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
	}))
}

func subsAdd(args []string) error {
	return subsMutate("add", "/api/backend/subscriptions/add", args)
}

func subsRemove(args []string) error {
	return subsMutate("remove", "/api/backend/subscriptions/remove", args)
}

func subsMutate(cmd, path string, args []string) error {
	fs := flag.NewFlagSet("subs "+cmd, flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarilo-admin backend subscriptions %s <user> <folder> [--namespace NS]", cmd)
	}
	return printJSON(backendAPIPost(path, map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
	}))
}
