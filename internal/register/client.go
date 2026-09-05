package register

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
