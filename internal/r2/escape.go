package r2

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// uriEscape is RFC3986 escaping as SigV4 defines it: unreserved characters
// pass, everything else becomes %XX with UPPERCASE hex.
//
// net/url is not usable here. QueryEscape turns a space into '+', and
// PathEscape leaves several characters SigV4 requires escaped — either
// produces a canonical request that differs from the one the service
// recomputes, and the only symptom is a 403 with no detail.
func uriEscape(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// escapeKeySegments escapes an object key for a URL path, keeping '/' as a
// separator: a key is a path, and escaping its slashes would address one
// object whose name contains them.
func escapeKeySegments(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = uriEscape(p)
	}
	return strings.Join(parts, "/")
}

// deriveSigningKey is the SigV4 four-step key derivation.
func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	return hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256(
		[]byte("AWS4"+secret), []byte(dateStamp)), []byte(region)), []byte(service)),
		[]byte("aws4_request"))
}

func hexEncode(b []byte) string { return hex.EncodeToString(b) }

// hostOf extracts the host from the endpoint URL.
func hostOf(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("endpoint %q is not a URL: %w", endpoint, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("endpoint %q has no host", endpoint)
	}
	return u.Host, nil
}
