// Package web is the manage service's HTTP surface: the unauthenticated
// registration endpoints, the session-gated read API, and the server-rendered
// operator pages.
//
// The routing SPLIT is the point of this package (release-management.md §6):
//
//	/                       public — reads promoted rows only, nothing else
//	/api/v1/releases/*      machine-authenticated (nonce + release signature)
//	/api/v1/manage/*        session-gated; writes CSRF-gated as well
//	/manage/*               the same, rendered as pages
//
// The public half can only ever see what promote made public, because the only
// query it runs is CurrentPublic. That is a property of the code rather than a
// rule someone has to remember when adding the download page in feature 03.
//
// Pages are server-rendered from embedded html/template files. There is no SPA
// build: this surface is a handful of forms an operator uses a few times a
// month, and a build step would be the largest single thing to keep working.
package web

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/clawee-git/release/internal/manage/auth"
	"github.com/clawee-git/release/internal/manage/backend"
	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/intake"
	"github.com/clawee-git/release/internal/manage/store"
)

//go:embed templates
var templateFS embed.FS

// Backends carries the remote-store seams. Each may be nil, and each nil one
// disables exactly the operation that needs it — with a 503 that names the
// missing piece, never a silent no-op. A manage service with no GitHub
// publisher can still show the catalog and mint invites; what it cannot do is
// promote, and it says so.
type Backends struct {
	Staging backend.Staging
	Public  backend.Public
	GitHub  backend.GitHub
}

// Server holds the surface's dependencies.
type Server struct {
	Store    *store.Store
	Auth     *auth.Service
	Intake   *intake.Handler
	Backends Backends
	Log      *slog.Logger
	Now      func() time.Time

	pages map[string]*template.Template
}

// New builds the server and parses the templates. A template that fails to
// parse is a startup error, not a 500 the first operator to visit discovers.
func New(st *store.Store, a *auth.Service, in *intake.Handler, backends Backends, log *slog.Logger, now func() time.Time) (*Server, error) {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Server{Store: st, Auth: a, Intake: in, Backends: backends, Log: log, Now: now,
		pages: map[string]*template.Template{}}
	for _, name := range []string{"login", "totp", "index", "history", "invites", "public"} {
		t, err := template.ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse template %q: %w", name, err)
		}
		s.pages[name] = t
	}
	return s, nil
}

// Handler builds the mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ── Registration: machine-authenticated, never session-gated ──────────
	mux.HandleFunc("/api/v1/releases/nonce", s.Intake.HandleNonce)
	mux.HandleFunc("/api/v1/releases/register", s.Intake.HandleRegister)

	// ── Read API. The per-channel paths are DERIVED from catalog.Channels,
	// so a channel the validator accepts always has a route: a channel with
	// no route is a 405 wearing a 200's clothes.
	for _, ch := range catalog.Channels {
		mux.HandleFunc("GET /api/v1/manage/releases/"+ch+"/versions", s.apiGuard(s.handleVersionSummary))
		mux.HandleFunc("GET /api/v1/manage/releases/"+ch+"/versions/{comp}", s.apiGuard(s.handleVersionDetail))
	}

	// ── Batch B's write API. These exist NOW, answering 501, so the pages
	// that link to them are final and the routing split is complete.
	mux.HandleFunc("PATCH /api/v1/manage/releases/{id}", s.apiGuard(s.handleNotImplementedAPI))
	mux.HandleFunc("POST /api/v1/manage/releases/{channel}/install-url", s.apiGuard(s.handleInstallURL))
	mux.HandleFunc("GET /api/v1/manage/releases/{channel}/invites", s.apiGuard(s.handleInvitesAPI))

	// Everything else under the manage API. It exists so an unauthenticated
	// request to ANY /api/v1/manage/* path is a 401 rather than a 404 that
	// tells an anonymous caller which routes are real.
	mux.HandleFunc("/api/v1/manage/", s.apiGuard(s.handleManageAPIFallback))

	// ── Operator pages ────────────────────────────────────────────────────
	mux.HandleFunc("GET /manage/login", s.handleLoginPage)
	mux.HandleFunc("POST /manage/login", s.handleLoginSubmit)
	mux.HandleFunc("GET /manage/login/totp", s.handleTOTPPage)
	mux.HandleFunc("POST /manage/login/totp", s.handleTOTPSubmit)
	mux.HandleFunc("POST /manage/logout", s.handleLogout)
	// Both spellings, not one plus the mux's 301: an operator who types
	// /manage must land on the page, not on a redirect that drops the method
	// on anything but a GET.
	mux.HandleFunc("GET /manage", s.pageGuard(s.handleIndexPage))
	mux.HandleFunc("GET /manage/{$}", s.pageGuard(s.handleIndexPage))
	mux.HandleFunc("GET /manage/invites", s.pageGuard(s.handleInvitesPage))
	mux.HandleFunc("GET /manage/releases/{comp}", s.pageGuard(s.handleHistoryPage))
	mux.HandleFunc("POST /manage/releases/{id}/mint", s.pageGuard(s.handleMintPage))
	for _, action := range []string{"promote", "yank"} {
		mux.HandleFunc("POST /manage/releases/{id}/"+action, s.pageGuard(s.handleNotImplementedPage))
	}
	// Anything else under /manage/ goes through the guard too, so an anonymous
	// visitor gets the login form rather than a 404 that maps which manage
	// pages exist. It is the page half of the same rule the manage API keeps.
	mux.HandleFunc("/manage/", s.pageGuard(s.handleNotFoundPage))

	// ── Public ────────────────────────────────────────────────────────────
	mux.HandleFunc("GET /{$}", s.handlePublicIndex)
	mux.HandleFunc("/", s.handleNotFound)

	return mux
}

// apiGuard is session + CSRF for a JSON route. Both refusals are JSON, because
// a caller that asked for JSON and got an HTML login page cannot tell a
// refusal from a bug.
func (s *Server) apiGuard(h func(http.ResponseWriter, *http.Request, *store.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.Auth.Session(r)
		if err != nil {
			if !errors.Is(err, auth.ErrUnauthorized) {
				s.Log.Error("api: session", "err", err, "path", r.URL.Path)
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in at /manage/login"})
			return
		}
		if err := s.Auth.CheckCSRF(r, sess); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "missing or invalid CSRF token; echo the " + auth.CSRFCookie + " cookie in " + auth.CSRFHeader,
			})
			return
		}
		h(w, r, sess)
	}
}

// pageGuard is the same for a rendered page: an unauthenticated browser is
// sent to the login form with a return path, which is what a person needs;
// a CSRF failure is still a refusal, not a redirect.
func (s *Server) pageGuard(h func(http.ResponseWriter, *http.Request, *store.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.Auth.Session(r)
		if err != nil {
			// A half-authenticated session goes to the code page rather than
			// back to the password form it has already satisfied.
			if pending, perr := s.Auth.PendingSession(r); perr == nil && !pending.MFAOK {
				http.Redirect(w, r, "/manage/login/totp", http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, "/manage/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if err := s.Auth.CheckCSRF(r, sess); err != nil {
			http.Error(w, "missing or invalid CSRF token", http.StatusForbidden)
			return
		}
		h(w, r, sess)
	}
}

// channelParam resolves the ?channel= query for a page, defaulting to stable
// and refusing anything outside the vocabulary.
func channelParam(r *http.Request) (string, error) {
	ch := r.URL.Query().Get("channel")
	if ch == "" {
		return catalog.ChannelStable, nil
	}
	if !catalog.ValidChannel(ch) {
		return "", fmt.Errorf("unknown channel %q; the channels are %s", ch, strings.Join(catalog.Channels, ", "))
	}
	return ch, nil
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not found", http.StatusNotFound)
}

// handleNotFoundPage is the 404 an AUTHENTICATED operator gets for a manage
// path that does not exist. An anonymous one never reaches it: pageGuard sent
// them to the login form first.
func (s *Server) handleNotFoundPage(w http.ResponseWriter, r *http.Request, _ *store.Session) {
	http.Error(w, "no such manage page", http.StatusNotFound)
}
