package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

func dispatchUser(args []string) error {
	if len(args) == 0 {
		printUserUsage()
		return nil
	}
	switch args[0] {
	case "info":
		return userInfo(args[1:])
	case "usage":
		return userUsage(args[1:])
	case "iterate", "list":
		return userIterate(args[1:])
	default:
		// Treat bare email-like argument as an implicit "info <user>".
		if looksLikeUser(args[0]) {
			return userInfo(args)
		}
		return fmt.Errorf("unknown user command %q — available: info, usage, iterate", args[0])
	}
}

// looksLikeUser reports whether s is a username rather than a subcommand (contains "@").
func looksLikeUser(s string) bool {
	for _, c := range s {
		if c == '@' {
			return true
		}
	}
	return false
}

func printUserUsage() {
	fmt.Println(`yarctl backend user <command>

Commands:
  info     <user>  — username, resolved home, configured namespaces,
                     and (when yarilo-auth is wired into
                     backend_api.auth_master_addr) the userdb block
                     plus userdb_status ("ok" / "not_found" / "error").
  usage    <user>  — per-folder message + byte totals across every namespace.
  iterate          — every username yarilo-auth's userdb backend can
                     enumerate. Requires backend_api.auth_master_addr
                     to be configured; 503 otherwise.`)
}

func userInfo(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: yarctl backend user info <user>")
	}
	data, err := backendAPIPost("/api/backend/user/info", map[string]any{"user": args[0]})
	return printOutput(data, err, humanUserInfo)
}

func humanUserInfo(data []byte) error {
	var r struct {
		Username   string `json:"username"`
		Home       string `json:"home"`
		MailPath   string `json:"mail_path"`
		MailInbox  string `json:"mail_inbox_path"`
		Namespaces []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Prefix   string `json:"prefix"`
			Home     string `json:"home"`
			Location string `json:"location"`
			Exists   bool   `json:"exists"`
		} `json:"namespaces"`
		Userdb map[string]json.RawMessage `json:"userdb"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	fmt.Printf("%-16s %s\n", "Username:", r.Username)
	fmt.Printf("%-16s %s\n", "Home:", r.Home)
	fmt.Printf("%-16s %s\n", "Mail path:", r.MailPath)
	fmt.Printf("%-16s %s\n", "Mail inbox:", r.MailInbox)
	if len(r.Namespaces) > 0 {
		fmt.Println("\nNamespaces:")
		for _, ns := range r.Namespaces {
			exists := ""
			if ns.Exists {
				exists = " [exists]"
			} else {
				exists = " [missing]"
			}
			home := ns.Home
			if home == "" {
				home = ns.Location
			}
			fmt.Printf("  %-10s %-8s %s%s\n", ns.Name, ns.Type, home, exists)
		}
	}
	if len(r.Userdb) > 0 {
		fmt.Println("\nUserdb:")
		keys := make([]string, 0, len(r.Userdb))
		for k := range r.Userdb {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %-30s %s\n", k, r.Userdb[k])
		}
	}
	return nil
}

func userUsage(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: yarctl backend user usage <user>")
	}
	data, err := backendAPIPost("/api/backend/user/usage", map[string]any{"user": args[0]})
	return printOutput(data, err, humanUserUsage)
}

func humanUserUsage(data []byte) error {
	var r struct {
		Folders []struct {
			Namespace string `json:"namespace"`
			Folder    string `json:"folder"`
			Messages  uint32 `json:"messages"`
			SizeBytes uint64 `json:"size_bytes"`
		} `json:"folders"`
		TotalMessages  uint32 `json:"total_messages"`
		TotalSizeBytes uint64 `json:"total_size_bytes"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	fmt.Printf("%-12s  %-30s  %10s  %12s\n", "Namespace", "Folder", "Messages", "Size")
	for _, f := range r.Folders {
		fmt.Printf("%-12s  %-30s  %10s  %12s\n",
			f.Namespace, f.Folder,
			formatCount(int64(f.Messages)),
			formatBytes(int64(f.SizeBytes)),
		)
	}
	fmt.Printf("%s\n", "────────────────────────────────────────────────────────────")
	fmt.Printf("%-12s  %-30s  %10s  %12s\n",
		"", "Total",
		formatCount(int64(r.TotalMessages)),
		formatBytes(int64(r.TotalSizeBytes)),
	)
	return nil
}

func userIterate(_ []string) error {
	data, err := backendAPIPost("/api/backend/user/iterate", map[string]any{})
	return printOutput(data, err, humanUserIterate)
}

func humanUserIterate(data []byte) error {
	var r struct {
		Users []string `json:"users"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	for _, u := range r.Users {
		fmt.Println(u)
	}
	return nil
}
