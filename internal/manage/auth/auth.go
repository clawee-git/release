// Package auth is the manage service's operator authentication: password plus
// TOTP, a session cookie, and a CSRF token bound to that session.
//
// Accounts are provisioned on the host with `clawee-release-manage admin add`
// and never self-registered (release-management.md §6). This service publishes
// releases; there is no signup surface to get wrong because there is no signup.
//
// Registration from the cut does NOT come through here — it is machine-
// authenticated by the release signing key (internal/manage/register). No
// admin credential ever lives in a release kit.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/manage/totp"
)

// The cookie namespace is the service's own — release-management.md §6 asks
// for a namespace no other surface shares, and these names are why a session
// on the public site could never be mistaken for a manage session.
const (
	SessionCookie = "clawee_manage_session"
	CSRFCookie    = "clawee_manage_csrf"
	// CSRFHeader is where a non-GET request must echo the CSRF cookie. A
	// header is the check, not a form field, because a cross-origin form POST
	// cannot set one and a same-origin fetch can.
	CSRFHeader = "X-CSRF-Token"

	// SessionTTL is deliberately short for a surface whose buttons publish
	// software; an operator promotes once and logs back in tomorrow.
	SessionTTL = 12 * time.Hour

	// Issuer is what an authenticator app labels the entry.
	Issuer = "Clawee Release"
)

// Errors the handlers map onto responses. They are distinguished here and
// DELIBERATELY collapsed at the boundary: the login page says "wrong username,
// password or code" for the first three, because telling an attacker which
// half was wrong is the whole value of a login oracle.
var (
	ErrBadCredentials = errors.New("auth: wrong username or password")
	ErrBadCode        = errors.New("auth: wrong verification code")
	ErrRateLimited    = errors.New("auth: too many attempts")
	ErrUnauthorized   = errors.New("auth: no valid session")
	ErrCSRF           = errors.New("auth: missing or invalid CSRF token")
	ErrNotEnrolled    = errors.New("auth: second factor not enrolled")
)

// Service carries the auth dependencies. Now is injected so the whole flow —
// session expiry, TOTP steps, rate-limit windows — is testable as arithmetic.
type Service struct {
	Store  *store.Store
	Sealer *Sealer
	Now    func() time.Time
	// Secure marks the cookies Secure. It is derived from the base URL's
	// scheme rather than hard-coded: a Secure cookie is never sent over the
	// http loopback a test or a first local run uses, so hard-coding true
	// makes the service unusable exactly where it is first tried.
	Secure  bool
	limiter *limiter
}

// New builds the service. now may be nil, meaning time.Now.
func New(st *store.Store, sealer *Sealer, secure bool, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{Store: st, Sealer: sealer, Now: now, Secure: secure, limiter: newLimiter()}
}

// AddAdmin provisions an account. It is the CLI's entry point; there is no
// HTTP route that reaches it.
func (s *Service) AddAdmin(name, password string) error {
	if err := ValidAdminName(name); err != nil {
		return err
	}
	if err := ValidPassword(password); err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return s.Store.CreateAdmin(name, hash, s.Now())
}

// ValidAdminName bounds what an account may be called. The name reaches an
// otpauth:// URI and every audit line, so it is restricted to a shape that
// needs no escaping anywhere it is rendered.
func ValidAdminName(name string) error {
	if len(name) < 2 || len(name) > 32 {
		return fmt.Errorf("admin name must be 2–32 characters, got %d", len(name))
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("admin name %q may only contain lowercase letters, digits, '-', '_' and '.'", name)
		}
	}
	return nil
}

// ValidPassword is a length floor, not a composition rule. Composition rules
// produce Passw0rd!; a floor plus a second factor is what actually holds.
func ValidPassword(password string) error {
	if len([]rune(password)) < 12 {
		return fmt.Errorf("password must be at least 12 characters")
	}
	return nil
}

// Enrolment is returned by StartLogin the ONE time an account enrols. The
// plaintext secret is shown once and never again: it is stored sealed, and
// there is no route that reads it back.
type Enrolment struct {
	Secret     string
	OTPAuthURL string
}

// StartLogin verifies the password and opens a half-authenticated session,
// setting the session and CSRF cookies. It returns a non-nil Enrolment when
// this login is the account's first — the caller must show the secret on the
// TOTP page, because it is the only time it exists in the clear — and the
// session's CSRF token.
//
// The token is RETURNED rather than left to be read back from the request,
// because the cookies were set on the RESPONSE: the request in hand does not
// carry them yet. A caller that tried to look it up would render the code form
// with an empty token, and the operator's first sign-in would fail the CSRF
// check on a page that looked perfectly normal.
//
// The session exists BEFORE the second factor because the TOTP step needs
// something to address, and it is useless until CompleteTOTP flips it: every
// gate in Session checks MFAOK.
func (s *Service) StartLogin(w http.ResponseWriter, r *http.Request, name, password string) (*Enrolment, string, error) {
	now := s.Now()
	key := passwordKey(ClientIP(r), name)
	if !s.limiter.allow(key, now) {
		return nil, "", ErrRateLimited
	}

	admin, err := s.Store.Admin(name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Hash a throwaway password anyway. Without it, an unknown account
			// answers in microseconds and a known one in the tens of
			// milliseconds argon2id costs — a timing oracle that enumerates
			// the admin list.
			_, _ = HashPassword("unknown-account-timing-equaliser")
			s.limiter.fail(key, now)
			return nil, "", ErrBadCredentials
		}
		return nil, "", err
	}
	ok, err := VerifyPassword(admin.PasswordHash, password)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		s.limiter.fail(key, now)
		return nil, "", ErrBadCredentials
	}
	// Clears the PASSWORD stage only. The second factor's counter is a
	// different key and is untouched here — see the comment on totpKey.
	s.limiter.succeed(key)

	var enrolment *Enrolment
	if !admin.Enrolled() {
		secret, err := totp.GenerateSecret()
		if err != nil {
			return nil, "", err
		}
		sealed, err := s.Sealer.Seal([]byte(secret))
		if err != nil {
			return nil, "", err
		}
		// Enrol before the session is usable, and only if no secret is there
		// already (the store enforces that). A second enrolment would be how
		// someone holding just the password fits their own second factor.
		if err := s.Store.EnrolTOTP(name, sealed, now); err != nil {
			return nil, "", err
		}
		enrolment = &Enrolment{Secret: secret, OTPAuthURL: totp.OTPAuthURL(Issuer, name, secret)}
	}

	sid, err := randomToken()
	if err != nil {
		return nil, "", err
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, "", err
	}
	expires := now.Add(SessionTTL)
	if err := s.Store.CreateSession(sid, name, now, expires); err != nil {
		return nil, "", err
	}
	if err := s.Store.CreateCSRF(csrf, sid, expires); err != nil {
		return nil, "", err
	}
	s.setCookie(w, SessionCookie, sid, expires, true)
	// The CSRF cookie is deliberately NOT HttpOnly: the page's own script has
	// to read it to echo it in the header. That is the double-submit pattern,
	// and it is safe precisely because a cross-origin page can neither read
	// this cookie nor set the header.
	s.setCookie(w, CSRFCookie, csrf, expires, false)
	return enrolment, csrf, nil
}

// CompleteTOTP verifies the code for the half-authenticated session in r and
// promotes the session to fully authenticated.
func (s *Service) CompleteTOTP(r *http.Request, code string) error {
	now := s.Now()
	sess, err := s.pendingSession(r)
	if err != nil {
		return err
	}
	key := totpKey(ClientIP(r), sess.Admin)
	if !s.limiter.allow(key, now) {
		return ErrRateLimited
	}
	admin, err := s.Store.Admin(sess.Admin)
	if err != nil {
		return err
	}
	if !admin.Enrolled() {
		return ErrNotEnrolled
	}
	secret, err := s.Sealer.Open(admin.TOTPSecretEnc)
	if err != nil {
		return err
	}
	step, ok := totp.Verify(string(secret), strings.TrimSpace(code), now, admin.TOTPLastStep)
	if !ok {
		s.limiter.fail(key, now)
		return ErrBadCode
	}
	// Advance the watermark BEFORE marking the session, so a crash between the
	// two leaves the code spent rather than replayable.
	if err := s.Store.SetTOTPStep(sess.Admin, step); err != nil {
		return err
	}
	s.limiter.succeed(key)
	return s.Store.MarkSessionMFA(sess.ID)
}

// Logout ends the session and clears both cookies.
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		_ = s.Store.DeleteSession(c.Value)
	}
	s.clearCookie(w, SessionCookie, true)
	s.clearCookie(w, CSRFCookie, false)
}

// pendingSession returns the session in r whether or not MFA is complete. Only
// the TOTP step uses it.
func (s *Service) pendingSession(r *http.Request) (*store.Session, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return nil, ErrUnauthorized
	}
	sess, err := s.Store.Session(c.Value, s.Now())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrUnauthorized
		}
		return nil, err
	}
	return sess, nil
}

// Session returns the fully authenticated session in r, or ErrUnauthorized.
// A half-authenticated session is NOT a session here — that is the whole point
// of the second factor.
func (s *Service) Session(r *http.Request) (*store.Session, error) {
	sess, err := s.pendingSession(r)
	if err != nil {
		return nil, err
	}
	if !sess.MFAOK {
		return nil, ErrUnauthorized
	}
	return sess, nil
}

// PendingSession reports the half-authenticated session, for the TOTP page.
func (s *Service) PendingSession(r *http.Request) (*store.Session, error) {
	return s.pendingSession(r)
}

// CheckCSRF validates the CSRF token on a state-changing request.
//
// Safe methods are exempt because they are not state-changing; every other
// method is checked, including the ones nobody has written a handler for yet.
// A gate that lists the methods it guards is a gate that misses the method
// added next.
func (s *Service) CheckCSRF(r *http.Request, sess *store.Session) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	token := r.Header.Get(CSRFHeader)
	if token == "" {
		// A server-rendered form has no script to set a header, so the form
		// field is accepted as the same token by the same name. It is still a
		// double submit: the value has to match the cookie's row.
		token = r.PostFormValue("csrf_token")
	}
	if token == "" {
		return ErrCSRF
	}
	ok, err := s.Store.CSRFValid(token, sess.ID, s.Now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrCSRF
	}
	return nil
}

// CSRFToken returns the live token for a session, minting one if the session
// has none (a session whose token expired alongside it cannot reach here).
func (s *Service) CSRFToken(w http.ResponseWriter, r *http.Request, sess *store.Session) (string, error) {
	if c, err := r.Cookie(CSRFCookie); err == nil && c.Value != "" {
		if ok, err := s.Store.CSRFValid(c.Value, sess.ID, s.Now()); err == nil && ok {
			return c.Value, nil
		}
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	if err := s.Store.CreateCSRF(token, sess.ID, sess.ExpiresAt); err != nil {
		return "", err
	}
	s.setCookie(w, CSRFCookie, token, sess.ExpiresAt, false)
	return token, nil
}

func (s *Service) setCookie(w http.ResponseWriter, name, value string, expires time.Time, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: httpOnly,
		Secure:   s.Secure,
		// Lax, not Strict: the manage pages are reached by following a link
		// from a chat message or a mail as often as by typing the URL, and
		// Strict logs the operator out of exactly that arrival. Lax still
		// withholds the cookie from every cross-site POST, which is the case
		// that matters, and CSRF is checked besides.
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) clearCookie(w http.ResponseWriter, name string, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: httpOnly, Secure: s.Secure, SameSite: http.SameSiteLaxMode,
	})
}

// ClientIP is the rate limiter's notion of "where from". It reads
// RemoteAddr ONLY — never X-Forwarded-For — because a header an attacker
// controls turns a per-IP limit into no limit at all. The service sits behind
// nginx on the same host (ops/nginx), so RemoteAddr is the proxy and the limit
// is effectively per-account there; a deployment that needs per-client limits
// behind a proxy must resolve the trusted-proxy question explicitly rather
// than have this function guess.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// The two login stages have SEPARATE rate-limit keys, and only a correct code
// clears the second one.
//
// Sharing one key was a real hole: a correct password called succeed(), which
// deletes the failure record, so an attacker who already had the password could
// run "password → five wrong codes → password again" forever and guess the
// second factor five at a time, at a cost of one argon2id per five guesses.
// The password stage cannot be allowed to vouch for the stage it is supposed to
// be gated by.
//
// Consequences of the split, both intended:
//   - A correct password never resets the code counter. Once an account's code
//     counter is tripped from an IP, that IP waits out the window even with the
//     right password.
//   - The code counter is keyed by the ADMIN the session belongs to, not by the
//     name typed at the password form, so it cannot be diluted by varying the
//     spelling of the account name.
func passwordKey(ip, name string) string { return "pw\x00" + ip + "\x00" + name }

func totpKey(ip, admin string) string { return "totp\x00" + ip + "\x00" + admin }

// randomToken mints a 256-bit URL-safe token for session ids and CSRF tokens.
func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
