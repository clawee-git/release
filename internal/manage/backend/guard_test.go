package backend

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGuardRefusesTheShapesThatMatter(t *testing.T) {
	g := &Guard{}
	cases := []struct {
		raw  string
		want string
	}{
		{"http://example.com/x", "not https"},
		// https://api.github.com@evil.example reads as GitHub to a human and
		// resolves to evil.example.
		{"https://api.github.com@evil.example/x", "userinfo"},
		{"https://93.184.216.34/x", "IP literal"},
		{"https://127.0.0.1/x", "private network"},
		{"https://10.0.0.1/x", "private network"},
		{"https://169.254.169.254/latest/meta-data/", "private network"},
		{"https://[::1]/x", "private network"},
		{"ftp://example.com/x", "not https"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", c.raw, err)
		}
		err = g.CheckURL(u)
		if err == nil {
			t.Errorf("%s was accepted", c.raw)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one naming %q", c.raw, err, c.want)
		}
	}
	if err := g.CheckURL(mustURL(t, "https://api.github.com/repos/x/y")); err != nil {
		t.Fatalf("a plain https URL was refused: %v", err)
	}
}

// Carrier-grade NAT is not "private" by Go's definition and is very much not
// the public internet.
func TestGuardRefusesCarrierGradeNAT(t *testing.T) {
	g := &Guard{}
	if err := g.CheckURL(mustURL(t, "https://100.64.0.1/x")); err == nil {
		t.Fatal("100.64/10 was accepted")
	}
}

func TestGuardChecksEveryRedirectHop(t *testing.T) {
	// The final hop is the one that matters: a first hop to a legitimate host
	// that redirects onto plaintext is the whole attack.
	var plaintext *httptest.Server
	plaintext = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should never be read"))
	}))
	defer plaintext.Close()

	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plaintext.URL+"/x", http.StatusFound)
	}))
	defer redirector.Close()

	// AllowPrivate is on (httptest is loopback), so the ONLY thing that can
	// refuse the second hop is the scheme rule.
	g := &Guard{AllowPrivate: false}
	client := g.Client()
	client.Transport = redirector.Client().Transport
	client.CheckRedirect = g.Client().CheckRedirect
	_, err := client.Get(redirector.URL)
	if err == nil {
		t.Fatal("a redirect onto a plaintext hop was followed")
	}
	if !strings.Contains(err.Error(), "not https") && !strings.Contains(err.Error(), "private") {
		t.Fatalf("err = %v", err)
	}
}

func TestGuardAllowsLoopbackWhenAsked(t *testing.T) {
	g := &Guard{AllowPrivate: true}
	if err := g.CheckURL(mustURL(t, "http://127.0.0.1:8080/x")); err != nil {
		t.Fatalf("AllowPrivate did not allow loopback: %v", err)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestGuardedClientHasNoWholeExchangeTimeout is a regression test for a defect
// class this repo has already paid for once. A blanket http.Client.Timeout
// bounds the request BODY as well as the wait for a response, so an upload
// that is making steady progress dies as "Client.Timeout exceeded while
// awaiting headers". On 2026-09-03 that stalled a cut mid-distribute and left
// the GitHub release published with the R2 catalog un-updated
// (internal/r2/r2.go). Promote pushes four ~11 MB zips through this client.
func TestGuardedClientHasNoWholeExchangeTimeout(t *testing.T) {
	c := (&Guard{}).Client()
	if c.Timeout != 0 {
		t.Fatalf("the guarded client has a whole-exchange timeout of %s; it must bound phases only", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("the guarded client has no *http.Transport")
	}
	// …but the phases that CAN wedge are bounded, or the fix is just a leak.
	for _, b := range []struct {
		name string
		val  time.Duration
	}{
		{"TLSHandshakeTimeout", tr.TLSHandshakeTimeout},
		{"ResponseHeaderTimeout", tr.ResponseHeaderTimeout},
		{"IdleConnTimeout", tr.IdleConnTimeout},
		{"ExpectContinueTimeout", tr.ExpectContinueTimeout},
	} {
		if b.val <= 0 {
			t.Errorf("%s is unbounded", b.name)
		}
	}
	if tr.Proxy != nil {
		t.Error("a proxy is configured; it would resolve and dial on our behalf, defeating the guard")
	}
}

// TestGuardedClientSurvivesATricklingUpload is the same defect from the other
// side: a body written slowly over a period that a blanket timeout of the old
// shape would have killed. The old shape is exercised alongside it, so the
// test demonstrates the difference rather than asserting into a vacuum.
func TestGuardedClientSurvivesATricklingUpload(t *testing.T) {
	const (
		chunks    = 12
		perChunk  = 60 * time.Millisecond // ~720ms of steady progress
		oldBudget = 250 * time.Millisecond
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "read %d bytes", n)
	}))
	defer srv.Close()

	// trickle writes the body in chunks, pausing between them: progress is
	// steady but the exchange takes far longer than any single chunk.
	trickle := func() io.Reader {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			for i := 0; i < chunks; i++ {
				if _, err := pw.Write(bytes.Repeat([]byte("x"), 1024)); err != nil {
					return
				}
				time.Sleep(perChunk)
			}
		}()
		return pr
	}

	g := &Guard{AllowPrivate: true}
	client := g.Client()
	req, err := http.NewRequest(http.MethodPut, srv.URL, trickle())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the guarded client killed an upload that was making progress: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := fmt.Sprintf("read %d bytes", chunks*1024); string(body) != want {
		t.Fatalf("server read %q, want %q", body, want)
	}

	// The old shape, for contrast: a whole-exchange budget shorter than the
	// transfer takes, killing a transfer that never stalled.
	old := *client
	old.Timeout = oldBudget
	req2, _ := http.NewRequest(http.MethodPut, srv.URL, trickle())
	if _, err := old.Do(req2); err == nil {
		t.Fatal("the contrast case did not fail; the test is not exercising the defect")
	}
}
