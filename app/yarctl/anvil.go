package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
)

// dispatchAnvil is the `anvil` subtree — introspection of the shared
// connection-accounting service. Routed via the backend plane (backend-api ->
// anvil), like `backend who`.
//
//	yarctl anvil dump [--output table|json]
func dispatchAnvil(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: yarctl anvil dump [--output table|json]")
	}
	switch args[0] {
	case "dump":
		return anvilDump(args[1:])
	default:
		return fmt.Errorf("unknown anvil command %q — available: dump", args[0])
	}
}

// anvilDump prints the anvil state snapshot: per-user@IP connection counters
// with their live session tally (DRIFT = counter - live; non-zero means a leaked
// counter) and the per-IP penalty entries with remaining TTL.
func anvilDump(args []string) error {
	fs := flag.NewFlagSet("anvil dump", flag.ContinueOnError)
	output := fs.String("output", "table", "table | json")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: yarctl anvil dump [--output table|json]")
	}
	body, err := backendAPIGet("/api/backend/anvil/dump")
	if err != nil {
		return err
	}
	if *output == "json" {
		os.Stdout.Write(body)
		if n := len(body); n == 0 || body[n-1] != '\n' {
			fmt.Println()
		}
		return nil
	}

	var d struct {
		Counters []struct {
			UserIP  string `json:"user_ip"`
			Counter int    `json:"counter"`
			Live    int    `json:"live"`
		} `json:"counters"`
		Penalties []struct {
			IP      string `json:"ip"`
			Count   int    `json:"count"`
			TTLSecs int    `json:"ttl_secs"`
		} `json:"penalties"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return fmt.Errorf("parse dump: %w", err)
	}

	if len(d.Counters) == 0 && len(d.Penalties) == 0 {
		fmt.Println("(no counters or penalties)")
		return nil
	}

	if len(d.Counters) > 0 {
		sort.Slice(d.Counters, func(i, j int) bool { return d.Counters[i].UserIP < d.Counters[j].UserIP })
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "USER@IP\tCOUNTER\tLIVE\tDRIFT")
		for _, c := range d.Counters {
			drift := c.Counter - c.Live
			mark := ""
			if drift != 0 {
				mark = " *"
			}
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d%s\n", c.UserIP, c.Counter, c.Live, drift, mark)
		}
		tw.Flush()
	}

	if len(d.Penalties) > 0 {
		sort.Slice(d.Penalties, func(i, j int) bool { return d.Penalties[i].IP < d.Penalties[j].IP })
		fmt.Println()
		pw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(pw, "IP\tPENALTY\tTTL(s)")
		for _, p := range d.Penalties {
			fmt.Fprintf(pw, "%s\t%d\t%d\n", p.IP, p.Count, p.TTLSecs)
		}
		pw.Flush()
	}
	return nil
}
