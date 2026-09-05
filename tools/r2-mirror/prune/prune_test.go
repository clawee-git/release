package prune

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
)

// fakeStore satisfies Store: List returns the fixed key set filtered by prefix,
// Delete records what was removed.
type fakeStore struct {
	keys    []string
	deleted []string
	failOn  string // if non-empty, Delete of this key returns an error
}

func (f *fakeStore) List(_ context.Context, prefix string) ([]string, error) {
	var out []string
	for _, k := range f.keys {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *fakeStore) Delete(_ context.Context, key string) error {
	if key == f.failOn {
		return errors.New("boom")
	}
	f.deleted = append(f.deleted, key)
	return nil
}

// objectsFor expands stamps into the six-object shape a real cut uploads
// under <comp>/<sub><stamp>/ (sub is "" or "beta/").
func objectsFor(comp, sub string, stamps ...string) []string {
	var out []string
	for _, s := range stamps {
		for _, f := range []string{
			"SHA256SUMS.txt", "SHA256SUMS.txt.minisig",
			comp + "-darwin-amd64.zip", comp + "-darwin-arm64.zip",
			comp + "-linux-amd64.zip", comp + "-linux-arm64.zip",
		} {
			out = append(out, comp+"/"+sub+s+"/"+f)
		}
	}
	return out
}

// twelveStamps is ascending oldest→newest (verified against real `sort -V`).
var twelveStamps = []string{
	"v0.1.4.2026.06.13.15646772",
	"v0.1.9.2026.06.13.15646772",
	"v0.1.12.2026.06.14.3449c8b9",
	"v0.1.18.2026.06.14.3449c8b9",
	"v0.1.20.2026.06.15.1ffa0702",
	"v0.1.44.2026.06.23.b44ee15d",
	"v0.2.1.2026.08.10.aa11bb22",
	"v0.2.5.2026.08.20.abcdef00",
	"v0.2.9.2026.08.22.cc33dd44",
	"v0.2.12.2026.08.25.ee55ff66",
	"v0.2.20.2026.08.28.11223344",
	"v0.2.25.2026.08.29.2c953a94",
}

var threeBetas = []string{
	"v0.2.0.beta.2026.09.05.aaaaaaaa",
	"v0.2.1.beta.2026.09.08.bbbbbbbb",
	"v0.2.2.beta.2026.09.11.cccccccc",
}

func TestPruneKeepsNewestNAndDeletesTheRest(t *testing.T) {
	store := &fakeStore{keys: objectsFor("clawee", "", twelveStamps...)}

	n, err := Prune(context.Background(), store, "clawee", "stable", DefaultKeepStable, true, io.Discard)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	dropN := len(twelveStamps) - DefaultKeepStable
	want := dropN * 6
	if n != want {
		t.Errorf("deleted count = %d, want %d", n, want)
	}
	keepSet := map[string]bool{}
	for _, s := range twelveStamps[:dropN] {
		keepSet[s] = true // actually the DROP set
	}
	for _, k := range store.deleted {
		stamp := strings.Split(k, "/")[1]
		if !keepSet[stamp] {
			t.Errorf("deleted a stamp that should have been kept: %s", k)
		}
	}
	if len(store.deleted) != want {
		t.Errorf("recorded %d deletes, want %d", len(store.deleted), want)
	}
}

func TestPruneDryRunDeletesNothing(t *testing.T) {
	store := &fakeStore{keys: objectsFor("clawee", "", twelveStamps...)}

	var out strings.Builder
	n, err := Prune(context.Background(), store, "clawee", "stable", DefaultKeepStable, false, &out)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	want := (len(twelveStamps) - DefaultKeepStable) * 6
	if n != want {
		t.Errorf("planned count = %d, want %d", n, want)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("dry-run deleted %d objects, want 0: %v", len(store.deleted), store.deleted)
	}
	if !strings.Contains(out.String(), "would delete") {
		t.Errorf("dry-run output missing 'would delete' lines:\n%s", out.String())
	}
}

// TestManifestsAreNeverCandidates: <comp>/latest.json and
// <comp>/beta/latest.json survive both passes.
func TestManifestsAreNeverCandidates(t *testing.T) {
	keys := append(objectsFor("clawee", "", twelveStamps...), objectsFor("clawee", "beta/", threeBetas...)...)
	keys = append(keys, "clawee/latest.json", "clawee/beta/latest.json")
	store := &fakeStore{keys: keys}

	if _, err := Prune(context.Background(), store, "clawee", "stable", DefaultKeepStable, true, io.Discard); err != nil {
		t.Fatalf("stable Prune: %v", err)
	}
	if _, err := Prune(context.Background(), store, "clawee", "beta", DefaultKeepBeta, true, io.Discard); err != nil {
		t.Fatalf("beta Prune: %v", err)
	}
	for _, k := range store.deleted {
		if strings.HasSuffix(k, "latest.json") {
			t.Fatalf("prune deleted a manifest: %s — installers resolve 'latest' from it", k)
		}
	}
}

// TestStablePassIgnoresBetaPrefix: the stable pass lists <comp>/ and therefore
// sees <comp>/beta/<stamp>/… — those keys have "beta" as their stamp segment,
// which is no stamp at all, and must be neither counted nor deleted. With
// 12 stables and keep 3, exactly the objects of the 9 oldest stables go.
func TestStablePassIgnoresBetaPrefix(t *testing.T) {
	keys := append(objectsFor("clawee", "", twelveStamps...), objectsFor("clawee", "beta/", threeBetas...)...)
	store := &fakeStore{keys: keys}

	n, err := Prune(context.Background(), store, "clawee", "stable", DefaultKeepStable, true, io.Discard)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	want := (len(twelveStamps) - DefaultKeepStable) * 6
	if n != want {
		t.Errorf("stable pass deleted %d objects, want %d (oldest stables only)", n, want)
	}
	for _, k := range store.deleted {
		if strings.HasPrefix(k, "clawee/beta/") {
			t.Fatalf("stable pass deleted a beta object: %s", k)
		}
	}
}

// TestBetaPassKeepsOne: three betas, keep 1 → the two oldest go, the newest
// stays, and no stable object is touched.
func TestBetaPassKeepsOne(t *testing.T) {
	keys := append(objectsFor("clawee", "", twelveStamps...), objectsFor("clawee", "beta/", threeBetas...)...)
	store := &fakeStore{keys: keys}

	n, err := Prune(context.Background(), store, "clawee", "beta", DefaultKeepBeta, true, io.Discard)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 12 {
		t.Errorf("beta pass deleted %d objects, want 12 (2 betas × 6)", n)
	}
	for _, k := range store.deleted {
		if !strings.HasPrefix(k, "clawee/beta/") {
			t.Fatalf("beta pass deleted a non-beta object: %s", k)
		}
		if strings.Contains(k, threeBetas[2]) {
			t.Fatalf("beta pass deleted the newest beta: %s", k)
		}
	}
}

// TestNeitherShapeIgnored: a directory matching neither shape (a pre-release
// spelled with a dash, a nightly, a hand-upload) is ignored on BOTH passes —
// never counted, never deleted.
func TestNeitherShapeIgnored(t *testing.T) {
	odd := []string{
		"clawee/v0.9.9-rc1/clawee-linux-amd64.zip",
		"clawee/nightly/clawee-darwin-arm64.zip",
		"clawee/v0.1.99-hand-upload/notes.txt",
		"clawee/beta/v0.9.9-rc1/clawee-linux-amd64.zip",
		"clawee/beta/v0.2.0.beta.2026.09.05.aaaaaaaa.extra/x.zip",
	}
	keys := append(objectsFor("clawee", "", twelveStamps...), objectsFor("clawee", "beta/", threeBetas...)...)
	keys = append(keys, odd...)
	store := &fakeStore{keys: keys}

	var out strings.Builder
	if _, err := Prune(context.Background(), store, "clawee", "stable", DefaultKeepStable, true, &out); err != nil {
		t.Fatalf("stable Prune: %v", err)
	}
	if _, err := Prune(context.Background(), store, "clawee", "beta", DefaultKeepBeta, true, &out); err != nil {
		t.Fatalf("beta Prune: %v", err)
	}
	for _, k := range store.deleted {
		for _, o := range odd {
			if k == o {
				t.Errorf("prune deleted a neither-shape key: %s", k)
			}
		}
	}
	// Counted? The stable pass must report exactly 12 stamps, the beta pass 3.
	if !strings.Contains(out.String(), "[clawee/stable] 12 stamp(s)") || !strings.Contains(out.String(), "[clawee/beta] 3 stamp(s)") {
		t.Errorf("a neither-shape key was counted:\n%s", out.String())
	}
}

func TestPruneUnderRetentionDoesNothing(t *testing.T) {
	store := &fakeStore{keys: objectsFor("clawee", "", twelveStamps[:DefaultKeepStable]...)}

	var out strings.Builder
	n, err := Prune(context.Background(), store, "clawee", "stable", DefaultKeepStable, true, &out)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted %d objects with %d stamps and keep=%d, want 0", n, DefaultKeepStable, DefaultKeepStable)
	}
	if !strings.Contains(out.String(), "nothing to prune") {
		t.Errorf("expected 'nothing to prune', got:\n%s", out.String())
	}
}

func TestPruneSkipsPermanentInDropWindow(t *testing.T) {
	store := &fakeStore{keys: objectsFor("clawee", "", twelveStamps[:5]...)}
	protect := map[string]struct{}{"clawee/" + twelveStamps[0]: {}}
	n, err := PruneProtect(context.Background(), store, "clawee", "stable", 3, true, io.Discard, protect)
	if err != nil {
		t.Fatalf("PruneProtect: %v", err)
	}
	// 5 stamps, keep 3 → drop window is [0] and [1]; pin skips [0], so only [1] × 6.
	if n != 6 {
		t.Errorf("deleted count = %d, want 6", n)
	}
	for _, k := range store.deleted {
		if strings.Contains(k, twelveStamps[0]) {
			t.Errorf("deleted the permanent stamp: %s", k)
		}
	}
}

func TestPruneRejectsKeepBelowOneAndUnknownChannel(t *testing.T) {
	store := &fakeStore{keys: objectsFor("clawee", "", twelveStamps...)}
	if _, err := Prune(context.Background(), store, "clawee", "stable", 0, true, io.Discard); err == nil {
		t.Fatal("keep=0 must be rejected — it would empty the component prefix")
	}
	if _, err := Prune(context.Background(), store, "clawee", "nightly", 1, true, io.Discard); err == nil {
		t.Fatal("an unknown channel must be rejected")
	}
	if len(store.deleted) != 0 {
		t.Errorf("a refused call deleted %d objects", len(store.deleted))
	}
	if DefaultKeep("stable") != 3 || DefaultKeep("beta") != 1 || DefaultKeep("x") != 0 {
		t.Fatal("DefaultKeep: want stable 3, beta 1, unknown 0")
	}
}

func TestPruneStopsAndReportsOnDeleteFailure(t *testing.T) {
	keys := objectsFor("clawee", "", twelveStamps...)
	store := &fakeStore{keys: keys, failOn: "clawee/" + twelveStamps[0] + "/SHA256SUMS.txt"}

	_, err := Prune(context.Background(), store, "clawee", "stable", DefaultKeepStable, true, io.Discard)
	if err == nil {
		t.Fatal("a failing Delete must surface as an error, not a silent partial prune")
	}
}

func TestPruneOnlyReadsItsOwnComponent(t *testing.T) {
	keys := append(objectsFor("clawee", "", twelveStamps...), objectsFor("claweed", "", twelveStamps...)...)
	store := &fakeStore{keys: keys}

	if _, err := Prune(context.Background(), store, "clawee", "stable", DefaultKeepStable, true, io.Discard); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for _, k := range store.deleted {
		if strings.HasPrefix(k, "claweed/") {
			t.Errorf("pruning clawee deleted an claweed object: %s", k)
		}
	}
}

func TestVersionOrderMatchesSortV(t *testing.T) {
	// Exactly what GNU `sort -V` produces for this set (captured from the shell).
	in := []string{
		"v0.1.9.2026.06.13.15646772",
		"v0.1.12.2026.06.14.3449c8b9",
		"v0.1.44.2026.06.23.b44ee15d",
		"v0.2.25.2026.08.29.2c953a94",
		"v0.1.4.2026.06.13.15646772",
	}
	want := []string{
		"v0.1.4.2026.06.13.15646772",
		"v0.1.9.2026.06.13.15646772",
		"v0.1.12.2026.06.14.3449c8b9",
		"v0.1.44.2026.06.23.b44ee15d",
		"v0.2.25.2026.08.29.2c953a94",
	}
	got := append([]string(nil), in...)
	sort.Sort(byVersionSort(got))
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sort mismatch:\n got: %v\nwant: %v", got, want)
		}
	}
}

func TestVersionOrderShaTieBreak(t *testing.T) {
	// Same triple and date, differing only in the trailing 8-hex sha. `sort -V`
	// puts alpha-leading shas BEFORE numeric-leading ones; naive field-wise
	// lexical comparison gets this backwards. Captured from the shell.
	in := []string{
		"v0.2.5.2026.08.20.0abcdef0",
		"v0.2.5.2026.08.20.abcdef00",
		"v0.2.5.2026.08.20.f048cdba",
		"v0.2.5.2026.08.20.5048cdba",
	}
	want := []string{
		"v0.2.5.2026.08.20.abcdef00",
		"v0.2.5.2026.08.20.f048cdba",
		"v0.2.5.2026.08.20.0abcdef0",
		"v0.2.5.2026.08.20.5048cdba",
	}
	got := append([]string(nil), in...)
	sort.Sort(byVersionSort(got))
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sha tie-break mismatch:\n got: %v\nwant: %v", got, want)
		}
	}
}
