package main

import (
	"flag"
	"fmt"
)

func dispatchMdbox(args []string) error {
	if len(args) == 0 {
		printMdboxUsage()
		return nil
	}
	switch args[0] {
	case "purge":
		return mdboxPurge(args[1:])
	case "altmove":
		return mdboxAltMove(args[1:])
	default:
		return fmt.Errorf("unknown mdbox command %q — available: purge, altmove", args[0])
	}
}

func printMdboxUsage() {
	fmt.Println(`yarctl backend mdbox <command>

Commands:
  purge <user> [--namespace NS]
        Compact the user's mdbox storage tree: every m.<N> with
        at least one zero-ref record is rewritten without those
        records (or unlinked entirely if all records are dead).
        The global map is rewritten atomically; per-folder
        indexes referencing live map_uids continue to work
        without per-folder I/O.

  altmove <user> [--namespace NS] [--before RFC3339] [--reverse]
        Move messages to alt (cold) storage. (yarctl mdbox altmove)
        --before:  only move messages with InternalDate before this
                   RFC3339 timestamp (e.g. 2025-01-01T00:00:00Z).
                   Omit to move all messages.
        --reverse: move FROM alt storage back to primary.
        Requires storage.mdbox_alt_storage_path to be configured.`)
}

func mdboxPurge(args []string) error {
	fs := flag.NewFlagSet("mdbox purge", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarctl backend mdbox purge <user> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/mdbox/purge", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
	}))
}

func mdboxAltMove(args []string) error {
	fs := flag.NewFlagSet("mdbox altmove", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	before := fs.String("before", "", "RFC3339 cutoff — move messages with InternalDate before this timestamp")
	reverse := fs.Bool("reverse", false, "move FROM alt storage back to primary")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarctl backend mdbox altmove <user> [--namespace NS] [--before RFC3339] [--reverse]")
	}
	return printJSON(backendAPIPost("/api/backend/mdbox/altmove", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
		"before":    *before,
		"reverse":   *reverse,
	}))
}
