package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
)

func dispatchMailbox(args []string) error {
	// A wrong subcommand exits non-zero: a script around yarctl reads the exit
	// code, and printing usage while reporting success is how a typo passes
	// for a run.
	if len(args) == 0 || args[0] != "message" {
		printMailboxUsage()
		return fmt.Errorf("usage: yarctl backend mailbox message get <mime|raw> ...")
	}
	rest := args[1:]
	if len(rest) < 2 || rest[0] != "get" {
		printMailboxUsage()
		return fmt.Errorf("usage: yarctl backend mailbox message get <mime|raw> ...")
	}
	rest = rest[1:]
	switch rest[0] {
	case "mime", "raw":
		return messageGet(rest[0], rest[1:])
	default:
		return fmt.Errorf("unknown form %q — available: mime, raw", rest[0])
	}
}

func printMailboxUsage() {
	fmt.Println(`yarctl backend mailbox message get <form> <user> <folder> --uid N | --guid G

  mime   headers as written and each part's headers, part bodies elided
  raw    the message byte for byte

Reads nothing else: no flag is set, no counter moves. Every call is
recorded on the backend, since this returns the content of a mailbox.`)
}

func messageGet(mode string, args []string) error {
	fs := flag.NewFlagSet("mailbox message get "+mode, flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	uid := fs.Uint("uid", 0, "message UID within the folder")
	guid := fs.String("guid", "", "message GUID (as reported in logs)")
	out := fs.String("out", "", "write to this file instead of stdout")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: yarctl backend mailbox message get %s <user> <folder> --uid N | --guid G", mode)
	}
	// Exactly one way to name the message: two would leave the server to guess
	// which the caller meant, and the wrong message is worse than an error.
	if (*uid == 0) == (*guid == "") {
		return fmt.Errorf("name the message by exactly one of --uid or --guid")
	}

	body := map[string]any{
		"user":      fs.Arg(0),
		"folder":    fs.Arg(1),
		"namespace": *ns,
		"mode":      mode,
	}
	if *uid != 0 {
		body["uid"] = *uid
	} else {
		body["guid"] = *guid
	}

	rc, err := backendAPIStream("/api/backend/message/get", body)
	if err != nil {
		return err
	}
	defer rc.Close() //nolint:errcheck

	dst := io.Writer(os.Stdout)
	if *out != "" {
		f, ferr := os.Create(*out)
		if ferr != nil {
			return fmt.Errorf("create %s: %w", *out, ferr)
		}
		defer f.Close() //nolint:errcheck
		dst = f
	}
	// Streamed, never buffered: a message is as large as somebody sent it, and
	// this is the tool reached for when one of them is the problem.
	n, cerr := io.Copy(dst, rc)
	if cerr != nil {
		return fmt.Errorf("read message: %w", cerr)
	}
	if *out != "" {
		fmt.Fprintf(os.Stderr, "wrote %s bytes to %s\n", strconv.FormatInt(n, 10), *out)
	}
	return nil
}
