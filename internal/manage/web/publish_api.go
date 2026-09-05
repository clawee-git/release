package web

// Promote and yank: the two operator acts that change what every installer
// fetches next.
//
// Both stream. A promote moves several hundred megabytes, and a blind
// synchronous one is indistinguishable from a hung one — an operator who
// cannot tell those apart eventually kills a promote mid-copy, which is the
// one thing the ordered pipeline cannot protect them from.
//
// Streaming means the status code is spent on the FIRST byte, so every refusal
// that deserves its own code is decided before the stream opens: a missing
// publisher is 503, an unknown row 404, a wrong state 409. Once the body
// starts, a failure is an error EVENT in the stream — which is why Promote
// emits one as well as returning an error.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/publish"
	"github.com/clawee-git/release/internal/manage/store"
)

func (s *Server) publishDeps() publish.Deps {
	return publish.Deps{
		Store: s.Store, Staging: s.Backends.Staging, Public: s.Backends.Public,
		GitHub: s.Backends.GitHub, Now: s.Now, Log: s.Log,
		Retain: s.retain,
	}
}

// handleReleaseAction serves PATCH /api/v1/manage/releases/{id}.
func (s *Server) handleReleaseAction(w http.ResponseWriter, r *http.Request, _ *store.Session) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad row id"})
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request body: " + err.Error()})
		return
	}
	if req.Action != "promote" && req.Action != "yank" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown action %q; the actions are promote, yank", req.Action)})
		return
	}
	if status, msg := s.preflight(id, req.Action); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// NDJSON: one JSON object per line, flushed as it happens. Not SSE, and
	// not a JSON array — an array cannot be read until it closes, which is the
	// property being avoided.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if req.Action == "promote" {
		_ = publish.Promote(r.Context(), s.publishDeps(), id, w)
		return
	}
	_ = publish.Yank(r.Context(), s.publishDeps(), id, w)
}

// preflight answers the refusals that deserve a status code, before any byte
// of the stream is written. A zero status means "go ahead".
func (s *Server) preflight(id int64, action string) (int, string) {
	if s.Backends.Public == nil || s.Backends.Staging == nil {
		return http.StatusServiceUnavailable,
			"this service has no object store configured; nothing can be published"
	}
	if action == "promote" && s.Backends.GitHub == nil {
		// Fail closed, and say which piece is missing. A beta row has no
		// public path other than the GitHub release.
		return http.StatusServiceUnavailable,
			"this service has no GitHub publisher configured; promote fails closed rather than " +
				"publish a release that installers can reach and humans cannot find"
	}
	row, err := s.Store.Get(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return http.StatusNotFound, fmt.Sprintf("no release row %d", id)
		}
		return http.StatusInternalServerError, "the catalog could not be read"
	}
	switch action {
	case "promote":
		if row.State != catalog.StateStaged {
			return http.StatusConflict,
				fmt.Sprintf("row %d is %s; only a staged row can be promoted", id, row.State)
		}
	case "yank":
		if row.State != catalog.StatePublic {
			return http.StatusConflict,
				fmt.Sprintf("row %d is %s; only a public row can be yanked", id, row.State)
		}
	}
	return 0, ""
}

// handleActionPage is the page-form twin: POST /manage/releases/{id}/promote
// and /yank. It streams a plain-text log, which is all an operator watching a
// promote needs — the structured stream is the API's.
func (s *Server) handleActionPage(action string) func(http.ResponseWriter, *http.Request, *store.Session) {
	return func(w http.ResponseWriter, r *http.Request, _ *store.Session) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad row id", http.StatusBadRequest)
			return
		}
		if status, msg := s.preflight(id, action); status != 0 {
			http.Error(w, msg, status)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		lw := &lineWriter{w: w}
		if f, ok := w.(http.Flusher); ok {
			lw.flusher = f
		}
		if action == "promote" {
			err = publish.Promote(r.Context(), s.publishDeps(), id, lw)
		} else {
			err = publish.Yank(r.Context(), s.publishDeps(), id, lw)
		}
		if err != nil {
			fmt.Fprintf(lw, "\n✗ %s failed: %v\n", action, err)
		} else {
			fmt.Fprintf(lw, "\n✓ %s finished\n", action)
		}
		fmt.Fprint(lw, "\nreturn to /manage\n")
	}
}

// lineWriter turns the pipeline's NDJSON into readable lines. It is a Writer
// so the SAME pipeline feeds both surfaces — a second rendering path inside
// promote would be a second place for the two to disagree about what happened.
type lineWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (l *lineWriter) Write(p []byte) (int, error) {
	var e publish.Event
	if err := json.Unmarshal(p, &e); err != nil {
		// Not an event: the handler's own trailing lines.
		n, err := l.w.Write(p)
		l.flush()
		return n, err
	}
	line := e.Step
	if e.File != "" {
		line += " " + e.File
	}
	line += " — " + e.Status
	if e.Detail != "" {
		line += " (" + e.Detail + ")"
	}
	if e.Error != "" {
		line += ": " + e.Error
	}
	fmt.Fprintln(l.w, line)
	l.flush()
	return len(p), nil
}

func (l *lineWriter) flush() {
	if l.flusher != nil {
		l.flusher.Flush()
	}
}

// retain is the retention pass promote runs at the end. A method rather than a
// closure so the "cannot prune" case is answered in exactly one place — and it
// is answered by publish.Retain itself, which refuses rather than expiring
// rows whose bytes it could not remove.
func (s *Server) retain(ctx context.Context, component, channel string) []publish.Event {
	return publish.Retain(ctx, s.publishDeps(), component, channel)
}
