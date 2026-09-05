package publish

// Retention: keep 3 stable and 1 beta promoted rows per component, on the
// public surface AND on GitHub, and never the current one
// (release-management.md §7).
//
// Two rules make the rest of this file readable:
//
//	The CATALOG is the source of truth, and bytes are reconciled to it —
//	never the other way round. The row is marked `expired` inside a
//	transaction; the pruning that follows is best effort, and a prune that
//	fails leaves an orphaned object, not a row that lies.
//
//	The CURRENT row is never expired, however old it is. It is not one of the
//	N most recent releases; it is the one the channel serves.
//
// A retention pass that only reports is not a control, so this runs at the end
// of every promote for that (component, channel) and again from the `retain`
// verb — the nightly net for a component nobody has promoted lately.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/manifest"
)

// The keep counts. Three stable is enough that a host a couple of cuts
// behind can still fetch; one beta is the current cycle's build and nothing
// else: a beta is a thing you are asked to test now, not an archive.
const (
	KeepStable = 3
	KeepBeta   = 1
)

// KeepFor is the count for a channel. An unknown channel keeps the stable
// count: over-keeping is a disk cost, under-keeping deletes releases.
func KeepFor(channel string) int {
	if channel == catalog.ChannelBeta {
		return KeepBeta
	}
	return KeepStable
}

// ErrNoStoresForRetention is Retain's refusal when it has nowhere to prune.
//
// Expiring a row is ONE-WAY: ExpireOldVersions returns only the rows it newly
// transitioned, so a row marked expired while the buckets were unwired is
// never revisited by a later, wired pass — its objects and its GitHub release
// are orphaned for good. Marking without pruning is therefore not a degraded
// retention pass, it is a permanent leak, and the only safe answer is to
// refuse and say which piece is missing.
var ErrNoStoresForRetention = errors.New(
	"publish: retention needs both a public store and a GitHub publisher; " +
		"expiring a row without pruning its bytes orphans them permanently, " +
		"because a later pass only ever sees rows it expires itself")

// CanRetain reports whether a retention pass can run without orphaning bytes.
func CanRetain(d Deps) error {
	if d.Public == nil || d.GitHub == nil {
		return ErrNoStoresForRetention
	}
	return nil
}

// Retain runs one retention pass and returns the progress events.
//
// It never returns an error. Every failure it can have AFTER the gate is a
// pruning failure, and a pruning failure must not fail the promote it runs at
// the end of: the release is live, the catalog is correct, and an orphaned
// object in a bucket is a tidiness problem. The events carry what went wrong
// so an operator watching the stream sees it.
//
// The one thing it will not do is expire rows it cannot prune — see
// ErrNoStoresForRetention. That refusal is reported as an event and leaves the
// catalog untouched.
func Retain(ctx context.Context, d Deps, component, channel string) []Event {
	var events []Event
	send := func(e Event) { events = append(events, e) }

	if err := CanRetain(d); err != nil {
		send(Event{Step: "retention", Status: "error", Error: err.Error()})
		return events
	}

	keep := KeepFor(channel)
	send(Event{Step: "retention", Status: "start",
		Detail: fmt.Sprintf("%s/%s, keeping %d", component, channel, keep)})

	expired, err := d.Store.ExpireOldVersions(component, channel, keep, d.Now())
	if err != nil {
		send(Event{Step: "retention", Status: "error", Error: err.Error()})
		return events
	}
	if len(expired) == 0 {
		send(Event{Step: "retention", Status: "ok", Detail: "nothing to expire"})
		return events
	}

	log := d.Log
	for _, row := range expired {
		send(Event{Step: "expire", File: row.Stamp, Status: "ok"})
		for _, e := range prune(ctx, d, row) {
			send(e)
			if e.Status == "error" && log != nil {
				// Logged as well as streamed: the operator who ran the promote
				// sees the stream, and the one reading the journal a week
				// later sees the log.
				log.Warn("retention: prune failed", "stamp", row.Stamp, "file", e.File, "err", e.Error)
			}
		}
	}
	send(Event{Step: "retention", Status: "ok",
		Detail: fmt.Sprintf("expired %d row(s)", len(expired))})
	return events
}

// prune removes one expired row's bytes from the public surface and its GitHub
// release, best effort.
func prune(ctx context.Context, d Deps, row store.ReleaseVersion) []Event {
	var events []Event
	base := manifest.PublicBase(row.Component, row.Channel, row.Stamp)

	if d.Public != nil {
		keys, err := d.Public.List(ctx, base+"/")
		if err != nil {
			events = append(events, Event{Step: "prune", File: base, Status: "error", Error: err.Error()})
		}
		for _, k := range keys {
			// Belt and braces: List is prefix-scoped already, but a store that
			// answered a wider set would have this loop deleting a live
			// release's objects, and that is not a mistake worth being
			// recoverable from.
			if !strings.HasPrefix(k, base+"/") {
				continue
			}
			if err := d.Public.Delete(ctx, k); err != nil {
				events = append(events, Event{Step: "prune", File: k, Status: "error", Error: err.Error()})
				continue
			}
			events = append(events, Event{Step: "prune", File: k, Status: "ok"})
		}
	}

	if d.GitHub != nil {
		tag := ReleaseTag(row.Component, row.Stamp)
		if err := d.GitHub.DeleteRelease(ctx, tag); err != nil {
			events = append(events, Event{Step: "prune-github", File: tag, Status: "error", Error: err.Error()})
		} else {
			events = append(events, Event{Step: "prune-github", File: tag, Status: "ok"})
		}
	}
	return events
}

// RetainAll runs the pass for every component and channel. It is the nightly
// net the `retain` verb invokes: promote covers the (component, channel) it
// just published, and nothing else — a component nobody has promoted for a
// month never runs retention at all without this.
func RetainAll(ctx context.Context, d Deps) []Event {
	var events []Event
	for _, comp := range catalog.Components {
		for _, ch := range catalog.Channels {
			events = append(events, Retain(ctx, d, comp, ch)...)
		}
	}
	return events
}
