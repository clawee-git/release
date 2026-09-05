// Package totp implements RFC 6238 time-based one-time passwords (HMAC-SHA1,
// 6 digits, 30-second steps) for the manage service's second factor.
//
// It is a small composition over stdlib primitives rather than a dependency:
// the whole algorithm is under fifty lines, totp_test.go pins it to the
// official RFC 4226/6238 test vectors, and a release-signing surface is a poor
// place to add a transitive dependency tree for arithmetic this size. Ported
// from the console's internal/console/totp, which is the reference
// implementation this service was modelled on.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"time"
)

const (
	// Period is the TOTP time step in seconds (RFC 6238 default).
	Period = 30
	// secretSize is the secret length in bytes — 160 bits, SHA-1's block and
	// the RFC-recommended minimum.
	secretSize = 20
)

// encoding is base32 WITHOUT padding: the form authenticator apps expect both
// in otpauth:// URIs and in manual entry. Padded secrets are accepted by some
// apps and silently mis-decoded by others.
var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret mints a new random 160-bit secret, base32-encoded.
func GenerateSecret() (string, error) {
	raw := make([]byte, secretSize)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("totp: generate secret: %w", err)
	}
	return encoding.EncodeToString(raw), nil
}

// Code renders the 6-digit code for the time step containing at.
func Code(secret string, at time.Time) (string, error) {
	key, err := encoding.DecodeString(secret)
	if err != nil {
		return "", fmt.Errorf("totp: bad secret: %w", err)
	}
	return hotp(key, uint64(at.Unix()/Period)), nil
}

// Verify checks code against secret at now, accepting the current step and its
// two neighbours (±1 step of clock skew) — but only steps strictly greater
// than lastUsedStep. Returns the matched step, which the caller persists as
// the account's new watermark.
//
// The watermark is what stops a code observed inside the skew window from
// being replayed: without it, a code shoulder-surfed or captured from a
// phishing page stays valid for up to ninety seconds against the real service.
func Verify(secret, code string, now time.Time, lastUsedStep int64) (int64, bool) {
	key, err := encoding.DecodeString(secret)
	if err != nil {
		return 0, false
	}
	current := now.Unix() / Period
	for _, offset := range []int64{0, -1, 1} {
		step := current + offset
		if step <= lastUsedStep {
			continue
		}
		// Constant-time: the comparison is against a secret-derived value, so
		// a byte-at-a-time short circuit is a timing oracle on the code.
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(step))), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// OTPAuthURL renders the otpauth:// provisioning URI authenticator apps import.
func OTPAuthURL(issuer, account, secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}

// hotp computes the RFC 4226 dynamic-truncation code for one counter value.
func hotp(key []byte, counter uint64) string {
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(message[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1_000_000)
}
