package backend

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
