package main

import (
	"flag"
	"fmt"
)

func dispatchSessions(args []string) error {
	if len(args) == 0 {
		printSessionsUsage()
		return nil
	}
	switch args[0] {
	case "kick":
		return sessionsKick(args[1:])
	default:
		return fmt.Errorf("unknown sessions command %q — available: kick", args[0])
	}
}

func printSessionsUsage() {
	fmt.Println(`yarilo-admin backend sessions <command>

Commands:
  kick <sess-id> [--user U] [--protocols imap,pop3,...]
        Close the session whose id matches <sess-id>. The kick
        event is broadcast across every login + LMTP pod via
        yarilo-locks Emit; only the owner reacts. Listing the
        id is enough — find it with` + " `yarilo-admin backend who`" + `.
        --protocols narrows the broadcast (default: all four
        channels). --user is recorded for the audit log only.`)
}

func sessionsKick(args []string) error {
	fs := flag.NewFlagSet("sessions kick", flag.ContinueOnError)
	user := fs.String("user", "", "username (advisory, recorded in audit log)")
	protos := fs.String("protocols", "", "comma-separated protocol filter (default: all)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarilo-admin backend sessions kick <sess-id> [--user U] [--protocols imap,pop3,...]")
	}
	body := map[string]any{
		"session_id": fs.Arg(0),
		"user":       *user,
	}
	if *protos != "" {
		body["protocols"] = splitCSV(*protos)
	}
	return printJSON(backendAPIPost("/api/backend/sessions/kick", body))
}

// splitCSV is a tiny helper — flag.StringVar gives us one string,
// the wire wants a JSON array. Empty entries (trailing commas) are
// dropped silently so `--protocols imap,` does the right thing.
func splitCSV(s string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(s[i])
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
