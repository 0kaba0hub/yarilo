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
	case "rebuild-storage":
		return indexRebuildStorage(args[1:])
	case "cache-purge":
		return indexCachePurge(args[1:])
	case "optimize":
		return indexOptimize(args[1:])
	default:
		return fmt.Errorf("unknown index command %q — available: dump, rebuild, rebuild-storage, optimize, cache-purge", args[0])
	}
}

func printIndexUsage() {
	fmt.Println(`yarctl backend index <command>

Commands:
  dump     <user> <folder> [--namespace NS] [--limit N]
        Dump every fileindex record (UID, flags, modseq, size, GUID).

  rebuild  <user> <folder> [--namespace NS]
        Scan the on-disk storage and regenerate ONE folder's fileindex,
        preserving UIDs for filenames already known to the index.
        For maildir / sdbox. Returns 501 for mdbox — its storage is
        folder-agnostic, so use rebuild-storage instead.

  rebuild-storage <user> [--namespace NS] [--restore-orphans]
        Storage-wide rebuild for mdbox: reconcile the shared map against
        the physical m.<N> files, reset every folder index to the
        surviving messages, recompute refcounts from folder references
        (unreferenced -> zero-ref for the next purge; NOT resurrected),
        and drop map records whose message vanished. Refuses on an
        incomplete scan or an unmounted alt tier. Run with delivery to
        this user quiesced (operator repair tool, like force-resync).
        --restore-orphans re-files unreferenced messages that carry an
        ORIG_MAILBOX tag back into their home folder (default off, since
        a tag proves only "was once here", not "is lost").

  optimize <user> <folder> [--namespace NS]
  optimize <user> --all [--namespace NS]      fold every folder of the account
  cache-purge <user> <folder> [--namespace NS]
                             — rewrite yarilo.index.cache as a new generation
                               holding only what live messages point at, and
                               reclaim the rest. The cache is append-only and
                               never shrinks on its own; there is no automatic
                               trigger yet, so this is an operator action.
        Compact the .index.log overlay into the base .index file.
        No semantic change; safe to run while no IMAP session
        references this folder.`)
}

func indexDump(args []string) error {
	fs := flag.NewFlagSet("index dump", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	limit := fs.Int("limit", 0, "max records to return (0 = all)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend index dump <user> <folder> [--namespace NS] [--limit N]")
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
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend index rebuild <user> <folder> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/index/rebuild", map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
	}))
}

func indexRebuildStorage(args []string) error {
	fs := flag.NewFlagSet("index rebuild-storage", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	restore := fs.Bool("restore-orphans", false, "re-file unreferenced messages with an ORIG_MAILBOX tag back into their home folder (default: leave zero-ref for purge)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarctl backend index rebuild-storage <user> [--namespace NS] [--restore-orphans]")
	}
	return printJSON(backendAPIPost("/api/backend/index/rebuild-storage", map[string]any{
		"user":            fs.Arg(0),
		"namespace":       *ns,
		"restore_orphans": *restore,
	}))
}

func indexOptimize(args []string) error {
	fs := flag.NewFlagSet("index optimize", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	all := fs.Bool("all", false, "fold every folder of the account instead of one")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 || (!*all && fs.NArg() < 2) {
		return fmt.Errorf("usage: yarctl backend index optimize <user> <folder> [--namespace NS]\n" +
			"       yarctl backend index optimize <user> --all [--namespace NS]")
	}
	req := map[string]any{"user": fs.Arg(0), "namespace": *ns}
	if *all {
		req["all"] = true
	} else {
		req["folder"] = fs.Arg(1)
	}
	return printJSON(backendAPIPost("/api/backend/index/optimize", req))
}

// indexCachePurge reclaims a folder's index cache (#1030).
func indexCachePurge(args []string) error {
	fs := flag.NewFlagSet("index cache-purge", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend index cache-purge <user> <folder> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/index/cache-purge", map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
	}))
}
