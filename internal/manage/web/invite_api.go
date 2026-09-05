package web

// The invite surfaces: mint one, list them.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/invite"
	"github.com/clawee-git/release/internal/manage/store"
)

// handleInstallURL mints an invite.
// POST /api/v1/manage/releases/{channel}/install-url {component, version}
func (s *Server) handleInstallURL(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	channel := r.PathValue("channel")
	if !catalog.ValidChannel(channel) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown channel %q; the channels are %s", channel, strings.Join(catalog.Channels, ", ")),
		})
		return
	}
	if s.Backends.Staging == nil {
		// Fail closed and SAY so. An invite with no staging store is not a
		// degraded invite, it is no invite — and the operator needs to know it
		// is a configuration gap, not a bad row.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "this service has no staging store configured; an invite cannot be minted",
		})
		return
	}
	var req struct {
		Component string `json:"component"`
		Version   string `json:"version"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request body: " + err.Error()})
		return
	}
	if !catalog.ValidComponent(req.Component) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown component %q", req.Component)})
		return
	}
	row, err := s.Store.ByVersion(req.Component, channel, req.Version)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": fmt.Sprintf("no %s %s on %s", req.Component, req.Version, channel)})
			return
		}
		s.apiError(w, r, err)
		return
	}
	res, err := s.mint(r, row, sess.Admin)
	if err != nil {
		s.writeMintError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// handleInvitesAPI lists the audit trail for one channel.
// GET /api/v1/manage/releases/{channel}/invites
//
// The channel segment is HONOURED, not decoration. An invites listing that
// ignored it would answer the beta and stable URLs identically, which is worse
// than having one URL: a reader of the beta page would believe they were
// looking at beta mints. An invite has no channel of its own — it inherits the
// one of the row it installs — so the filter joins through that row.
func (s *Server) handleInvitesAPI(w http.ResponseWriter, r *http.Request, _ *store.Session) {
	channel := r.PathValue("channel")
	if !catalog.ValidChannel(channel) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown channel %q; the channels are %s", channel, strings.Join(catalog.Channels, ", ")),
		})
		return
	}
	invites, err := s.Store.ListInvites()
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	now := s.Now()
	type row struct {
		ID        int64  `json:"id"`
		RowID     int64  `json:"row_id"`
		Component string `json:"component"`
		Stamp     string `json:"stamp"`
		MintedBy  string `json:"minted_by"`
		CreatedAt string `json:"created_at"`
		ExpiresAt string `json:"expires_at"`
		Live      bool   `json:"live"`
		// Command is served ONLY while the link is live. Handing back the
		// command for a dead link is handing someone a URL that answers 403
		// and a reason to think the build is broken.
		Command string `json:"command,omitempty"`
	}
	out := make([]row, 0, len(invites))
	for _, inv := range invites {
		// The release row is resolved FIRST because it is what decides
		// membership. An invite whose row has gone is skipped rather than
		// listed channel-less: a row in a channel listing that belongs to no
		// channel is a row the reader cannot act on.
		rel, err := s.Store.Get(inv.RowID)
		if err != nil {
			s.Log.Warn("invite listing: release row is gone", "invite", inv.ID, "row", inv.RowID)
			continue
		}
		if rel.Channel != channel {
			continue
		}
		item := row{
			ID: inv.ID, RowID: inv.RowID, MintedBy: inv.MintedBy,
			Component: rel.Component, Stamp: rel.Stamp,
			CreatedAt: inv.CreatedAt.Format(timeFormat),
			ExpiresAt: inv.ExpiresAt.Format(timeFormat),
			Live:      inv.Live(now),
		}
		if item.Live {
			item.Command = invite.Command(inv.URL)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"channel": channel, "invites": out})
}

// handleMintPage is the page-form twin: POST /manage/releases/{id}/mint.
func (s *Server) handleMintPage(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad row id", http.StatusBadRequest)
		return
	}
	row, err := s.Store.Get(id)
	if err != nil {
		http.Error(w, "no such release row", http.StatusNotFound)
		return
	}
	if s.Backends.Staging == nil {
		http.Error(w, "this service has no staging store configured; an invite cannot be minted",
			http.StatusServiceUnavailable)
		return
	}
	if _, err := s.mint(r, row, sess.Admin); err != nil {
		if errors.Is(err, invite.ErrNotMintable) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		s.Log.Error("mint", "err", err, "row", id)
		http.Error(w, "the invite could not be minted", http.StatusInternalServerError)
		return
	}
	// Straight to the listing, which is where the command is read from — and
	// a redirect after a POST means a refresh does not mint a second link.
	http.Redirect(w, r, "/manage/invites", http.StatusSeeOther)
}

func (s *Server) mint(r *http.Request, row *store.ReleaseVersion, admin string) (invite.Result, error) {
	return invite.Mint(r.Context(), invite.Deps{
		Store: s.Store, Staging: s.Backends.Staging, Now: s.Now,
	}, row, admin)
}

func (s *Server) writeMintError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, invite.ErrNotMintable) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.Log.Error("mint", "err", err, "path", r.URL.Path)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the invite could not be minted"})
}
