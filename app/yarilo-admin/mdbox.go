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
	default:
		return fmt.Errorf("unknown mdbox command %q — available: purge", args[0])
	}
}

func printMdboxUsage() {
	fmt.Println(`yarilo-admin backend mdbox <command>

Commands:
  purge <user> [--namespace NS]
        Compact the user's mdbox storage tree: every m.<N> with
        at least one zero-ref record is rewritten without those
        records (or unlinked entirely if all records are dead).
        The global map is rewritten atomically; per-folder
        indexes referencing live map_uids continue to work
        without per-folder I/O.`)
}

func mdboxPurge(args []string) error {
	fs := flag.NewFlagSet("mdbox purge", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarilo-admin backend mdbox purge <user> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/mdbox/purge", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
	}))
}
