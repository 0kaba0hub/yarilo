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
	case "rebuild":
		return indexRebuild(args[1:])
	case "optimize":
		return indexOptimize(args[1:])
	default:
		return fmt.Errorf("unknown index command %q — available: dump, rebuild, optimize", args[0])
	}
}

func printIndexUsage() {
	fmt.Println(`yarilo-admin backend index <command>

Commands:
  dump     <user> <folder> [--namespace NS] [--limit N]
        Dump every fileindex record (UID, flags, modseq, size, GUID).

  rebuild  <user> <folder> [--namespace NS]
        Scan the on-disk storage and regenerate the fileindex,
        preserving UIDs for filenames already known to the index.
        mdbox returns 501 Not Implemented — see Phase MDBOX-PROD-READY.

  optimize <user> <folder> [--namespace NS]
        Compact the .index.log overlay into the base .index file.
        No semantic change; safe to run while no IMAP session
        references this folder.`)
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

func indexRebuild(args []string) error {
	fs := flag.NewFlagSet("index rebuild", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarilo-admin backend index rebuild <user> <folder> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/index/rebuild", map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
	}))
}

func indexOptimize(args []string) error {
	fs := flag.NewFlagSet("index optimize", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarilo-admin backend index optimize <user> <folder> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/index/optimize", map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
	}))
}
