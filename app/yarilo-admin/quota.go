package main

import (
	"encoding/json"
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
	case "clone":
		return quotaClone(args[1:])
	default:
		return fmt.Errorf("unknown quota command %q — available: show, recalc, set, clone", args[0])
	}
}

func dispatchQuotaClone(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: yarilo-admin backend quota clone <list|get> ...")
	}
	switch args[0] {
	case "list":
		return quotaCloneList(args[1:])
	case "get":
		return quotaCloneGet(args[1:])
	default:
		return fmt.Errorf("unknown quota clone command %q — available: list, get", args[0])
	}
}

func quotaClone(args []string) error { return dispatchQuotaClone(args) }

// quotaCloneList prints the configured quota_clone backend names.
func quotaCloneList(args []string) error {
	fs := flag.NewFlagSet("quota clone list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := backendAPIGet("/api/backend/quota/clone/list")
	return printOutput(data, err, func(data []byte) error {
		var r struct {
			Backends []string `json:"backends"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if len(r.Backends) == 0 {
			fmt.Println("(no quota_clone backends configured)")
			return nil
		}
		for _, b := range r.Backends {
			fmt.Println(b)
		}
		return nil
	})
}

// quotaCloneGet prints the mirrored usage one clone backend holds for a mailbox.
func quotaCloneGet(args []string) error {
	fs := flag.NewFlagSet("quota clone get", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: yarilo-admin backend quota clone get <backend> <user>")
	}
	data, err := backendAPIGet("/api/backend/quota/clone/get?backend=" +
		url.QueryEscape(fs.Arg(0)) + "&user=" + url.QueryEscape(fs.Arg(1)))
	return printOutput(data, err, humanQuotaCloneGet)
}

func humanQuotaCloneGet(data []byte) error {
	var r struct {
		Backend      string `json:"backend"`
		User         string `json:"user"`
		StorageBytes int64  `json:"storage_bytes"`
		Messages     int64  `json:"messages"`
		Found        bool   `json:"found"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	if !r.Found {
		fmt.Printf("no mirrored usage for %s in %s\n", r.User, r.Backend)
		return nil
	}
	fmt.Printf("%-20s%-11s%s\n", "Backend", "Type", "Value")
	fmt.Printf("%-20s%-11s%s\n", r.Backend, "STORAGE", formatBytes(r.StorageBytes))
	fmt.Printf("%-20s%-11s%s\n", r.Backend, "MESSAGE", formatCount(r.Messages))
	fmt.Println("(mirror value — authoritative usage is `quota show`, summed from the index)")
	return nil
}

func printQuotaUsage() {
	fmt.Println(`yarilo-admin backend quota <command>

Commands:
  show   <user>                     — current usage and configured limits
  recalc <user>                     — rescan all folders and rewrite counters (yarilo-admin quota recalc)
  set    <user> --bytes N           — override storage counter directly
                 --messages N       — override message counter directly
                 (either or both flags required)
  clone  list                       — configured quota_clone backends
  clone  get <backend> <user>       — mirrored usage a clone backend holds
                                      (advisory mirror; 'quota show' is authoritative)

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
	data, err := backendAPIGet("/api/backend/quota/show?user=" + url.QueryEscape(fs.Arg(0)))
	return printOutput(data, err, humanQuotaShow)
}

func humanQuotaShow(data []byte) error {
	var r struct {
		StorageValue   int64 `json:"storage_value"`
		StorageLimit   int64 `json:"storage_limit"`
		StoragePercent int   `json:"storage_percent"`
		MessageValue   int64 `json:"message_value"`
		MessageLimit   int64 `json:"message_limit"`
		MessagePercent int   `json:"message_percent"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	storageLimitStr := "-"
	if r.StorageLimit >= 0 {
		storageLimitStr = formatBytes(r.StorageLimit * 1024)
	}
	msgLimitStr := "-"
	if r.MessageLimit >= 0 {
		msgLimitStr = formatCount(r.MessageLimit)
	}
	fmt.Printf("%-12s  %-7s  %12s  %12s  %s\n", "Quota name", "Type", "Value", "Limit", "%")
	fmt.Printf("%-12s  %-7s  %12s  %12s  %3d\n", "User quota", "STORAGE", formatBytes(r.StorageValue*1024), storageLimitStr, r.StoragePercent)
	fmt.Printf("%-12s  %-7s  %12s  %12s  %3d\n", "User quota", "MESSAGE", formatCount(r.MessageValue), msgLimitStr, r.MessagePercent)
	return nil
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
	user := fs.Arg(0)
	data, err := backendAPIPost("/api/backend/quota/recalc", map[string]any{
		"user":      user,
		"namespace": *ns,
	})
	return printOutput(data, err, func(data []byte) error { return humanQuotaRecalc(data, user) })
}

func humanQuotaRecalc(data []byte, user string) error {
	var r struct {
		StorageBytes int64 `json:"storage_bytes"`
		Messages     int64 `json:"messages"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	fmt.Printf("Recalculated %s: %s messages, %s\n",
		user,
		formatCount(r.Messages),
		formatBytes(r.StorageBytes),
	)
	return nil
}

func formatCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	out := make([]byte, 0, len(s)+len(s)/3)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, byte(c))
	}
	return string(out)
}

func formatBytes(b int64) string {
	const (
		gib = 1 << 30
		mib = 1 << 20
		kib = 1 << 10
	)
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/gib)
	case b >= mib:
		return fmt.Sprintf("%.1f MiB", float64(b)/mib)
	case b >= kib:
		return fmt.Sprintf("%.1f KiB", float64(b)/kib)
	default:
		return fmt.Sprintf("%d B", b)
	}
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
