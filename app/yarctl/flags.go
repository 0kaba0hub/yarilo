package main

import (
	"flag"
	"strings"
)

// parseFlags parses fs but, unlike flag.FlagSet.Parse, tolerates a REGISTERED
// flag that appears AFTER positional arguments. Go's flag package stops at the
// first non-flag token and silently ignores every flag after it — a footgun for
// a repair CLI, where `cmd <user> --restore-orphans` would quietly run WITHOUT
// the flag. parseFlags pulls only the tokens that match a registered flag (and
// their values) to the front, then does a normal Parse; callers keep using
// fs.Arg / fs.NArg / fs.Args unchanged.
//
// Crucially, a dash-prefixed token that is NOT a registered flag is left in
// place as a positional — so legitimate dash-prefixed positionals still work
// exactly as stdlib treats them (a negative delta `atomic-inc k -5`, an ACL
// revoke `acl set u box r -lrs`). fs.Parse then applies its own semantics: such
// a token errors only if it lands before the first positional (an unknown flag),
// and is a plain arg otherwise. A literal "--" terminator is preserved verbatim.
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

// extractGlobalFlags scans the FULL argv and pulls out tokens that name a
// global (flag.CommandLine) flag — plus their values — from ANY position, so
// -O/--url/--token/--backend-* work whether they appear before the plane,
// between the plane and the command, or trailing (#836). Previously globals
// were read only in the prefix (flag.Parse stops at the plane word), so a
// trailing "-O json" was silently swallowed and broke scripts that parsed JSON.
//
// Everything that is not a global flag (the plane, the command, and every
// subcommand-scoped flag/positional) is returned in order as rest, untouched,
// for dispatch — a subcommand flag is never a CommandLine flag, so it is never
// stolen here; its own parseFlags handles it later. A literal "--" stops global
// extraction: it and the remainder pass through to rest verbatim so the
// subcommand still sees the terminator.
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
