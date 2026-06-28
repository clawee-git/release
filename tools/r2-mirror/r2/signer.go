package r2

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// signV4 adds AWS Signature Version 4 auth headers to req (S3-compatible).
// now is injected so the signature is deterministic under test.
func signV4(req *http.Request, accessKey, secret, region, service string, body []byte, now time.Time) {
	now = now.UTC()
	date := now.Format("20060102")
	datetime := now.Format("20060102T150405Z")

	bodyHash := hashHex(body)
	req.Header.Set("X-Amz-Date", datetime)
	req.Header.Set("X-Amz-Content-Sha256", bodyHash)

	signedHeaders, canonicalHeaders := buildHeaders(req)
	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalRequest := strings.Join([]string{
		req.Method, canonicalURI, req.URL.RawQuery,
		canonicalHeaders, signedHeaders, bodyHash,
	}, "\n")

	credentialScope := date + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + datetime + "\n" + credentialScope + "\n" +
		hashHex([]byte(canonicalRequest))

	signingKey := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256(
		[]byte("AWS4"+secret), []byte(date)), []byte(region)), []byte(service)), []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+accessKey+"/"+credentialScope+
			", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func buildHeaders(req *http.Request) (signedHeaders, canonicalHeaders string) {
	headers := make(map[string]string)
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if lk == "host" || strings.HasPrefix(lk, "x-amz-") || lk == "content-type" {
			headers[lk] = strings.Join(vs, ",")
		}
	}
	headers["host"] = req.Host
	if headers["host"] == "" {
		headers["host"] = req.URL.Host
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var chBuf, shBuf strings.Builder
	for i, k := range keys {
		chBuf.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
		if i > 0 {
			shBuf.WriteByte(';')
		}
		shBuf.WriteString(k)
	}
	return shBuf.String(), chBuf.String()
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}
func hashHex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
