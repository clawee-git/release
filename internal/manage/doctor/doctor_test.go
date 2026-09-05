package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawee-git/release/internal/manage/catalog"
)

const testKey = "RWQf6LRCGA9i53mlYecO4IzT51TGPpvWucNSCh1CBM0QTaLn73Y7GFO3"

// wired is a deployment where everything passes. Each test breaks exactly one
// thing, which is the only way to be sure a check fails for its OWN reason.
func wired(t *testing.T) Deps {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dataDir, "secret.key")
	if err := os.WriteFile(keyPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	kit := filepath.Join(dir, "kit")
	if err := os.MkdirAll(kit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kit, "clawee-release.pub"),
		[]byte("untrusted comment: test\n"+testKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, comp := range catalog.Components {
		if err := os.MkdirAll(filepath.Join(kit, comp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(kit, comp, "install.sh"),
			[]byte("#!/bin/sh\nPUBKEY='"+testKey+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	return Deps{
		Staging: &Bucket{Name: "staging-example", List: func(context.Context, string) ([]string, error) {
			return []string{"clawee/beta/v0.1.0/clawee-darwin-arm64.zip"}, nil
		}},
		Public: &Bucket{Name: "public-example", List: func(context.Context, string) ([]string, error) {
			return []string{"clawee/latest.json"}, nil
		}},
		AnonGet: func(context.Context, string, string) (int, error) { return 403, nil },
		Repo: func(context.Context) (RepoInfo, error) {
			return RepoInfo{FullName: "example-org/example-repo", CanPublishReleases: true}, nil
		},
		Catalog:        func() ([]string, error) { return []string{"1 initial catalog"}, nil },
		WantMigrations: 1,
		DataDir:        dataDir,
		SecretKeyPath:  keyPath,
		KitRoot:        kit,
		EmbeddedKey:    testKey,
		UID:            os.Getuid(),
	}
}

func run(t *testing.T, d Deps) Report {
	t.Helper()
	return Run(context.Background(), d)
}

func check(t *testing.T, r Report, name string) Result {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, r.Checks)
	return Result{}
}

func wantStatus(t *testing.T, r Report, name string, want Status) Result {
	t.Helper()
	c := check(t, r, name)
	if c.Status != want {
		t.Errorf("check %s is %s (%s), want %s", name, c.Status, c.Detail, want)
	}
	return c
}

func TestAFullyWiredDeploymentPassesEveryCheck(t *testing.T) {
	r := run(t, wired(t))
	for _, c := range r.Checks {
		if c.Status != StatusOK {
			t.Errorf("check %s is %s: %s", c.Name, c.Status, c.Detail)
		}
	}
	if r.Failed() {
		t.Error("a fully wired deployment reports a failure")
	}
}

// The check this verb was worth writing for: a staging bucket that answers
// anonymously has been publishing every unpromoted build since it was created,
// and nothing else in the system can notice.
func TestAStagingBucketThatAnswersAnonymouslyIsARefusal(t *testing.T) {
	for _, status := range []int{200, 206, 301, 302} {
		d := wired(t)
		d.AnonGet = func(context.Context, string, string) (int, error) { return status, nil }
		c := wantStatus(t, run(t, d), "staging-private", StatusFail)
		if !strings.Contains(c.Detail, "staging-example") {
			t.Errorf("the refusal does not name the bucket: %s", c.Detail)
		}
		if !strings.Contains(c.Detail, "unauthenticated") {
			t.Errorf("the refusal does not say what was done: %s", c.Detail)
		}
	}
}

// A transport failure is the ABSENCE of an answer. Reading it as privacy is
// the same lie as reading a 200 as privacy.
func TestAnUnprobableStagingBucketFailsRatherThanPassing(t *testing.T) {
	d := wired(t)
	d.AnonGet = func(context.Context, string, string) (int, error) { return 0, errors.New("dial tcp: timeout") }
	wantStatus(t, run(t, d), "staging-private", StatusFail)
}

// An empty staging bucket can only be probed with a key that never existed, so
// the 404 proves nothing about the keys that will exist. It passes — there is
// nothing to leak yet — and SAYS the check is weaker.
func TestAnEmptyStagingBucketSaysThePrivacyProbeIsWeak(t *testing.T) {
	d := wired(t)
	d.Staging.List = func(context.Context, string) ([]string, error) { return nil, nil }
	d.AnonGet = func(context.Context, string, string) (int, error) { return 404, nil }
	c := wantStatus(t, run(t, d), "staging-private", StatusOK)
	if !strings.Contains(c.Detail, "weaker") {
		t.Errorf("an empty-bucket probe does not disclose its weakness: %s", c.Detail)
	}
}

func TestAnUnexpectedAnonymousStatusIsNotReadAsPrivate(t *testing.T) {
	d := wired(t)
	d.AnonGet = func(context.Context, string, string) (int, error) { return 500, nil }
	wantStatus(t, run(t, d), "staging-private", StatusFail)
}

func TestAnUnreachableBucketFailsAndNamesIt(t *testing.T) {
	d := wired(t)
	d.Public.List = func(context.Context, string) ([]string, error) { return nil, errors.New("403 forbidden") }
	c := wantStatus(t, run(t, d), "public-bucket", StatusFail)
	if !strings.Contains(c.Detail, "public-example") {
		t.Errorf("the failure does not name the bucket: %s", c.Detail)
	}
}

func TestAnUnconfiguredStoreIsSkippedNotFailed(t *testing.T) {
	d := wired(t)
	d.Staging, d.Public, d.AnonGet, d.Repo = nil, nil, nil, nil
	r := run(t, d)
	for _, name := range []string{"staging-bucket", "staging-private", "public-bucket", "github"} {
		wantStatus(t, r, name, StatusSkipped)
	}
	if r.Failed() {
		t.Error("a partially wired deployment is reported as failing; it is being brought up in stages")
	}
}

func TestAGitHubTokenThatCannotReadTheRepoFails(t *testing.T) {
	d := wired(t)
	d.Repo = func(context.Context) (RepoInfo, error) { return RepoInfo{}, errors.New("status 404") }
	wantStatus(t, run(t, d), "github", StatusFail)
}

// --check-write asks a question about SCOPE, and it is answered by reading the
// repo's permissions. Nothing is created: a draft release minted by a health
// check is a publication nobody approved.
func TestCheckWriteFailsATokenThatCouldNotPublish(t *testing.T) {
	d := wired(t)
	d.Repo = func(context.Context) (RepoInfo, error) {
		return RepoInfo{FullName: "example-org/example-repo", CanPublishReleases: false}, nil
	}
	wantStatus(t, run(t, d), "github", StatusOK)

	d.CheckWrite = true
	c := wantStatus(t, run(t, d), "github", StatusFail)
	if !strings.Contains(c.Detail, "after the bytes are already copied") {
		t.Errorf("the failure does not say when promote would break: %s", c.Detail)
	}
}

func TestAKitWhosePubkeyDisagreesWithTheBakedKeyFails(t *testing.T) {
	d := wired(t)
	if err := os.WriteFile(filepath.Join(d.KitRoot, "clawee-release.pub"),
		[]byte("untrusted comment: other\nRWQOTHERKEYOTHERKEYOTHERKEY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, run(t, d), "release-key", StatusFail)
}

func TestABootstrapThatBakesADifferentKeyFails(t *testing.T) {
	d := wired(t)
	boot := filepath.Join(d.KitRoot, catalog.Components[0], "install.sh")
	if err := os.WriteFile(boot, []byte("#!/bin/sh\nPUBKEY='RWQOTHER'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := wantStatus(t, run(t, d), "release-key", StatusFail)
	if !strings.Contains(c.Detail, "install.sh") {
		t.Errorf("the failure does not name the bootstrap: %s", c.Detail)
	}
}

// Without a kit on the host the key cannot be compared with anything. That is
// a SKIP: reporting it as a pass would make a green doctor mean less than it
// says, and reporting it as a failure would make every service host red.
func TestNoKitRootSkipsTheKeyComparisonRatherThanPassingIt(t *testing.T) {
	d := wired(t)
	d.KitRoot = ""
	wantStatus(t, run(t, d), "release-key", StatusSkipped)
}

func TestAWorldReadableDataDirFails(t *testing.T) {
	d := wired(t)
	if err := os.Chmod(d.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := wantStatus(t, run(t, d), "data-dir", StatusFail)
	if !strings.Contains(c.Detail, "second factor") {
		t.Errorf("the failure does not say what leaks: %s", c.Detail)
	}
}

func TestASecretKeyAtTheWrongModeFails(t *testing.T) {
	d := wired(t)
	if err := os.Chmod(d.SecretKeyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	c := wantStatus(t, run(t, d), "secret-key", StatusFail)
	if !strings.Contains(c.Detail, "0600") {
		t.Errorf("the failure does not name the wanted mode: %s", c.Detail)
	}
}

func TestAMissingSecretKeyFailsAndSaysWhatCreatesIt(t *testing.T) {
	d := wired(t)
	if err := os.Remove(d.SecretKeyPath); err != nil {
		t.Fatal(err)
	}
	c := wantStatus(t, run(t, d), "secret-key", StatusFail)
	if !strings.Contains(c.Detail, "admin add") {
		t.Errorf("the failure does not say what creates the key: %s", c.Detail)
	}
}

func TestAFileOwnedByAnotherAccountFails(t *testing.T) {
	d := wired(t)
	d.UID = os.Getuid() + 1000
	c := wantStatus(t, run(t, d), "secret-key", StatusFail)
	if !strings.Contains(c.Detail, "owned by uid") {
		t.Errorf("the failure does not name the owner: %s", c.Detail)
	}
}

func TestACatalogBehindTheBinaryFailsAndSoDoesOneAhead(t *testing.T) {
	d := wired(t)
	d.WantMigrations = 3
	c := wantStatus(t, run(t, d), "catalog", StatusFail)
	if !strings.Contains(c.Detail, "has not run") {
		t.Errorf("a behind catalog is misdescribed: %s", c.Detail)
	}

	d = wired(t)
	d.Catalog = func() ([]string, error) { return []string{"1 a", "2 b"}, nil }
	c = wantStatus(t, run(t, d), "catalog", StatusFail)
	if !strings.Contains(c.Detail, "NEWER build") {
		t.Errorf("an ahead catalog is misdescribed: %s", c.Detail)
	}
}

func TestAnUnopenableCatalogFails(t *testing.T) {
	d := wired(t)
	d.Catalog = func() ([]string, error) { return nil, errors.New("database is locked") }
	wantStatus(t, run(t, d), "catalog", StatusFail)
}

// Every check runs even when an earlier one failed: the point of a doctor is
// to hand back the LIST, not one problem per run.
func TestNoFailureAbortsTheRestOfThePass(t *testing.T) {
	d := wired(t)
	d.Catalog = func() ([]string, error) { return nil, errors.New("boom") }
	d.Public.List = func(context.Context, string) ([]string, error) { return nil, errors.New("boom") }
	r := run(t, d)
	if len(r.Checks) != 8 {
		t.Fatalf("the pass reported %d checks, want all 8", len(r.Checks))
	}
	if r.Failures() != 2 {
		t.Errorf("the pass reported %d failures, want 2: %+v", r.Failures(), r.Checks)
	}
	wantStatus(t, r, "github", StatusOK)
}

// The stat seam exists so a check can be exercised against a filesystem the
// test does not have to own — a read-only mount, another account's tree.
func TestTheFilesystemIsASeam(t *testing.T) {
	d := wired(t)
	d.Stat = func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }
	wantStatus(t, run(t, d), "data-dir", StatusFail)
	wantStatus(t, run(t, d), "secret-key", StatusFail)
}

// R2's S3 endpoint answers an unsigned GET with 400 before it looks at the
// bucket; that is a refusal, and the doctor must read it as one rather than
// failing every R2 deployment as "inconclusive".
func TestDoctorStagingPrivateReads400AsARefusal(t *testing.T) {
	d := wired(t)
	d.AnonGet = func(context.Context, string, string) (int, error) { return 400, nil }
	wantStatus(t, run(t, d), "staging-private", StatusOK)
}
