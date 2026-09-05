package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConsumeNonceIsSingleUse(t *testing.T) {
	s := open(t)
	if err := s.IssueNonce("n1", base, base.Add(5*time.Minute)); err != nil {
		t.Fatalf("IssueNonce: %v", err)
	}
	if err := s.ConsumeNonce("n1", base.Add(time.Minute)); err != nil {
		t.Fatalf("first use: %v", err)
	}
	err := s.ConsumeNonce("n1", base.Add(2*time.Minute))
	if !errors.Is(err, ErrBadState) || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("replay: err = %v, want ErrBadState naming the earlier use", err)
	}
}

func TestConsumeNonceDistinguishesUnknownFromExpired(t *testing.T) {
	s := open(t)
	if err := s.ConsumeNonce("never-issued", base); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown nonce: err = %v, want ErrNotFound", err)
	}
	if err := s.IssueNonce("n2", base, base.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Exactly at the expiry is expired: the comparison is `now < expires`, so
	// a nonce is never usable on the boundary tick.
	err := s.ConsumeNonce("n2", base.Add(5*time.Minute))
	if !errors.Is(err, ErrBadState) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired nonce: err = %v, want ErrBadState naming expiry", err)
	}
	// An expired nonce is not consumed, so it does not become "already used".
	if err := s.ConsumeNonce("n2", base.Add(6*time.Minute)); !strings.Contains(err.Error(), "expired") {
		t.Fatalf("second attempt on expired nonce: err = %v", err)
	}
}
