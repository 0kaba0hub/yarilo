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

func printFTSUsage() {
	fmt.Println(`yarilo-admin backend fts <command>

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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: yarilo-admin backend fts status <user> --folder NAME")
	}
	user := fs.Arg(0)
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: yarilo-admin backend fts rescan <user> [--folder NAME]")
	}
	user := fs.Arg(0)
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: yarilo-admin backend fts optimize <user>")
	}
	user := fs.Arg(0)
	data, err := backendAPIPost("/api/backend/fts/optimize?user="+url.QueryEscape(user), nil)
	return printOutput(data, err, func(data []byte) error {
		fmt.Printf("Optimized indexes for %s\n", user)
		return nil
	})
}
