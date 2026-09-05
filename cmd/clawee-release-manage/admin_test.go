package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

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
