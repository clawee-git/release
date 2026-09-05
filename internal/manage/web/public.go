package web

// The PUBLIC surface: the pages release.clawee.org serves to anyone. It used
// to be a static index.html scp'd onto the host by the cut; it is rendered
// from the catalog now, so the version a visitor reads and the version the
// installers resolve cannot disagree.
//
// EVERY handler here reads the catalog through CurrentPublic or PublicHistory,
// and neither can return a `staged` row. That is the property the whole file
// is arranged around: a staged cut is a build an operator has NOT approved,
// and a public page naming its version or linking its bytes would publish it
// as surely as promote does. The rule is enforced by the store's method set
// rather than by a filter each handler remembers to apply, and a test renders
// every page over a catalog full of staged rows to prove it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/publish"
	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/manifest"
	"github.com/clawee-git/release/internal/register"
	"github.com/clawee-git/release/internal/staticsurface"
)

// PublicConfig is the non-secret configuration the public pages need in order
// to write absolute links. All three are flags with NO defaults: a guessed
// bucket URL is a download link to somebody else's bytes, and a guessed GitHub
// repo is a release link to a project we do not publish.
//
// A missing value degrades the page rather than breaking it — the download
// table simply carries no link — because the site must still come up on a
// deployment that has only wired half its seams.
type PublicConfig struct {
	// BaseURL is this service's own origin, used for the curl install lines.
	// The bootstraps are static files served from the same host.
	BaseURL string
	// DownloadsBase is the public bucket's origin (--public-base-url). Object
	// keys under it are the channel layout internal/manifest defines.
	DownloadsBase string
	// GitHubRepo is owner/repo (--github-repo), for the release links.
	GitHubRepo string
}

// publicNav is the site's own navigation, in order. It is a table rather than
// markup in the layout so the link check has something to walk and so the
// "current" mark is decided once.
var publicNav = []struct{ path, label string }{
	{"/", "Install"},
	{"/downloads", "Downloads"},
	{"/verify", "Verify"},
	{"/platforms", "Platforms"},
	{"/docs", "Docs"},
}

// publicPageData is what the public layout reads.
type publicPageData struct {
	Title      string
	Nav        []navLink
	PubkeyFile string
}

type navLink struct {
	Path    string
	Label   string
	Current bool
}

func (s *Server) publicData(title, current string) publicPageData {
	d := publicPageData{Title: title, PubkeyFile: staticsurface.Pubkey}
	for _, n := range publicNav {
		d.Nav = append(d.Nav, navLink{Path: n.path, Label: n.label, Current: n.path == current})
	}
	return d
}

// renderPublic writes a public page. It is deliberately NOT s.render: that one
// stamps Cache-Control: no-store, which is right for a page carrying a CSRF
// token and wrong for a page an anonymous visitor may fetch a hundred times an
// hour. These pages change only when an operator promotes, so they carry a
// short shared-cache window instead.
func (s *Server) renderPublic(w http.ResponseWriter, r *http.Request, name string, status int, data any) {
	t, ok := s.publicPages[name]
	if !ok {
		s.Log.Error("renderPublic: unknown template", "name", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "publiclayout", data); err != nil {
		s.Log.Error("renderPublic", "template", name, "err", err, "path", r.URL.Path)
	}
}

// ── Install ──────────────────────────────────────────────────────────────

type installPage struct {
	publicPageData
	Components []installComponent
}

type installComponent struct {
	Name          string
	Label         string
	IsDaemon      bool
	StableCommand string
	BetaCommand   string
	Stable        *channelBadge
	Beta          *channelBadge
}

// channelBadge is the version badge release-management.md §6 asks for: the
// version a channel is CURRENTLY serving and when it started serving it. Both
// come off the promoted row, never off versions/<comp> — that file is what the
// next cut will be numbered, which is not what anyone is running.
type channelBadge struct {
	Version    string
	Stamp      string
	PromotedAt string
}

// componentLabel is the one-line description beside each install command,
// carried over from the static page.
func componentLabel(comp string) string {
	switch comp {
	case catalog.ComponentDaemon:
		return comp + " — the PTY daemon"
	default:
		return comp + " — the terminal client"
	}
}

func (s *Server) handlePublicIndex(w http.ResponseWriter, r *http.Request) {
	page := installPage{publicPageData: s.publicData("Install", "/")}
	for _, comp := range catalog.Components {
		ic := installComponent{
			Name:     comp,
			Label:    componentLabel(comp),
			IsDaemon: comp == catalog.ComponentDaemon,
			// The bootstrap names are the static surface's, which is the one
			// list publish-static copies — so a command line here cannot name
			// a file nobody publishes.
			StableCommand: s.installCommand(comp + "/install.sh"),
			BetaCommand:   s.installCommand(comp + "/beta.install.sh"),
		}
		var err error
		if ic.Stable, err = s.badge(comp, catalog.ChannelStable); err != nil {
			s.pageError(w, r, err)
			return
		}
		// The beta line appears ONLY while a beta row is current. An always-on
		// beta command is a command that installs the last beta forever after
		// the cycle graduated, which is a downgrade dressed as an upgrade.
		if ic.Beta, err = s.badge(comp, catalog.ChannelBeta); err != nil {
			s.pageError(w, r, err)
			return
		}
		page.Components = append(page.Components, ic)
	}
	s.renderPublic(w, r, "install", http.StatusOK, page)
}

// installCommand renders the curl line for one bootstrap.
func (s *Server) installCommand(file string) string {
	base := strings.TrimSuffix(s.Public.BaseURL, "/")
	return "curl -fsSL --proto '=https' --tlsv1.2 " + base + "/" + file + " | sh"
}

// badge reads the channel's current promoted row, or nil when the channel is
// serving nothing.
func (s *Server) badge(comp, channel string) (*channelBadge, error) {
	cur, err := s.Store.CurrentPublic(comp, channel)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &channelBadge{
		Version: cur.Version, Stamp: cur.Stamp,
		PromotedAt: cur.PromotedAt.Format(dateFormat),
	}, nil
}

// dateFormat is the public pages' date: a day, not a timestamp. Nobody reading
// "which version am I on" needs the second, and a UTC clock time on a public
// page reads as more precision than the promotion decision has.
const dateFormat = "2006-01-02"

// ── Downloads ────────────────────────────────────────────────────────────

type downloadsPage struct {
	publicPageData
	Channel    string
	Tabs       []navLink
	Components []downloadsComponent
}

type downloadsComponent struct {
	Name    string
	Current *releaseView
	History []releaseView
}

type releaseView struct {
	Version    string
	Stamp      string
	PromotedAt string
	Yanked     bool
	Expired    bool
	Assets     []assetLink
	SumsURL    string
	MinisigURL string
	GitHubURL  string
}

type assetLink struct {
	Name     string
	Platform string
	URL      string
}

func (s *Server) handlePublicDownloads(w http.ResponseWriter, r *http.Request) {
	channel, err := channelParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	page := downloadsPage{
		publicPageData: s.publicData("Downloads", "/downloads"),
		Channel:        channel,
	}
	for _, ch := range catalog.Channels {
		page.Tabs = append(page.Tabs, navLink{
			Path: "/downloads?channel=" + ch, Label: ch, Current: ch == channel,
		})
	}
	for _, comp := range catalog.Components {
		dc := downloadsComponent{Name: comp}
		// ONE query, and it cannot return a staged row. The current release is
		// picked out of the same result rather than fetched separately, so the
		// page cannot show a "current" the history disagrees with.
		rows, err := s.Store.PublicHistory(comp, channel)
		if err != nil {
			s.pageError(w, r, err)
			return
		}
		for i := range rows {
			v := s.releaseView(&rows[i])
			if rows[i].IsCurrent && dc.Current == nil {
				dc.Current = &v
				continue
			}
			dc.History = append(dc.History, v)
		}
		page.Components = append(page.Components, dc)
	}
	s.renderPublic(w, r, "downloads", http.StatusOK, page)
}

// releaseView turns one promoted row into links into the public bucket's
// CHANNEL layout — the same keys promote copied the bytes to, derived from the
// same manifest.PublicBase, because a download page that computed the layout
// itself would 404 the day the layout moved.
//
// Neither an EXPIRED nor a YANKED row gets links, for two different reasons
// that land in the same place. Retention has pruned an expired row's bytes, so
// a link to them is a 404 that reads as a broken site rather than as the
// deliberate end of that build's life. A yanked row's bytes are usually still
// there — yank withdraws the release, it does not delete the objects or the
// GitHub release — and that is exactly why the links must go: a withdrawn
// build is one somebody decided nobody should install, and a public page that
// still hands out its zip and its signature is offering it anyway. The row
// stays visible and marked, because hiding it would rewrite the record.
func (s *Server) releaseView(rv *store.ReleaseVersion) releaseView {
	v := releaseView{
		Version: rv.Version, Stamp: rv.Stamp,
		Yanked:  rv.State == catalog.StateYanked,
		Expired: rv.State == catalog.StateExpired,
	}
	if !rv.PromotedAt.IsZero() {
		v.PromotedAt = rv.PromotedAt.Format(dateFormat)
	}
	if v.Expired || v.Yanked {
		return v
	}
	if base := s.objectBase(rv); base != "" {
		var artifacts []register.Artifact
		if err := json.Unmarshal([]byte(rv.ArtifactsJSON), &artifacts); err != nil {
			// A row whose artifact list will not parse is a catalog problem,
			// not a reason to fail the page: the release still has a stamp, a
			// version and a GitHub release, and those are what the reader
			// mostly came for.
			s.Log.Error("public: unreadable artifact list", "row", rv.ID, "err", err)
		}
		for _, a := range artifacts {
			name := path.Base(a.Key)
			v.Assets = append(v.Assets, assetLink{
				Name: name, Platform: a.Platform, URL: base + "/" + name,
			})
		}
		if rv.SumsKey != "" {
			v.SumsURL = base + "/" + path.Base(rv.SumsKey)
		}
		if rv.MinisigKey != "" {
			v.MinisigURL = base + "/" + path.Base(rv.MinisigKey)
		}
	}
	if repo := strings.TrimSpace(s.Public.GitHubRepo); repo != "" {
		v.GitHubURL = "https://github.com/" + repo + "/releases/tag/" +
			publish.ReleaseTag(rv.Component, rv.Stamp)
	}
	return v
}

// objectBase is the public bucket URL prefix a promoted row's files live
// under, or "" when no bucket base is configured.
func (s *Server) objectBase(rv *store.ReleaseVersion) string {
	base := strings.TrimSuffix(s.Public.DownloadsBase, "/")
	if base == "" {
		return ""
	}
	return base + "/" + manifest.PublicBase(rv.Component, rv.Channel, rv.Stamp)
}

// ── Verify · Platforms · Docs ────────────────────────────────────────────

func (s *Server) handlePublicVerify(w http.ResponseWriter, r *http.Request) {
	s.renderPublic(w, r, "verify", http.StatusOK, s.publicData("Verify", "/verify"))
}

func (s *Server) handlePublicPlatforms(w http.ResponseWriter, r *http.Request) {
	s.renderPublic(w, r, "platforms", http.StatusOK, s.publicData("Platforms", "/platforms"))
}

type docsPage struct {
	publicPageData
	Refs         []docRef
	InstallBase  string
	ManifestBase string
}

type docRef struct {
	Command string
	What    string
	RefName string
	URL     string
}

func (s *Server) handlePublicDocs(w http.ResponseWriter, r *http.Request) {
	page := docsPage{
		publicPageData: s.publicData("Docs", "/docs"),
		InstallBase:    strings.TrimSuffix(s.Public.BaseURL, "/"),
		ManifestBase:   strings.TrimSuffix(s.Public.DownloadsBase, "/"),
	}
	if page.ManifestBase == "" {
		// Naming a manifest host we were not configured with would be an
		// invented host in a page anyone can read.
		page.ManifestBase = "<the downloads base this channel is configured with>"
	}
	page.Refs = []docRef{
		{Command: "clawee docs", What: "the terminal client's command reference"},
		{Command: "claweed docs", What: "the PTY daemon's command reference"},
		{Command: "clawee-release-manage docs", What: "this service's own command reference",
			RefName: "docs/cli-help.md", URL: s.repoFileURL("docs/cli-help.md")},
	}
	s.renderPublic(w, r, "docs", http.StatusOK, page)
}

// repoFileURL links a file in the release repo, or "" when no repo is
// configured. Each COMPONENT's reference lives in that component's own repo
// and this service is told about neither, so those are named by the command
// that prints them rather than by a URL guessed from this one.
func (s *Server) repoFileURL(file string) string {
	repo := strings.TrimSpace(s.Public.GitHubRepo)
	if repo == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/blob/main/%s", repo, file)
}
