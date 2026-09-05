// Package r2 is a minimal stdlib AWS-SigV4 client for PUT/LIST/DELETE against a
// Cloudflare R2 bucket (S3-compatible API). Ported from the console's
// r2_mirror.go signer; no SDK dependency.
package r2

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type Client struct {
	endpoint    string // https://<account>.r2.cloudflarestorage.com
	bucket      string
	accessKeyID string
	secret      string
	doer        Doer
	// now is injected so a presigned URL is reproducible under test. A
	// signature is a function of the clock, so a test that cannot fix the
	// clock can only assert that the URL is "some string".
	now func() time.Time
}

// Bucket is the bucket this client addresses. Copy needs the SOURCE client's
// bucket name to build x-amz-copy-source, and asking the client rather than
// re-threading the name through the caller keeps one spelling of it.
func (c *Client) Bucket() string { return c.bucket }

// SetClock replaces the client's clock. Tests only.
func (c *Client) SetClock(now func() time.Time) { c.now = now }

// New builds a Client. doer nil → an http.Client with per-phase timeouts.
func New(accountID, bucket, accessKeyID, secret string, doer Doer) *Client {
	if doer == nil {
		// No blanket Client.Timeout: it bounds the WHOLE exchange, body upload
		// included, so a multi-MB PUT on a slow uplink fails as "Client.Timeout
		// exceeded while awaiting headers" while the transfer is still making
		// progress. The old 30s needed ~380 KB/s sustained to mirror one ~11 MB
		// zip; a degraded uplink stalled a cut mid-distribute (claweed v0.2.21,
		// 2026-09-03) and left the GitHub release published with the R2 catalog
		// un-updated. Bound the phases that can actually wedge and let a slow
		// upload take the time it needs.
		doer = &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 10 * time.Minute,
				IdleConnTimeout:       90 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		}
	}
	return &Client{
		endpoint:    "https://" + accountID + ".r2.cloudflarestorage.com",
		bucket:      strings.Trim(bucket, "/"),
		accessKeyID: accessKeyID,
		secret:      secret,
		doer:        doer,
		now:         time.Now,
	}
}

// Put uploads body to <endpoint>/<bucket>/<key> with a SigV4-signed PUT.
func (c *Client) Put(ctx context.Context, key string, body []byte, contentType string) error {
	url := c.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("r2: put: new request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(body))
	signV4(req, c.accessKeyID, c.secret, "auto", "s3", body, c.now())
	resp, err := c.doer.Do(req)
	if err != nil {
		return fmt.Errorf("r2: put %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("r2: put %s: status %d: %s", key, resp.StatusCode, b)
	}
	return nil
}

// listResult is the subset of the S3 ListObjectsV2 XML response we read.
type listResult struct {
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	IsTruncated bool   `xml:"IsTruncated"`
	NextToken   string `xml:"NextContinuationToken"`
}

// List returns every object key under prefix, following continuation tokens so
// buckets with more than 1000 objects are fully enumerated. An empty body is
// signed (GETs carry no payload).
func (c *Client) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", prefix)
		if token != "" {
			q.Set("continuation-token", token)
		}
		// url.Values.Encode encodes spaces as '+', but SigV4 canonicalization
		// (signer.go signs req.URL.RawQuery verbatim) requires '%20'. Convert so
		// the signed query matches what S3/R2 re-encodes for verification — a
		// latent 403 once any value carries a space.
		enc := strings.ReplaceAll(q.Encode(), "+", "%20")
		reqURL := fmt.Sprintf("%s/%s?%s", c.endpoint, c.bucket, enc)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("r2: list %q: new request: %w", prefix, err)
		}
		signV4(req, c.accessKeyID, c.secret, "auto", "s3", nil, c.now())
		resp, err := c.doer.Do(req)
		if err != nil {
			return nil, fmt.Errorf("r2: list %q: %w", prefix, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("r2: list %q: read response: %w", prefix, err)
		}
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("r2: list %q: status %d: %s", prefix, resp.StatusCode, body)
		}
		var lr listResult
		if err := xml.Unmarshal(body, &lr); err != nil {
			return nil, fmt.Errorf("r2: list %q: parse response: %w", prefix, err)
		}
		for _, o := range lr.Contents {
			keys = append(keys, o.Key)
		}
		if !lr.IsTruncated || lr.NextToken == "" {
			break
		}
		token = lr.NextToken
	}
	return keys, nil
}

// Delete removes the object at key. A 404/NoSuchKey is treated as success
// (deleting an absent key is a no-op, which keeps prune idempotent).
func (c *Client) Delete(ctx context.Context, key string) error {
	reqURL := c.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return fmt.Errorf("r2: delete %s: new request: %w", key, err)
	}
	signV4(req, c.accessKeyID, c.secret, "auto", "s3", nil, c.now())
	resp, err := c.doer.Do(req)
	if err != nil {
		return fmt.Errorf("r2: delete %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("r2: delete %s: status %d: %s", key, resp.StatusCode, b)
	}
	return nil
}
