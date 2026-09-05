package intake

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/register"
)

// NonceTTL is short on purpose: the kit fetches a nonce and posts the signed
// row in the same breath, so the only thing a longer window buys is a larger
// set of live challenges for a captured signature to be replayed against.
const NonceTTL = 5 * time.Minute

// maxBody caps the register body. A catalog row is a few kilobytes; anything
// approaching this is not one.
const maxBody = 1 << 20

// Handler serves the two unauthenticated registration endpoints.
//
// Unauthenticated in the session sense only — both are gated, one by the nonce
// it issues and one by a signature over the release key. There is no route
// here a browser session reaches, and no route the manage session gates.
type Handler struct {
	Store   *store.Store
	Key     ed25519.PublicKey
	KeyID   string
	BaseURL string
	Now     func() time.Time
	Log     *slog.Logger
}

// New builds a Handler over the baked release key.
func New(st *store.Store, baseURL string, now func() time.Time, log *slog.Logger) (*Handler, error) {
	key, keyID, err := ReleaseKey()
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		Store: st, Key: key, KeyID: keyID,
		BaseURL: strings.TrimRight(baseURL, "/"), Now: now, Log: log,
	}, nil
}

// HandleNonce issues a single-use challenge. POST /api/v1/releases/nonce.
func (h *Handler) HandleNonce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "this endpoint takes POST")
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		h.Log.Error("nonce: generate", "err", err)
		writeError(w, http.StatusInternalServerError, "could not issue a nonce")
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(raw)
	now := h.Now()
	if err := h.Store.IssueNonce(nonce, now, now.Add(NonceTTL)); err != nil {
		h.Log.Error("nonce: record", "err", err)
		writeError(w, http.StatusInternalServerError, "could not issue a nonce")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nonce":      nonce,
		"expires_in": int(NonceTTL.Seconds()),
	})
}

// HandleRegister records a signed catalog row.
// POST /api/v1/releases/register.
//
// The refusals and their statuses, and why each is the status it is:
//
//	400  the row is malformed, or its component/channel/stamp/keys disagree
//	403  the nonce is unknown, expired or spent — or the signature is wrong
//	409  this (component, channel, stamp) is already catalogued
//
// 403 covers all three nonce cases AND a bad signature deliberately: an
// attacker learns nothing about which half failed. The log line the operator
// reads afterwards does distinguish them, because a clock-skewed build host
// and a replayed capture need different answers.
func (h *Handler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "this endpoint takes POST")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request body")
		return
	}
	var p register.Payload
	dec := json.NewDecoder(strings.NewReader(string(body)))
	// A field this service does not know is a client this service does not
	// know. Accepting it silently would mean verifying a signature over bytes
	// that carry something the row never records.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "the request body is not a register payload: "+err.Error())
		return
	}

	if msg, ok := validate(p); !ok {
		h.Log.Warn("register: refused", "reason", msg, "component", p.Component, "stamp", p.Stamp)
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	// Signature BEFORE nonce: verification is stateless and cheap, and a
	// request that cannot be signed should not be able to spend a nonce.
	signed, err := p.SigningBytes()
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not canonicalise the payload: "+err.Error())
		return
	}
	sig, err := base64.StdEncoding.DecodeString(p.Signature)
	if err != nil || !ed25519.Verify(h.Key, signed, sig) {
		h.Log.Warn("register: bad signature", "component", p.Component, "stamp", p.Stamp,
			"key_id", h.KeyID, "remote", r.RemoteAddr)
		writeError(w, http.StatusForbidden, "signature does not verify against the release public key")
		return
	}

	if err := h.Store.ConsumeNonce(p.Nonce, h.Now()); err != nil {
		// The reason is logged and NOT returned: unknown, expired and spent
		// are one answer on the wire.
		h.Log.Warn("register: nonce refused", "err", err, "component", p.Component,
			"stamp", p.Stamp, "remote", r.RemoteAddr)
		writeError(w, http.StatusForbidden, "nonce is unknown, expired or already used")
		return
	}

	artifacts, err := json.Marshal(p.Artifacts)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not record the artifact list: "+err.Error())
		return
	}
	id, err := h.Store.Stage(store.ReleaseVersion{
		Component:     p.Component,
		Channel:       p.Channel,
		Version:       p.Version,
		Stamp:         p.Stamp,
		ArtifactsJSON: string(artifacts),
		SumsKey:       p.SumsKey,
		MinisigKey:    p.MinisigKey,
		CreatedAt:     h.Now(),
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrAlreadyExists):
			h.Log.Warn("register: duplicate", "component", p.Component, "channel", p.Channel, "stamp", p.Stamp)
			writeError(w, http.StatusConflict,
				fmt.Sprintf("%s %s on %s is already catalogued", p.Component, p.Stamp, p.Channel))
		case errors.Is(err, store.ErrBadValue):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			h.Log.Error("register: stage", "err", err)
			writeError(w, http.StatusInternalServerError, "could not record the row")
		}
		return
	}

	h.Log.Info("register: staged", "id", id, "component", p.Component, "channel", p.Channel,
		"stamp", p.Stamp, "artifacts", len(p.Artifacts))
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":    id,
		"state": catalog.StateStaged,
		// The kit prints this URL as "the row"; it is the operator's way from
		// a finished cut to the button that promotes it.
		"url": fmt.Sprintf("%s/manage/releases/%s?channel=%s#row-%d", h.BaseURL, p.Component, p.Channel, id),
	})
}

// validate is the boundary check on the payload. It runs BEFORE the signature
// so a malformed row is answered as malformed rather than as unsigned, and it
// checks one thing the store cannot: that every object key the row records
// lives under the prefix the cut claims.
//
// That last check matters because promote reads these keys back VERBATIM — the
// row is what says where the bytes are. A row whose keys point somewhere else
// is a row that publishes objects nobody staged for it.
func validate(p register.Payload) (string, bool) {
	if !catalog.ValidComponent(p.Component) {
		return fmt.Sprintf("unknown component %q", p.Component), false
	}
	if !catalog.ValidChannel(p.Channel) {
		return fmt.Sprintf("unknown channel %q", p.Channel), false
	}
	if !catalog.StampMatchesChannel(p.Stamp, p.Channel) {
		return fmt.Sprintf("stamp %q is not a %s stamp", p.Stamp, p.Channel), false
	}
	if strings.TrimSpace(p.Version) == "" {
		return "version is empty", false
	}
	if strings.TrimSpace(p.Nonce) == "" {
		return "nonce is empty", false
	}
	if strings.TrimSpace(p.Signature) == "" {
		return "signature is empty", false
	}
	if len(p.Artifacts) == 0 {
		return "the row lists no artifacts", false
	}
	base := register.KeyBase(p.Component, p.Channel, p.Stamp) + "/"
	for _, a := range p.Artifacts {
		if !strings.HasPrefix(a.Key, base) {
			return fmt.Sprintf("artifact key %q is not under %q", a.Key, base), false
		}
		if a.Size <= 0 {
			return fmt.Sprintf("artifact %q has size %d", a.Key, a.Size), false
		}
		if len(a.SHA256) != 64 {
			return fmt.Sprintf("artifact %q has a %d-character sha256", a.Key, len(a.SHA256)), false
		}
	}
	if !strings.HasPrefix(p.SumsKey, base) {
		return fmt.Sprintf("sums key %q is not under %q", p.SumsKey, base), false
	}
	if !strings.HasPrefix(p.MinisigKey, base) {
		return fmt.Sprintf("minisig key %q is not under %q", p.MinisigKey, base), false
	}
	return "", true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
