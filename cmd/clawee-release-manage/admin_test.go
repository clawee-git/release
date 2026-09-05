package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawee-git/release/internal/manage/auth"
	"github.com/clawee-git/release/internal/manage/store"
)

// execStdin is exec with something on standard input, for --password-stdin.
func execStdin(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(&env{stdout: &out, stderr: &errb, stdin: strings.NewReader(stdin)}, args)
	return out.String(), errb.String(), code
}

func TestAdminAddListRemoveRoundTrip(t *testing.T) {
	dir := t.TempDir()

	out, errb, code := execStdin(t, "correct-horse-battery\n",
		"admin", "add", "ada", "--data-dir", dir, "--password-stdin")
	if code != 0 {
		t.Fatalf("admin add: code %d, stderr %q", code, errb)
	}
	if !strings.Contains(out, `admin "ada" added`) {
		t.Fatalf("admin add said: %q", out)
	}

	out, _, code = exec(t, "admin", "list", "--data-dir", dir)
	if code != 0 || !strings.Contains(out, "ada") || !strings.Contains(out, "pending") {
		t.Fatalf("admin list: code %d, out %q", code, out)
	}

	// The password reached the store as an argon2id hash, and the account is
	// usable: the CLI and the login path agree on the format.
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, err := st.Admin("ada")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.PasswordHash, "$argon2id$") {
		t.Fatalf("stored hash = %q", a.PasswordHash)
	}
	ok, err := auth.VerifyPassword(a.PasswordHash, "correct-horse-battery")
	if err != nil || !ok {
		t.Fatalf("the stored hash does not verify the password given: %v, %v", ok, err)
	}

	// A duplicate is a plain failure (exit 1), not a usage error: the command
	// ran, understood itself, and could not do the thing.
	_, errb, code = execStdin(t, "correct-horse-battery\n",
		"admin", "add", "ada", "--data-dir", dir, "--password-stdin")
	if code != 1 || !strings.Contains(errb, "already exists") {
		t.Fatalf("duplicate add: code %d, stderr %q", code, errb)
	}

	_, errb, code = exec(t, "admin", "remove", "bob", "--data-dir", dir)
	if code != 1 || !strings.Contains(errb, "no admin named") {
		t.Fatalf("remove unknown: code %d, stderr %q", code, errb)
	}
	out, errb, code = exec(t, "admin", "remove", "ada", "--data-dir", dir)
	if code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("remove: code %d, stderr %q", code, errb)
	}
	out, _, _ = exec(t, "admin", "list", "--data-dir", dir)
	if !strings.Contains(out, "no admins") {
		t.Fatalf("list after remove: %q", out)
	}
}

func TestAdminAddRefusesAMalformedNameAsAUsageError(t *testing.T) {
	dir := t.TempDir()
	_, errb, code := execStdin(t, "correct-horse-battery\n",
		"admin", "add", "Ada Lovelace", "--data-dir", dir, "--password-stdin")
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errb, "may only contain lowercase") || !strings.Contains(errb, "Usage:") {
		t.Fatalf("stderr = %q", errb)
	}
}

func TestAdminAddRefusesAShortPassword(t *testing.T) {
	dir := t.TempDir()
	_, errb, code := execStdin(t, "short\n", "admin", "add", "ada", "--data-dir", dir, "--password-stdin")
	if code != 1 || !strings.Contains(errb, "at least 12 characters") {
		t.Fatalf("code %d, stderr %q", code, errb)
	}
}

// The CLI creates the secret key on first use, with the mode the sealer
// demands — so `serve` does not meet a key file it must refuse.
func TestAdminAddCreatesTheSecretKeyLockedDown(t *testing.T) {
	dir := t.TempDir()
	if _, _, code := execStdin(t, "correct-horse-battery\n",
		"admin", "add", "ada", "--data-dir", dir, "--password-stdin"); code != 0 {
		t.Fatal("admin add failed")
	}
	if _, err := auth.LoadSealer(filepath.Join(dir, auth.SecretKeyFile)); err != nil {
		t.Fatalf("the key the CLI created is not one the sealer will load: %v", err)
	}
}

func TestVersionAnswersWithoutACatalog(t *testing.T) {
	out, errb, code := exec(t, "version")
	if code != 0 || errb != "" || !strings.Contains(out, toolName) {
		t.Fatalf("version on a host with no catalog: code %d, out %q, stderr %q", code, out, errb)
	}
	dir := t.TempDir()
	if _, _, code := execStdin(t, "correct-horse-battery\n",
		"admin", "add", "ada", "--data-dir", dir, "--password-stdin"); code != 0 {
		t.Fatal("admin add failed")
	}
	out, _, code = exec(t, "version", "--data-dir", dir)
	if code != 0 || !strings.Contains(out, "migration 1 initial catalog") {
		t.Fatalf("version --data-dir: code %d, out %q", code, out)
	}
}

func TestRetainDryRunReportsWithoutTouchingAnything(t *testing.T) {
	dir := t.TempDir()
	if _, _, code := execStdin(t, "correct-horse-battery\n",
		"admin", "add", "ada", "--data-dir", dir, "--password-stdin"); code != 0 {
		t.Fatal("admin add failed")
	}
	out, errb, code := exec(t, "retain", "--data-dir", dir, "--dry-run")
	if code != 0 {
		t.Fatalf("retain --dry-run: code %d, stderr %q", code, errb)
	}
	// An empty catalog reports nothing and, above all, changes nothing.
	if strings.Contains(out, "EXPIRE") {
		t.Fatalf("a dry run over an empty catalog expired something:\n%s", out)
	}
}

// A half-configured store is refused rather than silently disabled: an
// operator who spelled one flag and forgot its partner meant to have it.
func TestRetainRefusesAHalfConfiguredStore(t *testing.T) {
	dir := t.TempDir()
	_, errb, code := exec(t, "retain", "--data-dir", dir, "--staging-bucket", "clawee-staging")
	if code != exitUsage || !strings.Contains(errb, "--r2-account is required") {
		t.Fatalf("code %d, stderr %q", code, errb)
	}

	creds := filepath.Join(dir, "r2.key")
	if err := os.WriteFile(creds,
		[]byte("access_key_id = \"AK\"\nsecret_access_key = \"S\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// One bucket for both is not a configuration, it is a public staging
	// store: everything a cut uploads would be readable before anyone
	// promoted it.
	_, errb, code = exec(t, "retain", "--data-dir", dir, "--r2-account", "acct",
		"--r2-creds", creds, "--staging-bucket", "same", "--public-bucket", "same")
	if code != exitUsage || !strings.Contains(errb, "private by construction") {
		t.Fatalf("same bucket twice: code %d, stderr %q", code, errb)
	}

	_, errb, code = exec(t, "retain", "--data-dir", dir, "--github-repo", "clawee-git/release")
	if code != exitUsage || !strings.Contains(errb, "github-token-file") {
		t.Fatalf("repo without a token: code %d, stderr %q", code, errb)
	}
}

// The seam summary is still what a refusal reports: an operator who runs
// retain on a catalog-only deployment needs to see WHICH store is missing, not
// just that something is.
//
// This replaces an earlier test that asserted retain ran happily with no
// stores at all. That behaviour was the defect: it expired rows it could not
// prune, and expiry is one-way, so their bytes were orphaned for good.
func TestRetainNamesTheMissingSeamsWhenItRefuses(t *testing.T) {
	dir := t.TempDir()
	_, errb, code := exec(t, "retain", "--data-dir", dir)
	if code != 1 {
		t.Fatalf("code %d, want 1", code)
	}
	for _, want := range []string{"staging: ABSENT", "public: ABSENT", "github: ABSENT"} {
		if !strings.Contains(errb, want) {
			t.Errorf("the refusal does not report %q:\n%s", want, errb)
		}
	}
}

// The server must not bound the whole response. Promote's response is a
// progress stream open for the entire publish — four ~11 MB verifies, six
// copies, six GitHub uploads — and a WriteTimeout cut the operator's log off
// mid-publish while the promote carried on invisibly. That is precisely the
// "is it hung?" ambiguity the stream exists to remove, and it is the same
// lesson as the outbound clients', from the server side.
func TestServerDoesNotBoundTheWholeResponse(t *testing.T) {
	intended := func() *http.Server {
		// Built fresh each time: http.Server carries a noCopy, so a struct
		// copy is a vet failure rather than a shortcut.
		return &http.Server{ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	}
	if err := checkServerTimeouts(intended()); err != nil {
		t.Fatalf("the intended configuration was rejected: %v", err)
	}
	withWrite := intended()
	withWrite.WriteTimeout = 60 * time.Second
	if err := checkServerTimeouts(withWrite); err == nil {
		t.Fatal("a WriteTimeout was accepted; it would truncate every long promote stream")
	}
	// …but a client that connects and says nothing is still bounded, or the
	// fix is a leak wearing a fix's name.
	for _, bad := range []*http.Server{
		{IdleTimeout: time.Minute},
		{ReadHeaderTimeout: time.Second},
	} {
		if err := checkServerTimeouts(bad); err == nil {
			t.Fatalf("an unbounded idle/header wait was accepted: %+v", bad)
		}
	}
}

// The real retain pass refuses without the stores, because expiring a row it
// cannot prune orphans the bytes permanently. --dry-run stays usable: it
// touches neither the buckets nor the catalog.
func TestRetainRefusesToExpireWhatItCannotPrune(t *testing.T) {
	dir := t.TempDir()
	_, errb, code := exec(t, "retain", "--data-dir", dir)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	for _, want := range []string{"orphans them permanently", "public: ABSENT", "--dry-run"} {
		if !strings.Contains(errb, want) {
			t.Errorf("the refusal is missing %q:\n%s", want, errb)
		}
	}

	out, errb, code := exec(t, "retain", "--data-dir", dir, "--dry-run")
	if code != 0 {
		t.Fatalf("--dry-run refused too: code %d, stderr %q", code, errb)
	}
	if strings.Contains(out, "orphans") {
		t.Fatalf("--dry-run took the refusal path:\n%s", out)
	}
}

// The dry run reports the plan, marks the current row and yanked rows, and
// changes nothing in the catalog.
func TestRetainDryRunPreviewsThePlan(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i := 1; i <= 3; i++ {
		at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour)
		id, err := st.Stage(store.ReleaseVersion{
			Component: "clawee", Channel: "beta", Version: fmt.Sprintf("0.3.%d", i),
			Stamp:         fmt.Sprintf("v0.3.%d.beta.2026.09.04.%08x", i, i),
			ArtifactsJSON: "[]", CreatedAt: at,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Promote(id, at); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	st.Close()

	out, errb, code := exec(t, "retain", "--data-dir", dir, "--dry-run")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb)
	}
	if !strings.Contains(out, "(current)") {
		t.Errorf("the plan does not mark the current row:\n%s", out)
	}
	if !strings.Contains(out, "EXPIRE") {
		t.Errorf("three beta rows and a keep of 1: something should be expired:\n%s", out)
	}

	// Nothing moved.
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	for _, id := range ids {
		row, err := st2.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State == "expired" {
			t.Fatalf("--dry-run expired row %d", id)
		}
	}
}
