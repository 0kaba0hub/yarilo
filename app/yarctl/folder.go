package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
)

func dispatchFolder(args []string) error {
	if len(args) == 0 {
		printFolderUsage()
		return nil
	}
	switch args[0] {
	case "list":
		return folderList(args[1:])
	case "info":
		return folderInfo(args[1:])
	case "guid":
		return folderGUID(args[1:])
	case "stats":
		return folderStats(args[1:])
	case "repair":
		return folderRepair(args[1:])
	case "create":
		return folderCreate(args[1:])
	case "delete", "rm":
		return folderDelete(args[1:])
	case "rename", "mv":
		return folderRename(args[1:])
	case "expunge":
		return folderExpunge(args[1:])
	default:
		return fmt.Errorf("unknown folder command %q — available: list, info, guid, stats, repair, create, delete, rename, expunge", args[0])
	}
}

func printFolderUsage() {
	fmt.Println(`yarctl backend folder <command>

Commands:
  list    <user> [--namespace NS]                              — list folder names in a namespace
  info    <user> <folder> [--namespace NS]                     — folder metadata (GUID, msg count, modseq, ...)
  guid    <user> <folder> [--namespace NS]                     — print the rename-stable GUID hex
  stats   <user> <folder> [--namespace NS]                     — info plus on-disk size totals
  repair  <user> <folder> [--namespace NS]                     — rebuild fileindex from disk + compact log
  create  <user> <folder> [--namespace NS] [--special-use ATTR]
                                                                — create folder (RFC 6154 special-use attr is personal-only)
  delete  <user> <folder> [--namespace NS]                     — delete folder + ACL state
  rename  <user> <old> <new> [--namespace NS]                  — rename folder (INBOX not supported)
  expunge <user> <folder> [--namespace NS] [--uids 1,2,3]      — drop \Deleted-flagged messages
                                                                  (entire mailbox when --uids omitted)

Namespace defaults to "personal". Mutating ops (create/delete/
rename/expunge) bypass ACL — backend-api is the operator surface,
authorised by Token + AllowedNets + mTLS.`)
}

func folderRepair(args []string) error {
	return folderSingleFolderCommand("repair", "/api/backend/folder/repair", args)
}

func folderList(args []string) error {
	fs := flag.NewFlagSet("folder list", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarctl backend folder list <user> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/folder/list", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
	}))
}

func folderInfo(args []string) error {
	return folderSingleFolderCommand("info", "/api/backend/folder/info", args)
}

func folderGUID(args []string) error {
	return folderSingleFolderCommand("guid", "/api/backend/folder/guid", args)
}

func folderStats(args []string) error {
	return folderSingleFolderCommand("stats", "/api/backend/folder/stats", args)
}

func folderCreate(args []string) error {
	fs := flag.NewFlagSet("folder create", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	specialUse := fs.String("special-use", "", "RFC 6154 special-use attr (e.g. \\Sent, \\Drafts) — personal namespace only")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend folder create <user> <folder> [--namespace NS] [--special-use ATTR]")
	}
	body := map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
	}
	if *specialUse != "" {
		body["special_use"] = *specialUse
	}
	return printJSON(backendAPIPost("/api/backend/folder/create", body))
}

func folderDelete(args []string) error {
	fs := flag.NewFlagSet("folder delete", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend folder delete <user> <folder> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/folder/delete", map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
	}))
}

func folderRename(args []string) error {
	fs := flag.NewFlagSet("folder rename", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 3 {
		return fmt.Errorf("usage: yarctl backend folder rename <user> <old> <new> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/folder/rename", map[string]any{
		"user":       fs.Arg(0),
		"old_folder": fs.Arg(1),
		"new_folder": fs.Arg(2),
		"namespace":  *ns,
	}))
}

func folderExpunge(args []string) error {
	fs := flag.NewFlagSet("folder expunge", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	uidList := fs.String("uids", "", "comma-separated UID set; empty = expunge every \\Deleted message")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend folder expunge <user> <folder> [--namespace NS] [--uids 1,2,3]")
	}
	body := map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
	}
	if *uidList != "" {
		uids, err := parseUIDList(*uidList)
		if err != nil {
			return fmt.Errorf("--uids: %w", err)
		}
		body["uids"] = uids
	}
	return printJSON(backendAPIPost("/api/backend/folder/expunge", body))
}

// parseUIDList accepts comma-separated unsigned 32-bit ints. Bare
// (non-list) input passes through as a single-element slice so
// `--uids 42` works without forcing a trailing comma.
func parseUIDList(s string) ([]uint32, error) {
	var out []uint32
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		n, err := strconv.ParseUint(tok, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid uid %q: %w", tok, err)
		}
		out = append(out, uint32(n))
	}
	return out, nil
}

func folderSingleFolderCommand(cmd, path string, args []string) error {
	fs := flag.NewFlagSet("folder "+cmd, flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend folder %s <user> <folder> [--namespace NS]", cmd)
	}
	return printJSON(backendAPIPost(path, map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
	}))
}
