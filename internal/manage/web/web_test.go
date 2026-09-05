package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawee-git/release/internal/manage/auth"
	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/intake"
	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/manage/totp"
)

const (
	adminName     = "ada"
	adminPassword = "correct-horse-battery"
	stableStamp   = "v0.2.28.2026.09.04.deadbeef"
	betaStamp     = "v0.3.0.beta.2026.09.04.deadbeef"
)

type fixture struct {
	t      *testing.T
	st     *store.Store
	auth   *auth.Service
	sealer *auth.Sealer
	server *httptest.Server
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sealer, err := auth.LoadSealer(filepath.Join(dir, auth.SecretKeyFile))
	if err != nil {
		t.Fatalf("LoadSealer: %v", err)
	}
	// The clock is injected but anchored to the REAL now: session and CSRF
	// cookies carry absolute expiry times, and a jar drops a cookie that
	// expired before the test started. A fixed historical instant made every
	// login in this file silently cookie-less.
	f := &fixture{t: t, st: st, sealer: sealer, now: time.Now().UTC().Truncate(time.Second)}
	// secure=false: httptest speaks http, and a Secure cookie would never be
	// sent back — the same reason the real service derives it from --base-url.
	f.auth = auth.New(st, sealer, false, func() time.Time { return f.now })
	in, err := intake.New(st, "https://release.example.invalid", func() time.Time { return f.now }, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("intake.New: %v", err)
	}
	srv, err := New(st, f.auth, in, slog.New(slog.DiscardHandler), func() time.Time { return f.now })
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	f.server = httptest.NewServer(srv.Handler())
	t.Cleanup(f.server.Close)
	return f
}

// stage inserts a staged row directly, so read-surface tests do not have to
// run the signing handshake (intake's own tests cover that).
func (f *fixture) stage(comp, channel, stamp string, at time.Time) int64 {
	f.t.Helper()
	id, err := f.st.Stage(store.ReleaseVersion{
		Component: comp, Channel: channel, Version: "0.2.28", Stamp: stamp,
		ArtifactsJSON: `[{"platform":"darwin/arm64","key":"` + comp + "/" + channel + "/" + stamp +
			`/x.zip","sha256":"aa","size":12}]`,
		SumsKey: "s", MinisigKey: "m", CreatedAt: at,
	})
	if err != nil {
		f.t.Fatalf("Stage: %v", err)
	}
	return id
}

// client returns an HTTP client with a cookie jar that does NOT follow
// redirects, so a test sees the 303 itself.
func (f *fixture) client() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// login drives the real password + TOTP flow over HTTP and returns an
// authenticated client plus its CSRF token.
func (f *fixture) login(c *http.Client) string {
	f.t.Helper()
	if err := f.auth.AddAdmin(adminName, adminPassword); err != nil {
		f.t.Fatalf("AddAdmin: %v", err)
	}
	resp, err := c.PostForm(f.server.URL+"/manage/login", url.Values{
		"name": {adminName}, "password": {adminPassword},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	body := readBody(f.t, resp)
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("login: HTTP %d %s", resp.StatusCode, body)
	}
	// The enrolment page shows the secret once. The test reads it from the
	// catalog instead, through the sealer, which also proves the secret really
	// was sealed rather than stored in the clear.
	admin, err := f.st.Admin(adminName)
	if err != nil {
		f.t.Fatal(err)
	}
	secretBytes, err := f.sealer.Open(admin.TOTPSecretEnc)
	if err != nil {
		f.t.Fatalf("the enrolled secret does not unseal: %v", err)
	}
	if !strings.Contains(body, string(secretBytes)) {
		f.t.Fatal("the enrolment page did not show the secret; it exists only in that response")
	}

	// The token comes off the RENDERED FORM, not the jar: that is the path a
	// browser takes, and it is the one that broke when the handler tried to
	// read the session back out of a request whose cookies had only just been
	// set on the response.
	csrf := csrfFieldOf(f.t, body)
	code, _ := totp.Code(string(secretBytes), f.now)
	resp, err = c.PostForm(f.server.URL+"/manage/login/totp", url.Values{
		"code": {code}, "csrf_token": {csrf},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		f.t.Fatalf("totp: HTTP %d %s", resp.StatusCode, readBody(f.t, resp))
	}
	return f.csrfFrom(c)
}

// csrfFieldOf pulls the hidden csrf_token value out of a rendered form.
func csrfFieldOf(t *testing.T, html string) string {
	t.Helper()
	const marker = `name="csrf_token" value="`
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatal("the rendered form carries no csrf_token field")
	}
	rest := html[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j <= 0 {
		t.Fatal("the rendered form's csrf_token field is empty; the first sign-in would fail its own CSRF check")
	}
	return rest[:j]
}

func (f *fixture) csrfFrom(c *http.Client) string {
	u, _ := url.Parse(f.server.URL)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == auth.CSRFCookie {
			return ck.Value
		}
	}
	return ""
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

func (f *fixture) get(c *http.Client, path string) (*http.Response, string) {
	f.t.Helper()
	resp, err := c.Get(f.server.URL + path)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp, readBody(f.t, resp)
}

// ── AC1: the manage API is closed, registration is not ───────────────────

func TestEveryManageAPIRouteIs401WithoutASession(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	paths := []struct {
		method, path string
	}{
		{"GET", "/api/v1/manage/releases/stable/versions"},
		{"GET", "/api/v1/manage/releases/beta/versions"},
		{"GET", "/api/v1/manage/releases/stable/versions/clawee"},
		{"GET", "/api/v1/manage/releases/beta/invites"},
		{"PATCH", "/api/v1/manage/releases/1"},
		{"POST", "/api/v1/manage/releases/beta/install-url"},
		// An unknown channel and an entirely made-up path answer 401 too: an
		// anonymous caller must not be able to map the surface by reading
		// which paths 404 and which do not.
		{"GET", "/api/v1/manage/releases/nightly/versions"},
		{"GET", "/api/v1/manage/anything/at/all"},
	}
	for _, p := range paths {
		req, _ := http.NewRequest(p.method, f.server.URL+p.path, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s: HTTP %d %s, want 401", p.method, p.path, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s %s: refusal Content-Type %q; a JSON caller cannot read an HTML login page", p.method, p.path, ct)
		}
	}
}

func TestRegistrationEndpointsNeedNoSession(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	resp, err := c.Post(f.server.URL+"/api/v1/releases/nonce", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"nonce"`) {
		t.Fatalf("nonce without a session: HTTP %d %s", resp.StatusCode, body)
	}
	// register refuses on its own terms (bad payload), not for want of a
	// session: a 401 here would mean the cut needs an admin credential.
	resp, err = c.Post(f.server.URL+"/api/v1/releases/register", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("register asked for a session: %s", body)
	}
}

func TestManagePagesRedirectToLoginWhenAnonymous(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	for _, path := range []string{"/manage", "/manage/invites", "/manage/releases/clawee"} {
		resp, _ := f.get(c, path)
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("%s: HTTP %d, want 303 to the login form", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/manage/login") {
			t.Errorf("%s: Location %q", path, loc)
		}
	}
}

// ── AC2: login needs both factors, writes need CSRF ──────────────────────

func TestPasswordAloneDoesNotReachTheManagePages(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	if err := f.auth.AddAdmin(adminName, adminPassword); err != nil {
		t.Fatal(err)
	}
	resp, err := c.PostForm(f.server.URL+"/manage/login", url.Values{
		"name": {adminName}, "password": {adminPassword}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// A half-authenticated session is sent to the CODE page, not back to the
	// password form it has already satisfied.
	got, _ := f.get(c, "/manage")
	if got.StatusCode != http.StatusSeeOther || got.Header.Get("Location") != "/manage/login/totp" {
		t.Fatalf("half-authenticated /manage: HTTP %d Location %q", got.StatusCode, got.Header.Get("Location"))
	}
	// And the API refuses it outright.
	api, body := f.get(c, "/api/v1/manage/releases/stable/versions")
	if api.StatusCode != http.StatusUnauthorized {
		t.Fatalf("half-authenticated API call: HTTP %d %s", api.StatusCode, body)
	}
}

func TestWrongCredentialsSayNothingUseful(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	if err := f.auth.AddAdmin(adminName, adminPassword); err != nil {
		t.Fatal(err)
	}
	for _, v := range []url.Values{
		{"name": {adminName}, "password": {"wrong-password-entirely"}},
		{"name": {"nobody"}, "password": {adminPassword}},
	} {
		resp, err := c.PostForm(f.server.URL+"/manage/login", v)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "wrong account or password") {
			t.Fatalf("%v: HTTP %d %s", v, resp.StatusCode, body)
		}
	}
}

func TestWritesWithoutTheCSRFTokenAre403(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	csrf := f.login(c)
	if csrf == "" {
		t.Fatal("no CSRF cookie after login")
	}

	// The session cookie alone is not enough: a cross-origin form POST carries
	// it (SameSite=Lax withholds it, but the check must not depend on that)
	// and cannot set the header.
	req, _ := http.NewRequest("PATCH", f.server.URL+"/api/v1/manage/releases/1", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "CSRF") {
		t.Fatalf("PATCH without a token: HTTP %d %s", resp.StatusCode, body)
	}

	// A form POST carrying the token passes the gate and reaches the handler,
	// which is batch B's 501.
	resp, err = c.PostForm(f.server.URL+"/manage/releases/1/promote", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("promote with a token: HTTP %d %s", resp.StatusCode, body)
	}

	// The same POST without it is refused before the handler.
	resp, err = c.PostForm(f.server.URL+"/manage/releases/1/promote", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("promote without a token: HTTP %d", resp.StatusCode)
	}

	// A GET is never gated: a read that demanded a token would be a read no
	// link could reach.
	got, _ := f.get(c, "/api/v1/manage/releases/stable/versions")
	if got.StatusCode != http.StatusOK {
		t.Fatalf("GET with a session: HTTP %d", got.StatusCode)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	csrf := f.login(c)
	resp, err := c.PostForm(f.server.URL+"/manage/logout", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got, _ := f.get(c, "/api/v1/manage/releases/stable/versions")
	if got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout: HTTP %d", got.StatusCode)
	}
}

// ── AC3: the read shapes, for both channels ──────────────────────────────

type summaryResponse struct {
	Channel    string `json:"channel"`
	Components []struct {
		Component  string       `json:"component"`
		Current    *releaseJSON `json:"current"`
		Unpromoted *releaseJSON `json:"unpromoted"`
	} `json:"components"`
}

func TestVersionSummaryShapeForBothChannels(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	csrf := f.login(c)
	_ = csrf

	promoted := f.stage(catalog.ComponentCLI, catalog.ChannelStable, stableStamp, f.now)
	if err := f.st.Promote(promoted, f.now); err != nil {
		t.Fatal(err)
	}
	newer := f.stage(catalog.ComponentCLI, catalog.ChannelStable, "v0.2.29.2026.09.05.deadbeef", f.now.Add(time.Hour))
	f.stage(catalog.ComponentCLI, catalog.ChannelBeta, betaStamp, f.now)

	for _, ch := range catalog.Channels {
		resp, body := f.get(c, "/api/v1/manage/releases/"+ch+"/versions")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: HTTP %d %s", ch, resp.StatusCode, body)
		}
		var got summaryResponse
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%s: %v\n%s", ch, err, body)
		}
		if got.Channel != ch {
			t.Errorf("%s: channel = %q", ch, got.Channel)
		}
		// Every component appears, whether or not it has rows: a card that
		// vanishes when a component has nothing staged is a card an operator
		// cannot tell from a component that no longer exists.
		if len(got.Components) != len(catalog.Components) {
			t.Fatalf("%s: %d components, want %d", ch, len(got.Components), len(catalog.Components))
		}
		daemon := got.Components[1]
		if daemon.Component != catalog.ComponentDaemon || daemon.Current != nil || daemon.Unpromoted != nil {
			t.Errorf("%s: a component with no rows = %+v", ch, daemon)
		}
	}

	resp, body := f.get(c, "/api/v1/manage/releases/stable/versions")
	var got summaryResponse
	json.Unmarshal([]byte(body), &got)
	cli := got.Components[0]
	if cli.Current == nil || cli.Current.ID != promoted || !cli.Current.IsCurrent {
		t.Fatalf("stable current = %+v, want row %d", cli.Current, promoted)
	}
	if cli.Unpromoted == nil || cli.Unpromoted.ID != newer || cli.Unpromoted.State != catalog.StateStaged {
		t.Fatalf("stable unpromoted = %+v, want row %d", cli.Unpromoted, newer)
	}
	if len(cli.Current.Artifacts) != 1 || cli.Current.Artifacts[0].Size != 12 {
		t.Fatalf("artifacts came through as %+v", cli.Current.Artifacts)
	}
	_ = resp

	// The beta row is on the beta channel and nowhere else.
	_, body = f.get(c, "/api/v1/manage/releases/beta/versions")
	json.Unmarshal([]byte(body), &got)
	if got.Components[0].Unpromoted == nil || got.Components[0].Unpromoted.Stamp != betaStamp {
		t.Fatalf("beta unpromoted = %+v", got.Components[0].Unpromoted)
	}
	if strings.Contains(body, stableStamp) {
		t.Fatal("a stable row appeared in the beta summary")
	}
}

func TestVersionDetailIsTheWholeHistoryNewestFirst(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	f.login(c)

	old := f.stage(catalog.ComponentCLI, catalog.ChannelStable, stableStamp, f.now)
	newer := f.stage(catalog.ComponentCLI, catalog.ChannelStable, "v0.2.29.2026.09.05.deadbeef", f.now.Add(time.Hour))
	if err := f.st.Promote(old, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Yank(old, f.now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	resp, body := f.get(c, "/api/v1/manage/releases/stable/versions/clawee")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d %s", resp.StatusCode, body)
	}
	var got struct {
		Channel   string         `json:"channel"`
		Component string         `json:"component"`
		Versions  []*releaseJSON `json:"versions"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Component != "clawee" || got.Channel != "stable" {
		t.Fatalf("got %+v", got)
	}
	if len(got.Versions) != 2 || got.Versions[0].ID != newer || got.Versions[1].ID != old {
		t.Fatalf("versions = %+v, want newest first", got.Versions)
	}
	// Every state is history, including the yanked one — that is what a
	// history page is for.
	if got.Versions[1].State != catalog.StateYanked || got.Versions[1].YankedAt == "" {
		t.Fatalf("yanked row = %+v", got.Versions[1])
	}
}

func TestUnknownChannelIs400AndUnknownComponentIs400(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	f.login(c)

	resp, body := f.get(c, "/api/v1/manage/releases/nightly/versions")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "unknown channel") {
		t.Fatalf("unknown channel: HTTP %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "stable, beta") {
		t.Fatalf("the refusal does not name the channels that exist: %s", body)
	}
	resp, body = f.get(c, "/api/v1/manage/releases/stable/versions/nope")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "unknown component") {
		t.Fatalf("unknown component: HTTP %d %s", resp.StatusCode, body)
	}
}

// ── Pages ────────────────────────────────────────────────────────────────

func TestManagePageRendersCardsAndTheBatchBButtons(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	f.login(c)
	promoted := f.stage(catalog.ComponentCLI, catalog.ChannelStable, stableStamp, f.now)
	if err := f.st.Promote(promoted, f.now); err != nil {
		t.Fatal(err)
	}
	staged := f.stage(catalog.ComponentCLI, catalog.ChannelStable, "v0.2.29.2026.09.05.deadbeef", f.now.Add(time.Hour))

	resp, body := f.get(c, "/manage")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d %s", resp.StatusCode, body)
	}
	for _, want := range []string{
		stableStamp, "v0.2.29.2026.09.05.deadbeef",
		`href="/manage?channel=stable"`, `href="/manage?channel=beta"`,
		"/manage/releases/" + itoa(staged) + "/promote",
		"/manage/releases/" + itoa(staged) + "/mint",
		"/manage/releases/" + itoa(promoted) + "/yank",
		"claweed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the manage page is missing %q", want)
		}
	}
	// The buttons are real forms with real CSRF tokens, not disabled
	// placeholders: batch B fills in a handler, not a page.
	if strings.Contains(body, "disabled") {
		t.Error("the page renders a disabled control; the buttons are meant to be live")
	}

	resp, body = f.get(c, "/manage?channel=nightly")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown channel on the page: HTTP %d %s", resp.StatusCode, body)
	}
}

func TestHistoryAndInvitesPagesRender(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	f.login(c)
	row := f.stage(catalog.ComponentCLI, catalog.ChannelBeta, betaStamp, f.now)

	resp, body := f.get(c, "/manage/releases/clawee?channel=beta")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, betaStamp) {
		t.Fatalf("history page: HTTP %d\n%s", resp.StatusCode, body)
	}
	resp, body = f.get(c, "/manage/releases/nope")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("history for an unknown component: HTTP %d %s", resp.StatusCode, body)
	}

	// An invite listing offers copy-again only while the link is live.
	if _, err := f.st.CreateInvite(store.Invite{
		RowID: row, MintedBy: adminName, ScriptKey: "k",
		URL: "https://staging.example.invalid/live", CreatedAt: f.now, ExpiresAt: f.now.Add(48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateInvite(store.Invite{
		RowID: row, MintedBy: adminName, ScriptKey: "k2",
		URL: "https://staging.example.invalid/dead", CreatedAt: f.now.Add(-72 * time.Hour), ExpiresAt: f.now.Add(-24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	resp, body = f.get(c, "/manage/invites")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("invites page: HTTP %d", resp.StatusCode)
	}
	if !strings.Contains(body, "/live") {
		t.Error("the live invite's command is not offered")
	}
	if strings.Contains(body, "/dead") {
		t.Error("an expired invite's URL is offered for copy-again; it would 403 for whoever ran it")
	}
}

// ── The public split ─────────────────────────────────────────────────────

func TestPublicIndexShowsOnlyPromotedRows(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	staged := f.stage(catalog.ComponentCLI, catalog.ChannelStable, stableStamp, f.now)

	resp, body := f.get(c, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	if strings.Contains(body, stableStamp) {
		t.Fatal("a staged stamp is on the public page — the private half of the split leaked")
	}

	if err := f.st.Promote(staged, f.now); err != nil {
		t.Fatal(err)
	}
	_, body = f.get(c, "/")
	if !strings.Contains(body, stableStamp) {
		t.Fatal("a promoted row is missing from the public page")
	}

	// And the public page needs no session at all.
	anon := f.client()
	resp, _ = f.get(anon, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public page anonymously: HTTP %d", resp.StatusCode)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	f := newFixture(t)
	resp, _ := f.get(f.client(), "/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("HTTP %d, want 404", resp.StatusCode)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
