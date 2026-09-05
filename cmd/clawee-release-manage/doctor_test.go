package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawee-git/release/internal/manage/auth"
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
	return dir
}

// Exit 0 with the stores unwired: the local half is checked, the rest is
// reported as skipped, and a host being brought up in stages is not red.
func TestDoctorExitsZeroOnAProvisionedHostWithNoStoresYet(t *testing.T) {
	stdout, stderr, code := exec(t, "doctor", "--data-dir", dataDirWithKey(t))
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
	dir := t.TempDir() // no secret key
	stdout, _, code := exec(t, "doctor", "--data-dir", dir)
	if code != 1 {
		t.Fatalf("doctor exited %d, want 1:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "checks failed") {
		t.Errorf("the report does not summarise the failures:\n%s", stdout)
	}
}

func TestDoctorJSONIsParseableAndCarriesEveryCheck(t *testing.T) {
	stdout, _, code := exec(t, "doctor", "--data-dir", dataDirWithKey(t), "--json")
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
		"--staging-bucket", "staging-example")
	if code != exitUsage {
		t.Fatalf("exited %d, want %d: %s", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "r2-account") {
		t.Errorf("the refusal does not name the missing flag: %s", stderr)
	}
}
