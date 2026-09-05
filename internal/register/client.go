package register

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpTimeout bounds each manage-service call. Registration is NOT optional
// (a staged upload nobody can promote is a stranded artifact), but a
// black-holed service must fail rather than hold the decrypted signing key on
// disk indefinitely.
const httpTimeout = 30 * time.Second

// maxBody caps what we read back from the service; only a nonce and an error
// string are ever expected.
const maxBody = 1 << 20

// Client talks to the manage service. HTTP is injectable so the tests exercise
// the real handshake against a fake server instead of a mocked-out one.
type Client struct {
	ManageURL string
	HTTP      *http.Client
}

// NewClient returns a Client with the default bounded HTTP client.
func NewClient(manageURL string) *Client {
	return &Client{
		ManageURL: strings.TrimRight(strings.TrimSpace(manageURL), "/"),
		HTTP:      &http.Client{Timeout: httpTimeout},
	}
}

// checkManageURL refuses a manage URL that is not https.
//
// What travels this connection is a signed catalog row and, on the way back, a
// nonce: over http both are readable and rewritable in flight, and the row is
// what an operator later promotes. A loopback host is the exception, because a
// test server has no certificate and there is no network to be on the wire of.
func checkManageURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("manage URL %q is not a URL: %w", raw, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return fmt.Errorf("manage URL %q is http — a release row must not be registered over a connection anyone can read or rewrite; use https (http is allowed only for a loopback test server)", raw)
	default:
		return fmt.Errorf("manage URL %q has scheme %q; want https", raw, u.Scheme)
	}
}

// Register performs the nonce → sign → register handshake and returns the
// manage URL of the row the service recorded.
//
// The nonce is a FIELD OF THE SIGNED BODY, not a separate header: that is what
// binds one signature to one issuance, so a captured register request cannot
// be replayed to create a second row.
func (c *Client) Register(ctx context.Context, p Payload, key SigningKey) (Payload, string, error) {
	if c.ManageURL == "" {
		return p, "", fmt.Errorf("manage URL is empty")
	}
	if err := checkManageURL(c.ManageURL); err != nil {
		return p, "", err
	}
	nonce, err := c.nonce(ctx)
	if err != nil {
		return p, "", err
	}
	p.Nonce = nonce

	msg, err := p.SigningBytes()
	if err != nil {
		return p, "", err
	}
	p.Signature = key.Sign(msg)

	body, err := json.Marshal(p)
	if err != nil {
		return p, "", fmt.Errorf("marshal register request: %w", err)
	}
	resp, err := c.post(ctx, "/api/v1/releases/register", body)
	if err != nil {
		return p, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return p, "", fmt.Errorf("register refused by %s: HTTP %d %s", c.ManageURL+"/api/v1/releases/register", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.URL == "" {
		out.URL = c.ManageURL
	}
	return p, out.URL, nil
}

func (c *Client) nonce(ctx context.Context) (string, error) {
	resp, err := c.post(ctx, "/api/v1/releases/nonce", []byte(`{}`))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("nonce refused by %s: HTTP %d %s", c.ManageURL+"/api/v1/releases/nonce", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode nonce response: %w", err)
	}
	if out.Nonce == "" {
		return "", fmt.Errorf("nonce response from %s carried no nonce", c.ManageURL)
	}
	return out.Nonce, nil
}

func (c *Client) post(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ManageURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: httpTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", c.ManageURL+path, err)
	}
	return resp, nil
}
