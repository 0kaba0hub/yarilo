package main

import (
	"flag"
	"fmt"
)

// dispatchWho is the `backend who` subtree. Implemented as a
// dispatcher (not a flat command) so future per-protocol
// subcommands (kick by id, currently-selected folder, etc.) can
// land without breaking existing flags.
//
// Subcommands:
//
//	yarilo-admin backend who                       — full list (default)
//	yarilo-admin backend who count [protocol]      — aggregate count(s)
func dispatchWho(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "count":
			return whoCount(args[1:])
		case "list":
			return whoList(args[1:])
		}
	}
	return whoList(args)
}

func whoList(args []string) error {
	fs := flag.NewFlagSet("backend who", flag.ContinueOnError)
	service := fs.String("protocol", "", "filter by service (imap | pop3 | submission | lmtp); empty = all")
	user := fs.String("user", "", "filter by user")
	groupBy := fs.String("group-by", "user", `output grouping: "user" (default) groups sessions by mailbox owner; "none" returns a flat list`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: yarilo-admin backend who [list] [--protocol IMAP] [--user U] [--group-by user|none]")
	}
	return printJSON(backendAPIPost("/api/backend/who", map[string]any{
		"service":  *service,
		"user":     *user,
		"group_by": *groupBy,
	}))
}

func whoCount(args []string) error {
	fs := flag.NewFlagSet("backend who count", flag.ContinueOnError)
	user := fs.String("user", "", "filter by user")
	by := fs.String("by", "", `breakdown dimension: "" (single total), "protocol", or "user"`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	service := ""
	if fs.NArg() > 0 {
		service = fs.Arg(0)
	}
	return printJSON(backendAPIPost("/api/backend/who/count", map[string]any{
		"service": service,
		"user":    *user,
		"by":      *by,
	}))
}
