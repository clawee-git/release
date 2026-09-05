package main

// The `retain` verb: the nightly net.
//
// Promote runs retention for the (component, channel) it just published, and
// nothing else — so a component nobody has promoted for a month never runs it
// at all. This verb runs the same pass over every component and channel, and
// is what a timer invokes. A retention pass that only reports is not a
// control, so this one prunes.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/clawee-git/release/internal/manage/publish"
	"github.com/clawee-git/release/internal/manage/store"
)

type retainOpts struct {
	commonOpts
	storeOpts
	dryRun bool
}

func (o *retainOpts) register(fs *flag.FlagSet) {
	o.registerDataDir(fs)
	o.storeOpts.register(fs)
	fs.BoolVar(&o.dryRun, "dry-run", false,
		"print which rows WOULD be expired and prune nothing")
}

func runRetain(e *env, n *node, args []string) error {
	var o retainOpts
	fs := flag.NewFlagSet(toolName+" "+pathOf(n), flag.ContinueOnError)
	o.register(fs)
	handled, err := parseVerbFlags(e, n, fs, args)
	if handled || err != nil {
		return err
	}
	if err := rejectResiduals(n, fs); err != nil {
		return err
	}
	if err := requireDataDir(n, o.dataDir); err != nil {
		return err
	}
	backends, err := o.backends(n)
	if err != nil {
		return err
	}

	st, err := store.Open(o.dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	log := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	deps := publish.Deps{
		Store: st, Staging: backends.Staging, Public: backends.Public,
		GitHub: backends.GitHub, Now: nowFunc(), Log: log,
	}
	if o.dryRun {
		// A dry run must not reach the STORE either, not just the buckets:
		// ExpireOldVersions writes the state change, and "print what would
		// happen" that changed the catalog would be the worst of both. It is
		// therefore safe — and useful — before the buckets are wired.
		return dryRunRetention(e, st)
	}

	// Refused up front, the way the promote route's preflight refuses, and for
	// a sharper reason: expiring a row is one-way, so a pass that marked rows
	// without pruning them would orphan their bytes permanently.
	if err := publish.CanRetain(deps); err != nil {
		return fmt.Errorf("%w\n  seams: %s\n  `%s retain --dry-run` reports what a real pass would expire and changes nothing",
			err, o.describe(backends), toolName)
	}
	fmt.Fprintf(e.stdout, "%s\n", o.describe(backends))
	for _, ev := range publish.RetainAll(context.Background(), deps) {
		line := ev.Step
		if ev.File != "" {
			line += " " + ev.File
		}
		line += " — " + ev.Status
		if ev.Detail != "" {
			line += " (" + ev.Detail + ")"
		}
		if ev.Error != "" {
			line += ": " + ev.Error
		}
		fmt.Fprintln(e.stdout, line)
	}
	return nil
}

// dryRunRetention reports what a real pass would expire, touching nothing.
//
// It asks the STORE for the plan rather than re-deriving the keep rule: a dry
// run that re-implemented it would eventually disagree with the pass it claims
// to preview, and the disagreement would be found by an operator who trusted
// the preview.
func dryRunRetention(e *env, st *store.Store) error {
	for _, cc := range componentsAndChannels() {
		keep := publish.KeepFor(cc.channel)
		expire, retain, err := st.PlanExpiry(cc.component, cc.channel, keep)
		if err != nil {
			return err
		}
		for _, row := range retain {
			note := ""
			if row.IsCurrent {
				note = " (current)"
			}
			fmt.Fprintf(e.stdout, "keep    %s/%s %s%s\n", cc.component, cc.channel, row.Stamp, note)
		}
		for _, row := range expire {
			note := ""
			if row.State == "yanked" {
				note = " (yanked)"
			}
			fmt.Fprintf(e.stdout, "EXPIRE  %s/%s %s%s\n", cc.component, cc.channel, row.Stamp, note)
		}
	}
	return nil
}
