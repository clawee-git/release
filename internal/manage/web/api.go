package web

// The read surfaces. Both are session-gated: a staged row names an unreleased
// stamp and the objects behind it, which is not public information until
// promote makes it so.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/register"
)

// releaseJSON is the wire shape of one catalog row. It is a separate struct
// from store.ReleaseVersion on purpose: the store's shape is free to grow
// columns the API does not publish, and artifacts travel as a decoded array
// rather than as the opaque string the store holds.
type releaseJSON struct {
	ID         int64               `json:"id"`
	Component  string              `json:"component"`
	Channel    string              `json:"channel"`
	Version    string              `json:"version"`
	Stamp      string              `json:"stamp"`
	State      string              `json:"state"`
	IsCurrent  bool                `json:"is_current"`
	Artifacts  []register.Artifact `json:"artifacts"`
	SumsKey    string              `json:"sums_key"`
	MinisigKey string              `json:"minisig_key"`
	CreatedAt  string              `json:"created_at"`
	PromotedAt string              `json:"promoted_at,omitempty"`
	YankedAt   string              `json:"yanked_at,omitempty"`
}

func toJSON(rv *store.ReleaseVersion) *releaseJSON {
	if rv == nil {
		return nil
	}
	out := &releaseJSON{
		ID: rv.ID, Component: rv.Component, Channel: rv.Channel, Version: rv.Version,
		Stamp: rv.Stamp, State: rv.State, IsCurrent: rv.IsCurrent,
		SumsKey: rv.SumsKey, MinisigKey: rv.MinisigKey,
		CreatedAt: rv.CreatedAt.Format(timeFormat),
	}
	if !rv.PromotedAt.IsZero() {
		out.PromotedAt = rv.PromotedAt.Format(timeFormat)
	}
	if !rv.YankedAt.IsZero() {
		out.YankedAt = rv.YankedAt.Format(timeFormat)
	}
	// An unreadable artifacts blob is left empty rather than failing the whole
	// listing: the row's identity is still useful, and the operator needs to
	// see that the row exists in order to act on it.
	if err := json.Unmarshal([]byte(rv.ArtifactsJSON), &out.Artifacts); err != nil {
		out.Artifacts = nil
	}
	return out
}

const timeFormat = "2006-01-02 15:04:05Z"

// handleVersionSummary serves
// GET /api/v1/manage/releases/{channel}/versions — the per-component
// {current, unpromoted} pair the manage page's cards render from.
func (s *Server) handleVersionSummary(w http.ResponseWriter, r *http.Request, _ *store.Session) {
	channel := channelOfPath(r)
	type card struct {
		Component  string       `json:"component"`
		Current    *releaseJSON `json:"current"`
		Unpromoted *releaseJSON `json:"unpromoted"`
	}
	out := struct {
		Channel    string `json:"channel"`
		Components []card `json:"components"`
	}{Channel: channel}

	for _, comp := range catalog.Components {
		c := card{Component: comp}
		cur, err := s.Store.CurrentPublic(comp, channel)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.apiError(w, r, err)
			return
		}
		c.Current = toJSON(cur)

		un, err := s.Store.Unpromoted(comp, channel)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.apiError(w, r, err)
			return
		}
		c.Unpromoted = toJSON(un)
		out.Components = append(out.Components, c)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleVersionDetail serves
// GET /api/v1/manage/releases/{channel}/versions/{comp} — the full history,
// every state, newest first.
func (s *Server) handleVersionDetail(w http.ResponseWriter, r *http.Request, _ *store.Session) {
	channel := channelOfPath(r)
	comp := r.PathValue("comp")
	if !catalog.ValidComponent(comp) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown component %q; the components are %s", comp, strings.Join(catalog.Components, ", ")),
		})
		return
	}
	rows, err := s.Store.ListByComponent(comp, channel)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	versions := make([]*releaseJSON, 0, len(rows))
	for i := range rows {
		versions = append(versions, toJSON(&rows[i]))
	}
	writeJSON(w, http.StatusOK, struct {
		Channel   string         `json:"channel"`
		Component string         `json:"component"`
		Versions  []*releaseJSON `json:"versions"`
	}{Channel: channel, Component: comp, Versions: versions})
}

// handleManageAPIFallback answers anything under /api/v1/manage/ that no
// registered route claimed. It runs BEHIND the session guard, so an
// unauthenticated caller gets 401 everywhere in the subtree and cannot map the
// surface by reading which paths 404.
//
// A …/versions path with an unrecognised channel lands here, because the
// channel routes are registered one literal per known channel. That is the
// 400 the acceptance criteria ask for, and it is produced by the routing
// table's shape rather than by a second list of channels.
func (s *Server) handleManageAPIFallback(w http.ResponseWriter, r *http.Request, _ *store.Session) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/manage/releases/")
	if parts := strings.Split(rest, "/"); len(parts) >= 2 && parts[1] == "versions" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown channel %q; the channels are %s", parts[0], strings.Join(catalog.Channels, ", ")),
		})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such manage endpoint"})
}

// channelOfPath reads the channel literal out of the request path. The routes
// are registered per channel, so by the time a handler runs the segment is
// known-good — this only has to find it.
func channelOfPath(r *http.Request) string {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/manage/releases/"), "/")
	if len(parts) > 0 && catalog.ValidChannel(parts[0]) {
		return parts[0]
	}
	return catalog.ChannelStable
}

// apiError is the one place a store failure becomes a response. It logs the
// real error and returns a generic one: the operator reading the log needs the
// detail, the browser does not.
func (s *Server) apiError(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("manage api", "err", err, "path", r.URL.Path)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the catalog could not be read"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
