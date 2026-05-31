package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Source-of-dict resolution model:
//
// `yarilo-admin dict` is a thin HTTP client over the yarilo-admin-api
// endpoint (--admin-url, default http://localhost:9105 or
// $YARILO_ADMIN_API_URL). The admin-api process owns the live
// dict.Dict instances; this CLI never opens dicts directly.
//
// Per-op identity (OpSettings.Username / HomeDir / ExpireSecs) is
// passed in the request body of every op that needs it. The CLI
// exposes them via --user / --home / --expire-secs flags so an
// operator can run a single ad-hoc op without setting environment.
//
// Phase OPS-ADMIN-PROXY (future) will let --url point at a director,
// which transparently proxies /api/admin/... requests to the right
// backend's yarilo-admin-api based on the user-to-backend ring.

func dispatchDict(args []string) error {
	if len(args) == 0 {
		printDictUsage()
		return nil
	}
	switch args[0] {
	case "drivers":
		return cmdDictDrivers()
	case "exists":
		return cmdDictExists(args[1:])
	case "lookup":
		return cmdDictLookup(args[1:])
	case "iterate":
		return cmdDictIterate(args[1:])
	case "set":
		return cmdDictSet(args[1:])
	case "unset":
		return cmdDictUnset(args[1:])
	case "atomic-inc":
		return cmdDictAtomicInc(args[1:])
	case "expire-scan":
		return cmdDictExpireScan(args[1:])
	case "commit-batch":
		return cmdDictCommitBatch(args[1:])
	default:
		return fmt.Errorf("unknown dict command %q — run 'yarilo-admin dict' for usage", args[0])
	}
}

// opSettingsFlags wires the standard per-op identity flags onto fs.
// Returned closure assembles the wire-shape OpSettings struct after
// fs.Parse runs. All three fields are optional — the admin-api
// treats a zero-value OpSettings as "use defaults".
type opSettingsWire struct {
	Username   string `json:"username,omitempty"`
	HomeDir    string `json:"home_dir,omitempty"`
	ExpireSecs uint32 `json:"expire_secs,omitempty"`
}

func addOpFlags(fs *flag.FlagSet) func() opSettingsWire {
	var (
		user       string
		home       string
		expireSecs uint
	)
	fs.StringVar(&user, "user", "", "OpSettings.Username")
	fs.StringVar(&home, "home", "", "OpSettings.HomeDir")
	fs.UintVar(&expireSecs, "expire-secs", 0, "OpSettings.ExpireSecs (default TTL for writes; drivers without TTL ignore)")
	return func() opSettingsWire {
		return opSettingsWire{
			Username:   user,
			HomeDir:    home,
			ExpireSecs: uint32(expireSecs),
		}
	}
}

// dictPath returns "/api/admin/dict/<name>/<suffix>" with the dict
// name URL-escaped so weird characters in operator names do not
// corrupt the route.
func dictPath(name, suffix string) string {
	return "/api/admin/dict/" + url.PathEscape(name) + "/" + suffix
}

// ----- subcommands ---------------------------------------------------------

func cmdDictDrivers() error {
	return printJSON(adminAPIGet("/api/admin/dict/drivers"))
}

func cmdDictExists(args []string) error {
	fs := flag.NewFlagSet("exists", flag.ExitOnError)
	fs.Parse(args) //nolint:errcheck
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: dict exists NAME")
	}
	return printJSON(adminAPIGet("/api/admin/dict/" + url.PathEscape(fs.Arg(0)) + "/exists"))
}

type lookupBody struct {
	Key string         `json:"key"`
	Op  opSettingsWire `json:"op,omitempty"`
}

func cmdDictLookup(args []string) error {
	fs := flag.NewFlagSet("lookup", flag.ExitOnError)
	resolveOp := addOpFlags(fs)
	fs.Parse(args) //nolint:errcheck
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: dict lookup [op-flags] NAME KEY")
	}
	data, err := adminAPIPost(dictPath(fs.Arg(0), "lookup"), lookupBody{
		Key: fs.Arg(1),
		Op:  resolveOp(),
	})
	if err != nil {
		return err
	}
	var resp struct {
		Found  bool     `json:"found"`
		Values [][]byte `json:"values"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if !resp.Found {
		fmt.Println("(not found)")
		return nil
	}
	for _, v := range resp.Values {
		fmt.Println(formatValue(v))
	}
	return nil
}

type iterateBody struct {
	Path  string         `json:"path"`
	Flags uint32         `json:"flags,omitempty"`
	Op    opSettingsWire `json:"op,omitempty"`
}

func cmdDictIterate(args []string) error {
	fs := flag.NewFlagSet("iterate", flag.ExitOnError)
	recurse := fs.Bool("recurse", false, "descend into sub-hierarchies")
	noValue := fs.Bool("no-value", false, "print keys only")
	exact := fs.Bool("exact", false, "exact-key mode (all values for one key)")
	sortKey := fs.Bool("sort-key", false, "sort results by key")
	sortValue := fs.Bool("sort-value", false, "sort results by value")
	resolveOp := addOpFlags(fs)
	fs.Parse(args) //nolint:errcheck
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: dict iterate [op-flags] [--recurse] [--no-value] [--exact] [--sort-key|--sort-value] NAME PATH")
	}
	flagsMask := uint32(0)
	// Bits match pkg/dict.IterFlag: Recurse=1, SortByKey=2, SortByValue=4, NoValue=8, ExactKey=16.
	if *recurse {
		flagsMask |= 1
	}
	if *sortKey {
		flagsMask |= 2
	}
	if *sortValue {
		flagsMask |= 4
	}
	if *noValue {
		flagsMask |= 8
	}
	if *exact {
		flagsMask |= 16
	}

	body := iterateBody{Path: fs.Arg(1), Flags: flagsMask, Op: resolveOp()}
	stream, err := adminAPIStream(dictPath(fs.Arg(0), "iterate"), body)
	if err != nil {
		return err
	}
	defer stream.Close() //nolint:errcheck

	// NDJSON decode — one row per line. Server emits a final
	// {"error": ...} row on mid-stream failure (HTTP status is
	// already 200, so we check each row).
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		// Try error envelope first.
		var maybeErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(line, &maybeErr) == nil && maybeErr.Error != "" {
			return fmt.Errorf("stream error: %s", maybeErr.Error)
		}
		var row struct {
			Key    string   `json:"key"`
			Values [][]byte `json:"values"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("decode ndjson row: %w", err)
		}
		if *noValue {
			fmt.Println(row.Key)
			continue
		}
		if len(row.Values) == 0 {
			fmt.Printf("%s\t(no value)\n", row.Key)
			continue
		}
		fmt.Printf("%s\t%s\n", row.Key, formatValue(row.Values[0]))
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	return nil
}

type setBody struct {
	Key   string         `json:"key"`
	Value []byte         `json:"value"`
	Op    opSettingsWire `json:"op,omitempty"`
}

func cmdDictSet(args []string) error {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	stdinValue := fs.Bool("value-stdin", false, "read value from stdin instead of args")
	resolveOp := addOpFlags(fs)
	fs.Parse(args) //nolint:errcheck

	var name, key string
	var value []byte
	switch {
	case *stdinValue && fs.NArg() == 2:
		name = fs.Arg(0)
		key = fs.Arg(1)
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		value = b
	case fs.NArg() == 3:
		name = fs.Arg(0)
		key = fs.Arg(1)
		value = []byte(fs.Arg(2))
	default:
		return fmt.Errorf("usage: dict set [op-flags] NAME KEY VALUE  |  dict set --value-stdin [op-flags] NAME KEY")
	}
	return printCommit(adminAPIPost(dictPath(name, "set"), setBody{Key: key, Value: value, Op: resolveOp()}))
}

type unsetBody struct {
	Key string         `json:"key"`
	Op  opSettingsWire `json:"op,omitempty"`
}

func cmdDictUnset(args []string) error {
	fs := flag.NewFlagSet("unset", flag.ExitOnError)
	resolveOp := addOpFlags(fs)
	fs.Parse(args) //nolint:errcheck
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: dict unset [op-flags] NAME KEY")
	}
	return printCommit(adminAPIPost(dictPath(fs.Arg(0), "unset"), unsetBody{Key: fs.Arg(1), Op: resolveOp()}))
}

type atomicIncBody struct {
	Key   string         `json:"key"`
	Delta int64          `json:"delta"`
	Op    opSettingsWire `json:"op,omitempty"`
}

func cmdDictAtomicInc(args []string) error {
	fs := flag.NewFlagSet("atomic-inc", flag.ExitOnError)
	resolveOp := addOpFlags(fs)
	fs.Parse(args) //nolint:errcheck
	if fs.NArg() != 3 {
		return fmt.Errorf("usage: dict atomic-inc [op-flags] NAME KEY DELTA")
	}
	delta, err := strconv.ParseInt(fs.Arg(2), 10, 64)
	if err != nil {
		return fmt.Errorf("DELTA: %w", err)
	}
	return printCommit(adminAPIPost(dictPath(fs.Arg(0), "atomic-inc"), atomicIncBody{
		Key: fs.Arg(1), Delta: delta, Op: resolveOp(),
	}))
}

func cmdDictExpireScan(args []string) error {
	fs := flag.NewFlagSet("expire-scan", flag.ExitOnError)
	fs.Parse(args) //nolint:errcheck
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: dict expire-scan NAME")
	}
	return printJSON(adminAPIPost(dictPath(fs.Arg(0), "expire-scan"), struct{}{}))
}

type batchOpWire struct {
	Kind  string `json:"kind"`
	Key   string `json:"key"`
	Value []byte `json:"value,omitempty"`
	Delta int64  `json:"delta,omitempty"`
}

type commitBatchBody struct {
	Op  opSettingsWire `json:"op,omitempty"`
	Ops []batchOpWire  `json:"ops"`
}

// cmdDictCommitBatch reads a TAB-delimited script from stdin and
// sends it as a single atomic transaction. Format per line:
//
//	set\tKEY\tBASE64-VALUE
//	unset\tKEY
//	atomic-inc\tKEY\tDELTA
//
// '#' comments and empty lines ignored. Base64 keeps binary values
// safe through line-based parsing.
func cmdDictCommitBatch(args []string) error {
	fs := flag.NewFlagSet("commit-batch", flag.ExitOnError)
	resolveOp := addOpFlags(fs)
	fs.Parse(args) //nolint:errcheck
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: dict commit-batch [op-flags] NAME < script")
	}

	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	body := commitBatchBody{Op: resolveOp()}
	for i, line := range strings.Split(string(in), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		switch parts[0] {
		case "set":
			if len(parts) != 3 {
				return fmt.Errorf("line %d: 'set' needs KEY and base64 VALUE", i+1)
			}
			v, err := base64.StdEncoding.DecodeString(parts[2])
			if err != nil {
				return fmt.Errorf("line %d: decode value: %w", i+1, err)
			}
			body.Ops = append(body.Ops, batchOpWire{Kind: "set", Key: parts[1], Value: v})
		case "unset":
			if len(parts) != 2 {
				return fmt.Errorf("line %d: 'unset' needs KEY", i+1)
			}
			body.Ops = append(body.Ops, batchOpWire{Kind: "unset", Key: parts[1]})
		case "atomic-inc":
			if len(parts) != 3 {
				return fmt.Errorf("line %d: 'atomic-inc' needs KEY and DELTA", i+1)
			}
			delta, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				return fmt.Errorf("line %d: DELTA: %w", i+1, err)
			}
			body.Ops = append(body.Ops, batchOpWire{Kind: "atomic-inc", Key: parts[1], Delta: delta})
		default:
			return fmt.Errorf("line %d: unknown op %q", i+1, parts[0])
		}
	}
	if len(body.Ops) == 0 {
		return fmt.Errorf("no ops parsed from stdin")
	}
	return printCommit(adminAPIPost(dictPath(fs.Arg(0), "commit-batch"), body))
}

// ----- helpers --------------------------------------------------------------

// printCommit decodes the wire commitResp and prints a one-line
// human summary mirroring the pre-v1.23 direct-access CLI output so
// existing operator workflows do not change.
func printCommit(data []byte, err error) error {
	if err != nil {
		return err
	}
	var resp struct {
		Result int    `json:"result"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("parse commit response: %w", err)
	}
	switch resp.Status {
	case "ok":
		fmt.Println("ok")
	case "not-found":
		fmt.Println("not-found (atomic-inc on missing key)")
	case "write-uncertain":
		fmt.Println("write-uncertain")
	default:
		fmt.Printf("commit-result=%d status=%s\n", resp.Result, resp.Status)
	}
	return nil
}

func formatValue(v []byte) string {
	if isPrintable(v) {
		return string(v)
	}
	return "base64:" + base64.StdEncoding.EncodeToString(v)
}

func isPrintable(v []byte) bool {
	if len(v) == 0 {
		return true
	}
	for _, b := range v {
		if b == '\t' || b == '\r' || b == '\n' {
			continue
		}
		if b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

func printDictUsage() {
	fmt.Fprintln(os.Stderr, `yarilo-admin dict <command>

Talks to yarilo-admin-api (default http://localhost:9105 or
$YARILO_ADMIN_API_URL). Override per-invocation:
  --admin-url URL
  --admin-token TOKEN

Per-op identity flags (where applicable):
  --user USER          OpSettings.Username
  --home DIR           OpSettings.HomeDir
  --expire-secs N      OpSettings.ExpireSecs (default TTL for writes)

Commands:
  drivers                                List dict drivers registered on admin-api
  exists NAME                            Does this dict name resolve to a configured dict?
  lookup [op-flags] NAME KEY             Print value for KEY
  iterate [flags] [op-flags] NAME PATH   List rows (NDJSON streamed)
                                         flags: --recurse --no-value --exact --sort-key --sort-value
  set [--value-stdin] [op-flags] NAME KEY [VALUE]
                                         Write VALUE at KEY (binary via --value-stdin)
  unset [op-flags] NAME KEY              Remove KEY
  atomic-inc [op-flags] NAME KEY DELTA   Atomic integer add
  expire-scan NAME                       Drop TTL-expired rows
  commit-batch [op-flags] NAME           Read multi-op TAB-delimited script from stdin
                                         (set\tKEY\tBASE64, unset\tKEY, atomic-inc\tKEY\tDELTA)

Examples:
  yarilo-admin dict drivers
  yarilo-admin dict lookup metadata priv/box/abc123/comment
  yarilo-admin dict iterate --recurse --sort-key metadata priv/
  yarilo-admin dict set metadata priv/foo bar
  yarilo-admin dict atomic-inc quota priv/quota/storage 1024`)
}
