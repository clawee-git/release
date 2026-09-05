// Command clawee-release-manage is the publish-management service for the
// Clawee release channel: the catalog a cut registers into, the operator
// surface that promotes a staged row, and the public face the installers read.
//
// A cut stages privately and registers a row in state `staged`; nothing public
// changes. Going live is a second, operator-only act — promote — and it
// happens here (release-management.md §1). That is the whole reason this
// binary exists: the release kit deliberately cannot publish.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// env is where output goes. It exists so run() is testable through the REAL
// entry point: a help test that calls a subcommand function directly certifies
// a path the user cannot reach.
type env struct {
	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader
}

// handler runs one leaf verb with the arguments that follow it.
type handler func(e *env, n *node, args []string) error

// dispatch is a TABLE keyed by the typed path, not a switch: the table is what
// the lockstep test can walk, and the test is what makes "every tree node has
// a handler and every handler is in the tree" a fact rather than a habit.
var dispatch = map[string]handler{
	"serve":          runServe,
	"retain":         runRetain,
	"publish-static": runPublishStatic,
	"ops render":     runOpsRender,
	"doctor":         runDoctor,
	"admin add":      runAdminAdd,
	"admin list":     runAdminList,
	"admin remove":   runAdminRemove,
	"version":        runVersion,
	"docs":           runDocs,
}

func main() {
	e := &env{stdout: os.Stdout, stderr: os.Stderr, stdin: os.Stdin}
	os.Exit(run(e, os.Args[1:]))
}

// run resolves the command path and dispatches. Its three exits are the whole
// contract: 0 for work done or help asked for, exitUsage for "you typed it
// wrong", 1 for a command that ran and failed.
func run(e *env, args []string) int {
	// A bare invocation is an implicit help request: stdout, exit 0.
	if len(args) == 0 || (len(args) > 0 && helpSpellings[args[0]]) {
		fmt.Fprint(e.stdout, page(root))
		return 0
	}

	cur := root
	i := 0
	for i < len(args) {
		if strings.HasPrefix(args[i], "-") {
			break
		}
		var next *node
		for _, c := range cur.children {
			if c.name == args[i] {
				next = c
				break
			}
		}
		if next == nil {
			// The refusal carries the page for the level they got RIGHT, not
			// the whole tool's and not just the rejected token: naming the
			// mistake while hiding the answer is the commonest form of this
			// defect, and the valid list is three lines away in the code.
			return refuse(e, cur, fmt.Sprintf("unknown %s %q", nounFor(cur), args[i]))
		}
		cur = next
		i++
		if cur.leaf() {
			break
		}
	}

	if !cur.leaf() {
		// A parent answers the help spellings with ITS page, like every other
		// level — the loop above stops at the first "-", so this is where a
		// `<tool> <parent> --help` lands.
		if wantsHelp(args[i:]) {
			fmt.Fprint(e.stdout, page(cur))
			return 0
		}
		// A bare parent falls back to its own page — the level-down twin of
		// the bare tool. Anything left over here is a flag before a
		// subcommand, which is a refusal.
		if i < len(args) {
			return refuse(e, cur, fmt.Sprintf("unknown %s %q", nounFor(cur), args[i]))
		}
		fmt.Fprint(e.stdout, page(cur))
		return 0
	}

	h, ok := dispatch[pathOf(cur)]
	if !ok {
		// Unreachable while the lockstep test passes; if it ever fires, the
		// tree and the table have diverged and saying so beats a nil call.
		fmt.Fprintf(e.stderr, "%s: %s has no handler — the command tree and the dispatch table have diverged\n", toolName, pathOf(cur))
		return 1
	}
	if err := h(e, cur, args[i:]); err != nil {
		var ue *usageError
		if errors.As(err, &ue) {
			return refuse(e, ue.node, ue.msg)
		}
		fmt.Fprintf(e.stderr, "✗ %s %s: %v\n", toolName, pathOf(cur), err)
		return 1
	}
	return 0
}

// refuse prints the message and the level's page to STDERR and returns the
// usage status. Exiting 0 here would make every script pass on a typo.
func refuse(e *env, n *node, msg string) int {
	name := toolName
	if n != root {
		name = toolName + " " + pathOf(n)
	}
	fmt.Fprintf(e.stderr, "%s: %s\n\n", name, msg)
	fmt.Fprint(e.stderr, page(n))
	fmt.Fprint(e.stderr, flagTable(n))
	return exitUsage
}

// nounFor names what the user got wrong at this level, so the message reads
// the way the tree does.
func nounFor(n *node) string {
	if n == root {
		return "command"
	}
	return "subcommand"
}
