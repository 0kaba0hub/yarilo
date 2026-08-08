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
	case "migrate":
		return subsMigrate(args[1:])
	default:
		return fmt.Errorf("unknown subscriptions command %q — available: list, add, remove, migrate", args[0])
	}
}

func printSubsUsage() {
	fmt.Println(`yarctl backend subscriptions <command>

Commands:
  list   <user> [--namespace NS]                — every subscribed folder
  add    <user> <folder> [--namespace NS]       — record SUBSCRIBE
  remove <user> <folder> [--namespace NS]       — drop SUBSCRIBE
  migrate <user> --namespace NS [--apply]       — fold a namespace's old per-namespace
                                                  subscription file into the user's own
                                                  (subscriptions follow the subscriber);
                                                  dry run unless --apply

Reuses the same on-disk format + lock key as IMAP SUBSCRIBE so
concurrent sessions see admin writes immediately.`)
}

func subsList(args []string) error {
	fs := flag.NewFlagSet("subs list", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarctl backend subscriptions list <user> [--namespace NS]")
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
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend subscriptions %s <user> <folder> [--namespace NS]", cmd)
	}
	return printJSON(backendAPIPost(path, map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
	}))
}

// subsMigrate folds a namespace's old per-namespace subscription file into the
// user's own. Idempotent: once the old file is gone a repeat run finds nothing.
func subsMigrate(args []string) error {
	fs := flag.NewFlagSet("subs migrate", flag.ContinueOnError)
	ns := fs.String("namespace", "", "namespace slug (the one that no longer keeps its own subscriptions)")
	apply := fs.Bool("apply", false, "write, instead of reporting what would change")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 || *ns == "" {
		return fmt.Errorf("usage: yarctl backend subscriptions migrate <user> --namespace NS [--apply]")
	}
	return printJSON(backendAPIPost("/api/backend/subscriptions/migrate", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
		"apply":     *apply,
	}))
}
