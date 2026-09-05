package auth

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretKeyIsCreatedLockedDownAndStable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SecretKeyFile)

	s1, err := LoadSealer(path)
	if err != nil {
		t.Fatalf("LoadSealer (create): %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret key created mode %04o, want 0600", info.Mode().Perm())
	}
	if info.Size() != secretKeyLen {
		t.Fatalf("secret key is %d bytes, want %d", info.Size(), secretKeyLen)
	}

	sealed, err := s1.Seal([]byte("the totp secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, []byte("the totp secret")) {
		t.Fatal("the plaintext is present in the sealed value")
	}

	// A second load of the same file opens what the first sealed: the key is
	// not regenerated on restart, or every enrolment would break on a reboot.
	s2, err := LoadSealer(path)
	if err != nil {
		t.Fatalf("LoadSealer (reopen): %v", err)
	}
	got, err := s2.Open(sealed)
	if err != nil || string(got) != "the totp secret" {
		t.Fatalf("Open after reopen = %q, %v", got, err)
	}
}

func TestSealedValueDoesNotOpenUnderADifferentKey(t *testing.T) {
	a, err := LoadSealer(filepath.Join(t.TempDir(), SecretKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadSealer(filepath.Join(t.TempDir(), SecretKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := a.Seal([]byte("secret"))
	// The failure names the real cause — a mismatched key file, not a broken
	// account — because the wrong reading sends an operator re-enrolling.
	_, err = b.Open(sealed)
	if err == nil || !strings.Contains(err.Error(), "does not match this catalog") {
		t.Fatalf("cross-key Open: err = %v", err)
	}
}

func TestSecretKeyPathIsValidatedAtItsWriter(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadSealer("relative/secret.key"); err == nil {
		t.Fatal("a relative secret key path was accepted")
	}
	// Built by concatenation, not filepath.Join: Join cleans, and the point is
	// that a path carrying a ".." component never reaches the open.
	if _, err := LoadSealer(dir + "/../" + filepath.Base(dir) + "/" + SecretKeyFile); err == nil {
		t.Fatal("an unclean secret key path was accepted")
	}

	// A symlink is refused rather than followed: the whole point of the mode
	// check is defeated if the path can point somewhere else.
	real := filepath.Join(dir, "real.key")
	if err := os.WriteFile(real, bytes.Repeat([]byte{7}, secretKeyLen), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSealer(link); err == nil {
		t.Fatal("a symlinked secret key was followed")
	}

	// A world-readable key file has already leaked; reading it anyway would
	// pretend otherwise.
	loose := filepath.Join(dir, "loose.key")
	if err := os.WriteFile(loose, bytes.Repeat([]byte{7}, secretKeyLen), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSealer(loose)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable key: err = %v, want a refusal naming the fix", err)
	}

	// A file of the wrong length was not written by this service; guessing
	// what it means is worse than saying so.
	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSealer(short); err == nil {
		t.Fatal("a short secret key was accepted")
	}

	// A LONGER file is equally not ours: taking its first 32 bytes would
	// silently derive a key from an unrelated file.
	long := filepath.Join(dir, "long.key")
	if err := os.WriteFile(long, bytes.Repeat([]byte{7}, secretKeyLen+8), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSealer(long); err == nil {
		t.Fatal("an over-long secret key was accepted")
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	h, err := HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(h, "correct-horse") {
		t.Fatal("the password is present in its own hash")
	}
	ok, err := VerifyPassword(h, "correct-horse-battery")
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(right) = %v, %v", ok, err)
	}
	ok, err = VerifyPassword(h, "correct-horse-batterz")
	if err != nil || ok {
		t.Fatalf("VerifyPassword(wrong) = %v, %v", ok, err)
	}
	// Two hashes of one password differ: the salt is real.
	h2, _ := HashPassword("correct-horse-battery")
	if h == h2 {
		t.Fatal("two hashes of the same password are identical — no salt")
	}
	// A hand-edited admins row is reported, not silently treated as a
	// mismatch: the operator who edited it needs to know.
	if _, err := VerifyPassword("not-a-hash", "x"); err == nil {
		t.Fatal("a malformed stored hash verified without complaint")
	}
	if _, err := HashPassword(""); err == nil {
		t.Fatal("an empty password hashed")
	}
}
