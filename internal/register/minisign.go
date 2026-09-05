// Package register builds, signs and POSTs the catalog row for a staged cut.
//
// The release kit holds no admin credential (release-management.md §6): the
// manage service authenticates a cut by the SAME Ed25519 key that signs
// SHA256SUMS.txt, whose public half the service already bakes in. This file is
// the key half — reading that private key out of the minisign secret-key file
// the cut already decrypts, so no second key, key format or key location is
// introduced by registration.
package register

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// Minisign secret-key layout (untrusted comment line, then one base64 line):
//
//	signature_algorithm  2   "Ed"
//	kdf_algorithm        2   0x0000 when the key has no password
//	cksum_algorithm      2
//	kdf_salt            32
//	kdf_opslimit         8
//	kdf_memlimit         8
//	keynum_sk:  key_id   8
//	            secret  64   the Ed25519 private key (seed || public)
//	            checksum 32
//
// With a password the keynum_sk block is XORed with a scrypt stream and cannot
// be read without it. The release key is password-less by construction — the
// cut signs non-interactively — so a password-protected key here is a
// configuration error to name, not a prompt to raise.
const (
	minisignSecretLen = 158
	kdfOffset         = 2
	keynumOffset      = 54
	keyIDLen          = 8
	secretKeyLen      = 64
)

// SigningKey is the Ed25519 identity read out of a minisign secret-key file.
type SigningKey struct {
	KeyID   string // hex-ish base64 of the 8-byte minisign key id, for messages
	Private ed25519.PrivateKey
}

// LoadSigningKey parses a password-less minisign secret-key file.
func LoadSigningKey(path string) (SigningKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SigningKey{}, fmt.Errorf("read signing key %q: %w", path, err)
	}
	var b64 string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "untrusted comment:") {
			continue
		}
		b64 = line
		break
	}
	if b64 == "" {
		return SigningKey{}, fmt.Errorf("signing key %q: no key line found", path)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return SigningKey{}, fmt.Errorf("signing key %q: not base64: %w", path, err)
	}
	if len(raw) != minisignSecretLen {
		return SigningKey{}, fmt.Errorf("signing key %q: expected %d bytes, got %d", path, minisignSecretLen, len(raw))
	}
	if raw[kdfOffset] != 0 || raw[kdfOffset+1] != 0 {
		return SigningKey{}, fmt.Errorf("signing key %q is password-protected; the release key must be password-less (minisign -G -W)", path)
	}
	keyID := raw[keynumOffset : keynumOffset+keyIDLen]
	secret := raw[keynumOffset+keyIDLen : keynumOffset+keyIDLen+secretKeyLen]
	priv := make(ed25519.PrivateKey, secretKeyLen)
	copy(priv, secret)
	return SigningKey{
		KeyID:   base64.StdEncoding.EncodeToString(keyID),
		Private: priv,
	}, nil
}

// Sign returns the base64 Ed25519 signature over msg.
func (k SigningKey) Sign(msg []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(k.Private, msg))
}
