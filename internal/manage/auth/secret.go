package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// secretKeyLen is the size of the service's root secret. Everything the
// service seals is derived from it, so it is generated once and never rotated
// in place: rotating it invalidates every enrolled TOTP secret, which is an
// operator decision (remove and re-add the accounts), not a startup behaviour.
const secretKeyLen = 32

// SecretKeyFile is the default filename of the root secret under --data-dir.
const SecretKeyFile = "secret.key"

// Sealer encrypts at-rest secrets — today only enrolled TOTP secrets.
//
// The point is narrow and worth stating: the catalog file is a single SQLite
// database an operator will back up, copy to a laptop to inspect, and hand to
// a support process. Plaintext TOTP secrets in it would make every one of
// those copies a working set of second factors. The key lives in a separate
// mode-0600 file that is deliberately NOT part of the catalog, so a copy of
// the database alone is inert.
type Sealer struct {
	aead cipher.AEAD
}

// LoadSealer reads (creating on first run) the root secret at path and returns
// a Sealer over it.
//
// path is a FLAG-STEERED root, so it is validated here, at its own writer
// (privilege.md): absolute, lexically clean, a regular file, opened with
// O_NOFOLLOW so no component of the final element is a symlink into somewhere
// else, and refused outright when its mode lets anyone but the owner read it.
// A key file the service will happily read at mode 0644 is a key file that has
// already leaked.
func LoadSealer(path string) (*Sealer, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("secret key path %q must be absolute", path)
	}
	if filepath.Clean(path) != path {
		return nil, fmt.Errorf("secret key path %q is not clean; write it as %q", path, filepath.Clean(path))
	}

	key, err := readSecretKey(path)
	if err != nil {
		return nil, err
	}
	// Domain separation: the root secret is the input, never the AES key
	// itself, so a second at-rest use added later gets its own derived key
	// rather than sharing this one.
	aesKey, err := hkdf.Key(sha256.New, key, nil, "clawee-manage/totp-secret/v1", 32)
	if err != nil {
		return nil, fmt.Errorf("derive sealing key: %w", err)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("sealing cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("sealing mode: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

func readSecretKey(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return createSecretKey(path)
		}
		// ELOOP here means the path IS a symlink — a distinct and alarming
		// condition worth naming rather than reporting as a generic open error.
		return nil, fmt.Errorf("open secret key %q (a symlink here is refused deliberately): %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat secret key %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("secret key %q is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("secret key %q is mode %04o; it must not be readable by group or other — run: chmod 600 %s", path, perm, path)
	}
	key := make([]byte, secretKeyLen+1)
	n, err := f.Read(key)
	if err != nil && n == 0 {
		return nil, fmt.Errorf("read secret key %q: %w", path, err)
	}
	if n != secretKeyLen {
		return nil, fmt.Errorf("secret key %q is %d bytes, want exactly %d — it was not written by this service", path, n, secretKeyLen)
	}
	return key[:secretKeyLen], nil
}

// createSecretKey mints the root secret on first run. O_EXCL means two
// services racing the same data dir cannot both create it and end up with
// different keys over the same catalog; the loser reports the collision.
func createSecretKey(path string) ([]byte, error) {
	key := make([]byte, secretKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, fs.FileMode(0o600))
	if err != nil {
		return nil, fmt.Errorf("create secret key %q: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(key); err != nil {
		return nil, fmt.Errorf("write secret key %q: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("sync secret key %q: %w", path, err)
	}
	return key, nil
}

// Seal encrypts plaintext. The random nonce is prefixed to the ciphertext, so
// a sealed value is one opaque blob the store writes without knowing its shape.
func (s *Sealer) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts a value produced by Seal. A failure means the key file does
// not match the catalog — say that, because the alternative reading ("the
// account is broken") sends an operator to re-enrol accounts that are fine.
func (s *Sealer) Open(sealed []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, fmt.Errorf("open: sealed value is %d bytes, shorter than a nonce", len(sealed))
	}
	out, err := s.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return nil, fmt.Errorf("open: sealed value does not decrypt — the secret key file does not match this catalog: %w", err)
	}
	return out, nil
}
