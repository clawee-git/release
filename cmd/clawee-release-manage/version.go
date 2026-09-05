package main

import (
	"flag"
	"fmt"
	"runtime/debug"

	"github.com/clawee-git/release/internal/manage/store"
)

func runVersion(e *env, n *node, args []string) error {
	var o versionOpts
	fs := flag.NewFlagSet(toolName+" "+pathOf(n), flag.ContinueOnError)
	o.register(fs)
	handled, err := parseVerbFlags(e, n, fs, args)
	if handled || err != nil {
		return err
	}
	if err := rejectResiduals(n, fs); err != nil {
		return err
	}

	fmt.Fprintf(e.stdout, "%s %s\n", toolName, buildStamp())

	// --data-dir is optional here on purpose: `version` must answer on a host
	// with no catalog at all, which is exactly the host an operator is
	// checking when they run it.
	if o.dataDir == "" {
		return nil
	}
	st, err := store.Open(o.dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	applied, err := st.AppliedMigrations()
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "catalog: %s\n", o.dataDir)
	for _, m := range applied {
		fmt.Fprintf(e.stdout, "  migration %s\n", m)
	}
	return nil
}

// buildStamp reads the version out of the embedded build info rather than a
// constant someone has to remember to bump. A binary built from a dirty tree
// says so, which is the case worth seeing on a host.
func buildStamp() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(no build info)"
	}
	version := info.Main.Version
	if version == "" || version == "(devel)" {
		version = "devel"
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 8 {
				revision = s.Value[:8]
			} else {
				revision = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				modified = "-dirty"
			}
		}
	}
	if revision == "" {
		return version
	}
	return version + " (" + revision + modified + ")"
}
