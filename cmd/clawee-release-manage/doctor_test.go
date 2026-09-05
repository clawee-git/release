package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawee-git/release/internal/manage/auth"
	"github.com/clawee-git/release/internal/manage/store"
	_ "modernc.org/sqlite" // the fixture below edits the ledger directly
)

// dataDirWithKey is a host that has been provisioned but has no stores wired
// yet — the state the runbook has an operator run `doctor` in first.
func dataDirWithKey(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, auth.SecretKeyFile), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The catalog is created by `admin add` or the first start, which is why
	// the runbook runs doctor after provisioning — doctor itself opens it
	// read-only and creates nothing.
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	return dir
}

// currentUserName is the account this test process runs as: the one user that
// is certain to resolve on any host the suite runs on.
func currentUserName(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skipf("no current user: %v", err)
	}
	return u.Username
}

// Exit 0 with the stores unwired: the local half is checked, the rest is
// reported as skipped, and a host being brought up in stages is not red.
func TestDoctorExitsZeroOnAProvisionedHostWithNoStoresYet(t *testing.T) {
	stdout, stderr, code := exec(t, "doctor", "--data-dir", dataDirWithKey(t), "--user", currentUserName(t))
	if code != 0 {
		t.Fatalf("doctor exited %d: %s\n%s", code, stdout, stderr)
	}
	for _, want := range []string{"catalog", "data-dir", "secret-key", "staging-private"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report does not name the %s check:\n%s", want, stdout)
		}
	}
}

// Exit 1, not 2: the checks ran and the deployment is wrong, which is not the
// same thing as a mistyped invocation.
func TestDoctorExitsOneWhenACheckFails(t *testing.T) {
	dir := t.TempDir() // no secret key, no catalog
	stdout, _, code := exec(t, "doctor", "--data-dir", dir, "--user", currentUserName(t))
	if code != 1 {
		t.Fatalf("doctor exited %d, want 1:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "checks failed") {
		t.Errorf("the report does not summarise the failures:\n%s", stdout)
	}
}

func TestDoctorJSONIsParseableAndCarriesEveryCheck(t *testing.T) {
	stdout, _, code := exec(t, "doctor", "--data-dir", dataDirWithKey(t), "--user", currentUserName(t), "--json")
	if code != 0 {
		t.Fatalf("doctor --json exited %d:\n%s", code, stdout)
	}
	var report struct {
		Checks []struct {
			Name, Status, Detail string
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("doctor --json is not JSON: %v\n%s", err, stdout)
	}
	if len(report.Checks) != 8 {
		t.Errorf("doctor --json reported %d checks, want 8", len(report.Checks))
	}
	for _, c := range report.Checks {
		if c.Name == "" || c.Status == "" {
			t.Errorf("a check has no name or status: %+v", c)
		}
	}
}

// Exit 2 is the tool's ONE typo status, and a missing --data-dir is an
// invocation-shape error exactly as a bad flag is.
func TestDoctorRefusesWithoutADataDir(t *testing.T) {
	_, stderr, code := exec(t, "doctor")
	if code != exitUsage {
		t.Fatalf("doctor with no --data-dir exited %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "--data-dir is required") {
		t.Errorf("the refusal does not name the flag: %s", stderr)
	}
}

// A half-configured store is a usage error here for the same reason it is one
// in `serve`: an operator who spelled one flag and forgot its partner meant to
// have that store, and a doctor that quietly skipped it would report a green
// pass for a deployment that cannot promote.
func TestDoctorRefusesAHalfConfiguredStore(t *testing.T) {
	_, stderr, code := exec(t, "doctor", "--data-dir", dataDirWithKey(t),
		"--user", currentUserName(t), "--staging-bucket", "staging-example")
	if code != exitUsage {
		t.Fatalf("exited %d, want %d: %s", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "r2-account") {
		t.Errorf("the refusal does not name the missing flag: %s", stderr)
	}
}

// The catalog check goes through the REAL opener, and this is what it must do
// with a data dir that has no catalog: fail, name the path, and leave nothing
// behind. Through the migrating opener it did the opposite — sqlite created
// catalog.db for the mistyped path and doctor reported ✓ for a deployment that
// did not exist a second earlier.
func TestDoctorDoesNotCreateACatalogForAMistypedDataDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, auth.SecretKeyFile), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := exec(t, "doctor", "--data-dir", dir, "--user", currentUserName(t))
	if code != 1 {
		t.Fatalf("doctor exited %d for a data dir with no catalog, want 1:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "cannot read the catalog") {
		t.Errorf("the report does not say the catalog is unreadable:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, store.DBFile)); !os.IsNotExist(err) {
		t.Fatalf("doctor CREATED %s; it must open the catalog read-only", store.DBFile)
	}
}

// Which account was compared is part of the answer. Against a user that does
// not exist on this host the check still runs — against the invoking account —
// and every line says that is what happened, so a vacuous pass cannot be
// mistaken for a real one.
func TestDoctorNamesTheAccountItComparedAgainst(t *testing.T) {
	dir := dataDirWithKey(t)

	stdout, _, _ := exec(t, "doctor", "--data-dir", dir, "--user", currentUserName(t))
	if !strings.Contains(stdout, currentUserName(t)+" (uid ") {
		t.Errorf("the report does not name the service account it compared:\n%s", stdout)
	}

	stdout, _, _ = exec(t, "doctor", "--data-dir", dir, "--user", "no-such-account-here")
	if !strings.Contains(stdout, "NOT the service account") {
		t.Errorf("an unresolvable --user is not disclosed:\n%s", stdout)
	}
}

// The behind-the-ledger case, over the REAL read-only opener rather than a
// fake, because that is the case the migrating opener made unobservable: Open
// applied the missing rung on its way past, so the check could only ever see a
// current ledger and its "behind" branch was dead code through real wiring.
//
// A rung is removed from the LEDGER rather than the schema, which is the
// honest shape: what the check reads is the ledger, and a host that ran an
// older binary has exactly this — fewer recorded rungs than the binary carries.
func TestDoctorReportsALedgerBehindTheBinaryAndDoesNotMigrateIt(t *testing.T) {
	dir := dataDirWithKey(t)
	path := filepath.Join(dir, store.DBFile)

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM migrations WHERE version = (SELECT MAX(version) FROM migrations)`); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := db.QueryRow(`SELECT COUNT(*) FROM migrations`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if before != store.LatestMigration()-1 {
		t.Fatalf("the fixture has %d rungs, want %d — it is not one behind", before, store.LatestMigration()-1)
	}

	stdout, _, code := exec(t, "doctor", "--data-dir", dir, "--user", currentUserName(t))
	if code != 1 {
		t.Fatalf("doctor exited %d for a catalog one rung behind, want 1:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "has not run") {
		t.Errorf("the report does not say the catalog is behind the binary:\n%s", stdout)
	}

	// And it REPORTED the state rather than repairing it: a doctor that
	// migrates what it inspects turns a read into a deployment change, and the
	// operator never learns the host was behind.
	ro, err := store.OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("re-open read-only: %v", err)
	}
	defer ro.Close()
	after, err := ro.AppliedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != before {
		t.Errorf("doctor changed the ledger: %d rungs before, %d after", before, len(after))
	}
}
