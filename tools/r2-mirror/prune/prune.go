// Package prune drops all but the newest N per-stamp directories under a
// component's channel prefix in the public Cloudflare R2 downloads bucket.
//
// R2 accumulates every stamp ever cut and nothing has ever removed one. Prune
// is the retention pass, ONE CHANNEL per call: the stable pass lists <comp>/
// and keeps DefaultKeepStable stamps; the beta pass lists <comp>/beta/ and
// keeps DefaultKeepBeta. Shape of the clawee release repo's prune package,
// plus the channel (burrowee's 2026-08-31-release-retention-and-beta-layout
// design §3).
package prune

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Retention per channel: stable keeps the newest 3 stamps; beta keeps 1 —
// a beta is disposable, cutting a new one expires the previous, and there
// is no artifact-level rollback to it. tools/retain-permanent pins are kept
// in addition to this window.
const (
	DefaultKeepStable = 3
	DefaultKeepBeta   = 1
)

// DefaultKeep returns the channel's retention, or 0 for an unknown channel.
func DefaultKeep(channel string) int {
	switch channel {
	case "stable":
		return DefaultKeepStable
	case "beta":
		return DefaultKeepBeta
	}
	return 0
}

// Store is the R2 surface Prune needs. Satisfied by *r2.Client.
type Store interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// The two stamp shapes tools/version.sh emits, anchored:
// v<major>.<minor>.<patch>.<YYYY>.<MM>.<DD>.<8-hex-sha> on stable, with a
// ".beta." segment after the semver on beta.
//
// Both matches are anchored on purpose. A directory that matches NEITHER
// belongs to neither the count nor the delete list: a hand-upload, a legacy
// layout or a typo'd stamp is left exactly where it is rather than being
// swept up as "old" by a prune that never understood it.
var (
	stableRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+\.[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9a-f]{8}$`)
	betaRe   = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+\.beta\.[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9a-f]{8}$`)
)

// chOf classifies one path segment: "stable" for the stable stamp shape,
// "beta" for the beta stamp shape, "" for anything else — INCLUDING the
// literal segment "beta", which is the beta channel's directory, not a stamp.
// That last case is what keeps the stable pass (which lists <comp>/ and so
// sees <comp>/beta/<stamp>/… too) from ever treating the beta subtree as a
// stale stable stamp: on the stable pass a key whose second segment is
// "beta" yields no channel and is ignored. A substring probe (".beta." in
// the segment) would get the shapes right and this case wrong.
func chOf(segment string) string {
	switch {
	case stableRe.MatchString(segment):
		return "stable"
	case betaRe.MatchString(segment):
		return "beta"
	}
	return ""
}

// Prune keeps the newest keep stamp directories under the channel's prefix
// (<comp>/ on stable, <comp>/beta/ on beta) and deletes every object beneath
// the rest. "Newest" uses the same ordering as the rest of the tooling (GNU
// `sort -V`, see version.go): the version triple dominates and the date+sha
// suffix breaks ties chronologically. Only stamps of the CHANNEL's shape are
// counted or deleted; every other key under the prefix is ignored.
//
// <comp>/latest.json and <comp>/beta/latest.json are not <prefix><stamp>/<file>
// keys, so they are never candidates — the manifests survive every prune, and
// the stamp each points at is the newest one, which is always inside the
// kept set.
//
// When execute is false (the default) nothing is deleted: the planned
// deletions are written to out as "would delete <key>" lines and counted.
// Returns the number of objects deleted (execute=true) or that would be.
func Prune(ctx context.Context, store Store, comp, channel string, keep int, execute bool, out io.Writer) (int, error) {
	return PruneProtect(ctx, store, comp, channel, keep, execute, out, nil)
}

// PruneProtect is Prune with a permanent-pin set. protect may be nil.
func PruneProtect(ctx context.Context, store Store, comp, channel string, keep int, execute bool, out io.Writer, protect map[string]struct{}) (int, error) {
	if out == nil {
		out = io.Discard
	}
	if channel != "stable" && channel != "beta" {
		return 0, fmt.Errorf("prune %s: unknown channel %q (want stable | beta)", comp, channel)
	}
	if keep < 1 {
		return 0, fmt.Errorf("prune %s/%s: keep must be >= 1 (got %d)", comp, channel, keep)
	}
	prefix := comp + "/"
	if channel == "beta" {
		prefix = comp + "/beta/"
	}

	keys, err := store.List(ctx, prefix)
	if err != nil {
		return 0, err
	}

	// Group keys by the stamp directory right after the prefix, skipping
	// anything that is not a <prefix><stamp>/<file> key or whose stamp is not
	// THIS channel's shape.
	byStamp := map[string][]string{}
	for _, k := range keys {
		rest := strings.TrimPrefix(k, prefix)
		stamp, _, ok := strings.Cut(rest, "/")
		if !ok || chOf(stamp) != channel {
			continue // latest.json, the other channel's subtree, or an unrecognised directory
		}
		byStamp[stamp] = append(byStamp[stamp], k)
	}

	stamps := make([]string, 0, len(byStamp))
	for s := range byStamp {
		stamps = append(stamps, s)
	}
	sort.Sort(byVersionSort(stamps))

	mode := "DRY-RUN"
	if execute {
		mode = "EXECUTE"
	}
	fmt.Fprintf(out, "[%s/%s] %d stamp(s) under %s — keep newest %d (%s)\n", comp, channel, len(stamps), prefix, keep, mode)

	if len(stamps) <= keep {
		fmt.Fprintf(out, "[%s/%s] nothing to prune\n", comp, channel)
		return 0, nil
	}

	drop := stamps[:len(stamps)-keep]
	kept := stamps[len(stamps)-keep:]
	fmt.Fprintf(out, "[%s/%s] keep: %s\n", comp, channel, strings.Join(kept, " "))

	deleted := 0
	for _, stamp := range drop {
		if Protected(protect, comp, stamp) {
			fmt.Fprintf(out, "  keep permanent %s/%s\n", comp, stamp)
			continue
		}
		// Sort each stamp's keys so a run's output is stable and diffable.
		sort.Strings(byStamp[stamp])
		for _, key := range byStamp[stamp] {
			if execute {
				if err := store.Delete(ctx, key); err != nil {
					return deleted, err
				}
				fmt.Fprintf(out, "  ✓ deleted %s\n", key)
			} else {
				fmt.Fprintf(out, "  - would delete %s\n", key)
			}
			deleted++
		}
	}
	return deleted, nil
}
