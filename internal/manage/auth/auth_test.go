package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/manage/totp"
)

var base = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

const goodPassword = "correct-horse-battery"

type fixture struct {
	svc *Service
	st  *store.Store
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sealer, err := LoadSealer(filepath.Join(dir, SecretKeyFile))
	if err != nil {
		t.Fatalf("LoadSealer: %v", err)
	}
	f := &fixture{st: st, now: base}
	f.svc = New(st, sealer, false, func() time.Time { return f.now }, slog.New(slog.DiscardHandler))
	return f
}

// login runs the password step and returns the response recorder, so a test
// can read the cookies the service set.
func (f *fixture) login(t *testing.T, name, password string) (*httptest.ResponseRecorder, *Enrolment, error) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/manage/login", nil)
	r.RemoteAddr = "192.0.2.10:5555"
	enrol, _, err := f.svc.StartLogin(w, r, name, password)
	return w, enrol, err
}

// authed returns a request carrying the cookies from w.
func authed(method, path string, w *httptest.ResponseRecorder) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "192.0.2.10:5555"
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	return r
}

func cookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestFirstLoginEnrolsAndSecondFactorIsRequired(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.AddAdmin("system@clawee.org", goodPassword); err != nil {
		t.Fatalf("address-style admin name refused: %v", err)
	}
	if err := f.svc.AddAdmin("ada", goodPassword); err != nil {
		t.Fatalf("AddAdmin: %v", err)
	}

	w, enrol, err := f.login(t, "ada", goodPassword)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if enrol == nil {
		t.Fatal("first login returned no enrolment — the secret is shown once or never")
	}
	if !strings.Contains(enrol.OTPAuthURL, "otpauth://totp/Clawee%20Release:ada") {
		t.Fatalf("otpauth URL = %q", enrol.OTPAuthURL)
	}

	// The CSRF token is returned, not read back from the request: the cookies
	// were set on the RESPONSE, so a caller looking it up would render the
	// code form with an empty token and fail its own CSRF check.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/manage/login", nil)
	r2.RemoteAddr = "192.0.2.11:5555"
	if err := f.svc.AddAdmin("bob", goodPassword); err != nil {
		t.Fatal(err)
	}
	_, token, err := f.svc.StartLogin(w2, r2, "bob", goodPassword)
	if err != nil {
		t.Fatalf("StartLogin(bob): %v", err)
	}
	if token == "" || token != cookie(w2, CSRFCookie).Value {
		t.Fatalf("returned CSRF token %q does not match the cookie", token)
	}

	sc := cookie(w, SessionCookie)
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie = %+v; want HttpOnly, SameSite=Lax", sc)
	}
	cc := cookie(w, CSRFCookie)
	if cc == nil || cc.HttpOnly {
		t.Fatalf("CSRF cookie = %+v; the page's script must be able to read it", cc)
	}

	// The password alone buys nothing: the session is not a session until the
	// code lands.
	if _, err := f.svc.Session(authed(http.MethodGet, "/manage", w)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("half-authenticated session accepted: err = %v", err)
	}

	code, _ := totp.Code(enrol.Secret, f.now)
	if err := f.svc.CompleteTOTP(authed(http.MethodPost, "/manage/login/totp", w), code); err != nil {
		t.Fatalf("CompleteTOTP: %v", err)
	}
	sess, err := f.svc.Session(authed(http.MethodGet, "/manage", w))
	if err != nil || sess.Admin != "ada" {
		t.Fatalf("Session after TOTP = %v, %v", sess, err)
	}

	// A second login does not re-enrol: the secret exists exactly once.
	w2, enrol2, err := f.login(t, "ada", goodPassword)
	if err != nil {
		t.Fatalf("second StartLogin: %v", err)
	}
	if enrol2 != nil {
		t.Fatal("second login re-enrolled — a password alone would then fit a new second factor")
	}
	code2, _ := totp.Code(enrol.Secret, f.now.Add(time.Minute))
	f.now = f.now.Add(time.Minute)
	if err := f.svc.CompleteTOTP(authed(http.MethodPost, "/x", w2), code2); err != nil {
		t.Fatalf("second CompleteTOTP: %v", err)
	}
}

func TestTOTPCodeIsSingleUse(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.AddAdmin("ada", goodPassword); err != nil {
		t.Fatal(err)
	}
	w, enrol, err := f.login(t, "ada", goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := totp.Code(enrol.Secret, f.now)
	if err := f.svc.CompleteTOTP(authed(http.MethodPost, "/x", w), code); err != nil {
		t.Fatal(err)
	}
	// Same code, a fresh password step, same time step: rejected by the
	// watermark, which is what makes an intercepted code worthless.
	w2, _, err := f.login(t, "ada", goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CompleteTOTP(authed(http.MethodPost, "/x", w2), code); !errors.Is(err, ErrBadCode) {
		t.Fatalf("replayed code: err = %v, want ErrBadCode", err)
	}
}

func TestWrongPasswordAndUnknownAccountAreIndistinguishable(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.AddAdmin("ada", goodPassword); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.login(t, "ada", "wrong-password-here"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password: err = %v", err)
	}
	if _, _, err := f.login(t, "nobody", goodPassword); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown account: err = %v", err)
	}
}

func TestLoginIsRateLimitedPerIPAndName(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.AddAdmin("ada", goodPassword); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < loginMaxFailures; i++ {
		if _, _, err := f.login(t, "ada", "nope-nope-nope"); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("attempt %d: err = %v", i, err)
		}
	}
	// The RIGHT password is now refused too: the limit is on attempts, not on
	// wrong answers, or an attacker learns when they got it right.
	if _, _, err := f.login(t, "ada", goodPassword); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after %d failures: err = %v, want ErrRateLimited", loginMaxFailures, err)
	}
	// Another account from the same IP is unaffected: a per-IP-only limit
	// would let one bad run lock out every admin.
	if err := f.svc.AddAdmin("bob", goodPassword); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.login(t, "bob", goodPassword); err != nil {
		t.Fatalf("second account locked out by the first account's failures: %v", err)
	}
	// The window ages out.
	f.now = f.now.Add(loginWindow + time.Minute)
	if _, _, err := f.login(t, "ada", goodPassword); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestSessionExpiresWithTheTTL(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.AddAdmin("ada", goodPassword); err != nil {
		t.Fatal(err)
	}
	w, enrol, _ := f.login(t, "ada", goodPassword)
	code, _ := totp.Code(enrol.Secret, f.now)
	if err := f.svc.CompleteTOTP(authed(http.MethodPost, "/x", w), code); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(SessionTTL - time.Minute)
	if _, err := f.svc.Session(authed(http.MethodGet, "/manage", w)); err != nil {
		t.Fatalf("session just inside the TTL: %v", err)
	}
	f.now = f.now.Add(2 * time.Minute)
	if _, err := f.svc.Session(authed(http.MethodGet, "/manage", w)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("session past the TTL: err = %v", err)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.AddAdmin("ada", goodPassword); err != nil {
		t.Fatal(err)
	}
	w, enrol, _ := f.login(t, "ada", goodPassword)
	code, _ := totp.Code(enrol.Secret, f.now)
	if err := f.svc.CompleteTOTP(authed(http.MethodPost, "/x", w), code); err != nil {
		t.Fatal(err)
	}
	r := authed(http.MethodPost, "/manage/logout", w)
	f.svc.Logout(httptest.NewRecorder(), r)
	if _, err := f.svc.Session(authed(http.MethodGet, "/manage", w)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("session survived logout: err = %v", err)
	}
}

func TestCheckCSRF(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.AddAdmin("ada", goodPassword); err != nil {
		t.Fatal(err)
	}
	w, enrol, _ := f.login(t, "ada", goodPassword)
	code, _ := totp.Code(enrol.Secret, f.now)
	if err := f.svc.CompleteTOTP(authed(http.MethodPost, "/x", w), code); err != nil {
		t.Fatal(err)
	}
	sess, err := f.svc.Session(authed(http.MethodGet, "/manage", w))
	if err != nil {
		t.Fatal(err)
	}
	token := cookie(w, CSRFCookie).Value

	// A GET is never gated.
	if err := f.svc.CheckCSRF(authed(http.MethodGet, "/manage", w), sess); err != nil {
		t.Fatalf("GET was CSRF-gated: %v", err)
	}
	// A POST with no token is refused — this is the cross-origin form case.
	if err := f.svc.CheckCSRF(authed(http.MethodPost, "/manage/promote", w), sess); !errors.Is(err, ErrCSRF) {
		t.Fatalf("POST without a token: err = %v, want ErrCSRF", err)
	}
	// A POST echoing the cookie in the header passes.
	r := authed(http.MethodPost, "/manage/promote", w)
	r.Header.Set(CSRFHeader, token)
	if err := f.svc.CheckCSRF(r, sess); err != nil {
		t.Fatalf("POST with the header token: %v", err)
	}
	// A well-formed token that is not this session's is refused.
	r = authed(http.MethodPost, "/manage/promote", w)
	r.Header.Set(CSRFHeader, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if err := f.svc.CheckCSRF(r, sess); !errors.Is(err, ErrCSRF) {
		t.Fatalf("foreign token: err = %v, want ErrCSRF", err)
	}
	// Every non-safe method is gated, including the ones no handler serves yet.
	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		if err := f.svc.CheckCSRF(authed(m, "/manage/x", w), sess); !errors.Is(err, ErrCSRF) {
			t.Fatalf("%s was not CSRF-gated", m)
		}
	}
}

func TestAdminNameAndPasswordValidation(t *testing.T) {
	f := newFixture(t)
	for _, bad := range []string{"a", "Ada", "ada bob", strings.Repeat("a", 65), "ada/../root", "@clawee.org", "system@", "a@b@c"} {
		if err := f.svc.AddAdmin(bad, goodPassword); err == nil {
			t.Fatalf("admin name %q accepted", bad)
		}
	}
	if err := f.svc.AddAdmin("ada", "short"); err == nil {
		t.Fatal("a five-character password was accepted")
	}
	if err := f.svc.AddAdmin("ada", goodPassword); err != nil {
		t.Fatalf("AddAdmin: %v", err)
	}
	if err := f.svc.AddAdmin("ada", goodPassword); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate admin: err = %v", err)
	}
}

// A correct password must NOT reset the second factor's counter.
//
// Sharing one limiter key between the two stages meant an attacker holding the
// password could run "password → five wrong codes → password again" forever,
// guessing TOTP codes five at a time at a cost of one argon2id per five
// guesses. The password stage cannot vouch for the stage it is gated by.
func TestCorrectPasswordDoesNotResetTheSecondFactorLimit(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.AddAdmin("ada", goodPassword); err != nil {
		t.Fatal(err)
	}
	w, enrol, err := f.login(t, "ada", goodPassword)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < loginMaxFailures; i++ {
		if err := f.svc.CompleteTOTP(authed(http.MethodPost, "/x", w), "000000"); !errors.Is(err, ErrBadCode) {
			t.Fatalf("wrong code %d: err = %v, want ErrBadCode", i, err)
		}
	}
	if err := f.svc.CompleteTOTP(authed(http.MethodPost, "/x", w), "000000"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after %d wrong codes: err = %v, want ErrRateLimited", loginMaxFailures, err)
	}

	// The attacker's reset move: log in again with the password they hold.
	w2, _, err := f.login(t, "ada", goodPassword)
	if err != nil {
		t.Fatalf("re-login with the correct password: %v", err)
	}
	if err := f.svc.CompleteTOTP(authed(http.MethodPost, "/x", w2), "000000"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("a correct password reopened the code counter: err = %v, want ErrRateLimited", err)
	}
	// Even the RIGHT code is refused while the window stands: the limit is on
	// attempts, so it cannot leak which guess would have been correct.
	code, _ := totp.Code(enrol.Secret, f.now)
	if err := f.svc.CompleteTOTP(authed(http.MethodPost, "/x", w2), code); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("the right code inside the window: err = %v, want ErrRateLimited", err)
	}

	// The window ages out, and only then does a correct code work.
	f.now = f.now.Add(loginWindow + time.Minute)
	code, _ = totp.Code(enrol.Secret, f.now)
	if err := f.svc.CompleteTOTP(authed(http.MethodPost, "/x", w2), code); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// The two stages are limited independently: tripping the code counter must not
// lock the password form, and vice versa.
func TestTheTwoLoginStagesAreLimitedSeparately(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.AddAdmin("ada", goodPassword); err != nil {
		t.Fatal(err)
	}
	w, _, err := f.login(t, "ada", goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < loginMaxFailures; i++ {
		f.svc.CompleteTOTP(authed(http.MethodPost, "/x", w), "000000")
	}
	// The password stage still answers — an operator who fat-fingered codes is
	// not locked out of the form that tells them so.
	if _, _, err := f.login(t, "ada", goodPassword); err != nil {
		t.Fatalf("the code counter locked the password stage: %v", err)
	}
}

// The rate limit survives a restart. Unpersisted, an attacker who could
// provoke a crash — or who simply waited for a deploy — resumed a guessing run
// with a clean counter.
func TestRateLimitSurvivesARestart(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.AddAdmin("ada", goodPassword); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < loginMaxFailures; i++ {
		f.login(t, "ada", "nope-nope-nope")
	}
	if _, _, err := f.login(t, "ada", goodPassword); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("before the restart: err = %v, want ErrRateLimited", err)
	}

	// A brand-new Service over the SAME catalog is what a restart looks like.
	restarted := New(f.st, f.svc.Sealer, false, func() time.Time { return f.now }, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/manage/login", nil)
	r.RemoteAddr = "192.0.2.10:5555"
	if _, _, err := restarted.StartLogin(w, r, "ada", goodPassword); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after the restart: err = %v, want ErrRateLimited — the counter was in memory", err)
	}
}
