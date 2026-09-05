package main

// The shared parse helper. Every leaf goes through it: a hand-rolled argv loop
// is a defect class, not a style choice — it recognises help only in first
// position, rejects the --flag=value spelling the rest of the tool honours,
// and silently accepts unknown flags as positionals (cli-help.md §7).

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// exitUsage is the tool's ONE "you typed it wrong" status. 1 stays reserved
// for a command that ran and failed; a binary with two typo statuses is worse
// than either choice (cli-help.md §4).
const exitUsage = 2

// usageError is a refusal that carries the page for the level the user got
// wrong. Every malformed-invocation refusal in every verb returns one of these
// — including POST-PARSE checks like "a required flag is missing", which are
// invocation-shape errors exactly as a bad flag is.
type usageError struct {
	node *node
	msg  string
}

func (e *usageError) Error() string { return e.msg }

// usagef builds a usage error for n.
func usagef(n *node, format string, a ...any) error {
	return &usageError{node: n, msg: fmt.Sprintf(format, a...)}
}

// parseVerbFlags runs a leaf's flags through fs and answers the help contract.
//
// It returns handled=true when the invocation was a help request and the page
// has already been printed to stdout — the caller returns nil, exit 0.
func parseVerbFlags(e *env, n *node, fs *flag.FlagSet, args []string) (handled bool, err error) {
	// Suppress the parser's own output: flag.FlagSet writes usage to stderr
	// even for an explicit -h, which would make every help request a refusal
	// on the wrong stream (cli-help.md §4, the stdlib trap).
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	if wantsHelp(args) {
		fmt.Fprint(e.stdout, page(n))
		return true, nil
	}
	// Go's flag stops at the first non-flag argument, so `admin add ada
	// --data-dir /x` would silently leave --data-dir unparsed and land in
	// Args() as two stray positionals. Reordering first means a verb accepts
	// flags before OR after its positionals, which is how people actually type
	// them — and it is done here, once, rather than by each verb inventing its
	// own argv loop.
	if err := fs.Parse(permute(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(e.stdout, page(n))
			return true, nil
		}
		return false, usagef(n, "%s", err.Error())
	}
	return false, nil
}

// rejectResiduals refuses positionals the verb did not claim. Go's flag parses
// what it knows and leaves the rest in Args(); nothing complains if nobody
// looks, and a stray positional is usually a mistyped flag.
func rejectResiduals(n *node, fs *flag.FlagSet) error {
	if fs.NArg() == 0 {
		return nil
	}
	return usagef(n, "unexpected argument %q", fs.Arg(0))
}

// requireArgs enforces an exact positional count. Verbs whose refusal is an
// arity check satisfy the same contract as rejectResiduals — same status, same
// page — and the same test covers both.
func requireArgs(n *node, fs *flag.FlagSet, want int) error {
	if fs.NArg() == want {
		return nil
	}
	shape := n.shape
	if shape == "" {
		shape = "no arguments"
	}
	return usagef(n, "%s %s takes %s, got %d", toolName, pathOf(n), shape, fs.NArg())
}

// requireDataDir is the post-parse invocation check every persistent verb
// runs. There is deliberately NO default and no environment read: the data dir
// is where the catalog and the service's root secret live, and a guessed root
// is either a second empty catalog or a write into somebody else's
// (privilege.md — a flag-steered root is validated at its own writer).
func requireDataDir(n *node, dir string) error {
	if strings.TrimSpace(dir) == "" {
		return usagef(n, "--data-dir is required; there is no default, because a guessed data directory is either an empty second catalog or a write into another deployment's")
	}
	return nil
}

// permute moves flags ahead of positionals, using the FlagSet to tell a
// boolean flag (which consumes nothing) from one that takes a value.
//
// An UNKNOWN flag is left consuming only itself, so flag.Parse reports it as
// "flag provided but not defined" rather than this function guessing whether
// the token after it was its value.
func permute(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			flags = append(flags, a)
			continue
		}
		flags = append(flags, a)
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	if len(positional) == 0 {
		return flags
	}
	// A "--" between the two halves is what keeps a positional that LOOKS like
	// a flag a positional: without it, `-- --help` reordered into the flag half
	// would be read as a help request rather than as the argument it is.
	return append(append(flags, "--"), positional...)
}
