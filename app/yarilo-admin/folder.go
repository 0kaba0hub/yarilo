package main

import (
	"flag"
	"fmt"
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
	default:
		return fmt.Errorf("unknown folder command %q — available: list, info, guid, stats, repair", args[0])
	}
}

func printFolderUsage() {
	fmt.Println(`yarilo-admin backend folder <command>

Commands:
  list   <user> [--namespace NS]                 — list folder names in a namespace
  info   <user> <folder> [--namespace NS]        — folder metadata (GUID, msg count, modseq, ...)
  guid   <user> <folder> [--namespace NS]        — print the rename-stable GUID hex
  stats  <user> <folder> [--namespace NS]        — info plus on-disk size totals
  repair <user> <folder> [--namespace NS]        — rebuild fileindex from disk + compact log

Namespace defaults to "personal".`)
}

func folderRepair(args []string) error {
	return folderSingleFolderCommand("repair", "/api/backend/folder/repair", args)
}

func folderList(args []string) error {
	fs := flag.NewFlagSet("folder list", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarilo-admin backend folder list <user> [--namespace NS]")
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

func folderSingleFolderCommand(cmd, path string, args []string) error {
	fs := flag.NewFlagSet("folder "+cmd, flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarilo-admin backend folder %s <user> <folder> [--namespace NS]", cmd)
	}
	return printJSON(backendAPIPost(path, map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
	}))
}
