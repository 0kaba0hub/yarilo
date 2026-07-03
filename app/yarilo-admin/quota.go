package main

import (
	"flag"
	"fmt"
	"net/url"
)

func dispatchQuota(args []string) error {
	if len(args) == 0 {
		printQuotaUsage()
		return nil
	}
	switch args[0] {
	case "show":
		return quotaShow(args[1:])
	case "recalc":
		return quotaRecalc(args[1:])
	case "set":
		return quotaSet(args[1:])
	default:
		return fmt.Errorf("unknown quota command %q — available: show, recalc, set", args[0])
	}
}

func printQuotaUsage() {
	fmt.Println(`yarilo-admin backend quota <command>

Commands:
  show   <user>                     — current usage and configured limits
  recalc <user>                     — rescan all folders and rewrite counters (yarilo-admin quota recalc)
  set    <user> --bytes N           — override storage counter directly
                 --messages N       — override message counter directly
                 (either or both flags required)

Common flags:
  --namespace NS    namespace slug for recalc; default "personal"

Limits shown by 'show' are always 0 (unlimited) from this endpoint —
quota_rules live in the auth/userdb layer. Use 'yarilo-admin auth passdb
show <user>' to inspect per-user rules.`)
}

// quotaShow prints current usage for a user.
// GET /api/backend/quota/show?user=<user>
func quotaShow(args []string) error {
	fs := flag.NewFlagSet("quota show", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: yarilo-admin backend quota show <user>")
	}
	return printJSON(backendAPIGet("/api/backend/quota/show?user=" + url.QueryEscape(fs.Arg(0))))
}

// quotaRecalc scans all folder indexes for the user and rewrites
// the dict counters from the actual on-disk data.
// POST /api/backend/quota/recalc
func quotaRecalc(args []string) error {
	fs := flag.NewFlagSet("quota recalc", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: yarilo-admin backend quota recalc <user>")
	}
	return printJSON(backendAPIPost("/api/backend/quota/recalc", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
	}))
}

// quotaSet directly overwrites one or both quota counters without a
// full rescan. Useful for manual corrections.
// POST /api/backend/quota/set
func quotaSet(args []string) error {
	fs := flag.NewFlagSet("quota set", flag.ContinueOnError)
	bytesFlag := fs.Int64("bytes", -1, "storage bytes to set (omit to keep current)")
	msgsFlag := fs.Int64("messages", -1, "message count to set (omit to keep current)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: yarilo-admin backend quota set <user> [--bytes N] [--messages N]")
	}
	if *bytesFlag < 0 && *msgsFlag < 0 {
		return fmt.Errorf("quota set: at least one of --bytes or --messages is required")
	}
	body := map[string]any{"user": fs.Arg(0)}
	if *bytesFlag >= 0 {
		body["storage_bytes"] = *bytesFlag
	}
	if *msgsFlag >= 0 {
		body["messages"] = *msgsFlag
	}
	return printJSON(backendAPIPost("/api/backend/quota/set", body))
}
