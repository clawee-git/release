package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/clawee-git/release/internal/manage/backend/backendtest"
	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/manifest"
	"github.com/clawee-git/release/internal/register"
)

var base = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

type fixture struct {
	t       *testing.T
	st      *store.Store
	rec     *backendtest.Recorder
	staging *backendtest.Staging
	public  *backendtest.Public
	github  *backendtest.GitHub
	deps    Deps
	now     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rec := &backendtest.Recorder{}
	f := &fixture{t: t, st: st, rec: rec, now: base}
	f.staging = backendtest.NewStaging(rec, "clawee-staging")
	f.public = backendtest.NewPublic(rec, f.staging)
	f.github = backendtest.NewGitHub(rec)
	f.deps = Deps{
		Store: st, Staging: f.staging, Public: f.public, GitHub: f.github,
		Now: func() time.Time { return f.now }, Log: slog.New(slog.DiscardHandler),
	}
	return f
}

func stampFor(channel string, n int) string {
	if channel == catalog.ChannelBeta {
		return fmt.Sprintf("v0.3.%d.beta.2026.09.04.%08x", n, n)
	}
	return fmt.Sprintf("v0.2.%d.2026.09.04.%08x", n, n)
}

// stage inserts a row and seeds the staging bucket with matching bytes.
func (f *fixture) stage(comp, channel string, n int, at time.Time) int64 {
	f.t.Helper()
	stamp := stampFor(channel, n)
	keyBase := register.KeyBase(comp, channel, stamp)
	var artifacts []register.Artifact
	for _, plat := range []string{"darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64"} {
		name := "clawee-" + comp + "-" + plat + ".zip"
		key := keyBase + "/" + name
		body := []byte("zip bytes for " + key)
		f.staging.Objects[key] = body
		sum := sha256.Sum256(body)
		artifacts = append(artifacts, register.Artifact{
			Platform: strings.Replace(plat, "-", "/", 1), Key: key,
			SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body)),
		})
	}
	sumsKey, minisigKey := keyBase+"/"+register.SumsName, keyBase+"/"+register.MinisigName
	f.staging.Objects[sumsKey] = []byte("the sums file")
	f.staging.Objects[minisigKey] = []byte("the signature")
	blob, _ := json.Marshal(artifacts)
	id, err := f.st.Stage(store.ReleaseVersion{
		Component: comp, Channel: channel, Version: fmt.Sprintf("0.2.%d", n), Stamp: stamp,
		ArtifactsJSON: string(blob), SumsKey: sumsKey, MinisigKey: minisigKey, CreatedAt: at,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) promote(id int64) (string, error) {
	var out bytes.Buffer
	err := Promote(context.Background(), f.deps, id, &out)
	return out.String(), err
}

func events(t *testing.T, ndjson string) []Event {
	t.Helper()
	var out []Event
	for _, line := range strings.Split(strings.TrimSpace(ndjson), "\n") {
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("progress line %q is not JSON: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// TestPromoteOrder is the acceptance test for the sequence. It reads ONE call
// log shared by all three fakes, so what it asserts is the interleaving —
// verify before any copy, every copy before GitHub, GitHub before the
// manifest, manifest before the flip.
func TestPromoteOrder(t *testing.T) {
	f := newFixture(t)
	id := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	out, err := f.promote(id)
	if err != nil {
		t.Fatalf("Promote: %v\n%s", err, out)
	}

	ops := f.rec.Ops()
	lastVerify := lastIndexOfPrefix(ops, "staging.")
	firstCopy := f.rec.FirstIndexOf("public.copy")
	firstGitHub := f.rec.FirstIndexOf("github.create")
	manifestPut := f.rec.FirstIndexOf("public.put")
	if firstCopy < 0 || firstGitHub < 0 || manifestPut < 0 {
		t.Fatalf("a whole step is missing: %v", ops)
	}
	if lastVerify > firstCopy {
		t.Fatalf("a staging read happened after the first copy: %v", ops)
	}
	if firstGitHub > manifestPut {
		t.Fatalf("the manifest was written before GitHub: %v", ops)
	}
	// The manifest is LAST of the remote writes: nothing resolves to the new
	// release until every byte is in place.
	if manifestPut != len(ops)-1 {
		t.Fatalf("the manifest write is not the last remote call: %v", ops)
	}
	// Every artifact plus the sums file and its signature were copied.
	copies := 0
	for _, c := range f.rec.Calls() {
		if c.Op == "public.copy" {
			copies++
		}
	}
	if copies != 6 {
		t.Fatalf("%d copies, want 6 (4 zips + sums + minisig)", copies)
	}

	// The flip happened, and it happened after the manifest.
	row, err := f.st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != catalog.StatePublic || !row.IsCurrent {
		t.Fatalf("row after promote = %+v", row)
	}
	evs := events(t, out)
	if idx(evs, "manifest", "ok") > idx(evs, "flip", "ok") {
		t.Fatal("the stream reports the flip before the manifest")
	}
	if evs[len(evs)-1].Status != "done" {
		t.Fatalf("the stream does not end with done: %+v", evs[len(evs)-1])
	}

	// The public objects carry the STAGED bytes, under the stable layout.
	wantBase := manifest.PublicBase(catalog.ComponentCLI, catalog.ChannelStable, row.Stamp)
	if wantBase != catalog.ComponentCLI+"/"+row.Stamp {
		t.Fatalf("stable layout = %q", wantBase)
	}
	got := f.public.Objects[wantBase+"/clawee-clawee-darwin-arm64.zip"]
	src := f.staging.Objects[catalog.ComponentCLI+"/stable/"+row.Stamp+"/clawee-clawee-darwin-arm64.zip"]
	if !bytes.Equal(got, src) {
		t.Fatalf("the public object is not the staged one: %q vs %q", got, src)
	}
}

func TestPromoteUsesTheBetaLayoutAndPrerelease(t *testing.T) {
	f := newFixture(t)
	id := f.stage(catalog.ComponentCLI, catalog.ChannelBeta, 1, base)
	if _, err := f.promote(id); err != nil {
		t.Fatal(err)
	}
	row, _ := f.st.Get(id)
	wantBase := catalog.ComponentCLI + "/beta/" + row.Stamp
	if _, ok := f.public.Objects[wantBase+"/clawee-clawee-linux-amd64.zip"]; !ok {
		t.Fatalf("beta artifacts are not under %s: %v", wantBase, keys(f.public.Objects))
	}
	if _, ok := f.public.Objects["clawee/beta/latest.json"]; !ok {
		t.Fatalf("the beta manifest was not written: %v", keys(f.public.Objects))
	}
	if _, ok := f.public.Objects["clawee/latest.json"]; ok {
		t.Fatal("a beta promote wrote the STABLE manifest")
	}
	tag := ReleaseTag(catalog.ComponentCLI, row.Stamp)
	if !f.github.Prerelease[tag] {
		t.Fatal("the beta release was not marked pre-release")
	}
}

// The manifest schema must match what the cut's mirror writes, or installers
// stop resolving.
func TestManifestContentsNameEveryArtifact(t *testing.T) {
	f := newFixture(t)
	id := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	if _, err := f.promote(id); err != nil {
		t.Fatal(err)
	}
	row, _ := f.st.Get(id)
	var m manifest.Latest
	if err := json.Unmarshal(f.public.Objects["clawee/latest.json"], &m); err != nil {
		t.Fatalf("the manifest is not JSON: %v", err)
	}
	if m.Component != catalog.ComponentCLI || m.Stamp != row.Stamp || m.Version != row.Version {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.Zips) != 4 {
		t.Fatalf("manifest names %d zips, want 4", len(m.Zips))
	}
	if m.Path != catalog.ComponentCLI+"/"+row.Stamp {
		t.Fatalf("manifest path = %q", m.Path)
	}
	if !strings.HasSuffix(m.SHA256Sums, "/SHA256SUMS.txt") || !strings.HasSuffix(m.Minisig, ".minisig") {
		t.Fatalf("manifest verification refs = %q, %q", m.SHA256Sums, m.Minisig)
	}
	if m.Updated != base.UTC().Format(time.RFC3339) {
		t.Fatalf("manifest updated = %q; the clock is not injected", m.Updated)
	}
}

// ── Failure modes ────────────────────────────────────────────────────────

func TestPromoteWithoutAGitHubPublisherCopiesNothing(t *testing.T) {
	f := newFixture(t)
	f.deps.GitHub = nil
	id := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	out, err := f.promote(id)
	if !errors.Is(err, ErrNoPublisher) {
		t.Fatalf("err = %v, want ErrNoPublisher", err)
	}
	// Nothing at all: the refusal is BEFORE the first read, so a promote that
	// cannot finish never starts.
	if len(f.rec.Calls()) != 0 {
		t.Fatalf("a refused promote touched the stores: %v", f.rec.Ops())
	}
	if len(f.public.Objects) != 0 {
		t.Fatal("a refused promote wrote to the public bucket")
	}
	row, _ := f.st.Get(id)
	if row.State != catalog.StateStaged {
		t.Fatalf("row = %s, want staged", row.State)
	}
	_ = out
}

func TestVerifyFailureLeavesEverythingAlone(t *testing.T) {
	cases := []struct {
		name    string
		break_  func(f *fixture, id int64)
		wantMsg string
	}{
		{"wrong size", func(f *fixture, id int64) {
			row, _ := f.st.Get(id)
			var arts []register.Artifact
			json.Unmarshal([]byte(row.ArtifactsJSON), &arts)
			f.staging.Objects[arts[0].Key] = []byte("shorter")
		}, "bytes, the catalog says"},
		{"wrong bytes", func(f *fixture, id int64) {
			row, _ := f.st.Get(id)
			var arts []register.Artifact
			json.Unmarshal([]byte(row.ArtifactsJSON), &arts)
			same := make([]byte, arts[0].Size)
			copy(same, []byte("tampered"))
			f.staging.Objects[arts[0].Key] = same
		}, "sha256 is"},
		{"unreadable", func(f *fixture, id int64) { f.staging.FailGet = "linux-arm64" }, "refused"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			id := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 1, base)
			c.break_(f, id)
			out, err := f.promote(id)
			if err == nil || !strings.Contains(err.Error(), c.wantMsg) {
				t.Fatalf("err = %v, want one naming %q", err, c.wantMsg)
			}
			if f.rec.Has("public.copy") || f.rec.Has("public.put") || f.rec.Has("github.create") {
				t.Fatalf("a failed verify still published: %v", f.rec.Ops())
			}
			row, _ := f.st.Get(id)
			if row.State != catalog.StateStaged {
				t.Fatalf("row = %s, want staged", row.State)
			}
			// The stream carries the error, not just the return value: an
			// operator watching the log must see why it stopped.
			if !strings.Contains(out, `"status":"error"`) {
				t.Fatalf("the progress stream has no error event:\n%s", out)
			}
		})
	}
}

func TestCopyFailureLeavesStagedWithNoManifestAndNoFlip(t *testing.T) {
	f := newFixture(t)
	id := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	f.public.FailCopy = "linux-amd64"
	out, err := f.promote(id)
	if err == nil || !strings.Contains(err.Error(), "copy") {
		t.Fatalf("err = %v", err)
	}
	if f.rec.Has("public.put") {
		t.Fatal("the manifest was written after a copy failure")
	}
	if f.rec.Has("github.create") {
		t.Fatal("a GitHub release was created after a copy failure")
	}
	row, _ := f.st.Get(id)
	if row.State != catalog.StateStaged || row.IsCurrent {
		t.Fatalf("row = %+v, want staged and not current", row)
	}
	if !strings.Contains(out, `"status":"error"`) {
		t.Fatalf("no error event in the stream:\n%s", out)
	}
	// A retry after the transient failure completes.
	f.public.FailCopy = ""
	if _, err := f.promote(id); err != nil {
		t.Fatalf("retry after a copy failure: %v", err)
	}
	row, _ = f.st.Get(id)
	if row.State != catalog.StatePublic {
		t.Fatalf("row after retry = %s", row.State)
	}
}

func TestGitHubFailureLeavesStagedAndNoManifest(t *testing.T) {
	f := newFixture(t)
	id := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	f.github.FailUpload = "SHA256SUMS.txt.minisig"
	_, err := f.promote(id)
	if err == nil || !strings.Contains(err.Error(), "github asset") {
		t.Fatalf("err = %v", err)
	}
	if f.rec.Has("public.put") {
		t.Fatal("the manifest was written after a GitHub failure")
	}
	row, _ := f.st.Get(id)
	if row.State != catalog.StateStaged {
		t.Fatalf("row = %s, want staged", row.State)
	}
}

func TestPromoteRefusesANonStagedRow(t *testing.T) {
	f := newFixture(t)
	id := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	if _, err := f.promote(id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.promote(id); !errors.Is(err, store.ErrBadState) {
		t.Fatalf("re-promote: err = %v, want ErrBadState", err)
	}
}

// ── Yank ─────────────────────────────────────────────────────────────────

func TestYankRepointsTheManifestAtTheNewestRemainingPublicRow(t *testing.T) {
	f := newFixture(t)
	older := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	newer := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 2, base.Add(time.Hour))
	if _, err := f.promote(older); err != nil {
		t.Fatal(err)
	}
	f.now = base.Add(time.Hour)
	if _, err := f.promote(newer); err != nil {
		t.Fatal(err)
	}
	newerRow, _ := f.st.Get(newer)
	olderRow, _ := f.st.Get(older)

	var out bytes.Buffer
	if err := Yank(context.Background(), f.deps, newer, &out); err != nil {
		t.Fatalf("Yank: %v\n%s", out.String(), err)
	}
	var m manifest.Latest
	if err := json.Unmarshal(f.public.Objects["clawee/latest.json"], &m); err != nil {
		t.Fatal(err)
	}
	if m.Stamp != olderRow.Stamp {
		t.Fatalf("manifest still names %q; want the older row %q", m.Stamp, olderRow.Stamp)
	}
	row, _ := f.st.Get(newer)
	if row.State != catalog.StateYanked || row.IsCurrent {
		t.Fatalf("yanked row = %+v", row)
	}
	_ = newerRow

	// The manifest moves BEFORE the row flips: a failure between the two must
	// leave the withdrawn build unserved, never still being handed out.
	ops := f.rec.Ops()
	lastPut := lastIndexOf(ops, "public.put")
	if lastPut < 0 {
		t.Fatal("no manifest write during yank")
	}
	evs := events(t, out.String())
	if idx(evs, "manifest", "ok") > idx(evs, "flip", "ok") {
		t.Fatal("the stream reports the flip before the manifest re-point")
	}
}

func TestYankOfTheLastPublicRowRemovesTheManifest(t *testing.T) {
	f := newFixture(t)
	id := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	if _, err := f.promote(id); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Yank(context.Background(), f.deps, id, &out); err != nil {
		t.Fatalf("Yank: %v", err)
	}
	if _, ok := f.public.Objects["clawee/latest.json"]; ok {
		t.Fatal("the manifest still exists; an installer would resolve a withdrawn release")
	}
	if !f.rec.Has("public.delete") {
		t.Fatalf("the manifest was not deleted: %v", f.rec.Ops())
	}
	// The bytes are NOT deleted: yank does not touch existing installs.
	row, _ := f.st.Get(id)
	if _, ok := f.public.Objects[catalog.ComponentCLI+"/"+row.Stamp+"/clawee-clawee-darwin-arm64.zip"]; !ok {
		t.Fatal("yank deleted the release's artifacts")
	}
}

func TestYankRefusesANonPublicRow(t *testing.T) {
	f := newFixture(t)
	id := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	if err := Yank(context.Background(), f.deps, id, nil); !errors.Is(err, store.ErrBadState) {
		t.Fatalf("yank of a staged row: err = %v, want ErrBadState", err)
	}
	if f.rec.Has("public.put") || f.rec.Has("public.delete") {
		t.Fatal("a refused yank touched the manifest")
	}
}

func TestYankIsPerChannel(t *testing.T) {
	f := newFixture(t)
	stable := f.stage(catalog.ComponentCLI, catalog.ChannelStable, 1, base)
	beta := f.stage(catalog.ComponentCLI, catalog.ChannelBeta, 1, base)
	if _, err := f.promote(stable); err != nil {
		t.Fatal(err)
	}
	if _, err := f.promote(beta); err != nil {
		t.Fatal(err)
	}
	if err := Yank(context.Background(), f.deps, beta, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.public.Objects["clawee/latest.json"]; !ok {
		t.Fatal("yanking a beta row removed the stable manifest")
	}
	if _, ok := f.public.Objects["clawee/beta/latest.json"]; ok {
		t.Fatal("the beta manifest survived the yank of the only beta row")
	}
}

func idx(evs []Event, step, status string) int {
	for i, e := range evs {
		if e.Step == step && e.Status == status {
			return i
		}
	}
	return -1
}

func lastIndexOf(ops []string, want string) int {
	last := -1
	for i, o := range ops {
		if o == want {
			last = i
		}
	}
	return last
}

func lastIndexOfPrefix(ops []string, prefix string) int {
	last := -1
	for i, o := range ops {
		if strings.HasPrefix(o, prefix) {
			last = i
		}
	}
	return last
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
