// Package doctor is the pre-flight for a manage deployment: every assumption
// the service makes about its host, its buckets and its token, checked once,
// by name, before an operator promotes anything.
//
// It exists because every one of these failures is silent until the moment it
// is expensive. A staging bucket that answers an unauthenticated GET has been
// publishing every staged build since the day it was created, and nothing in
// the service can tell — promote works, the pages render, the catalog is
// right. A GitHub token without release scope looks fine until the middle of
// a promote, after the bytes are already copied. A secret key at 0644 is a
// working service whose second factors are readable.
//
// Two rules shape the design:
//
//	EVERY check is behind a seam, so the suite proves each FAILURE path
//	without a bucket, a token, or a host. A doctor whose checks can only be
//	exercised against live infrastructure is a doctor nobody trusts.
//
//	NOTHING here writes. Not a probe object, not a draft release, not a
//	touched file. The write scope of this whole package is zero, which is
//	what makes it safe to run against production from a session — the one
//	sanctioned thing to do on a production host is look (release.md §11).
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/clawee-git/release/internal/manage/catalog"
)

// Status is a check's outcome. Three, not two: a check that could not run —
// no kit checkout on this host, a store the deployment has not wired yet — is
// NOT a pass, and reporting it as one is how a green doctor comes to mean
// nothing. It is not a failure either, so it does not fail the exit status.
type Status string

const (
	StatusOK      Status = "ok"
	StatusFail    Status = "fail"
	StatusSkipped Status = "skipped"
)

// Result is one named check. Name is stable — it is what an operator greps a
// log for and what the runbook tells them to look at.
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Report is the whole pass.
type Report struct {
	Checks []Result `json:"checks"`
}

// Failed reports whether any check failed. A skip is not a failure.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			return true
		}
	}
	return false
}

// Failures counts them, for the one-line summary.
func (r Report) Failures() int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			n++
		}
	}
	return n
}

// Bucket is one object store, named. The name is carried because every
// message about a bucket has to say WHICH bucket — "reachable" against the
// wrong one is the failure this whole package exists to catch.
type Bucket struct {
	Name string
	List func(ctx context.Context, prefix string) ([]string, error)
}

// RepoInfo is the little of GitHub doctor needs.
type RepoInfo struct {
	FullName string
	// CanPublishReleases is whether the token's permissions on this repo
	// include creating a release. It is READ from the repo metadata; nothing
	// here creates one to find out, because a draft release created by a
	// health check is a publication nobody approved.
	CanPublishReleases bool
}

// Deps are the seams. A nil func is "not wired", and its check reports
// skipped rather than failing: the service is designed to be brought up in
// stages, and a doctor that failed on a stage the operator has not reached
// yet would train them to ignore it.
type Deps struct {
	// Staging and Public are the two buckets, listed with credentials.
	Staging *Bucket
	Public  *Bucket

	// AnonGet performs an UNAUTHENTICATED GET against a bucket's object
	// endpoint and returns the HTTP status. It is a separate seam from
	// Staging on purpose: the whole question is what the bucket answers to a
	// caller holding nothing, so it must not be able to reuse the credentials
	// the rest of the pass carries.
	AnonGet func(ctx context.Context, bucket, key string) (int, error)

	// Repo reads the release repository's metadata.
	Repo func(ctx context.Context) (RepoInfo, error)
	// CheckWrite additionally requires that the token could publish a
	// release. Off by default because it is a question about scope, and an
	// operator running doctor to debug a page has no reason to care.
	CheckWrite bool

	// Catalog opens the catalog and returns the applied migration ledger.
	Catalog func() ([]string, error)
	// WantMigrations is how many rungs this binary carries.
	WantMigrations int

	// DataDir and SecretKeyPath are checked for mode and owner.
	DataDir       string
	SecretKeyPath string

	// KitRoot is a release-kit checkout, when one is on this host. The
	// signing key is compared against it.
	KitRoot string
	// EmbeddedKey is the minisign key line this binary bakes in.
	EmbeddedKey string

	// Stat and ReadFile are the filesystem, injected so the mode and key
	// checks are testable without a fixture that a umask can change under.
	Stat     func(string) (fs.FileInfo, error)
	ReadFile func(string) ([]byte, error)
	// UID is the account the SERVICE runs as — not the account running this
	// command. The two differ in exactly the cases that matter: under `sudo`
	// a correctly owned data dir would fail against the invoking uid, and as
	// root against a root-owned tree the check would pass while saying
	// nothing. The caller resolves the service user and puts its uid here.
	UID int
	// UIDLabel names that account in the report, so a pass says WHICH uid it
	// compared against. A check whose subject is invisible is a check an
	// operator cannot tell apart from a vacuous one.
	UIDLabel string
}

// Run executes every check IN ORDER and returns the report. The order is the
// order an operator reads them in: the local host first (nothing external can
// be diagnosed while the catalog is broken), then the buckets, then GitHub.
//
// No check aborts the pass. A doctor that stopped at the first failure would
// hand back one problem per run, and the whole point is to hand back the list.
func Run(ctx context.Context, d Deps) Report {
	if d.Stat == nil {
		d.Stat = os.Stat
	}
	if d.ReadFile == nil {
		d.ReadFile = os.ReadFile
	}
	if d.UIDLabel == "" {
		d.UIDLabel = fmt.Sprintf("uid %d", d.UID)
	}
	var r Report
	for _, check := range []func(context.Context, Deps) Result{
		checkCatalog,
		checkDataDir,
		checkSecretKey,
		checkReleaseKey,
		checkStagingReachable,
		checkStagingPrivate,
		checkPublicReachable,
		checkGitHub,
	} {
		r.Checks = append(r.Checks, check(ctx, d))
	}
	return r
}

func ok(name, format string, a ...any) Result {
	return Result{Name: name, Status: StatusOK, Detail: fmt.Sprintf(format, a...)}
}

func fail(name, format string, a ...any) Result {
	return Result{Name: name, Status: StatusFail, Detail: fmt.Sprintf(format, a...)}
}

func skip(name, format string, a ...any) Result {
	return Result{Name: name, Status: StatusSkipped, Detail: fmt.Sprintf(format, a...)}
}

// checkCatalog reads the ledger and compares the ledger against the rungs
// this binary carries. A host whose ledger is SHORTER than the binary is one
// where a migration has not run; a host whose ledger is LONGER is running an
// older binary than the catalog was migrated by, which is the direction that
// silently drops columns a newer schema filled (migrations.md).
func checkCatalog(_ context.Context, d Deps) Result {
	const name = "catalog"
	if d.Catalog == nil {
		return skip(name, "no catalog opener wired")
	}
	// The opener behind this seam is READ-ONLY and does not migrate. Through
	// the ordinary one, the branch below could never fire: Open applies the
	// pending rungs before returning, so a doctor wired to it fixed the state
	// it was asked to report and created a catalog for a mistyped data dir.
	applied, err := d.Catalog()
	if err != nil {
		return fail(name, "cannot read the catalog: %v", err)
	}
	switch {
	case len(applied) < d.WantMigrations:
		return fail(name, "%d of %d migrations applied; this binary carries rungs the catalog has not run",
			len(applied), d.WantMigrations)
	case len(applied) > d.WantMigrations:
		return fail(name, "the catalog has %d migrations and this binary knows %d; it was migrated by a NEWER build than the one running",
			len(applied), d.WantMigrations)
	}
	return ok(name, "%d migrations applied, ledger current", len(applied))
}

// checkDataDir is the root the service writes. Group or world access to it is
// access to the catalog file, which holds every sealed second factor and every
// session.
func checkDataDir(_ context.Context, d Deps) Result {
	const name = "data-dir"
	if d.DataDir == "" {
		return skip(name, "no data dir given")
	}
	info, err := d.Stat(d.DataDir)
	if err != nil {
		return fail(name, "%s: %v", d.DataDir, err)
	}
	if !info.IsDir() {
		return fail(name, "%s is not a directory", d.DataDir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fail(name, "%s is mode %04o; it holds the catalog, so anything readable by group or other is a copy of every sealed second factor", d.DataDir, perm)
	}
	if err := ownedBy(info, d.UID, d.UIDLabel); err != nil {
		return fail(name, "%s %v", d.DataDir, err)
	}
	return ok(name, "%s mode %04o, owned by %s", d.DataDir, info.Mode().Perm(), d.UIDLabel)
}

// checkSecretKey is the stricter twin: the key that seals the TOTP secrets is
// refused by the service itself at any mode another user can read, so a
// mismatch here is a service that will not start — worth saying before the
// restart rather than after it.
func checkSecretKey(_ context.Context, d Deps) Result {
	const name = "secret-key"
	if d.SecretKeyPath == "" {
		return skip(name, "no secret key path given")
	}
	info, err := d.Stat(d.SecretKeyPath)
	if errors.Is(err, fs.ErrNotExist) {
		return fail(name, "%s does not exist; it is created on the first `admin add` or the first start, so the service has not run here yet", d.SecretKeyPath)
	}
	if err != nil {
		return fail(name, "%s: %v", d.SecretKeyPath, err)
	}
	if !info.Mode().IsRegular() {
		return fail(name, "%s is not a regular file", d.SecretKeyPath)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return fail(name, "%s is mode %04o, want 0600; the service refuses to start at any mode another user can read", d.SecretKeyPath, perm)
	}
	if err := ownedBy(info, d.UID, d.UIDLabel); err != nil {
		return fail(name, "%s %v", d.SecretKeyPath, err)
	}
	return ok(name, "%s mode 0600, owned by %s", d.SecretKeyPath, d.UIDLabel)
}

// ownedBy compares the file's owner with the account the service runs as.
// It reports "unknown" rather than failing where the platform does not carry
// a uid: a check that cannot be made is not a check that failed.
func ownedBy(info fs.FileInfo, uid int, label string) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != uid {
		return fmt.Errorf("is owned by uid %d, but the service runs as %s", st.Uid, label)
	}
	return nil
}

// checkReleaseKey compares three copies of the signing public key: the one
// this binary bakes in (which is what verifies a cut's registration), the kit
// checkout's `clawee-release.pub` (which is what the host serves for manual
// verification), and the one baked into each generated bootstrap (which is
// what every `curl … | sh` trusts).
//
// They are three copies of a trust anchor. A rotation that updates two of them
// leaves the third rejecting — or, worse, trusting — signatures the others do
// not, and every symptom of that appears somewhere else: a cut that cannot
// register, an installer that refuses a good download.
func checkReleaseKey(_ context.Context, d Deps) Result {
	const name = "release-key"
	if d.EmbeddedKey == "" {
		return fail(name, "this binary bakes in no release key")
	}
	if d.KitRoot == "" {
		return skip(name, "no --kit-root given; the baked key is %s… and nothing on this host was compared against it", short(d.EmbeddedKey))
	}
	pubPath := filepath.Join(d.KitRoot, "clawee-release.pub")
	raw, err := d.ReadFile(pubPath)
	if err != nil {
		return fail(name, "%s: %v", pubPath, err)
	}
	if line := keyLine(string(raw)); line != d.EmbeddedKey {
		return fail(name, "%s carries a DIFFERENT key than this binary bakes in (%s… vs %s…)",
			pubPath, short(line), short(d.EmbeddedKey))
	}
	for _, comp := range catalog.Components {
		boot := filepath.Join(d.KitRoot, comp, "install.sh")
		body, err := d.ReadFile(boot)
		if err != nil {
			return fail(name, "%s: %v", boot, err)
		}
		if !strings.Contains(string(body), d.EmbeddedKey) {
			return fail(name, "%s does not bake in this key; a bootstrap and the service trust different signatures", boot)
		}
	}
	return ok(name, "the baked key, %s and every bootstrap agree", pubPath)
}

func keyLine(contents string) string {
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "untrusted comment:") {
			continue
		}
		return line
	}
	return ""
}

// short is the identifying prefix of a key line. Never the whole thing in an
// error: it is public, but a truncated form is what a human actually compares.
func short(line string) string {
	if len(line) > 12 {
		return line[:12]
	}
	return line
}

func checkStagingReachable(ctx context.Context, d Deps) Result {
	return checkBucket(ctx, "staging-bucket", d.Staging)
}

func checkPublicReachable(ctx context.Context, d Deps) Result {
	return checkBucket(ctx, "public-bucket", d.Public)
}

func checkBucket(ctx context.Context, name string, b *Bucket) Result {
	if b == nil || b.List == nil {
		return skip(name, "not configured")
	}
	keys, err := b.List(ctx, "")
	if err != nil {
		return fail(name, "%s is not reachable with these credentials: %v", b.Name, err)
	}
	return ok(name, "%s reachable, %d objects listed", b.Name, len(keys))
}

// checkStagingPrivate is the check this whole verb was worth writing for.
//
// The staging bucket holds every build that has been cut and not promoted.
// If it answers an unauthenticated GET, then everything an operator has NOT
// approved is already downloadable by anyone who can guess a key — and the
// staging/public split, which is the entire publication control in this
// product, has been decorative since the day the bucket was created. Nothing
// else in the system can notice: promote works, the pages are right, the
// catalog is right.
//
// So a 2xx here is a LOUD failure, not a warning, and the check probes a REAL
// object where one exists: a 404 for a key that was never there proves
// nothing about the keys that are.
func checkStagingPrivate(ctx context.Context, d Deps) Result {
	const name = "staging-private"
	if d.Staging == nil || d.AnonGet == nil {
		return skip(name, "not configured")
	}
	key := "clawee-doctor-probe/does-not-exist"
	real := false
	if d.Staging.List != nil {
		if keys, err := d.Staging.List(ctx, ""); err == nil && len(keys) > 0 {
			key, real = keys[0], true
		}
	}
	status, err := d.AnonGet(ctx, d.Staging.Name, key)
	if err != nil {
		// A transport failure is not proof of privacy — it is the absence of
		// an answer, and reporting it as a pass would be the same lie as a
		// 200 reported as a pass.
		return fail(name, "could not probe %s anonymously: %v", d.Staging.Name, err)
	}
	switch {
	case status >= 200 && status < 400:
		return fail(name, "%s SERVED %q to an unauthenticated GET (HTTP %d). Every staged, unpromoted build in this bucket is public. Remove the bucket's public access and any custom domain before promoting anything",
			d.Staging.Name, key, status)
	case status == 400 || status == 401 || status == 403 || status == 404:
		// 400 is R2's S3 endpoint refusing an UNSIGNED request outright
		// (InvalidArgument, before any bucket policy is consulted). It is a
		// refusal, not a miss. Note what this probe cannot see: R2 serves
		// public access through r2.dev or a custom domain, never through
		// this endpoint, so a bucket with either enabled still answers 400
		// here. The dashboard is the authority on those two switches.
		if !real {
			return ok(name, "%s answers HTTP %d anonymously — but it is empty, so the probe used a key that never existed and the check is weaker than it looks",
				d.Staging.Name, status)
		}
		return ok(name, "%s answers HTTP %d to an unauthenticated GET of a real object", d.Staging.Name, status)
	default:
		return fail(name, "%s answered HTTP %d to an unauthenticated GET; that is neither a refusal nor a miss, so it cannot be read as private",
			d.Staging.Name, status)
	}
}

// checkGitHub reads the release repo. With CheckWrite it additionally asks
// whether the token COULD publish a release — read from the repo's
// permissions, never by creating one: a draft release minted by a health
// check is a publication nobody approved, and deleting it afterwards is a
// second write to cover the first.
func checkGitHub(ctx context.Context, d Deps) Result {
	const name = "github"
	if d.Repo == nil {
		return skip(name, "not configured")
	}
	info, err := d.Repo(ctx)
	if err != nil {
		return fail(name, "the token cannot read the release repo: %v", err)
	}
	if d.CheckWrite && !info.CanPublishReleases {
		return fail(name, "the token reads %s but its permissions do not include publishing a release; promote would fail after the bytes are already copied", info.FullName)
	}
	if d.CheckWrite {
		return ok(name, "%s readable, and the token's permissions include publishing a release", info.FullName)
	}
	return ok(name, "%s readable (--check-write also asks whether a release could be published)", info.FullName)
}
