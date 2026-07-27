package main

import (
	"encoding/json"
	"flag"
	"fmt"
)

// dispatchACL routes `yarctl backend acl <command>` to the
// matching admin-API call. The wire format and on-disk semantics
// are identical to IMAP SETACL / DELETEACL so admin writes are
// visible to live IMAP sessions immediately.
func dispatchACL(args []string) error {
	if len(args) == 0 {
		printACLUsage()
		return nil
	}
	switch args[0] {
	case "list":
		return aclList(args[1:])
	case "get":
		return aclGet(args[1:])
	case "set":
		return aclSet(args[1:])
	case "delete", "rm":
		return aclDelete(args[1:])
	case "rebuild":
		return aclRebuild(args[1:])
	default:
		return fmt.Errorf("unknown acl command %q — available: list, get, set, delete, rebuild", args[0])
	}
}

func printACLUsage() {
	fmt.Println(`yarctl backend acl <command>

Commands:
  list    <user>                              — every (mailbox, identifier, rights) in
                                                the namespace-wide yarilo-acl-list index
  get     <user> <mailbox>                    — parsed ACL of one mailbox
  set     <user> <mailbox> <identifier> <rights>
                                              — upsert ONE entry (replaces matching
                                                identifier; '-' prefix marks negative)
  delete  <user> <mailbox> [<identifier>]     — without identifier: drop entire file;
                                                with identifier: drop just that entry
  rebuild <user> <folder> [<folder> ...]      — reseed namespace-wide index from
                                                per-mailbox files (admin recovery)

Common flags:
  --namespace NS    namespace slug; default "personal"

Reuses the same on-disk format + lock keys as IMAP SETACL/DELETEACL
so admin and live IMAP sessions stay consistent.`)
}

func aclList(args []string) error {
	fs := flag.NewFlagSet("acl list", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarctl backend acl list <user> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/acl/list", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
	}))
}

func aclGet(args []string) error {
	fs := flag.NewFlagSet("acl get", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend acl get <user> <mailbox> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/acl/get", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
		"folder":    fs.Arg(1),
	}))
}

// aclSet is upsert-one-entry semantics — the CLI doesn't accept a
// full ACL JSON (clumsy on the command line). It first GETs the
// current ACL, swaps in the supplied (identifier, rights), and
// then PUTs the whole thing back through the same /set endpoint
// the API uses for full-replace.
func aclSet(args []string) error {
	fs := flag.NewFlagSet("acl set", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 3 {
		return fmt.Errorf("usage: yarctl backend acl set <user> <mailbox> <identifier> <rights> [--namespace NS]")
	}
	user, mbox, identifier := fs.Arg(0), fs.Arg(1), fs.Arg(2)
	rights := ""
	if fs.NArg() >= 4 {
		rights = fs.Arg(3)
	}
	current, err := fetchACLEntries(user, *ns, mbox)
	if err != nil {
		return err
	}
	updated := upsertEntry(current, identifier, rights)
	return printJSON(backendAPIPost("/api/backend/acl/set", map[string]any{
		"user":      user,
		"namespace": *ns,
		"folder":    mbox,
		"acl":       updated,
	}))
}

func aclDelete(args []string) error {
	fs := flag.NewFlagSet("acl delete", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend acl delete <user> <mailbox> [<identifier>] [--namespace NS]")
	}
	user, mbox := fs.Arg(0), fs.Arg(1)
	if fs.NArg() == 2 {
		// No identifier — drop the entire file.
		return printJSON(backendAPIPost("/api/backend/acl/delete", map[string]any{
			"user":      user,
			"namespace": *ns,
			"folder":    mbox,
		}))
	}
	// Single-identifier delete = GET + drop matching entry + SET.
	identifier := fs.Arg(2)
	current, err := fetchACLEntries(user, *ns, mbox)
	if err != nil {
		return err
	}
	updated := dropIdentifier(current, identifier)
	return printJSON(backendAPIPost("/api/backend/acl/set", map[string]any{
		"user":      user,
		"namespace": *ns,
		"folder":    mbox,
		"acl":       updated,
	}))
}

func aclRebuild(args []string) error {
	fs := flag.NewFlagSet("acl rebuild", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend acl rebuild <user> <folder> [<folder> ...] [--namespace NS]")
	}
	folders := make([]string, 0, fs.NArg()-1)
	for i := 1; i < fs.NArg(); i++ {
		folders = append(folders, fs.Arg(i))
	}
	return printJSON(backendAPIPost("/api/backend/acl/rebuild", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
		"folders":   folders,
	}))
}

// ---- helpers ----

// aclEntryWire is the wire shape the /get and /set endpoints expose.
// Kept JSON-compatible with backendapi.aclEntryJSON so the CLI can
// round-trip without a separate struct registry.
type aclEntryWire struct {
	Identifier string `json:"identifier"`
	Rights     string `json:"rights"`
	Negative   bool   `json:"negative,omitempty"`
}

// fetchACLEntries GETs the current ACL of (user, mailbox) and decodes
// it into a slice the CLI can mutate before pushing back. The
// /get endpoint returns an empty array when no file exists, so the
// caller gets an empty slice rather than an error.
func fetchACLEntries(user, ns, mbox string) ([]aclEntryWire, error) {
	body, err := backendAPIPost("/api/backend/acl/get", map[string]any{
		"user":      user,
		"namespace": ns,
		"folder":    mbox,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		ACL []aclEntryWire `json:"acl"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode acl get: %w", err)
	}
	return resp.ACL, nil
}

// upsertEntry replaces the entry with matching identifier or appends
// when none exists. Empty rights with a non-`-` identifier collapses
// to "drop that identifier" — mirrors RFC 4314 SETACL empty-rights
// semantics so admin and IMAP paths stay symmetric.
func upsertEntry(in []aclEntryWire, identifier, rights string) []aclEntryWire {
	if rights == "" && len(identifier) > 0 && identifier[0] != '-' {
		return dropIdentifier(in, identifier)
	}
	out := make([]aclEntryWire, 0, len(in)+1)
	replaced := false
	for _, e := range in {
		if e.Identifier == identifier {
			out = append(out, aclEntryWire{Identifier: identifier, Rights: rights})
			replaced = true
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, aclEntryWire{Identifier: identifier, Rights: rights})
	}
	return out
}

// dropIdentifier removes every entry whose identifier matches (after
// normalising the '-' prefix — DELETEACL drops both positive and
// negative entries for the identifier per RFC 4314 §3.2).
func dropIdentifier(in []aclEntryWire, identifier string) []aclEntryWire {
	wantBare := identifier
	if len(wantBare) > 0 && wantBare[0] == '-' {
		wantBare = wantBare[1:]
	}
	out := make([]aclEntryWire, 0, len(in))
	for _, e := range in {
		bare := e.Identifier
		if len(bare) > 0 && bare[0] == '-' {
			bare = bare[1:]
		}
		if bare == wantBare {
			continue
		}
		out = append(out, e)
	}
	return out
}
