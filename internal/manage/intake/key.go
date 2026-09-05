// Package intake is the machine-authenticated registration of a cut.
//
// A release kit holds no admin credential (release-management.md §6). The
// service issues a single-use nonce; the kit signs the WHOLE row, nonce
// included, with the same Ed25519 key that signs SHA256SUMS.txt; the service
// verifies against the public half it bakes in. Because the nonce is a field
// of the signed body rather than a header, one signature buys exactly one row.
//
// The canonical bytes are internal/register's, imported rather than re-derived
// here: the signature covers the client's struct in declaration order, so a
// second spelling of "the canonical form" on this side would be a second thing
// to keep in step and a silent verification failure the day it drifted.
package intake

import (
	"crypto/ed25519"
	_ "embed"
	"encoding/base64"
	"fmt"
	"strings"
)

// releasePubKey is a COPY of the repo-root clawee-release.pub. go:embed cannot
// reach outside its own package directory, so the file is duplicated here and
// TestEmbeddedKeyMatchesTheRepoRoot byte-compares the two — a copy with a gate
// on it, rather than a copy that drifts.
//
//go:embed clawee-release.pub
var releasePubKey string

// Minisign public-key layout (untrusted comment line, then one base64 line):
//
//	signature_algorithm  2   "Ed"
//	key_id               8
//	public_key          32   the Ed25519 public key
const (
	minisignPublicLen = 42
	pubKeyIDOffset    = 2
	pubKeyIDLen       = 8
)

// ReleaseKey returns the baked release public key. It is parsed on demand
// rather than in an initialiser so a malformed key is an error the caller
// reports, not a panic at process start.
func ReleaseKey() (ed25519.PublicKey, string, error) {
	return parseMinisignPublicKey(releasePubKey)
}

// parseMinisignPublicKey reads a minisign public-key file's contents.
func parseMinisignPublicKey(contents string) (ed25519.PublicKey, string, error) {
	var b64 string
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "untrusted comment:") {
			continue
		}
		b64 = line
		break
	}
	if b64 == "" {
		return nil, "", fmt.Errorf("minisign public key: no key line found")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, "", fmt.Errorf("minisign public key: not base64: %w", err)
	}
	if len(raw) != minisignPublicLen {
		return nil, "", fmt.Errorf("minisign public key: expected %d bytes, got %d", minisignPublicLen, len(raw))
	}
	if string(raw[:2]) != "Ed" {
		return nil, "", fmt.Errorf("minisign public key: algorithm %q, want \"Ed\"", raw[:2])
	}
	keyID := base64.StdEncoding.EncodeToString(raw[pubKeyIDOffset : pubKeyIDOffset+pubKeyIDLen])
	pub := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(pub, raw[pubKeyIDOffset+pubKeyIDLen:])
	return pub, keyID, nil
}
