package main

import (
	"flag"
	"fmt"
)

// dispatchACL routes `yarctl backend acl <command>` to the matching admin-API
// call. Wire format and on-disk semantics match IMAP SETACL/DELETEACL, so admin
// writes are visible to live IMAP sessions immediately.
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
	case "materialise", "materialize":
		return aclMaterialise(args[1:])
	default:
		return fmt.Errorf("unknown acl command %q — available: list, get, set, delete, rebuild, materialise", args[0])
	}
}

func printACLUsage() {
	fmt.Println(`yarctl backend acl <command>

Commands:
  list    <user>                              — every (mailbox, identifier, rights) in
                                                the namespace-wide yarilo-acl-list index
  get     <user> <mailbox>                    — parsed ACL of one mailbox
  get     --root <user>                       — parsed ACL of the namespace root
  set     <user> <mailbox> <identifier> <rights>
                                              — upsert ONE entry (replaces matching
                                                identifier; '-' prefix marks negative)
  set     --root <user> <identifier> <rights> — same, on the namespace root
  delete  <user> <mailbox> [<identifier>]     — without identifier: drop entire file;
                                                with identifier: drop just that entry
  delete  --root <user> [<identifier>]        — same, on the namespace root
  rebuild <user> <folder> [<folder> ...]      — reseed those folders in the namespace-wide
                                                index from per-mailbox files (merges:
                                                other folders are left alone)
  rebuild <user> --all                        — reseed every folder, replacing the index
                                                (drops rows for folders that are gone)
  rebuild ... --dry-run                       — report the drift the rebuild would
                                                repair (missing / stale / mismatched
                                                rows), without writing
  materialise <user> <folder> [<folder> ...]  — write what each mailbox inherits into
                                                its own ACL; repairs mailboxes created
                                                before inheritance was materialised at
                                                creation. Dry run unless --apply.

Common flags:
  --namespace NS    namespace slug; default "personal"
  --apply           materialise only: write, instead of reporting what would change

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
	root := fs.Bool("root", false, "address the namespace root rather than a mailbox")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *root {
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: yarctl backend acl get --root <user> [--namespace NS]")
		}
		return printJSON(backendAPIPost("/api/backend/acl/get", map[string]any{
			"user":      fs.Arg(0),
			"namespace": *ns,
			"root":      true,
		}))
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend acl get <user> <mailbox> [--namespace NS]\n" +
			"       yarctl backend acl get --root <user> [--namespace NS]")
	}
	return printJSON(backendAPIPost("/api/backend/acl/get", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
		"folder":    fs.Arg(1),
	}))
}

// aclSet changes one entry through /acl/apply, which does the read-modify-write
// server-side under the folder lock. The CLI used to GET the whole ACL, edit it
// and push it back with no lock between the two, so a concurrent SETACL was lost
// and the canonical identifier form was the client's to guess (#1114).
func aclSet(args []string) error {
	fs := flag.NewFlagSet("acl set", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	// The namespace root is named by a flag, not by an empty mailbox argument:
	// a shell that swallows an argument would otherwise grant on the whole
	// namespace instead of failing (#1091).
	root := fs.Bool("root", false, "address the namespace root rather than a mailbox")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *root {
		return aclSetRoot(fs, *ns)
	}
	if fs.NArg() < 3 {
		return fmt.Errorf("usage: yarctl backend acl set <user> <mailbox> <identifier> <rights> [--namespace NS]\n" +
			"       yarctl backend acl set --root <user> <identifier> <rights> [--namespace NS]")
	}
	user, mbox, identifier := fs.Arg(0), fs.Arg(1), fs.Arg(2)
	rights := ""
	if fs.NArg() >= 4 {
		rights = fs.Arg(3)
	}
	return printJSON(backendAPIPost("/api/backend/acl/apply", map[string]any{
		"user":       user,
		"namespace":  *ns,
		"folder":     mbox,
		"identifier": identifier,
		"rights":     rights,
		"mode":       "replace",
	}))
}

func aclDelete(args []string) error {
	fs := flag.NewFlagSet("acl delete", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	root := fs.Bool("root", false, "address the namespace root rather than a mailbox")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *root {
		return aclDeleteRoot(fs, *ns)
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend acl delete <user> <mailbox> [<identifier>] [--namespace NS]\n" +
			"       yarctl backend acl delete --root <user> [<identifier>] [--namespace NS]")
	}
	user, mbox := fs.Arg(0), fs.Arg(1)
	if fs.NArg() == 2 {
		// No identifier: drop the entire file.
		return printJSON(backendAPIPost("/api/backend/acl/delete", map[string]any{
			"user":      user,
			"namespace": *ns,
			"folder":    mbox,
		}))
	}
	// Single-identifier delete is a replace with empty rights, applied
	// server-side under the lock -- DELETEACL semantics (RFC 4314 §3.2), and no
	// client-side read-modify-write to lose a concurrent write (#1114).
	identifier := fs.Arg(2)
	return printJSON(backendAPIPost("/api/backend/acl/apply", map[string]any{
		"user":       user,
		"namespace":  *ns,
		"folder":     mbox,
		"identifier": identifier,
		"rights":     "",
		"mode":       "replace",
	}))
}

// aclDeleteRoot mirrors aclDelete for the namespace root: set could write a
// root entry that nothing could then remove except a set with empty rights --
// a removal spelled as a write, discoverable only by guessing (#1163).
func aclDeleteRoot(fs *flag.FlagSet, ns string) error {
	switch fs.NArg() {
	case 1: // drop the entire root ACL file
		return printJSON(backendAPIPost("/api/backend/acl/delete", map[string]any{
			"user":      fs.Arg(0),
			"namespace": ns,
			"root":      true,
		}))
	case 2: // remove one identifier, DELETEACL semantics
		return printJSON(backendAPIPost("/api/backend/acl/apply", map[string]any{
			"user":       fs.Arg(0),
			"namespace":  ns,
			"root":       true,
			"identifier": fs.Arg(1),
			"rights":     "",
			"mode":       "replace",
		}))
	}
	return fmt.Errorf("usage: yarctl backend acl delete --root <user> [<identifier>] [--namespace NS]")
}

func aclRebuild(args []string) error {
	fs := flag.NewFlagSet("acl rebuild", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	all := fs.Bool("all", false, "rebuild every folder in the namespace")
	dryRun := fs.Bool("dry-run", false, "report the drift the rebuild would repair, without writing")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *all {
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: yarctl backend acl rebuild <user> --all [--dry-run] [--namespace NS] (no folder list with --all)")
		}
		return printJSON(backendAPIPost("/api/backend/acl/rebuild", map[string]any{
			"user":      fs.Arg(0),
			"namespace": *ns,
			"all":       true,
			"dry_run":   *dryRun,
		}))
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend acl rebuild <user> <folder> [<folder> ...] [--dry-run] [--namespace NS], or --all")
	}
	folders := make([]string, 0, fs.NArg()-1)
	for i := 1; i < fs.NArg(); i++ {
		folders = append(folders, fs.Arg(i))
	}
	return printJSON(backendAPIPost("/api/backend/acl/rebuild", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
		"folders":   folders,
		"dry_run":   *dryRun,
	}))
}

// ---- helpers ----

// aclSetRoot changes one entry on the namespace-root ACL through /acl/apply,
// atomic under the lock. A shared namespace
// needs it before anyone can create its first mailbox: the create right is
// checked on the parent, and for a top-level name the parent is the root.
func aclSetRoot(fs *flag.FlagSet, ns string) error {
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend acl set --root <user> <identifier> <rights> [--namespace NS]")
	}
	user, identifier := fs.Arg(0), fs.Arg(1)
	rights := ""
	if fs.NArg() >= 3 {
		rights = fs.Arg(2)
	}
	return printJSON(backendAPIPost("/api/backend/acl/apply", map[string]any{
		"user":       user,
		"namespace":  ns,
		"root":       true,
		"identifier": identifier,
		"rights":     rights,
		"mode":       "replace",
	}))
}

// aclMaterialise writes what each mailbox inherits into its own ACL, repairing
// mailboxes created before inheritance was materialised at creation (#1111).
//
// A dry run unless --apply: the operation changes who can reach mail, so
// "show me what you would do" is how it is meant to be run the first time
// rather than a flag an operator is expected to remember.
func aclMaterialise(args []string) error {
	fs := flag.NewFlagSet("acl materialise", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	apply := fs.Bool("apply", false, "write the changes; without it, report what would change")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend acl materialise <user> <folder> [<folder> ...] [--apply] [--namespace NS]")
	}
	folders := make([]string, 0, fs.NArg()-1)
	for i := 1; i < fs.NArg(); i++ {
		folders = append(folders, fs.Arg(i))
	}
	return printJSON(backendAPIPost("/api/backend/acl/materialise", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
		"folders":   folders,
		"apply":     *apply,
	}))
}
