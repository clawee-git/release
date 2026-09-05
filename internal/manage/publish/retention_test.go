package publish

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manifest"
)

// promoteN stages and promotes n rows on a channel, one per hour, and returns
// their ids oldest first.
func (f *fixture) promoteN(channel string, n int) []int64 {
	f.t.Helper()
	var ids []int64
	for i := 1; i <= n; i++ {
		at := base.Add(time.Duration(i) * time.Hour)
		id := f.stage(catalog.ComponentCLI, channel, i, at)
		f.now = at
		if out, err := f.promote(id); err != nil {
			f.t.Fatalf("promote %d: %v\n%s", i, err, out)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestRetentionKeepsTenStableAndNeverTheCurrentRow(t *testing.T) {
	f := newFixture(t)
	f.deps.Retain = func(ctx context.Context, comp, ch string) []Event {
		return Retain(ctx, f.deps, comp, ch)
	}
	ids := f.promoteN(catalog.ChannelStable, 12)

	// 12 promoted rows: the newest is current (never counted, never expired),
	// ten are kept, one goes.
	var expired []int64
	for _, id := range ids {
		row, _ := f.st.Get(id)
		if row.State == catalog.StateExpired {
			expired = append(expired, id)
		}
	}
	if len(expired) != 1 || expired[0] != ids[0] {
		t.Fatalf("expired = %v, want exactly the oldest row %d", expired, ids[0])
	}
	current, err := f.st.CurrentPublic(catalog.ComponentCLI, catalog.ChannelStable)
	if err != nil || current.ID != ids[11] {
		t.Fatalf("current = %v, %v", current, err)
	}

	// The expired row's bytes are gone from the public surface and its GitHub
	// release with them.
	oldRow, _ := f.st.Get(ids[0])
	prefix := manifest.PublicBase(oldRow.Component, oldRow.Channel, oldRow.Stamp) + "/"
	for k := range f.public.Objects {
		if strings.HasPrefix(k, prefix) {
			t.Fatalf("an expired row's object survived: %s", k)
		}
	}
	if _, ok := f.github.Releases[ReleaseTag(oldRow.Component, oldRow.Stamp)]; ok {
		t.Fatal("the expired row's GitHub release survived")
	}

	// The kept rows are untouched, current included.
	keptRow, _ := f.st.Get(ids[5])
	keptPrefix := manifest.PublicBase(keptRow.Component, keptRow.Channel, keptRow.Stamp) + "/"
	found := false
	for k := range f.public.Objects {
		if strings.HasPrefix(k, keptPrefix) {
			found = true
		}
	}
	if !found {
		t.Fatal("a kept row's objects were pruned")
	}
	if _, ok := f.public.Objects["clawee/latest.json"]; !ok {
		t.Fatal("retention removed the manifest")
	}
}

func TestRetentionKeepsOneBeta(t *testing.T) {
	f := newFixture(t)
	f.deps.Retain = func(ctx context.Context, comp, ch string) []Event {
		return Retain(ctx, f.deps, comp, ch)
	}
	ids := f.promoteN(catalog.ChannelBeta, 3)

	// Newest is current, one kept, one expired.
	states := map[string]int{}
	for _, id := range ids {
		row, _ := f.st.Get(id)
		states[row.State]++
	}
	if states[catalog.StateExpired] != 1 || states[catalog.StatePublic] != 2 {
		t.Fatalf("beta states = %v, want 1 expired and 2 public", states)
	}
	oldest, _ := f.st.Get(ids[0])
	if oldest.State != catalog.StateExpired {
		t.Fatalf("the oldest beta row is %s", oldest.State)
	}
}

// Retention on one channel must not touch the other's rows or bytes.
func TestRetentionIsPerChannel(t *testing.T) {
	f := newFixture(t)
	f.deps.Retain = func(ctx context.Context, comp, ch string) []Event {
		return Retain(ctx, f.deps, comp, ch)
	}
	stable := f.promoteN(catalog.ChannelStable, 3)
	f.promoteN(catalog.ChannelBeta, 3)

	for _, id := range stable {
		row, _ := f.st.Get(id)
		if row.State == catalog.StateExpired {
			t.Fatalf("beta retention expired the stable row %d", id)
		}
	}
	if _, ok := f.public.Objects["clawee/latest.json"]; !ok {
		t.Fatal("the stable manifest was removed by beta retention")
	}
}

// A prune failure is best effort: the row STAYS expired, and the failure is
// reported rather than swallowed. The catalog is the source of truth; bytes
// are reconciled to it, never the other way round.
func TestPruneFailureLeavesTheRowExpiredAndSaysSo(t *testing.T) {
	f := newFixture(t)
	ids := f.promoteN(catalog.ChannelBeta, 3)
	f.public.FailDelete = ".zip"
	f.github.FailDelete = "clawee/"

	events := Retain(context.Background(), f.deps, catalog.ComponentCLI, catalog.ChannelBeta)

	row, _ := f.st.Get(ids[0])
	if row.State != catalog.StateExpired {
		t.Fatalf("a failed prune rolled the row back to %s", row.State)
	}
	var pruneErrors, githubErrors int
	for _, e := range events {
		if e.Status == "error" && e.Step == "prune" {
			pruneErrors++
		}
		if e.Status == "error" && e.Step == "prune-github" {
			githubErrors++
		}
	}
	if pruneErrors == 0 {
		t.Fatalf("a failed object delete was not reported: %+v", events)
	}
	if githubErrors == 0 {
		t.Fatalf("a failed GitHub delete was not reported: %+v", events)
	}
	// Retention itself still reports success: the release is live, the catalog
	// is right, and an orphaned object is a tidiness problem — not a reason to
	// fail the promote this runs at the end of.
	last := events[len(events)-1]
	if last.Step != "retention" || last.Status != "ok" {
		t.Fatalf("retention ended with %+v", last)
	}
}

func TestRetentionRunsAtTheEndOfPromote(t *testing.T) {
	f := newFixture(t)
	f.deps.Retain = func(ctx context.Context, comp, ch string) []Event {
		return Retain(ctx, f.deps, comp, ch)
	}
	f.promoteN(catalog.ChannelStable, 1)
	id := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 2, base.Add(2*time.Hour))
	f.now = base.Add(2 * time.Hour)
	out, err := f.promote(id)
	if err != nil {
		t.Fatal(err)
	}
	evs := events(t, out)
	flip := idx(evs, "flip", "ok")
	retention := idx(evs, "retention", "start")
	if retention < 0 {
		t.Fatalf("promote did not run retention:\n%s", out)
	}
	if retention < flip {
		t.Fatal("retention ran before the flip; it would count the row being promoted as an old one")
	}
}

func TestRetainAllCoversEveryComponentAndChannel(t *testing.T) {
	f := newFixture(t)
	events := RetainAll(context.Background(), f.deps)
	starts := 0
	for _, e := range events {
		if e.Step == "retention" && e.Status == "start" {
			starts++
		}
	}
	if want := len(catalog.Components) * len(catalog.Channels); starts != want {
		t.Fatalf("%d retention passes, want %d (every component × every channel)", starts, want)
	}
}

func TestKeepCounts(t *testing.T) {
	if KeepFor(catalog.ChannelStable) != 10 || KeepFor(catalog.ChannelBeta) != 1 {
		t.Fatalf("keep counts = %d stable, %d beta", KeepFor(catalog.ChannelStable), KeepFor(catalog.ChannelBeta))
	}
	// An unknown channel over-keeps rather than under-keeps: over-keeping
	// costs disk, under-keeping deletes releases.
	if KeepFor("nightly") != KeepStable {
		t.Fatal("an unknown channel does not fall back to the stable count")
	}
}
