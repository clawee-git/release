package r2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

// recorder captures the request the client made and answers with a canned
// response, so every test here asserts on what went ON THE WIRE.
type recorder struct {
	req      *http.Request
	status   int
	body     string
	headers  map[string]string
	reqCount int
}

func (r *recorder) Do(req *http.Request) (*http.Response, error) {
	r.req = req
	r.reqCount++
	h := http.Header{}
	for k, v := range r.headers {
		h.Set(k, v)
	}
	status := r.status
	if status == 0 {
		status = 200
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

func newTestClient(doer Doer) *Client {
	c := New("acct", "staging", "AKIDEXAMPLE", "secret", doer)
	c.SetClock(func() time.Time { return fixedNow })
	return c
}

func TestPresignIsSelfContainedAndDeterministic(t *testing.T) {
	c := newTestClient(&recorder{})
	got, err := c.Presign("clawee/beta/v0.3.0.beta.2026.09.04.deadbeef/x.zip", 48*time.Hour)
	if err != nil {
		t.Fatalf("Presign: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("presigned URL does not parse: %v", err)
	}
	if u.Scheme != "https" {
		t.Fatalf("presigned URL is %s, not https", u.Scheme)
	}
	q := u.Query()
	for _, k := range []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date",
		"X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Signature"} {
		if q.Get(k) == "" {
			t.Errorf("presigned URL has no %s", k)
		}
	}
	if q.Get("X-Amz-Expires") != "172800" {
		t.Errorf("X-Amz-Expires = %q, want 172800 (48h)", q.Get("X-Amz-Expires"))
	}
	// Self-contained: no header is needed to use it, so nothing but the query
	// carries the credential.
	if strings.Contains(got, "secret") {
		t.Fatal("the secret key appears in the presigned URL")
	}
	// Deterministic under a fixed clock — otherwise a test can only assert
	// that some string came back.
	again, _ := c.Presign("clawee/beta/v0.3.0.beta.2026.09.04.deadbeef/x.zip", 48*time.Hour)
	if again != got {
		t.Fatal("two presigns at the same instant differ")
	}
	// The signature covers the key: a different object is a different URL.
	other, _ := c.Presign("clawee/beta/v0.3.0.beta.2026.09.04.deadbeef/y.zip", 48*time.Hour)
	if other == got {
		t.Fatal("two different keys presigned identically")
	}
}

func TestPresignRefusesANonPositiveTTL(t *testing.T) {
	c := newTestClient(&recorder{})
	if _, err := c.Presign("k", 0); err == nil {
		t.Fatal("a zero TTL was accepted; the URL would be born expired")
	}
}

// A key segment needing escaping must be escaped the SigV4 way, not the
// net/url way — QueryEscape's '+' for a space produces a canonical request the
// service recomputes differently, and the only symptom is an unexplained 403.
func TestKeyEscapingIsSigV4Not(t *testing.T) {
	if got := uriEscape("a b+c~d"); got != "a%20b%2Bc~d" {
		t.Fatalf("uriEscape = %q, want a%%20b%%2Bc~d", got)
	}
	if got := escapeKeySegments("clawee/beta/a b.zip"); got != "clawee/beta/a%20b.zip" {
		t.Fatalf("escapeKeySegments = %q; slashes must survive as separators", got)
	}
}

func TestHeadReadsTheSize(t *testing.T) {
	rec := &recorder{headers: map[string]string{"Content-Length": "4096"}}
	c := newTestClient(rec)
	n, err := c.Head(context.Background(), "clawee/stable/s/x.zip")
	if err != nil || n != 4096 {
		t.Fatalf("Head = %d, %v", n, err)
	}
	if rec.req.Method != http.MethodHead {
		t.Fatalf("Head issued %s", rec.req.Method)
	}
	if rec.req.Header.Get("Authorization") == "" {
		t.Fatal("the HEAD was not signed")
	}
}

func TestHeadReportsAMissingLength(t *testing.T) {
	c := newTestClient(&recorder{})
	if _, err := c.Head(context.Background(), "k"); err == nil {
		t.Fatal("a response with no Content-Length was accepted; promote would compare against garbage")
	}
}

func TestGetReturnsTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("the object bytes"))
	}))
	defer srv.Close()
	c := newTestClient(srv.Client())
	c.endpoint = srv.URL
	body, err := c.Get(context.Background(), "clawee/stable/s/x.zip")
	if err != nil || string(body) != "the object bytes" {
		t.Fatalf("Get = %q, %v", body, err)
	}
}

func TestGetReportsANonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "NoSuchKey", http.StatusNotFound)
	}))
	defer srv.Close()
	c := newTestClient(srv.Client())
	c.endpoint = srv.URL
	if _, err := c.Get(context.Background(), "gone"); err == nil {
		t.Fatal("a 404 body was returned as the object")
	}
}

func TestCopySendsASignedCopySourceHeader(t *testing.T) {
	rec := &recorder{}
	c := newTestClient(rec)
	if err := c.Copy(context.Background(), "staging", "clawee/beta/s/x.zip", "clawee/beta/s/x.zip"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	src := rec.req.Header.Get("x-amz-copy-source")
	if src != "/staging/clawee/beta/s/x.zip" {
		t.Fatalf("x-amz-copy-source = %q", src)
	}
	// The header must be inside the signature, or the service rejects it.
	if auth := rec.req.Header.Get("Authorization"); !strings.Contains(auth, "x-amz-copy-source") {
		t.Fatalf("x-amz-copy-source is not in SignedHeaders: %q", auth)
	}
}

// S3 can answer 200 with an <Error> document: the status describes the
// connection, not the copy. Trusting the status is how a promote reports a
// file it never copied.
func TestCopyRefusesA200WithAnErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`<?xml version="1.0"?><Error><Code>InternalError</Code></Error>`))
	}))
	defer srv.Close()
	c := newTestClient(srv.Client())
	c.endpoint = srv.URL
	err := c.Copy(context.Background(), "staging", "a", "b")
	if err == nil || !strings.Contains(err.Error(), "error body") {
		t.Fatalf("Copy over a 200-with-error: err = %v", err)
	}
}
