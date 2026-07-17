package main

import (
	"flag"
	"strings"
)

// parseFlags parses fs but, unlike flag.FlagSet.Parse, tolerates flags that
// appear AFTER positional arguments. Go's flag package stops at the first
// non-flag token and silently ignores every flag after it — a footgun for a
// repair CLI, where `cmd <user> --restore-orphans` would quietly run WITHOUT the
// flag. parseFlags reorders args so all flags (and their values) precede the
// positionals, then does a normal Parse; callers keep using fs.Arg / fs.NArg /
// fs.Args unchanged.
//
// A literal "--" ends flag parsing (everything after is positional). A token
// that looks like a flag but is unknown is passed through so Parse reports it.
func parseFlags(fs *flag.FlagSet, args []string) error {
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			// A non-bool flag written as "-name value" consumes the next token as
			// its value; "-name=value" and bool flags do not.
			if !strings.Contains(a, "=") && flagNeedsValue(fs, a) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return fs.Parse(append(flags, positionals...))
}

// flagNeedsValue reports whether the named flag takes a separate value token.
// Unknown flags return false so Parse surfaces the error naturally; bool flags
// (IsBoolFlag) take no value.
func flagNeedsValue(fs *flag.FlagSet, arg string) bool {
	f := fs.Lookup(strings.TrimLeft(arg, "-"))
	if f == nil {
		return false
	}
	if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
		return false
	}
	return true
}
