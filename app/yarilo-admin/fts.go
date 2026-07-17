package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strings"
)

func dispatchFTS(args []string) error {
	if len(args) == 0 {
		printFTSUsage()
		return nil
	}
	switch args[0] {
	case "status":
		return ftsStatus(args[1:])
	case "rescan":
		return ftsRescan(args[1:])
	case "optimize":
		return ftsOptimize(args[1:])
	default:
		return fmt.Errorf("unknown fts command %q — available: status, rescan, optimize", args[0])
	}
}

// parseUserArg accepts the positional <user> before or after the flags: the
// flag package stops parsing at the first positional argument, so
// "fts status u1 --folder X" would otherwise silently ignore the flags.
func parseUserArg(fs *flag.FlagSet, args []string) (string, error) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if err := fs.Parse(args[1:]); err != nil {
			return "", err
		}
		if fs.NArg() != 0 {
			return "", nil
		}
		return args[0], nil
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() != 1 {
		return "", nil
	}
	return fs.Arg(0), nil
}

func printFTSUsage() {
	fmt.Println(`yarilo-admin fts <command>

Commands:
  status   <user> --folder NAME     — per-mailbox indexing checkpoint
  rescan   <user> [--folder NAME]   — reconcile the index against the mailbox;
                                       without --folder every folder is rescanned
  optimize <user>                   — compact every index owned by the user

All commands reach the yarilo-fts service via yarilo-backend-api. They return
HTTP 501 when the backend-api has no fts_addr configured.`)
}

// ftsStatus prints the indexing checkpoint for one folder.
// GET /api/backend/fts/status?user=&folder=
func ftsStatus(args []string) error {
	fs := flag.NewFlagSet("fts status", flag.ContinueOnError)
	folder := fs.String("folder", "INBOX", "folder name")
	user, err := parseUserArg(fs, args)
	if err != nil {
		return err
	}
	if user == "" {
		return fmt.Errorf("usage: yarilo-admin fts status <user> [--folder NAME]")
	}
	data, err := backendAPIGet("/api/backend/fts/status?user=" +
		url.QueryEscape(user) + "&folder=" + url.QueryEscape(*folder))
	return printOutput(data, err, func(data []byte) error {
		var r struct {
			User             string `json:"user"`
			Folder           string `json:"folder"`
			LastIndexedUID   uint32 `json:"last_indexed_uid"`
			SettingsChecksum uint32 `json:"settings_checksum"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		fmt.Printf("%s [%s]: last indexed UID %d (settings checksum %08x)\n",
			r.User, r.Folder, r.LastIndexedUID, r.SettingsChecksum)
		return nil
	})
}

// ftsRescan reconciles one or every folder.
// POST /api/backend/fts/rescan?user=&folder=
func ftsRescan(args []string) error {
	fs := flag.NewFlagSet("fts rescan", flag.ContinueOnError)
	folder := fs.String("folder", "", "folder name; empty = all folders")
	user, err := parseUserArg(fs, args)
	if err != nil {
		return err
	}
	if user == "" {
		return fmt.Errorf("usage: yarilo-admin fts rescan <user> [--folder NAME]")
	}
	path := "/api/backend/fts/rescan?user=" + url.QueryEscape(user)
	if *folder != "" {
		path += "&folder=" + url.QueryEscape(*folder)
	}
	data, err := backendAPIPost(path, nil)
	return printOutput(data, err, func(data []byte) error {
		var r struct {
			User    string   `json:"user"`
			Folders []string `json:"folders"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		fmt.Printf("Rescanned %s: %d folder(s) [%s]\n",
			r.User, len(r.Folders), strings.Join(r.Folders, ", "))
		return nil
	})
}

// ftsOptimize compacts every index owned by the user.
// POST /api/backend/fts/optimize?user=
func ftsOptimize(args []string) error {
	fs := flag.NewFlagSet("fts optimize", flag.ContinueOnError)
	user, err := parseUserArg(fs, args)
	if err != nil {
		return err
	}
	if user == "" {
		return fmt.Errorf("usage: yarilo-admin fts optimize <user>")
	}
	data, err := backendAPIPost("/api/backend/fts/optimize?user="+url.QueryEscape(user), nil)
	return printOutput(data, err, func(data []byte) error {
		fmt.Printf("Optimized indexes for %s\n", user)
		return nil
	})
}
