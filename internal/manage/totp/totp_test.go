package totp

import (
	"testing"
	"time"
)

// rfcSecret is the RFC 4226/6238 shared test secret — ASCII
// "12345678901234567890" — base32-encoded.
const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

// TestCodeRFCVectors pins Code to the official RFC 6238 SHA-1 vectors
// (Appendix B), reduced from 8 to 6 digits — the HOTP value mod 10^6 is the
// last six digits of each published 8-digit value.
func TestCodeRFCVectors(t *testing.T) {
	vectors := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, v := range vectors {
		got, err := Code(rfcSecret, time.Unix(v.unix, 0))
		if err != nil {
			t.Fatalf("Code(%d): %v", v.unix, err)
		}
		if got != v.want {
			t.Fatalf("Code at T=%d = %q, want %q", v.unix, got, v.want)
		}
	}
}

func TestVerifySkewWindow(t *testing.T) {
	now := time.Unix(1111111109, 0)
	for _, offset := range []int64{-1, 0, 1} {
		code, _ := Code(rfcSecret, now.Add(time.Duration(offset*Period)*time.Second))
		step, ok := Verify(rfcSecret, code, now, 0)
		if !ok {
			t.Fatalf("offset %d: code rejected", offset)
		}
		if step != now.Unix()/Period+offset {
			t.Fatalf("offset %d: matched step = %d, want %d", offset, step, now.Unix()/Period+offset)
		}
	}
	for _, offset := range []int64{-2, 2} {
		code, _ := Code(rfcSecret, now.Add(time.Duration(offset*Period)*time.Second))
		if _, ok := Verify(rfcSecret, code, now, 0); ok {
			t.Fatalf("offset %d: code accepted, want rejected", offset)
		}
	}
}

func TestVerifyRejectsReplayWithinTheSkewWindow(t *testing.T) {
	now := time.Unix(1111111109, 0)
	code, _ := Code(rfcSecret, now)
	step, ok := Verify(rfcSecret, code, now, 0)
	if !ok {
		t.Fatal("first use rejected")
	}
	if _, ok := Verify(rfcSecret, code, now, step); ok {
		t.Fatal("replayed code accepted — the watermark is not being honoured")
	}
}

func TestVerifyRejectsWrongCodeAndBadSecret(t *testing.T) {
	if _, ok := Verify(rfcSecret, "000000", time.Unix(59, 0), 0); ok {
		t.Fatal("wrong code accepted")
	}
	if _, ok := Verify("not base32!", "000000", time.Unix(59, 0), 0); ok {
		t.Fatal("a code verified against an undecodable secret")
	}
}

func TestGenerateSecretIsDecodableAndFullLength(t *testing.T) {
	s, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	raw, err := encoding.DecodeString(s)
	if err != nil || len(raw) != secretSize {
		t.Fatalf("secret %q: decoded %d bytes, err = %v", s, len(raw), err)
	}
}

func TestOTPAuthURL(t *testing.T) {
	got := OTPAuthURL("Clawee Release", "ada", rfcSecret)
	want := "otpauth://totp/Clawee%20Release:ada?issuer=Clawee+Release&secret=" + rfcSecret
	if got != want {
		t.Fatalf("OTPAuthURL = %q, want %q", got, want)
	}
}
