// Package prune drops all but the newest N per-stamp directories under a
// component prefix in the public Cloudflare R2 downloads bucket.
//
// R2 is the install-time fallback mirror (GitHub Releases stay primary), so it
// accumulates every stamp ever cut and nothing has ever removed one. Prune is
// the retention pass: it keeps the newest DefaultKeep stamps per component and
// deletes every object beneath the older ones.
package prune

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// DefaultKeep is the retention: the newest 10 stamps per component survive a
// prune. It matches the Burrowee bucket's stable-channel retention so both
// brands' mirrors are governed by one number.
const DefaultKeep = 10

// Store is the R2 surface Prune needs. Satisfied by *r2.Client.
type Store interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// stampRe anchors the full stamp shape tools/version.sh emits:
// v<major>.<minor>.<patch>.<YYYY>.<MM>.<DD>.<8-hex-sha>. Clawee has no beta
// channel, so this is the only shape a release directory can have.
//
// The match is anchored on purpose. A directory that does NOT match belongs to
// neither the count nor the delete list: a hand-upload, a legacy layout or a
// typo'd stamp is left exactly where it is rather than being swept up as "old"
// by a prune that never understood it.
var stampRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+\.[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9a-f]{8}$`)

// Prune keeps the newest keep stamp directories under <comp>/ and deletes every
// object beneath the rest. "Newest" uses the same ordering as the rest of the
// tooling (GNU `sort -V`, see version.go): the version triple dominates and the
// date+sha suffix breaks ties chronologically.
//
// <comp>/latest.json is not a <comp>/<stamp>/<file> key, so it is never a
// candidate — the manifest survives every prune, and the stamp it points at is
// the newest one, which is always inside the kept set.
//
// When execute is false (the default) nothing is deleted: the planned deletions
// are written to out as "would delete <key>" lines and counted. Returns the
// number of objects deleted (execute=true) or that would be (execute=false).
func Prune(ctx context.Context, store Store, comp string, keep int, execute bool, out io.Writer) (int, error) {
	if out == nil {
		out = io.Discard
	}
	if keep < 1 {
		return 0, fmt.Errorf("prune %s: keep must be >= 1 (got %d)", comp, keep)
	}
	prefix := comp + "/"

	keys, err := store.List(ctx, prefix)
	if err != nil {
		return 0, err
	}

	// Group keys by the stamp directory right after <comp>/, skipping anything
	// that is not a <comp>/<stamp>/<file> key or whose stamp fails stampRe.
	byStamp := map[string][]string{}
	for _, k := range keys {
		rest := strings.TrimPrefix(k, prefix)
		stamp, _, ok := strings.Cut(rest, "/")
		if !ok || !stampRe.MatchString(stamp) {
			continue // latest.json, a nested oddity, or an unrecognised directory
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
	fmt.Fprintf(out, "[%s] %d stamp(s) under %s — keep newest %d (%s)\n", comp, len(stamps), prefix, keep, mode)

	if len(stamps) <= keep {
		fmt.Fprintf(out, "[%s] nothing to prune\n", comp)
		return 0, nil
	}

	drop := stamps[:len(stamps)-keep]
	kept := stamps[len(stamps)-keep:]
	fmt.Fprintf(out, "[%s] keep: %s\n", comp, strings.Join(kept, " "))

	deleted := 0
	for _, stamp := range drop {
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
