package web

// The public surface's own tests. The one they all exist for is the first:
// a `staged` row is a build NOBODY has approved, and every page here renders
// over a catalog that is full of them.

import (
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/staticsurface"
)

// publicPaths is every page the public surface serves, including the download
// page in both channel spellings. Tests walk this list rather than naming
// pages one at a time, so a page added without a staged-row test is a page the
// staged-row test already covers.
var publicPaths = []string{
	"/", "/downloads", "/downloads?channel=stable", "/downloads?channel=beta",
	"/verify", "/platforms", "/docs",
}

// stagedZip is the artifact basename only the staged rows carry, so "no staged
// link appears" can be asserted on a string that could only have come from one.
const stagedZip = "clawee-STAGED-ONLY-darwin-arm64.zip"

// stageRow inserts one row with a version and an artifact name of its own.
func (f *fixture) stageRow(comp, channel, stamp, version, zip string) int64 {
	f.t.Helper()
	id, err := f.st.Stage(store.ReleaseVersion{
		Component: comp, Channel: channel, Version: version, Stamp: stamp,
		ArtifactsJSON: `[{"platform":"darwin/arm64","key":"` + comp + "/" + channel + "/" + stamp + "/" + zip +
			`","sha256":"aa","size":12}]`,
		SumsKey:    comp + "/" + channel + "/" + stamp + "/SHA256SUMS.txt",
		MinisigKey: comp + "/" + channel + "/" + stamp + "/SHA256SUMS.txt.minisig",
		CreatedAt:  f.now,
	})
	if err != nil {
		f.t.Fatalf("Stage: %v", err)
	}
	return id
}

func (f *fixture) promoteRow(id int64, at time.Time) {
	f.t.Helper()
	if err := f.st.Promote(id, at); err != nil {
		f.t.Fatalf("Promote: %v", err)
	}
}

// seedCatalog builds a catalog carrying every state the public pages can meet:
// a current release, an older one it superseded, an expired one, a yanked one,
// and — on both channels — a staged row nobody promoted.
func seedCatalog(f *fixture) {
	t := f.t
	t.Helper()
	for _, comp := range catalog.Components {
		a := f.stageRow(comp, catalog.ChannelStable, "v0.2.1.2026.09.01.aaaaaaaa", "0.2.1", "zip-a.zip")
		b := f.stageRow(comp, catalog.ChannelStable, "v0.2.2.2026.09.02.bbbbbbbb", "0.2.2", "zip-b.zip")
		c := f.stageRow(comp, catalog.ChannelStable, "v0.2.3.2026.09.03.cccccccc", "0.2.3", "zip-c.zip")
		f.promoteRow(a, f.now.Add(1*time.Minute))
		f.promoteRow(b, f.now.Add(2*time.Minute))
		f.promoteRow(c, f.now.Add(3*time.Minute))
		// keep=1 leaves the current row alone, keeps one behind it, expires
		// the rest — so `a` becomes the expired row the page must gray out.
		if _, err := f.st.ExpireOldVersions(comp, catalog.ChannelStable, 1, f.now); err != nil {
			t.Fatal(err)
		}
		// Yanking the current one moves the channel back onto `b` and leaves
		// `c` in the history marked withdrawn.
		if err := f.st.Yank(c, b, f.now.Add(4*time.Minute)); err != nil {
			t.Fatal(err)
		}
		// The staged rows: one per channel, and the reason this file exists.
		f.stageRow(comp, catalog.ChannelStable, "v0.9.9.2026.09.04.99999999", "0.9.9", stagedZip)
		f.stageRow(comp, catalog.ChannelBeta, "v0.9.9.beta.2026.09.04.99999999", "0.9.9-beta", stagedZip)
	}
}

func TestNoStagedRowReachesAnyPublicPage(t *testing.T) {
	f := newFixture(t)
	seedCatalog(f)
	c := f.client()

	// Everything that could only have come from a staged row: its stamps, its
	// version, and the artifact basename only staged rows carry.
	forbidden := []string{
		"v0.9.9.2026.09.04.99999999",
		"v0.9.9.beta.2026.09.04.99999999",
		"0.9.9",
		stagedZip,
		catalog.StateStaged,
	}
	for _, path := range publicPaths {
		resp, body := f.get(c, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: HTTP %d", path, resp.StatusCode)
		}
		for _, bad := range forbidden {
			if strings.Contains(body, bad) {
				t.Errorf("%s: renders %q — a staged build is published on the public surface", path, bad)
			}
		}
	}

	// The negative assertion above is worthless if the pages render nothing,
	// so prove the promoted rows DO appear.
	_, downloads := f.get(c, "/downloads?channel=stable")
	for _, want := range []string{"0.2.2", "v0.2.2.2026.09.02.bbbbbbbb", "zip-b.zip"} {
		if !strings.Contains(downloads, want) {
			t.Errorf("/downloads is missing the promoted %q; the staged check above proves nothing", want)
		}
	}
}

func TestExpiredRowsAreShownWithoutLinksAndYankedRowsAreMarked(t *testing.T) {
	f := newFixture(t)
	seedCatalog(f)
	_, body := f.get(f.client(), "/downloads?channel=stable")

	if !strings.Contains(body, "v0.2.1.2026.09.01.aaaaaaaa") {
		t.Error("the expired release is missing from the history; it happened, and hiding it rewrites the record")
	}
	if strings.Contains(body, "zip-a.zip") {
		t.Error("the expired release links its bytes; retention pruned them and the link is a 404")
	}
	if !strings.Contains(body, "yanked") {
		t.Error("the yanked release is not marked; a withdrawn build must not read as an ordinary old one")
	}
}

func TestDownloadLinksPointAtThePublicBucketsChannelLayout(t *testing.T) {
	f := newFixture(t)
	seedCatalog(f)
	betaID := f.stageRow(catalog.ComponentCLI, catalog.ChannelBeta,
		"v0.3.0.beta.2026.09.05.dddddddd", "0.3.0-beta", "zip-beta.zip")
	f.promoteRow(betaID, f.now.Add(5*time.Minute))

	_, stable := f.get(f.client(), "/downloads?channel=stable")
	wantStable := testPublicConfig.DownloadsBase + "/clawee/v0.2.2.2026.09.02.bbbbbbbb/zip-b.zip"
	if !strings.Contains(stable, wantStable) {
		t.Errorf("stable download link missing: %s", wantStable)
	}
	if !strings.Contains(stable, "https://github.com/clawee-git/release/releases/tag/clawee/v0.2.2.2026.09.02.bbbbbbbb") {
		t.Error("the GitHub release link does not name the promoted tag")
	}

	_, beta := f.get(f.client(), "/downloads?channel=beta")
	// The channel is a PATH SEGMENT in the public layout, the same one
	// internal/manifest writes — a beta link without it points at the stable
	// prefix, where the bytes are not.
	wantBeta := testPublicConfig.DownloadsBase + "/clawee/beta/v0.3.0.beta.2026.09.05.dddddddd/zip-beta.zip"
	if !strings.Contains(beta, wantBeta) {
		t.Errorf("beta download link missing: %s", wantBeta)
	}
}

func TestTheBetaInstallLineAppearsOnlyWhileABetaIsCurrent(t *testing.T) {
	f := newFixture(t)
	seedCatalog(f)
	const betaCmd = "/clawee/beta.install.sh"

	_, body := f.get(f.client(), "/")
	if strings.Contains(body, betaCmd) {
		t.Fatal("the beta install line is offered with no beta promoted; it would install nothing, or the last cycle's build forever")
	}

	id := f.stageRow(catalog.ComponentCLI, catalog.ChannelBeta,
		"v0.3.0.beta.2026.09.05.dddddddd", "0.3.0-beta", "zip-beta.zip")
	f.promoteRow(id, f.now.Add(5*time.Minute))

	_, body = f.get(f.client(), "/")
	if !strings.Contains(body, betaCmd) {
		t.Fatal("a beta is current and the beta install line is missing")
	}
	if !strings.Contains(body, "0.3.0-beta") {
		t.Fatal("the beta version badge is missing while a beta is current")
	}
	// claweed has no beta, so its twin must still be absent: the line is per
	// COMPONENT and per channel, not a site-wide flag.
	if strings.Contains(body, "/claweed/beta.install.sh") {
		t.Fatal("claweed's beta line appears although claweed has no beta promoted")
	}
}

func TestTheStableInstallLineAndVersionBadgeAreRenderedPerChannel(t *testing.T) {
	f := newFixture(t)
	seedCatalog(f)
	_, body := f.get(f.client(), "/")
	// The command is compared through html/template's own escaping (the
	// single quotes around --proto '=https' come back as &#39;), because that
	// is the text a reader actually copies off the page.
	for _, want := range []string{
		template.HTMLEscapeString("curl -fsSL --proto '=https' --tlsv1.2 " + testPublicConfig.BaseURL + "/clawee/install.sh | sh"),
		template.HTMLEscapeString("curl -fsSL --proto '=https' --tlsv1.2 " + testPublicConfig.BaseURL + "/claweed/install.sh | sh"),
		"0.2.2",
		"never with sudo",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the install page is missing %q", want)
		}
	}
}

// hrefRe pulls every href out of a rendered page. It is deliberately dumb: the
// pages are ours, and a parser here would be a second HTML implementation to
// keep in step with html/template's escaping.
//
// The quantifier is `*`, not `+`, and that is the point rather than a detail.
// With `+` an <a href=""> — a link whose URL was never configured — matched
// nothing at all, so the one malformed href this check exists to find was the
// one shape it could not see.
var hrefRe = regexp.MustCompile(`href="([^"]*)"`)

func TestNoPublicPageLinksSomewhereThatDoesNotExist(t *testing.T) {
	f := newFixture(t)
	seedCatalog(f)
	c := f.client()

	for _, page := range publicPaths {
		_, body := f.get(c, page)
		for _, m := range hrefRe.FindAllStringSubmatch(body, -1) {
			href := m[1]
			// An EMPTY href is the worst of the three: it is not absolute, so
			// a "check only the relative ones" rule skips it, and in a browser
			// it reloads the page the reader is already on. It means a URL the
			// template expected was not configured, and the guard around it is
			// missing.
			if href == "" {
				t.Errorf("%s emits an empty href — a link whose URL was never configured", page)
				continue
			}
			if !strings.HasPrefix(href, "/") || strings.HasPrefix(href, "//") {
				continue // absolute links leave this deployment; not ours to check
			}
			// A file the release host serves as static bytes is checked
			// against the list publish-static actually copies — that is the
			// whole point of there being ONE list.
			if staticsurface.Has(strings.TrimPrefix(href, "/")) {
				continue
			}
			resp, _ := f.get(c, href)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s links to %s, which answers HTTP %d and is not a published static file",
					page, href, resp.StatusCode)
			}
		}
	}
}

// A deployment can be brought up before its buckets are wired, and the public
// site must come up with it. What it must NOT do is render links to a base it
// was never given: an <a href=""> reloads the page the reader is on, and it
// passes any link check that only inspects the relative ones.
func TestAnUnconfiguredDeploymentRendersNoLinksRatherThanEmptyOnes(t *testing.T) {
	f := newFixtureWithPublic(t, PublicConfig{BaseURL: "https://release.example.invalid"})
	seedCatalog(f)
	c := f.client()
	for _, page := range publicPaths {
		resp, body := f.get(c, page)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: HTTP %d with no bucket or repo configured", page, resp.StatusCode)
		}
		for _, m := range hrefRe.FindAllStringSubmatch(body, -1) {
			if m[1] == "" {
				t.Errorf("%s emits an empty href with no --public-base-url set", page)
			}
		}
	}
	// And the install lines still work, because the bootstraps are served from
	// this service's own host rather than from the bucket.
	_, install := f.get(c, "/")
	if !strings.Contains(install, "/clawee/install.sh") {
		t.Error("the install command is missing on an otherwise unconfigured deployment")
	}
}

func TestPublicPagesNeedNoSessionAndAreCacheable(t *testing.T) {
	f := newFixture(t)
	seedCatalog(f)
	anon := f.client()
	for _, page := range publicPaths {
		resp, _ := f.get(anon, page)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s anonymously: HTTP %d", page, resp.StatusCode)
		}
		// The public pages carry no token and no session, so no-store would
		// only cost the host a request per visitor per page.
		if cc := resp.Header.Get("Cache-Control"); strings.Contains(cc, "no-store") {
			t.Errorf("%s is marked no-store; it carries nothing session-bound", page)
		}
	}
}

func TestAnUnknownChannelOnTheDownloadPageIsRefused(t *testing.T) {
	f := newFixture(t)
	resp, _ := f.get(f.client(), "/downloads?channel=nightly")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("HTTP %d, want 400", resp.StatusCode)
	}
}
