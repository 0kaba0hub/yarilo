package main

import (
	"flag"
	"fmt"
)

func dispatchIndex(args []string) error {
	if len(args) == 0 {
		printIndexUsage()
		return nil
	}
	switch args[0] {
	case "dump":
		return indexDump(args[1:])
	default:
		return fmt.Errorf("unknown index command %q — available: dump (rebuild/optimize in a future phase)", args[0])
	}
}

func printIndexUsage() {
	fmt.Println(`yarilo-admin backend index <command>

Commands:
  dump <user> <folder> [--namespace NS] [--limit N]
        Dump every fileindex record (UID, flags, modseq, size, GUID).

Rebuild / optimize are deferred — they need per-driver resync logic
(different semantics for maildir vs dbox vs mdbox). See TODO.md.`)
}

func indexDump(args []string) error {
	fs := flag.NewFlagSet("index dump", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	limit := fs.Int("limit", 0, "max records to return (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarilo-admin backend index dump <user> <folder> [--namespace NS] [--limit N]")
	}
	return printJSON(backendAPIPost("/api/backend/index/dump", map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
		"limit":     *limit,
	}))
}
