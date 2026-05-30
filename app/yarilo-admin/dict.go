package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	_ "github.com/0kaba0hub/yarilo/pkg/dict/drivers/all" // register all built-in dict drivers

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Source-of-dict resolution: callers either point at a yarilo.yaml +
// named Dicts entry, or describe a dict inline via --driver / --setting.
// Inline mode is the daily-debugging path; config-mode is for ops who
// want to operate on the same instance the running server uses.
type dictSelect struct {
	configPath string
	dictName   string
	driver     string
	settings   map[string]any
	username   string
	homeDir    string
	expireSecs uint
}

func dispatchDict(args []string) error {
	if len(args) == 0 {
		printDictUsage()
		return nil
	}
	switch args[0] {
	case "drivers":
		return printDrivers()
	case "lookup":
		return cmdLookup(args[1:])
	case "iterate":
		return cmdIterate(args[1:])
	case "set":
		return cmdSet(args[1:])
	case "unset":
		return cmdUnset(args[1:])
	case "atomic-inc":
		return cmdAtomicInc(args[1:])
	case "expire-scan":
		return cmdExpireScan(args[1:])
	case "commit-batch":
		return cmdCommitBatch(args[1:])
	default:
		return fmt.Errorf("unknown dict command %q — run 'yarilo-admin dict' for usage", args[0])
	}
}

// addSelectFlags wires the shared --config/--dict/--driver/--setting/
// --user/--home/--expire-secs flags onto fs. Returns a func that
// resolves the chosen flags into a dictSelect after fs.Parse runs.
func addSelectFlags(fs *flag.FlagSet) func() dictSelect {
	var sel dictSelect
	sel.settings = map[string]any{}
	setting := stringSlice{}

	fs.StringVar(&sel.configPath, "config", os.Getenv("YARILO_CONFIG"), "path to yarilo.yaml (env: YARILO_CONFIG)")
	fs.StringVar(&sel.dictName, "dict", "", "name of dict in Config.Dicts (used with --config)")
	fs.StringVar(&sel.driver, "driver", "", "inline driver name (alternative to --config + --dict)")
	fs.Var(&setting, "setting", "inline driver setting (key=value); may be repeated")
	fs.StringVar(&sel.username, "user", "", "OpSettings.Username")
	fs.StringVar(&sel.homeDir, "home", "", "OpSettings.HomeDir")
	fs.UintVar(&sel.expireSecs, "expire-secs", 0, "OpSettings.ExpireSecs (default TTL for writes)")

	return func() dictSelect {
		for _, s := range setting {
			eq := strings.IndexByte(s, '=')
			if eq < 0 {
				fmt.Fprintf(os.Stderr, "ignoring malformed --setting %q (expected key=value)\n", s)
				continue
			}
			sel.settings[s[:eq]] = s[eq+1:]
		}
		return sel
	}
}

// resolve opens the chosen dict using either config or inline flags.
// Caller closes the returned Dict.
func (s dictSelect) resolve() (dict.Dict, *dict.OpSettings, error) {
	var cfg dict.Config

	switch {
	case s.driver != "":
		cfg = dict.Config{Driver: s.driver, Settings: s.settings}
	case s.configPath != "" && s.dictName != "":
		k := koanf.New(".")
		if err := k.Load(file.Provider(s.configPath), yaml.Parser()); err != nil {
			return nil, nil, fmt.Errorf("load %s: %w", s.configPath, err)
		}
		var full config.Config
		if err := k.Unmarshal("", &full); err != nil {
			return nil, nil, fmt.Errorf("unmarshal: %w", err)
		}
		dc, ok := full.Dicts[s.dictName]
		if !ok {
			names := make([]string, 0, len(full.Dicts))
			for n := range full.Dicts {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, nil, fmt.Errorf("dict %q not found in %s (have: %v)", s.dictName, s.configPath, names)
		}
		cfg = dict.Config{Driver: dc.Driver, Settings: dc.Settings}
		if s.username == "" {
			s.username = dc.Username
		}
		if s.homeDir == "" {
			s.homeDir = dc.HomeDir
		}
		if s.expireSecs == 0 {
			s.expireSecs = uint(dc.ExpireSecs)
		}
	default:
		return nil, nil, fmt.Errorf("specify either --config + --dict, or --driver + --setting")
	}

	d, err := dict.Open(cfg)
	if err != nil {
		return nil, nil, err
	}
	return d, &dict.OpSettings{
		Username:   s.username,
		HomeDir:    s.homeDir,
		ExpireSecs: uint32(s.expireSecs),
	}, nil
}

// stringSlice implements flag.Value for repeated --setting flags.
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ", ") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// ----- subcommands -----

func printDrivers() error {
	for _, d := range dict.Drivers() {
		fmt.Println(d)
	}
	return nil
}

func cmdLookup(args []string) error {
	fs := flag.NewFlagSet("lookup", flag.ExitOnError)
	resolveSel := addSelectFlags(fs)
	fs.Parse(args) //nolint:errcheck
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: dict lookup [select-flags] KEY")
	}
	d, ops, err := resolveSel().resolve()
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck

	vals, found, err := d.Lookup(context.Background(), ops, fs.Arg(0))
	if err != nil {
		return err
	}
	if !found {
		fmt.Println("(not found)")
		return nil
	}
	for _, v := range vals {
		fmt.Println(formatValue(v))
	}
	return nil
}

func cmdIterate(args []string) error {
	fs := flag.NewFlagSet("iterate", flag.ExitOnError)
	recurse := fs.Bool("recurse", false, "descend into sub-hierarchies")
	noValue := fs.Bool("no-value", false, "print keys only")
	exact := fs.Bool("exact", false, "exact-key mode (all values for one key)")
	sortKey := fs.Bool("sort-key", false, "sort results by key")
	sortValue := fs.Bool("sort-value", false, "sort results by value")
	resolveSel := addSelectFlags(fs)
	fs.Parse(args) //nolint:errcheck
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: dict iterate [select-flags] [--recurse] [--no-value] [--exact] [--sort-key|--sort-value] PATH")
	}

	flags := dict.IterFlag(0)
	if *recurse {
		flags |= dict.IterRecurse
	}
	if *noValue {
		flags |= dict.IterNoValue
	}
	if *exact {
		flags |= dict.IterExactKey
	}
	if *sortKey {
		flags |= dict.IterSortByKey
	}
	if *sortValue {
		flags |= dict.IterSortByValue
	}

	d, ops, err := resolveSel().resolve()
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck

	it, err := d.Iterate(context.Background(), ops, fs.Arg(0), flags)
	if err != nil {
		return err
	}
	defer it.Close() //nolint:errcheck
	for it.Next() {
		if *noValue {
			fmt.Println(it.Key())
			continue
		}
		vs := it.Values()
		if len(vs) == 0 {
			fmt.Printf("%s\t(no value)\n", it.Key())
			continue
		}
		fmt.Printf("%s\t%s\n", it.Key(), formatValue(vs[0]))
	}
	return it.Err()
}

func cmdSet(args []string) error {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	stdinValue := fs.Bool("value-stdin", false, "read value from stdin instead of args")
	resolveSel := addSelectFlags(fs)
	fs.Parse(args) //nolint:errcheck

	var key string
	var value []byte
	switch {
	case *stdinValue && fs.NArg() == 1:
		key = fs.Arg(0)
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		value = b
	case fs.NArg() == 2:
		key = fs.Arg(0)
		value = []byte(fs.Arg(1))
	default:
		return fmt.Errorf("usage: dict set [select-flags] KEY VALUE   |   dict set --value-stdin [select-flags] KEY")
	}

	d, ops, err := resolveSel().resolve()
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck

	tx, err := d.Begin(context.Background(), ops)
	if err != nil {
		return err
	}
	if err := tx.Set(key, value); err != nil {
		return err
	}
	return finalCommit(tx)
}

func cmdUnset(args []string) error {
	fs := flag.NewFlagSet("unset", flag.ExitOnError)
	resolveSel := addSelectFlags(fs)
	fs.Parse(args) //nolint:errcheck
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: dict unset [select-flags] KEY")
	}
	d, ops, err := resolveSel().resolve()
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck

	tx, err := d.Begin(context.Background(), ops)
	if err != nil {
		return err
	}
	if err := tx.Unset(fs.Arg(0)); err != nil {
		return err
	}
	return finalCommit(tx)
}

func cmdAtomicInc(args []string) error {
	fs := flag.NewFlagSet("atomic-inc", flag.ExitOnError)
	resolveSel := addSelectFlags(fs)
	fs.Parse(args) //nolint:errcheck
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: dict atomic-inc [select-flags] KEY DELTA")
	}
	delta, err := strconv.ParseInt(fs.Arg(1), 10, 64)
	if err != nil {
		return fmt.Errorf("DELTA: %w", err)
	}
	d, ops, err := resolveSel().resolve()
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck

	tx, err := d.Begin(context.Background(), ops)
	if err != nil {
		return err
	}
	if err := tx.AtomicInc(fs.Arg(0), delta); err != nil {
		return err
	}
	return finalCommit(tx)
}

func cmdExpireScan(args []string) error {
	fs := flag.NewFlagSet("expire-scan", flag.ExitOnError)
	resolveSel := addSelectFlags(fs)
	fs.Parse(args) //nolint:errcheck
	d, _, err := resolveSel().resolve()
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck
	if err := d.ExpireScan(context.Background()); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

// cmdCommitBatch reads a stream of TAB-delimited tx ops from stdin and
// commits them as a single atomic transaction. Format per line:
//
//	set\tKEY\tBASE64-VALUE
//	unset\tKEY
//	atomic-inc\tKEY\tDELTA
//
// Lines starting with '#' and empty lines are ignored. Value is base64
// so binary content survives line-based parsing. Useful for bulk imports
// without spawning a CLI per row.
func cmdCommitBatch(args []string) error {
	fs := flag.NewFlagSet("commit-batch", flag.ExitOnError)
	resolveSel := addSelectFlags(fs)
	fs.Parse(args) //nolint:errcheck

	d, ops, err := resolveSel().resolve()
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck

	tx, err := d.Begin(context.Background(), ops)
	if err != nil {
		return err
	}

	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("read stdin: %w", err)
	}
	for i, line := range strings.Split(string(in), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		switch parts[0] {
		case "set":
			if len(parts) != 3 {
				_ = tx.Rollback()
				return fmt.Errorf("line %d: 'set' needs KEY and base64 VALUE", i+1)
			}
			v, err := base64.StdEncoding.DecodeString(parts[2])
			if err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("line %d: decode value: %w", i+1, err)
			}
			if err := tx.Set(parts[1], v); err != nil {
				_ = tx.Rollback()
				return err
			}
		case "unset":
			if len(parts) != 2 {
				_ = tx.Rollback()
				return fmt.Errorf("line %d: 'unset' needs KEY", i+1)
			}
			if err := tx.Unset(parts[1]); err != nil {
				_ = tx.Rollback()
				return err
			}
		case "atomic-inc":
			if len(parts) != 3 {
				_ = tx.Rollback()
				return fmt.Errorf("line %d: 'atomic-inc' needs KEY and DELTA", i+1)
			}
			delta, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("line %d: DELTA: %w", i+1, err)
			}
			if err := tx.AtomicInc(parts[1], delta); err != nil {
				_ = tx.Rollback()
				return err
			}
		default:
			_ = tx.Rollback()
			return fmt.Errorf("line %d: unknown op %q", i+1, parts[0])
		}
	}
	return finalCommit(tx)
}

func finalCommit(tx dict.Tx) error {
	res, err := tx.Commit()
	if err != nil {
		return err
	}
	switch res {
	case dict.CommitOK:
		fmt.Println("ok")
	case dict.CommitNotFound:
		fmt.Println("not-found (atomic-inc on missing key)")
	case dict.CommitWriteUncertain:
		fmt.Println("write-uncertain")
	default:
		fmt.Printf("commit-result=%d\n", res)
	}
	return nil
}

// formatValue prints a value as UTF-8 text when it looks printable,
// otherwise base64. Saves operators from staring at "\x01\x02" garbage.
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

Select the dict either via config:
  --config PATH        path to yarilo.yaml (env: YARILO_CONFIG)
  --dict NAME          dict name in Config.Dicts

Or inline (ad-hoc):
  --driver DRIVER      driver name (run 'yarilo-admin dict drivers' for list)
  --setting key=value  driver-specific setting (repeatable)

Per-op identity:
  --user USER          OpSettings.Username
  --home DIR           OpSettings.HomeDir
  --expire-secs N      default TTL for writes (drivers that support it)

Commands:
  drivers                          List registered drivers
  lookup KEY                       Print value for KEY
  iterate [flags] PATH             List rows under PATH
                                   flags: --recurse --no-value --exact --sort-key --sort-value
  set [--value-stdin] KEY [VALUE]  Write VALUE at KEY (binary via --value-stdin)
  unset KEY                        Remove KEY
  atomic-inc KEY DELTA             Atomic integer add
  expire-scan                      Drop TTL-expired rows
  commit-batch                     Read multi-op TAB-delimited script from stdin

Examples:
  yarilo-admin dict --driver file --setting path=/tmp/m.dict set priv/foo bar
  yarilo-admin dict --config /etc/yarilo.yaml --dict metadata lookup priv/box/INBOX/comment
  yarilo-admin dict --driver redis --setting addr=localhost:6379 iterate --recurse --sort-key priv/`)
}
