package r2

// The read half of the client — HEAD, GET, server-side COPY and query
// presigning. The cut only ever wrote (PUT/LIST/DELETE); promote and the invite
// mint need to read what the cut staged, copy it to the public bucket without
// pushing the bytes back up, and hand out short-lived URLs.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// maxObject caps a Get. The largest thing this reads is a component zip; a
// response larger than this is not one, and reading it into memory unbounded
// is how a hostile or misconfigured endpoint takes the service down.
const maxObject = 512 << 20

// Head returns the object's size. Promote checks it against the catalog row
// BEFORE fetching the body, so a wrong-sized object is refused without pulling
// half a gigabyte first.
func (c *Client) Head(ctx context.Context, key string) (int64, error) {
	req, err := c.signedRequest(ctx, http.MethodHead, c.objectURL(key), nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.doer.Do(req)
	if err != nil {
		return 0, fmt.Errorf("r2: head %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("r2: head %s: status %d", key, resp.StatusCode)
	}
	n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("r2: head %s: unreadable Content-Length %q", key, resp.Header.Get("Content-Length"))
	}
	return n, nil
}

// Get reads the object body.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	req, err := c.signedRequest(ctx, http.MethodGet, c.objectURL(key), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("r2: get %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("r2: get %s: status %d: %s", key, resp.StatusCode, b)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxObject+1))
	if err != nil {
		return nil, fmt.Errorf("r2: get %s: read body: %w", key, err)
	}
	if len(body) > maxObject {
		return nil, fmt.Errorf("r2: get %s: object exceeds the %d-byte read limit", key, maxObject)
	}
	return body, nil
}

// Copy performs a server-side copy of <srcBucket>/<srcKey> into this client's
// bucket at dstKey.
//
// Server-side because promote moves four zips plus two small files per cut:
// pulling them down and pushing them back up doubles the transfer for bytes
// that never change, and the guideline asks for a server-side copy where the
// store supports it. Promote has already fetched and hashed each object
// against the catalog row before this runs, so what is copied is what was
// verified.
func (c *Client) Copy(ctx context.Context, srcBucket, srcKey, dstKey string) error {
	req, err := c.signedRequest(ctx, http.MethodPut, c.objectURL(dstKey), nil)
	if err != nil {
		return err
	}
	// The copy source is a header, and it must be signed: it is part of the
	// canonical headers block (x-amz-*), so it has to be set BEFORE signing.
	req.Header.Set("x-amz-copy-source", "/"+srcBucket+"/"+escapeKeySegments(srcKey))
	signV4(req, c.accessKeyID, c.secret, "auto", "s3", nil, c.now())
	resp, err := c.doer.Do(req)
	if err != nil {
		return fmt.Errorf("r2: copy %s -> %s: %w", srcKey, dstKey, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("r2: copy %s -> %s: status %d: %s", srcKey, dstKey, resp.StatusCode, body)
	}
	// S3 CopyObject can answer 200 with an <Error> document in the body: the
	// status is about the connection, not the copy. Treating a 200 as success
	// without looking is how a promote reports a file it never copied.
	if bodyHasError(body) {
		return fmt.Errorf("r2: copy %s -> %s: 200 with an error body: %s", srcKey, dstKey, body)
	}
	return nil
}

func bodyHasError(body []byte) bool {
	return len(body) > 0 && containsFold(string(body), "<Error")
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && indexFold(s, sub) >= 0
}

func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// Presign returns a SigV4 QUERY-presigned GET URL valid for ttl.
//
// Query auth, not headers: the URL has to be self-contained because it is
// handed to a person and run by `curl` inside a generated install script.
// UNSIGNED-PAYLOAD is the standard payload hash for a presigned GET.
func (c *Client) Presign(key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("r2: presign %s: ttl must be positive", key)
	}
	now := c.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	host, err := hostOf(c.endpoint)
	if err != nil {
		return "", fmt.Errorf("r2: presign %s: %w", key, err)
	}
	credentialScope := dateStamp + "/auto/s3/aws4_request"
	canonicalURI := "/" + c.bucket + "/" + escapeKeySegments(key)

	// Sorted lexicographically, each name and value RFC3986-escaped, per SigV4.
	canonicalQuery := "X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=" + uriEscape(c.accessKeyID+"/"+credentialScope) +
		"&X-Amz-Date=" + amzDate +
		"&X-Amz-Expires=" + strconv.Itoa(int(ttl.Seconds())) +
		"&X-Amz-SignedHeaders=host"

	canonicalRequest := "GET\n" + canonicalURI + "\n" + canonicalQuery + "\n" +
		"host:" + host + "\n\nhost\nUNSIGNED-PAYLOAD"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" +
		hashHex([]byte(canonicalRequest))
	signingKey := deriveSigningKey(c.secret, dateStamp, "auto", "s3")
	signature := hexEncode(hmacSHA256(signingKey, []byte(stringToSign)))

	return c.endpoint + canonicalURI + "?" + canonicalQuery + "&X-Amz-Signature=" + signature, nil
}

func (c *Client) objectURL(key string) string {
	return c.endpoint + "/" + c.bucket + "/" + escapeKeySegments(key)
}

func (c *Client) signedRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("r2: %s %s: new request: %w", method, url, err)
	}
	signV4(req, c.accessKeyID, c.secret, "auto", "s3", body, c.now())
	return req, nil
}
