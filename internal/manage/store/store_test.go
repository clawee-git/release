package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawee-git/release/internal/manage/catalog"
)

// base is the fixed instant every test counts from. The store is clock-free,
// so nothing here sleeps or reads the wall clock.
var base = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// stamp renders a stamp of the right shape for a channel. Tests that vary the
// stamp still have to satisfy StampMatchesChannel, which is the point.
func stamp(channel string, n int) string {
	sha := fmt.Sprintf("%08x", n)
	if channel == catalog.ChannelBeta {
		return fmt.Sprintf("v0.3.%d.beta.2026.09.04.%s", n, sha)
	}
	return fmt.Sprintf("v0.2.%d.2026.09.04.%s", n, sha)
}

func stage(t *testing.T, s *Store, comp, channel string, n int, at time.Time) int64 {
	t.Helper()
	id, err := s.Stage(ReleaseVersion{
		Component:     comp,
		Channel:       channel,
		Version:       fmt.Sprintf("0.2.%d", n),
		Stamp:         stamp(channel, n),
		ArtifactsJSON: `[{"platform":"darwin/arm64","key":"k","sha256":"s","size":1}]`,
		SumsKey:       "k/SHA256SUMS.txt",
		MinisigKey:    "k/SHA256SUMS.txt.minisig",
		CreatedAt:     at,
	})
	if err != nil {
		t.Fatalf("Stage(%s %s #%d): %v", comp, channel, n, err)
	}
	return id
}

func TestOpenIsIdempotentAndLedgersMigrations(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	applied, err := s.AppliedMigrations()
	if err != nil {
		t.Fatalf("AppliedMigrations: %v", err)
	}
	if len(applied) != len(migrations) {
		t.Fatalf("ledger has %d rungs, want %d", len(applied), len(migrations))
	}
	s.Close()

	// Re-opening must not re-run a rung: the CREATE TABLEs are not IF NOT
	// EXISTS, so a re-run would fail loudly rather than silently double-apply.
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer s2.Close()
	applied2, _ := s2.AppliedMigrations()
	if len(applied2) != len(applied) {
		t.Fatalf("re-open changed the ledger: %v -> %v", applied, applied2)
	}
}

func TestStageRejectsVocabularyAndMismatchedStamp(t *testing.T) {
	s := open(t)
	good := ReleaseVersion{
		Component: catalog.ComponentCLI, Channel: catalog.ChannelStable,
		Version: "0.2.1", Stamp: stamp(catalog.ChannelStable, 1),
		ArtifactsJSON: "[]", CreatedAt: base,
	}

	bad := good
	bad.Component = "clawee-cli"
	if _, err := s.Stage(bad); !errors.Is(err, ErrBadValue) {
		t.Fatalf("unknown component: err = %v, want ErrBadValue", err)
	}
	bad = good
	bad.Channel = "nightly"
	if _, err := s.Stage(bad); !errors.Is(err, ErrBadValue) {
		t.Fatalf("unknown channel: err = %v, want ErrBadValue", err)
	}
	// A beta stamp claiming the stable channel is the dangerous one: retention
	// and every installer read the channel, so it would be a beta build served
	// to every stable host.
	bad = good
	bad.Stamp = stamp(catalog.ChannelBeta, 1)
	if _, err := s.Stage(bad); !errors.Is(err, ErrBadValue) {
		t.Fatalf("beta stamp on stable: err = %v, want ErrBadValue", err)
	}
	if _, err := s.Stage(good); err != nil {
		t.Fatalf("Stage(good): %v", err)
	}
}

func TestStageRefusesDuplicateStamp(t *testing.T) {
	s := open(t)
	stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	_, err := s.Stage(ReleaseVersion{
		Component: catalog.ComponentCLI, Channel: catalog.ChannelStable,
		Version: "0.2.1", Stamp: stamp(catalog.ChannelStable, 1),
		ArtifactsJSON: "[]", CreatedAt: base.Add(time.Hour),
	})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate stamp: err = %v, want ErrAlreadyExists", err)
	}
	// The same stamp on the OTHER channel is a different row: the uniqueness
	// is per (component, channel, stamp), and a stamp shape belongs to exactly
	// one channel anyway.
	if _, err := s.Stage(ReleaseVersion{
		Component: catalog.ComponentDaemon, Channel: catalog.ChannelStable,
		Version: "0.2.1", Stamp: stamp(catalog.ChannelStable, 1),
		ArtifactsJSON: "[]", CreatedAt: base,
	}); err != nil {
		t.Fatalf("same stamp, other component: %v", err)
	}
}

func TestPromoteMovesCurrentAndRefusesNonStaged(t *testing.T) {
	s := open(t)
	first := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	second := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 2, base.Add(time.Hour))

	if err := s.Promote(first, base.Add(time.Minute)); err != nil {
		t.Fatalf("Promote(first): %v", err)
	}
	cur, err := s.CurrentPublic(catalog.ComponentCLI, catalog.ChannelStable)
	if err != nil || cur.ID != first {
		t.Fatalf("CurrentPublic after first promote = %v, %v", cur, err)
	}
	if cur.PromotedAt.IsZero() {
		t.Fatal("promoted_at not set")
	}

	if err := s.Promote(second, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("Promote(second): %v", err)
	}
	cur, err = s.CurrentPublic(catalog.ComponentCLI, catalog.ChannelStable)
	if err != nil || cur.ID != second {
		t.Fatalf("CurrentPublic after second promote = %v, %v", cur, err)
	}
	prev, _ := s.Get(first)
	if prev.State != catalog.StatePublic || prev.IsCurrent {
		t.Fatalf("previous current row = state %q is_current %v, want public/false", prev.State, prev.IsCurrent)
	}

	// A second promote of a row that is already public is a caller that lost
	// track of the state, not a re-point.
	if err := s.Promote(second, base); !errors.Is(err, ErrBadState) {
		t.Fatalf("re-promote: err = %v, want ErrBadState", err)
	}
	if err := s.Promote(9999, base); !errors.Is(err, ErrNotFound) {
		t.Fatalf("promote missing row: err = %v, want ErrNotFound", err)
	}
}

func TestPromoteIsPerChannel(t *testing.T) {
	s := open(t)
	st := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	be := stage(t, s, catalog.ComponentCLI, catalog.ChannelBeta, 1, base)
	if err := s.Promote(st, base); err != nil {
		t.Fatalf("promote stable: %v", err)
	}
	// Promoting on beta must not clear the stable current row — the partial
	// unique index is per (component, channel), and so is the manifest.
	if err := s.Promote(be, base); err != nil {
		t.Fatalf("promote beta: %v", err)
	}
	if cur, err := s.CurrentPublic(catalog.ComponentCLI, catalog.ChannelStable); err != nil || cur.ID != st {
		t.Fatalf("stable current lost when beta promoted: %v, %v", cur, err)
	}
	if cur, err := s.CurrentPublic(catalog.ComponentCLI, catalog.ChannelBeta); err != nil || cur.ID != be {
		t.Fatalf("beta current = %v, %v", cur, err)
	}
}

func TestYankMovesCurrentOntoTheSuccessor(t *testing.T) {
	s := open(t)
	old := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	newer := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 2, base.Add(time.Hour))
	if err := s.Promote(old, base); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(newer, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	successor, err := s.NewestPublicExcept(catalog.ComponentCLI, catalog.ChannelStable, newer)
	if err != nil || successor == nil || successor.ID != old {
		t.Fatalf("NewestPublicExcept = %v, %v; want row %d", successor, err, old)
	}
	if err := s.Yank(newer, successor.ID, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("Yank: %v", err)
	}

	row, _ := s.Get(newer)
	if row.State != catalog.StateYanked || row.IsCurrent || row.YankedAt.IsZero() {
		t.Fatalf("yanked row = %+v", row)
	}
	// The whole point: the channel has a current row again, and it is the one
	// the caller wrote into the manifest. Leaving is_current on nothing let
	// retention expire and prune the build the manifest was serving.
	cur, err := s.CurrentPublic(catalog.ComponentCLI, catalog.ChannelStable)
	if err != nil || cur.ID != old {
		t.Fatalf("CurrentPublic after yank = %v, %v; want row %d", cur, err, old)
	}
}

func TestYankWithNoSuccessorLeavesTheChannelEmpty(t *testing.T) {
	s := open(t)
	id := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	if err := s.Promote(id, base); err != nil {
		t.Fatal(err)
	}
	successor, err := s.NewestPublicExcept(catalog.ComponentCLI, catalog.ChannelStable, id)
	if err != nil {
		t.Fatal(err)
	}
	if successor != nil {
		t.Fatalf("successor = %+v, want none", successor)
	}
	// 0 means "there is none" — the caller removed the manifest entry.
	if err := s.Yank(id, 0, base.Add(time.Hour)); err != nil {
		t.Fatalf("Yank: %v", err)
	}
	if _, err := s.CurrentPublic(catalog.ComponentCLI, catalog.ChannelStable); !errors.Is(err, ErrNotFound) {
		t.Fatalf("current after the last yank: err = %v, want ErrNotFound", err)
	}
}

func TestYankRefusesABadSuccessor(t *testing.T) {
	s := open(t)
	stableID := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	betaID := stage(t, s, catalog.ComponentCLI, catalog.ChannelBeta, 1, base)
	stagedID := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 2, base.Add(time.Hour))
	for _, id := range []int64{stableID, betaID} {
		if err := s.Promote(id, base); err != nil {
			t.Fatal(err)
		}
	}

	// A successor on the other channel would put is_current on a build this
	// channel's manifest does not name — the same disagreement, mirrored.
	if err := s.Yank(stableID, betaID, base); !errors.Is(err, ErrBadValue) {
		t.Fatalf("cross-channel successor: err = %v, want ErrBadValue", err)
	}
	if err := s.Yank(stableID, stagedID, base); !errors.Is(err, ErrBadState) {
		t.Fatalf("staged successor: err = %v, want ErrBadState", err)
	}
	if err := s.Yank(stableID, stableID, base); !errors.Is(err, ErrBadValue) {
		t.Fatalf("self-succession: err = %v, want ErrBadValue", err)
	}
	if err := s.Yank(stableID, 9999, base); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing successor: err = %v, want ErrNotFound", err)
	}
	// Every refusal rolled back: the row is untouched.
	row, _ := s.Get(stableID)
	if row.State != catalog.StatePublic || !row.IsCurrent {
		t.Fatalf("a refused yank changed the row: %+v", row)
	}
}

func TestDoubleYankIsRefused(t *testing.T) {
	s := open(t)
	id := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	if err := s.Promote(id, base); err != nil {
		t.Fatal(err)
	}
	if err := s.Yank(id, 0, base); err != nil {
		t.Fatal(err)
	}
	if err := s.Yank(id, 0, base); !errors.Is(err, ErrBadState) {
		t.Fatalf("double yank: err = %v, want ErrBadState", err)
	}
}

// A yanked row must not hold a retention slot ahead of a serving one. With a
// keep of 1 it did: the yank consumed the slot and the SUCCESSOR — the build
// the manifest names — was expired and pruned.
func TestRetentionNeverExpiresTheSuccessorOfAYank(t *testing.T) {
	s := open(t)
	older := stage(t, s, catalog.ComponentCLI, catalog.ChannelBeta, 1, base)
	newer := stage(t, s, catalog.ComponentCLI, catalog.ChannelBeta, 2, base.Add(time.Hour))
	if err := s.Promote(older, base); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(newer, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	successor, _ := s.NewestPublicExcept(catalog.ComponentCLI, catalog.ChannelBeta, newer)
	if err := s.Yank(newer, successor.ID, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	expired, err := s.ExpireOldVersions(catalog.ComponentCLI, catalog.ChannelBeta, 1, base.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// The yanked row goes; the row the channel serves stays.
	if len(expired) != 1 || expired[0].ID != newer {
		t.Fatalf("expired = %v, want only the yanked row %d", expired, newer)
	}
	row, _ := s.Get(older)
	if row.State != catalog.StatePublic || !row.IsCurrent {
		t.Fatalf("retention touched the served build: %+v", row)
	}
}

// Yanked rows are always expired and never counted: nothing serves them, so
// keeping their bytes is the opposite of why they were yanked.
func TestYankedRowsDoNotCountTowardTheKeepWindow(t *testing.T) {
	s := open(t)
	var ids []int64
	for i := 1; i <= 4; i++ {
		id := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, i, base.Add(time.Duration(i)*time.Hour))
		if err := s.Promote(id, base.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// Yank the newest; row 3 becomes current.
	successor, _ := s.NewestPublicExcept(catalog.ComponentCLI, catalog.ChannelStable, ids[3])
	if err := s.Yank(ids[3], successor.ID, base.Add(9*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// keep=2: the current row is free, two public rows are kept, and the
	// yanked one is expired REGARDLESS of being the newest by date.
	expired, err := s.ExpireOldVersions(catalog.ComponentCLI, catalog.ChannelStable, 2, base.Add(10*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, e := range expired {
		got[e.ID] = true
	}
	if !got[ids[3]] {
		t.Fatalf("the yanked row was not expired: %v", expired)
	}
	for _, keptID := range []int64{ids[1], ids[2]} {
		if got[keptID] {
			t.Fatalf("row %d was expired although it is inside the keep window", keptID)
		}
	}
}

func TestExpireOldVersionsKeepsWindowCurrentAndStaged(t *testing.T) {
	s := open(t)
	// Twelve stable rows, promoted oldest first, so the NEWEST ends current.
	var ids []int64
	for i := 1; i <= 12; i++ {
		id := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, i, base.Add(time.Duration(i)*time.Hour))
		ids = append(ids, id)
	}
	for i, id := range ids {
		if err := s.Promote(id, base.Add(time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// Everything is public and row 12 is current.
	expired, err := s.ExpireOldVersions(catalog.ComponentCLI, catalog.ChannelStable, 10, base.Add(99*time.Hour))
	if err != nil {
		t.Fatalf("ExpireOldVersions: %v", err)
	}
	// 12 rows: one is current (not counted, never expired), 10 kept, 1 expired.
	if len(expired) != 1 || expired[0].ID != ids[0] {
		t.Fatalf("expired = %v, want exactly the oldest row %d", expired, ids[0])
	}
	if row, _ := s.Get(ids[0]); row.State != catalog.StateExpired {
		t.Fatalf("oldest row state = %q", row.State)
	}
	if row, _ := s.Get(ids[11]); row.State != catalog.StatePublic || !row.IsCurrent {
		t.Fatalf("current row was touched: %+v", row)
	}

	// A staged row is never expired and never counts toward the window.
	staged := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 20, base.Add(200*time.Hour))
	if _, err := s.ExpireOldVersions(catalog.ComponentCLI, catalog.ChannelStable, 10, base.Add(300*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.Get(staged); row.State != catalog.StateStaged {
		t.Fatalf("staged row expired: %+v", row)
	}
}

func TestExpireOldVersionsIsPerChannel(t *testing.T) {
	s := open(t)
	// Three beta rows and three stable rows. Retention of beta at keep=1 must
	// not look at, or be crowded by, the stable rows.
	var beta []int64
	for i := 1; i <= 3; i++ {
		beta = append(beta, stage(t, s, catalog.ComponentCLI, catalog.ChannelBeta, i, base.Add(time.Duration(i)*time.Hour)))
		id := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, i, base.Add(time.Duration(i)*time.Hour))
		if err := s.Promote(id, base); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range beta {
		if err := s.Promote(id, base.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	expired, err := s.ExpireOldVersions(catalog.ComponentCLI, catalog.ChannelBeta, 1, base.Add(99*time.Hour))
	if err != nil {
		t.Fatalf("ExpireOldVersions(beta): %v", err)
	}
	// beta[2] is current (not counted), beta[1] is the one kept, beta[0] goes.
	if len(expired) != 1 || expired[0].ID != beta[0] {
		t.Fatalf("beta retention expired %v, want only %d", expired, beta[0])
	}
	for _, id := range []int64{beta[1], beta[2]} {
		if row, _ := s.Get(id); row.State != catalog.StatePublic {
			t.Fatalf("beta row %d = %q, want public", id, row.State)
		}
	}
	// Not one stable row moved.
	rows, _ := s.ListByComponent(catalog.ComponentCLI, catalog.ChannelStable)
	for _, r := range rows {
		if r.State == catalog.StateExpired {
			t.Fatalf("beta retention expired a stable row: %+v", r)
		}
	}
}

func TestExpireOldVersionsRejectsBadInput(t *testing.T) {
	s := open(t)
	if _, err := s.ExpireOldVersions("nope", catalog.ChannelStable, 10, base); !errors.Is(err, ErrBadValue) {
		t.Fatalf("err = %v, want ErrBadValue", err)
	}
	if _, err := s.ExpireOldVersions(catalog.ComponentCLI, catalog.ChannelStable, 0, base); err == nil {
		t.Fatal("keep=0 accepted — it would expire every non-current row")
	}
}

func TestUnpromotedIsTheNewestStagedRow(t *testing.T) {
	s := open(t)
	if _, err := s.Unpromoted(catalog.ComponentCLI, catalog.ChannelStable); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty catalog: err = %v, want ErrNotFound", err)
	}
	stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	newest := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 2, base.Add(time.Hour))
	got, err := s.Unpromoted(catalog.ComponentCLI, catalog.ChannelStable)
	if err != nil || got.ID != newest {
		t.Fatalf("Unpromoted = %v, %v; want row %d", got, err, newest)
	}
	// Once promoted it is no longer unpromoted.
	if err := s.Promote(newest, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err = s.Unpromoted(catalog.ComponentCLI, catalog.ChannelStable)
	if err != nil || got.ID == newest {
		t.Fatalf("Unpromoted after promote = %v, %v", got, err)
	}
}

func TestListByComponentIsNewestFirstAllStates(t *testing.T) {
	s := open(t)
	a := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	b := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, 2, base.Add(time.Hour))
	stage(t, s, catalog.ComponentDaemon, catalog.ChannelStable, 3, base.Add(2*time.Hour))
	stage(t, s, catalog.ComponentCLI, catalog.ChannelBeta, 4, base.Add(3*time.Hour))
	if err := s.Promote(a, base); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListByComponent(catalog.ComponentCLI, catalog.ChannelStable)
	if err != nil {
		t.Fatalf("ListByComponent: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != b || rows[1].ID != a {
		t.Fatalf("rows = %v, want [%d %d] newest first", rows, b, a)
	}
	if rows[1].State != catalog.StatePublic {
		t.Fatalf("history dropped state: %+v", rows[1])
	}
}

func TestGetMissing(t *testing.T) {
	s := open(t)
	if _, err := s.Get(1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// PlanExpiry and ExpireOldVersions must agree exactly: the dry run previews
// the pass, and a preview that can disagree with what follows is worse than no
// preview, because an operator acts on it.
func TestPlanExpiryMatchesWhatExpireActuallyDoes(t *testing.T) {
	s := open(t)
	var ids []int64
	for i := 1; i <= 5; i++ {
		id := stage(t, s, catalog.ComponentCLI, catalog.ChannelStable, i, base.Add(time.Duration(i)*time.Hour))
		if err := s.Promote(id, base.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// A yank in the mix, so the plan has to get the awkward case right too.
	successor, _ := s.NewestPublicExcept(catalog.ComponentCLI, catalog.ChannelStable, ids[4])
	if err := s.Yank(ids[4], successor.ID, base.Add(9*time.Hour)); err != nil {
		t.Fatal(err)
	}

	planned, retained, err := s.PlanExpiry(catalog.ComponentCLI, catalog.ChannelStable, 2)
	if err != nil {
		t.Fatalf("PlanExpiry: %v", err)
	}
	if len(planned)+len(retained) == 0 {
		t.Fatal("the plan is empty; the test would prove nothing")
	}
	// The plan changed nothing.
	for _, id := range ids {
		row, _ := s.Get(id)
		if row.State == catalog.StateExpired {
			t.Fatalf("PlanExpiry expired row %d", id)
		}
	}

	actual, err := s.ExpireOldVersions(catalog.ComponentCLI, catalog.ChannelStable, 2, base.Add(10*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(actual) != len(planned) {
		t.Fatalf("plan expired %d rows, the pass expired %d", len(planned), len(actual))
	}
	want := map[int64]bool{}
	for _, rv := range planned {
		want[rv.ID] = true
	}
	for _, rv := range actual {
		if !want[rv.ID] {
			t.Errorf("the pass expired row %d, which the plan did not name", rv.ID)
		}
	}
}

// OpenReadOnly is what `doctor` uses, and the two things that matter about it
// are that it creates nothing and migrates nothing. Through the ordinary
// opener a health check created catalog.db for a mistyped data dir and
// reported it healthy, and it could never observe a ledger behind the binary
// because Open had already brought it forward.
func TestOpenReadOnlyRefusesAMissingCatalogAndCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenReadOnly(dir); err == nil {
		t.Fatal("OpenReadOnly accepted a data dir with no catalog")
	}
	if _, err := os.Stat(filepath.Join(dir, DBFile)); !os.IsNotExist(err) {
		t.Fatalf("OpenReadOnly created %s", DBFile)
	}
}

func TestOpenReadOnlyReadsTheLedgerAndAppliesNoMigration(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := s.AppliedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	got, err := ro.AppliedMigrations()
	if err != nil {
		t.Fatalf("AppliedMigrations read-only: %v", err)
	}
	if len(got) != len(applied) {
		t.Fatalf("read-only ledger has %d rungs, want %d", len(got), len(applied))
	}
	// A write through the read-only handle must be refused, not silently
	// queued: the whole guarantee is that inspecting a production catalog
	// cannot change it.
	if _, err := ro.db.Exec(`INSERT INTO migrations (version, name, applied_at) VALUES (99, 'x', 0)`); err == nil {
		t.Fatal("a read-only catalog accepted a write")
	}
}
