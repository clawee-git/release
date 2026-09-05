package web

// Promote, yank, mint and the invite listing are batch B. Their routes exist
// NOW and answer 501, which is deliberate:
//
//   - The pages that link to them are FINAL. A button rendered disabled with a
//     "coming soon" tooltip is a page that has to be rewritten later, and a
//     rewritten page is one whose CSRF wiring, form action and session gating
//     all get a second chance to be wrong.
//   - The routing split is complete and testable today: every one of these is
//     already session- and CSRF-gated, so batch B fills in a body rather than
//     adding a surface.
//
// What batch B must implement, by name, is listed in the manual (AGENTS.md,
// "The manage service"): the object store and GitHub publisher interfaces, the
// promote sequence, yank's manifest re-point, the invite mint, and retention.

import (
	"net/http"

	"github.com/clawee-git/release/internal/manage/store"
)

const notImplementedBody = "not implemented yet: promote, yank, mint invite and the invite listing " +
	"land in the second half of this feature. The row is registered and visible; going live is still " +
	"an operator action that has no implementation on this build."

func (s *Server) handleNotImplementedAPI(w http.ResponseWriter, r *http.Request, _ *store.Session) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": notImplementedBody})
}

func (s *Server) handleNotImplementedPage(w http.ResponseWriter, r *http.Request, _ *store.Session) {
	http.Error(w, notImplementedBody, http.StatusNotImplemented)
}
