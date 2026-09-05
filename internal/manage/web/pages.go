package web

// The server-rendered operator pages.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/clawee-git/release/internal/manage/auth"
	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/store"
)

// pageData is what every template's layout reads.
type pageData struct {
	Title    string
	Admin    string
	CSRF     string
	Error    string
	Channels []string
	Channel  string
}

// render writes a page. A template failure is logged and answered as a 500
// rather than left half-written: html/template may already have flushed bytes,
// so the operator sees a truncated page and the log says why.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, status int, data any) {
	t, ok := s.pages[name]
	if !ok {
		s.Log.Error("render: unknown template", "name", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.Log.Error("render", "template", name, "err", err, "path", r.URL.Path)
	}
}

// ── Login ────────────────────────────────────────────────────────────────

type loginPage struct {
	pageData
	Next string
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// An already-authenticated visitor is sent on rather than shown a form
	// they do not need.
	if _, err := s.Auth.Session(r); err == nil {
		http.Redirect(w, r, "/manage", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", http.StatusOK, loginPage{
		pageData: pageData{Title: "Sign in", Channels: catalog.Channels},
		Next:     safeNext(r.URL.Query().Get("next")),
	})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, r, "login", http.StatusBadRequest, loginPage{
			pageData: pageData{Title: "Sign in", Error: "could not read the form"}})
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	next := safeNext(r.PostFormValue("next"))

	enrolment, csrf, err := s.Auth.StartLogin(w, r, name, r.PostFormValue("password"))
	if err != nil {
		// The three cases are one message. Telling an attacker which half was
		// wrong is the entire value of a login oracle; the rate limit is the
		// one thing worth naming, because an operator who hits it needs to
		// know to wait rather than to keep trying.
		msg := "wrong account or password"
		if errors.Is(err, auth.ErrRateLimited) {
			msg = "too many attempts; wait a few minutes and try again"
		} else if !errors.Is(err, auth.ErrBadCredentials) {
			s.Log.Error("login", "err", err, "name", name)
			msg = "sign-in failed"
		}
		s.render(w, r, "login", http.StatusUnauthorized, loginPage{
			pageData: pageData{Title: "Sign in", Error: msg}, Next: next})
		return
	}

	// The enrolment secret is rendered INLINE rather than redirected to,
	// because it exists only in this response: it is sealed in the catalog and
	// there is no route that reads it back. A redirect would need somewhere to
	// stash it, and any such place is a second copy of the secret.
	s.render(w, r, "totp", http.StatusOK, totpPage{
		pageData:  pageData{Title: "Verification code", CSRF: csrf},
		Enrolment: enrolment,
		Next:      next,
	})
}

type totpPage struct {
	pageData
	Enrolment *auth.Enrolment
	Next      string
}

func (s *Server) handleTOTPPage(w http.ResponseWriter, r *http.Request) {
	sess, err := s.Auth.PendingSession(r)
	if err != nil {
		http.Redirect(w, r, "/manage/login", http.StatusSeeOther)
		return
	}
	if sess.MFAOK {
		http.Redirect(w, r, "/manage", http.StatusSeeOther)
		return
	}
	csrf, err := s.Auth.CSRFToken(w, r, sess)
	if err != nil {
		s.Log.Error("totp page: csrf", "err", err)
	}
	s.render(w, r, "totp", http.StatusOK, totpPage{
		pageData: pageData{Title: "Verification code", CSRF: csrf},
		Next:     safeNext(r.URL.Query().Get("next")),
	})
}

func (s *Server) handleTOTPSubmit(w http.ResponseWriter, r *http.Request) {
	sess, err := s.Auth.PendingSession(r)
	if err != nil {
		http.Redirect(w, r, "/manage/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}
	// The code step is a write, so it is CSRF-gated like every other write —
	// the session it acts on is the half-authenticated one.
	if err := s.Auth.CheckCSRF(r, sess); err != nil {
		http.Error(w, "missing or invalid CSRF token", http.StatusForbidden)
		return
	}
	next := safeNext(r.PostFormValue("next"))
	csrf, _ := s.Auth.CSRFToken(w, r, sess)

	if err := s.Auth.CompleteTOTP(r, r.PostFormValue("code")); err != nil {
		msg := "that code is not right"
		if errors.Is(err, auth.ErrRateLimited) {
			msg = "too many attempts; wait a few minutes and try again"
		} else if !errors.Is(err, auth.ErrBadCode) {
			s.Log.Error("totp", "err", err, "admin", sess.Admin)
			msg = "sign-in failed"
		}
		s.render(w, r, "totp", http.StatusUnauthorized, totpPage{
			pageData: pageData{Title: "Verification code", CSRF: csrf, Error: msg}, Next: next})
		return
	}
	if next == "" {
		next = "/manage"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}
	// Logout is a write: without the CSRF check any page on the internet could
	// sign an operator out mid-promote.
	if sess, err := s.Auth.PendingSession(r); err == nil {
		if err := s.Auth.CheckCSRF(r, sess); err != nil {
			http.Error(w, "missing or invalid CSRF token", http.StatusForbidden)
			return
		}
	}
	s.Auth.Logout(w, r)
	http.Redirect(w, r, "/manage/login", http.StatusSeeOther)
}

// safeNext bounds a post-login redirect to this service's own manage surface.
// Anything else — an absolute URL, a scheme-relative "//evil", a public path —
// is dropped: an open redirect on a login page is how a phishing link borrows
// this domain's name.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/manage") && !strings.HasPrefix(next, "//") {
		return next
	}
	return ""
}

// ── The manage pages ─────────────────────────────────────────────────────

type indexPage struct {
	pageData
	Cards []cardView
}

type cardView struct {
	Component  string
	Current    *rowView
	Unpromoted *rowView
}

type rowView struct {
	ID         int64
	Component  string
	Version    string
	Stamp      string
	State      string
	IsCurrent  bool
	CreatedAt  string
	PromotedAt string
	YankedAt   string
}

func toView(rv *store.ReleaseVersion) *rowView {
	if rv == nil {
		return nil
	}
	v := &rowView{
		ID: rv.ID, Component: rv.Component, Version: rv.Version, Stamp: rv.Stamp,
		State: rv.State, IsCurrent: rv.IsCurrent, CreatedAt: rv.CreatedAt.Format(timeFormat),
	}
	if !rv.PromotedAt.IsZero() {
		v.PromotedAt = rv.PromotedAt.Format(timeFormat)
	}
	if !rv.YankedAt.IsZero() {
		v.YankedAt = rv.YankedAt.Format(timeFormat)
	}
	return v
}

func (s *Server) handleIndexPage(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	channel, err := channelParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	csrf, cerr := s.Auth.CSRFToken(w, r, sess)
	if cerr != nil {
		s.Log.Error("index: csrf", "err", cerr)
	}
	page := indexPage{pageData: pageData{
		Title: "Releases", Admin: sess.Admin, CSRF: csrf,
		Channels: catalog.Channels, Channel: channel,
	}}
	for _, comp := range catalog.Components {
		card := cardView{Component: comp}
		cur, err := s.Store.CurrentPublic(comp, channel)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.pageError(w, r, err)
			return
		}
		card.Current = toView(cur)
		un, err := s.Store.Unpromoted(comp, channel)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.pageError(w, r, err)
			return
		}
		card.Unpromoted = toView(un)
		page.Cards = append(page.Cards, card)
	}
	s.render(w, r, "index", http.StatusOK, page)
}

type historyPage struct {
	pageData
	Component string
	Versions  []*rowView
}

func (s *Server) handleHistoryPage(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	comp := r.PathValue("comp")
	if !catalog.ValidComponent(comp) {
		http.Error(w, "unknown component "+comp, http.StatusBadRequest)
		return
	}
	channel, err := channelParam(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := s.Store.ListByComponent(comp, channel)
	if err != nil {
		s.pageError(w, r, err)
		return
	}
	csrf, _ := s.Auth.CSRFToken(w, r, sess)
	page := historyPage{
		pageData:  pageData{Title: comp + " history", Admin: sess.Admin, CSRF: csrf, Channels: catalog.Channels, Channel: channel},
		Component: comp,
	}
	for i := range rows {
		page.Versions = append(page.Versions, toView(&rows[i]))
	}
	s.render(w, r, "history", http.StatusOK, page)
}

type invitesPage struct {
	pageData
	Invites []inviteView
}

type inviteView struct {
	Stamp     string
	MintedBy  string
	URL       string
	CreatedAt string
	ExpiresAt string
	Live      bool
}

func (s *Server) handleInvitesPage(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	invites, err := s.Store.ListInvites()
	if err != nil {
		s.pageError(w, r, err)
		return
	}
	csrf, _ := s.Auth.CSRFToken(w, r, sess)
	page := invitesPage{pageData: pageData{
		Title: "Invites", Admin: sess.Admin, CSRF: csrf, Channels: catalog.Channels,
	}}
	now := s.Now()
	for _, inv := range invites {
		v := inviteView{
			MintedBy: inv.MintedBy, CreatedAt: inv.CreatedAt.Format(timeFormat),
			ExpiresAt: inv.ExpiresAt.Format(timeFormat), Live: inv.Live(now),
		}
		// The URL is served ONLY while the link is live. Offering copy-again
		// for a dead link hands an operator a URL that answers 403 and makes
		// the invitee think the build is broken.
		if v.Live {
			v.URL = inv.URL
		}
		if row, err := s.Store.Get(inv.RowID); err == nil {
			v.Stamp = row.Component + " " + row.Stamp
		}
		page.Invites = append(page.Invites, v)
	}
	s.render(w, r, "invites", http.StatusOK, page)
}

// ── Public ───────────────────────────────────────────────────────────────

type publicPage struct {
	pageData
	Channels []publicChannel
}

type publicChannel struct {
	Channel string
	Rows    []*rowView
}

// handlePublicIndex is feature 03's placeholder. It runs exactly ONE query —
// CurrentPublic — so "the public surface shows only promoted rows" is a
// property of the code rather than a rule the next author has to remember.
func (s *Server) handlePublicIndex(w http.ResponseWriter, r *http.Request) {
	page := publicPage{pageData: pageData{Title: "Clawee"}}
	for _, ch := range catalog.Channels {
		pc := publicChannel{Channel: ch}
		for _, comp := range catalog.Components {
			cur, err := s.Store.CurrentPublic(comp, ch)
			if err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					s.pageError(w, r, err)
					return
				}
				continue
			}
			pc.Rows = append(pc.Rows, toView(cur))
		}
		page.Channels = append(page.Channels, pc)
	}
	s.render(w, r, "public", http.StatusOK, page)
}

func (s *Server) pageError(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("manage page", "err", err, "path", r.URL.Path)
	http.Error(w, "the catalog could not be read", http.StatusInternalServerError)
}
