package main

import (
	"flag"
	"strings"
)

// parseFlags parses fs but, unlike flag.FlagSet.Parse, tolerates a registered
// flag appearing after positional arguments (stdlib stops at the first non-flag
// token and ignores flags after it, so `cmd <user> --restore-orphans` would run
// without the flag). It moves registered flags and their values to the front,
// then Parses; callers keep using fs.Arg/fs.NArg/fs.Args unchanged.
//
// A dash-prefixed token that is not a registered flag stays in place as a
// positional, so negative deltas (`atomic-inc k -5`) and ACL revokes
// (`acl set u box r -lrs`) still work. A literal "--" terminator is preserved.
func parseFlags(fs *flag.FlagSet, args []string) error {
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// Preserve the terminator so fs.Parse still stops flag scanning here.
			positionals = append(positionals, args[i:]...)
			break
		}
		if f := registeredFlag(fs, a); f != nil {
			flags = append(flags, a)
			// A non-bool flag written as "-name value" consumes the next token as
			// its value; "-name=value" and bool flags do not. Never swallow a "--".
			if !strings.Contains(a, "=") && !isBoolFlag(f) && i+1 < len(args) && args[i+1] != "--" {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		// Non-flag, or a dash-token that is not a registered flag: leave in place.
		positionals = append(positionals, a)
	}
	return fs.Parse(append(flags, positionals...))
}

// extractGlobalFlags scans the full argv and pulls out global (flag.CommandLine)
// flags and their values from any position, so -O/--url/--token/--backend-* work
// before the plane, between plane and command, or trailing (#836).
//
// Everything else (the plane, the command, and subcommand-scoped
// flags/positionals) is returned in order as rest for dispatch; a subcommand
// flag is never a CommandLine flag, so it is never stolen here and its own
// parseFlags handles it later. A literal "--" stops extraction: it and the
// remainder pass through to rest verbatim.
func extractGlobalFlags(argv []string) (globals, rest []string) {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			rest = append(rest, argv[i:]...)
			break
		}
		if f := registeredFlag(flag.CommandLine, a); f != nil {
			globals = append(globals, a)
			if !strings.Contains(a, "=") && !isBoolFlag(f) && i+1 < len(argv) && argv[i+1] != "--" {
				i++
				globals = append(globals, argv[i])
			}
			continue
		}
		rest = append(rest, a)
	}
	return globals, rest
}

// registeredFlag returns the flag arg refers to (handling -name / --name /
// -name=value), or nil if arg is not a dash-prefixed token naming a registered
// flag — in which case the caller treats it as a positional.
func registeredFlag(fs *flag.FlagSet, arg string) *flag.Flag {
	if len(arg) < 2 || arg[0] != '-' {
		return nil
	}
	name := strings.TrimLeft(arg, "-")
	if name == "" {
		return nil
	}
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		name = name[:eq]
	}
	return fs.Lookup(name)
}

func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}
