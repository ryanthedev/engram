package cli

import (
	"flag"
	"fmt"
)

// parseInterleaved parses args against fs, accepting flags both before and
// after positional arguments, and returns the positionals in order.
//
// Go's flag package stops at the first non-flag argument, so with a plain
// fs.Parse the documented `engram search QUERY -k 3` silently ignored -k (it
// landed in fs.Args() as a positional and was never read). This helper
// re-parses the remainder after each positional so the usage banner's order
// and the flags-first order both work, and an unknown flag after a
// positional is an error ("flag provided but not defined") instead of
// silently dropped.
//
// A bare "--" ends flag parsing exactly as in flag: everything after it is
// positional, even if it looks like a flag. (Corner case, documented not
// engineered around: "--" given as the VALUE of a string flag, e.g.
// `-token --`, also stops interleaving at that point.)
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positionals, nil
		}
		// flag consumed "--" as the terminator: the tail is all positional.
		consumed := len(rest) - fs.NArg()
		if consumed > 0 && rest[consumed-1] == "--" {
			return append(positionals, fs.Args()...), nil
		}
		positionals = append(positionals, fs.Arg(0))
		rest = fs.Args()[1:]
	}
}

// parseFlagsOnly is parseInterleaved for subcommands that take no
// positional arguments: any positional is an error rather than a silent
// point past which every later flag is ignored (e.g. `status junk -addr X`).
func parseFlagsOnly(fs *flag.FlagSet, args []string) error {
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("%s: unexpected argument %q", fs.Name(), pos[0])
	}
	return nil
}

// parseOnePositional is parseInterleaved for subcommands that take exactly
// one positional (named what for the error message, e.g. "<handle>").
func parseOnePositional(fs *flag.FlagSet, args []string, what string) (string, error) {
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return "", err
	}
	if len(pos) != 1 {
		return "", fmt.Errorf("%s: expected exactly one %s", fs.Name(), what)
	}
	return pos[0], nil
}
