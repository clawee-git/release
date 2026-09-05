package store

import (
	"errors"
	"testing"
	"time"
)

func TestAdminLifecycle(t *testing.T) {
	s := open(t)
	if err := s.CreateAdmin("ada", "hash", base); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	if err := s.CreateAdmin("ada", "other", base); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate admin: err = %v, want ErrAlreadyExists", err)
	}
	a, err := s.Admin("ada")
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if a.Enrolled() {
		t.Fatal("a fresh admin reports enrolled")
	}
	if _, err := s.Admin("bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown admin: err = %v, want ErrNotFound", err)
	}
	list, err := s.ListAdmins()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListAdmins = %v, %v", list, err)
	}
	if err := s.DeleteAdmin("bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown admin: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteAdmin("ada"); err != nil {
		t.Fatalf("DeleteAdmin: %v", err)
	}
}

func TestEnrolTOTPIsOnceOnly(t *testing.T) {
	s := open(t)
	if err := s.CreateAdmin("ada", "hash", base); err != nil {
		t.Fatal(err)
	}
	if err := s.EnrolTOTP("ada", []byte("sealed"), base); err != nil {
		t.Fatalf("EnrolTOTP: %v", err)
	}
	// A second enrolment is how an attacker holding only the password would
	// mint a second factor of their own.
	if err := s.EnrolTOTP("ada", []byte("attacker"), base); !errors.Is(err, ErrBadState) {
		t.Fatalf("re-enrol: err = %v, want ErrBadState", err)
	}
	a, _ := s.Admin("ada")
	if string(a.TOTPSecretEnc) != "sealed" || !a.Enrolled() || a.TOTPEnrolledAt.IsZero() {
		t.Fatalf("admin after enrol = %+v", a)
	}
}

func TestTOTPWatermarkOnlyMovesForward(t *testing.T) {
	s := open(t)
	if err := s.CreateAdmin("ada", "hash", base); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTOTPStep("ada", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTOTPStep("ada", 50); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Admin("ada")
	if a.TOTPLastStep != 100 {
		t.Fatalf("watermark walked backwards to %d", a.TOTPLastStep)
	}
}

func TestSessionExpiryIsEnforcedByTheStore(t *testing.T) {
	s := open(t)
	if err := s.CreateAdmin("ada", "hash", base); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("sid", "ada", base, base.Add(12*time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := s.Session("sid", base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if sess.MFAOK {
		t.Fatal("a fresh session is already MFA-complete")
	}
	if err := s.MarkSessionMFA("sid"); err != nil {
		t.Fatal(err)
	}
	sess, _ = s.Session("sid", base.Add(time.Hour))
	if !sess.MFAOK {
		t.Fatal("MarkSessionMFA did not stick")
	}
	// No caller can forget the expiry comparison: the store does it.
	if _, err := s.Session("sid", base.Add(13*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSession("sid"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session("sid", base); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session: err = %v", err)
	}
}

func TestCSRFIsBoundToItsSession(t *testing.T) {
	s := open(t)
	if err := s.CreateAdmin("ada", "hash", base); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAdmin("eve", "hash", base); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("ada-sid", "ada", base, base.Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("eve-sid", "eve", base, base.Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCSRF("tok", "eve-sid", base.Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// The attack the binding exists for: a token minted from the attacker's
	// own session, replayed against the victim's.
	if ok, err := s.CSRFValid("tok", "ada-sid", base); err != nil || ok {
		t.Fatalf("a token from another session validated (ok=%v err=%v)", ok, err)
	}
	if ok, err := s.CSRFValid("tok", "eve-sid", base); err != nil || !ok {
		t.Fatalf("the token's own session rejected it (ok=%v err=%v)", ok, err)
	}
	if ok, _ := s.CSRFValid("tok", "eve-sid", base.Add(13*time.Hour)); ok {
		t.Fatal("an expired CSRF token validated")
	}
	if ok, _ := s.CSRFValid("nope", "eve-sid", base); ok {
		t.Fatal("an unknown token validated")
	}

	// Ending the session takes its tokens with it.
	if err := s.DeleteSession("eve-sid"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.CSRFValid("tok", "eve-sid", base); ok {
		t.Fatal("a CSRF token outlived its session")
	}
}

func TestPurgeExpiredKeepsUsedNonces(t *testing.T) {
	s := open(t)
	if err := s.CreateAdmin("ada", "hash", base); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("sid", "ada", base, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.IssueNonce("spent", base, base.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.IssueNonce("unused", base, base.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeNonce("spent", base); err != nil {
		t.Fatal(err)
	}

	if err := s.PurgeExpired(base.Add(time.Hour)); err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if _, err := s.Session("sid", base); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session survived purge: %v", err)
	}
	// The unused, expired nonce goes; the spent one stays, so a replay is
	// still distinguishable from an unknown nonce in the logs.
	if err := s.ConsumeNonce("unused", base); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unused expired nonce survived purge: err = %v", err)
	}
	if err := s.ConsumeNonce("spent", base); !errors.Is(err, ErrBadState) {
		t.Fatalf("spent nonce: err = %v, want ErrBadState (already used), not ErrNotFound", err)
	}
}
