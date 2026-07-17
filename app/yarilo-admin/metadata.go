package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func dispatchMetadata(args []string) error {
	if len(args) == 0 {
		printMetadataUsage()
		return nil
	}
	switch args[0] {
	case "list":
		return metadataList(args[1:])
	case "get":
		return metadataGet(args[1:])
	case "set":
		return metadataSet(args[1:])
	case "delete", "del":
		return metadataDelete(args[1:])
	default:
		return fmt.Errorf("unknown metadata command %q — available: list, get, set, delete", args[0])
	}
}

func printMetadataUsage() {
	fmt.Println(`yarilo-admin backend metadata <command>

Commands:
  list   <user> [<folder>] [--namespace NS] [--scope private|shared] [--as-user U]
        List every annotation under the chosen scope. Empty folder
        targets server scope (vendor-prefixed under INBOX's GUID).

  get    <user> [<folder>] --entry /private/comment [--namespace NS] [--as-user U]
        Read a single entry; outputs base64 value when found.

  set    <user> [<folder>] --entry /private/comment --value 'literal' | --value-file path
        Write a value. Empty folder targets server scope.

  delete <user> [<folder>] --entry /private/comment [--namespace NS] [--as-user U]
        Delete an entry.

Entry names start with /private/ or /shared/ (RFC 5464).
--as-user defaults to <user>; matters only for shared/public folders
under /private/ scope where each user has their own slice.`)
}

func metadataList(args []string) error {
	fs := flag.NewFlagSet("metadata list", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	scope := fs.String("scope", "private", "private | shared")
	asUser := fs.String("as-user", "", "accessing user for shared-folder /private/ slice (defaults to <user>)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarilo-admin backend metadata list <user> [<folder>] [--namespace NS] [--scope ...] [--as-user U]")
	}
	folder := ""
	if fs.NArg() > 1 {
		folder = fs.Arg(1)
	}
	return printJSON(backendAPIPost("/api/backend/metadata/list", map[string]any{
		"user":      fs.Arg(0),
		"folder":    folder,
		"namespace": *ns,
		"scope":     *scope,
		"as_user":   *asUser,
	}))
}

func metadataGet(args []string) error {
	fs := flag.NewFlagSet("metadata get", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	entry := fs.String("entry", "", "entry name (e.g. /private/comment)")
	asUser := fs.String("as-user", "", "accessing user for shared-folder /private/ slice")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 || *entry == "" {
		return fmt.Errorf("usage: yarilo-admin backend metadata get <user> [<folder>] --entry /private/<name> [--namespace NS] [--as-user U]")
	}
	folder := ""
	if fs.NArg() > 1 {
		folder = fs.Arg(1)
	}
	return printJSON(backendAPIPost("/api/backend/metadata/get", map[string]any{
		"user":      fs.Arg(0),
		"folder":    folder,
		"namespace": *ns,
		"entry":     *entry,
		"as_user":   *asUser,
	}))
}

func metadataSet(args []string) error {
	fs := flag.NewFlagSet("metadata set", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	entry := fs.String("entry", "", "entry name")
	value := fs.String("value", "", "literal value (UTF-8)")
	valueFile := fs.String("value-file", "", "read value bytes from file (use - for stdin)")
	asUser := fs.String("as-user", "", "accessing user for shared-folder /private/ slice")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 || *entry == "" {
		return fmt.Errorf("usage: yarilo-admin backend metadata set <user> [<folder>] --entry /private/<name> --value V | --value-file path")
	}
	if *value == "" && *valueFile == "" {
		return fmt.Errorf("metadata set: --value or --value-file required")
	}
	raw := []byte(*value)
	if *valueFile != "" {
		buf, err := readValueFile(*valueFile)
		if err != nil {
			return err
		}
		raw = buf
	}
	folder := ""
	if fs.NArg() > 1 {
		folder = fs.Arg(1)
	}
	return printJSON(backendAPIPost("/api/backend/metadata/set", map[string]any{
		"user":      fs.Arg(0),
		"folder":    folder,
		"namespace": *ns,
		"entry":     *entry,
		"value":     base64.StdEncoding.EncodeToString(raw),
		"as_user":   *asUser,
	}))
}

func metadataDelete(args []string) error {
	fs := flag.NewFlagSet("metadata delete", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	entry := fs.String("entry", "", "entry name")
	asUser := fs.String("as-user", "", "accessing user for shared-folder /private/ slice")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 || *entry == "" {
		return fmt.Errorf("usage: yarilo-admin backend metadata delete <user> [<folder>] --entry /private/<name> [--namespace NS] [--as-user U]")
	}
	folder := ""
	if fs.NArg() > 1 {
		folder = fs.Arg(1)
	}
	return printJSON(backendAPIPost("/api/backend/metadata/delete", map[string]any{
		"user":      fs.Arg(0),
		"folder":    folder,
		"namespace": *ns,
		"entry":     *entry,
		"as_user":   *asUser,
	}))
}

func readValueFile(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "-" {
		buf, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return buf, nil
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return buf, nil
}
