package backend

// The GitHub release publisher: the only implementation of the GitHub seam
// that talks to a network. Ported in shape from the console's
// internal/console/release/github_publish.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GitHubClient publishes releases through the REST API.
type GitHubClient struct {
	Owner string
	Repo  string
	// Token is the fine-grained token with contents:write on Owner/Repo. It is
	// never logged and never rendered into a release body.
	Token string
	// APIBase and UploadBase are separate hosts on GitHub (api. and uploads.)
	// and are fields rather than constants ONLY so a test can point them at an
	// httptest server. Nothing configures them from a request.
	APIBase    string
	UploadBase string
	HTTP       *http.Client
}

// NewGitHubClient builds a client with a guarded HTTP client.
func NewGitHubClient(owner, repo, token string, guard *Guard) *GitHubClient {
	if guard == nil {
		guard = &Guard{}
	}
	return &GitHubClient{
		Owner: owner, Repo: repo, Token: token,
		APIBase:    "https://api.github.com",
		UploadBase: "https://uploads.github.com",
		HTTP:       guard.Client(),
	}
}

// CreateRelease creates the release for tag, or REUSES the one already there.
//
// Reuse rather than refuse: promote's steps run in order and a retry after a
// failure between "release created" and "assets uploaded" must be able to
// finish the job. A second attempt that refused because the release exists
// would leave a promote that can never complete and a tag that has to be
// deleted by hand.
func (c *GitHubClient) CreateRelease(ctx context.Context, tag, name, body string, prerelease bool) (Release, error) {
	if existing, err := c.releaseByTag(ctx, tag); err == nil {
		return existing, nil
	}
	payload, _ := json.Marshal(map[string]any{
		"tag_name": tag, "name": name, "body": body,
		"prerelease": prerelease, "draft": false,
	})
	resp, raw, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("%s/repos/%s/%s/releases", c.APIBase, c.Owner, c.Repo), "application/json", payload)
	if err != nil {
		return Release{}, err
	}
	if resp.StatusCode/100 != 2 {
		return Release{}, fmt.Errorf("github: create release %s: status %d: %s", tag, resp.StatusCode, raw)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == 0 {
		return Release{}, fmt.Errorf("github: create release %s: unreadable response", tag)
	}
	return Release{ID: out.ID, Tag: tag}, nil
}

// UploadAsset attaches one file to a release, REPLACING an asset of the same
// name if one is already there.
//
// Idempotent on purpose, and it is the other half of CreateRelease's reuse.
// Promote's steps run in order and a retry after a failure between "release
// created" and "all assets uploaded" has to be able to finish the job —
// otherwise the retry the whole pipeline promises is impossible, because
// GitHub answers 422 for a duplicate asset name and promote hard-fails on it
// forever. Delete-then-upload rather than skip-if-present: a retry after a
// PARTIAL upload leaves an asset of the right name and the wrong bytes, and
// skipping would publish it.
func (c *GitHubClient) UploadAsset(ctx context.Context, rel Release, name, contentType string, body []byte) error {
	if err := c.deleteAssetByName(ctx, rel, name); err != nil {
		return err
	}
	u := fmt.Sprintf("%s/repos/%s/%s/releases/%d/assets?name=%s",
		c.UploadBase, c.Owner, c.Repo, rel.ID, url.QueryEscape(name))
	resp, raw, err := c.do(ctx, http.MethodPost, u, contentType, body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github: upload %s to %s: status %d: %s", name, rel.Tag, resp.StatusCode, raw)
	}
	return nil
}

// DeleteRelease removes the release and then its tag. Both are needed: a
// deleted release leaves the tag behind, and a tag with no release still shows
// up in a clone and in the tag list.
func (c *GitHubClient) DeleteRelease(ctx context.Context, tag string) error {
	rel, err := c.releaseByTag(ctx, tag)
	if err == nil {
		resp, raw, derr := c.do(ctx, http.MethodDelete,
			fmt.Sprintf("%s/repos/%s/%s/releases/%d", c.APIBase, c.Owner, c.Repo, rel.ID), "", nil)
		if derr != nil {
			return derr
		}
		if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("github: delete release %s: status %d: %s", tag, resp.StatusCode, raw)
		}
	}
	resp, raw, err := c.do(ctx, http.MethodDelete,
		fmt.Sprintf("%s/repos/%s/%s/git/refs/tags/%s", c.APIBase, c.Owner, c.Repo, url.PathEscape(tag)), "", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound &&
		resp.StatusCode != http.StatusUnprocessableEntity {
		return fmt.Errorf("github: delete tag %s: status %d: %s", tag, resp.StatusCode, raw)
	}
	return nil
}

// deleteAssetByName removes an existing asset with this name, if any. A
// listing failure is reported rather than assumed-empty: proceeding would hit
// the 422 this exists to avoid, with a more confusing message.
func (c *GitHubClient) deleteAssetByName(ctx context.Context, rel Release, name string) error {
	resp, raw, err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("%s/repos/%s/%s/releases/%d/assets?per_page=100", c.APIBase, c.Owner, c.Repo, rel.ID), "", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil // the release has no assets yet
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github: list assets of %s: status %d: %s", rel.Tag, resp.StatusCode, raw)
	}
	var assets []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &assets); err != nil {
		return fmt.Errorf("github: list assets of %s: unreadable response: %w", rel.Tag, err)
	}
	for _, a := range assets {
		if a.Name != name {
			continue
		}
		resp, raw, err := c.do(ctx, http.MethodDelete,
			fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d", c.APIBase, c.Owner, c.Repo, a.ID), "", nil)
		if err != nil {
			return err
		}
		if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("github: delete asset %s from %s: status %d: %s", name, rel.Tag, resp.StatusCode, raw)
		}
	}
	return nil
}

func (c *GitHubClient) releaseByTag(ctx context.Context, tag string) (Release, error) {
	// The tag contains a slash (`clawee/v0.2.28…`), which is legal in a git
	// ref and must be escaped per segment in the path.
	resp, raw, err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", c.APIBase, c.Owner, c.Repo, escapePathSegments(tag)), "", nil)
	if err != nil {
		return Release{}, err
	}
	if resp.StatusCode/100 != 2 {
		return Release{}, fmt.Errorf("github: no release for tag %s (status %d)", tag, resp.StatusCode)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == 0 {
		return Release{}, fmt.Errorf("github: release %s: unreadable response", tag)
	}
	return Release{ID: out.ID, Tag: tag}, nil
}

func (c *GitHubClient) do(ctx context.Context, method, rawURL, contentType string, body []byte) (*http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, nil, fmt.Errorf("github: %s: %w", method, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	hc := c.HTTP
	if hc == nil {
		// The guarded client, never a bare one with a blanket timeout: a
		// zero-value GitHubClient must not quietly get weaker rules than a
		// constructed one — including the no-whole-exchange-timeout rule that
		// asset uploads depend on.
		hc = (&Guard{}).Client()
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The URL can carry a token in no case here, but Redacted() is cheap
		// insurance against a future one.
		return nil, nil, fmt.Errorf("github: %s %s: %w", method, redact(rawURL), err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, raw, nil
}

func escapePathSegments(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func redact(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Redacted()
	}
	return "<unparseable url>"
}
