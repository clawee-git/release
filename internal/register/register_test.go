package register

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMinisignKey writes a password-less minisign secret key file holding
// priv, in the on-disk layout minisign -G -W produces.
func writeMinisignKey(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	raw := make([]byte, minisignSecretLen)
	copy(raw[0:2], "Ed")
	// kdf_algorithm stays 0x0000 — password-less.
	copy(raw[keynumOffset:keynumOffset+keyIDLen], []byte("KEYID123"))
	copy(raw[keynumOffset+keyIDLen:], priv)
	path := filepath.Join(t.TempDir(), "test.key")
	body := "untrusted comment: minisign secret key\n" + base64.StdEncoding.EncodeToString(raw) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

func stageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"clawee-clawee-darwin-arm64.zip": "darwin-arm64-bytes",
		"clawee-clawee-linux-amd64.zip":  "linux-amd64",
		SumsName:                         "sums",
		MinisigName:                      "sig",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestLoadSigningKeyRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := LoadSigningKey(writeMinisignKey(t, priv))
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(key.Sign([]byte("hello")))
	if err != nil {
		t.Fatalf("signature is not base64: %v", err)
	}
	if !ed25519.Verify(pub, []byte("hello"), sig) {
		t.Fatal("signature from the parsed key does not verify against the generated public key")
	}
}

// A password-protected key cannot be read without the password, and silently
// signing with the XORed bytes would produce a signature the service rejects
// with no hint why. Name it here instead.
func TestLoadSigningKeyRefusesPasswordProtected(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	path := writeMinisignKey(t, priv)
	data, _ := os.ReadFile(path)
	line := strings.Split(string(data), "\n")[1]
	raw, _ := base64.StdEncoding.DecodeString(line)
	raw[kdfOffset+1] = 2 // scrypt
	if err := os.WriteFile(path, []byte("untrusted comment: x\n"+base64.StdEncoding.EncodeToString(raw)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigningKey(path); err == nil || !strings.Contains(err.Error(), "password-protected") {
		t.Fatalf("want a password-protected refusal, got %v", err)
	}
}

func TestBuildPayload(t *testing.T) {
	p, err := BuildPayload(stageDir(t), "clawee", "beta", "0.2.28", "v0.2.28.2026.09.04.deadbeef")
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	if len(p.Artifacts) != 2 {
		t.Fatalf("want 2 artifacts, got %d", len(p.Artifacts))
	}
	base := "clawee/beta/v0.2.28.2026.09.04.deadbeef"
	if p.SumsKey != base+"/"+SumsName || p.MinisigKey != base+"/"+MinisigName {
		t.Fatalf("sums/minisig keys not under the staging base: %q %q", p.SumsKey, p.MinisigKey)
	}
	// Sorted by key, so darwin/arm64 comes first.
	if p.Artifacts[0].Platform != "darwin/arm64" || p.Artifacts[0].Key != base+"/clawee-clawee-darwin-arm64.zip" {
		t.Fatalf("first artifact wrong: %+v", p.Artifacts[0])
	}
	if p.Artifacts[0].Size != int64(len("darwin-arm64-bytes")) {
		t.Fatalf("size not measured from the bytes: %d", p.Artifacts[0].Size)
	}
	if len(p.Artifacts[0].SHA256) != 64 {
		t.Fatalf("sha256 is not a hex digest: %q", p.Artifacts[0].SHA256)
	}
	if p.Signature != "" || p.Nonce != "" {
		t.Fatal("BuildPayload must not invent a nonce or a signature")
	}
}

func TestBuildPayloadRefusesIncompleteStage(t *testing.T) {
	dir := stageDir(t)
	if err := os.Remove(filepath.Join(dir, MinisigName)); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPayload(dir, "clawee", "stable", "0.2.28", "v0"); err == nil {
		t.Fatal("want a refusal on a stage dir with no signature")
	}
}

// SigningBytes must exclude the signature field and stay byte-stable, because
// the service recomputes it from the body it received. A change in shape here
// is a change in the wire contract.
func TestSigningBytesExcludesSignature(t *testing.T) {
	p := Payload{Component: "clawee", Channel: "beta", Nonce: "n"}
	a, err := p.SigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	p.Signature = "whatever"
	b, err := p.SigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("signature leaked into the signed bytes:\n%s\n%s", a, b)
	}
	if strings.Contains(string(a), "signature") {
		t.Fatalf("signed bytes name the signature field: %s", a)
	}
	if strings.HasSuffix(string(a), "\n") {
		t.Fatalf("signed bytes carry a trailing newline: %q", a)
	}
}

// fakeManage is the service half: it issues a nonce, then verifies the
// signature over the canonical body exactly as the real service must, and
// refuses a reused nonce. It is what lets the client half be tested without a
// service and without a network.
type fakeManage struct {
	pub        ed25519.PublicKey
	issued     map[string]bool
	spent      map[string]bool
	lastBody   Payload
	registerRC int
}

func (f *fakeManage) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/releases/nonce", func(w http.ResponseWriter, r *http.Request) {
		nonce := "nonce-1"
		f.issued[nonce] = true
		_ = json.NewEncoder(w).Encode(map[string]string{"nonce": nonce})
	})
	mux.HandleFunc("/api/v1/releases/register", func(w http.ResponseWriter, r *http.Request) {
		if f.registerRC != 0 {
			w.WriteHeader(f.registerRC)
			_, _ = w.Write([]byte("nope"))
			return
		}
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.lastBody = p
		if !f.issued[p.Nonce] || f.spent[p.Nonce] {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte("nonce reused or unknown"))
			return
		}
		msg, err := p.SigningBytes()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sig, err := base64.StdEncoding.DecodeString(p.Signature)
		if err != nil || !ed25519.Verify(f.pub, msg, sig) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("bad signature"))
			return
		}
		f.spent[p.Nonce] = true
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://manage.example/releases/1"})
	})
	return mux
}

func newFake(t *testing.T, pub ed25519.PublicKey) (*fakeManage, *httptest.Server) {
	t.Helper()
	f := &fakeManage{pub: pub, issued: map[string]bool{}, spent: map[string]bool{}}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return f, srv
}

func TestRegisterHandshake(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	key, err := LoadSigningKey(writeMinisignKey(t, priv))
	if err != nil {
		t.Fatal(err)
	}
	f, srv := newFake(t, pub)
	p, _ := BuildPayload(stageDir(t), "clawee", "beta", "0.2.28", "v0.2.28.2026.09.04.deadbeef")

	client := NewClient(srv.URL)
	sent, rowURL, err := client.Register(context.Background(), p, key)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if rowURL != "https://manage.example/releases/1" {
		t.Fatalf("row URL = %q", rowURL)
	}
	if sent.Nonce == "" || sent.Signature == "" {
		t.Fatal("the sent payload carries no nonce/signature")
	}
	if f.lastBody.Channel != "beta" || f.lastBody.Component != "clawee" {
		t.Fatalf("service saw the wrong row: %+v", f.lastBody)
	}
	if len(f.lastBody.Artifacts) != 2 {
		t.Fatalf("service saw %d artifacts", len(f.lastBody.Artifacts))
	}
}

// A second Register call spends a nonce the fake already marked used; the
// service refuses and the tool must surface that as an error, not a success.
func TestRegisterRefusesReusedNonce(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	key, _ := LoadSigningKey(writeMinisignKey(t, priv))
	_, srv := newFake(t, pub)
	p, _ := BuildPayload(stageDir(t), "clawee", "stable", "0.2.28", "v0.2.28.2026.09.04.deadbeef")
	client := NewClient(srv.URL)
	if _, _, err := client.Register(context.Background(), p, key); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	_, _, err := client.Register(context.Background(), p, key)
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("want a 409 refusal on the reused nonce, got %v", err)
	}
}

// Signing with a key the service does not know must be refused, and the
// refusal must name the URL and the status so an operator can act on it.
func TestRegisterRefusesBadSignature(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	key, _ := LoadSigningKey(writeMinisignKey(t, otherPriv))
	_, srv := newFake(t, pub)
	p, _ := BuildPayload(stageDir(t), "clawee", "stable", "0.2.28", "v0")
	_, _, err := client(srv.URL).Register(context.Background(), p, key)
	if err == nil {
		t.Fatal("want a refusal for a signature from the wrong key")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("refusal must name the status and the manage URL: %v", err)
	}
}

func TestRegisterSurfacesNon2xx(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	key, _ := LoadSigningKey(writeMinisignKey(t, priv))
	f, srv := newFake(t, pub)
	f.registerRC = http.StatusInternalServerError
	p, _ := BuildPayload(stageDir(t), "clawee", "stable", "0.2.28", "v0")
	if _, _, err := client(srv.URL).Register(context.Background(), p, key); err == nil ||
		!strings.Contains(err.Error(), "500") {
		t.Fatalf("want a 500 surfaced, got %v", err)
	}
}

func client(url string) *Client { return NewClient(url) }

// ---- fixture-based crypto tests --------------------------------------------
//
// The tests above are self-referential by construction and that is a real
// limit: writeMinisignKey synthesises a key file out of THIS parser's own
// offset constants, so a wrong offset would be wrong identically on both sides
// and the round trip would still pass. The two tests below close that.

// TestLoadSigningKeyAgainstRealFixture parses a key file produced by the real
// `minisign -G -W` and verifies a signature from it against the public key
// minisign printed alongside it. Neither half comes from this package, so a
// drifted layout constant fails here even though it would round-trip cleanly
// through writeMinisignKey.
func TestLoadSigningKeyAgainstRealFixture(t *testing.T) {
	key, err := LoadSigningKey(filepath.Join("testdata", "test-only.key"))
	if err != nil {
		t.Fatalf("LoadSigningKey on a real minisign key: %v", err)
	}

	// The minisign public key line is: "Ed" | key_id[8] | ed25519 pub[32],
	// base64. The key id must match the secret key's, which is what proves the
	// two halves are the same identity and that keynumOffset is right.
	pubLine, err := os.ReadFile(filepath.Join("testdata", "test-only.pub"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(pubLine)), "\n")
	rawPub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil {
		t.Fatalf("fixture public key is not base64: %v", err)
	}
	if len(rawPub) != 2+keyIDLen+ed25519.PublicKeySize {
		t.Fatalf("fixture public key is %d bytes, want %d", len(rawPub), 2+keyIDLen+ed25519.PublicKeySize)
	}
	wantKeyID := base64.StdEncoding.EncodeToString(rawPub[2 : 2+keyIDLen])
	if key.KeyID != wantKeyID {
		t.Fatalf("key id from the secret key = %q, public key says %q", key.KeyID, wantKeyID)
	}

	pub := ed25519.PublicKey(rawPub[2+keyIDLen:])
	msg := []byte("the bytes a catalog row would be")
	sig, err := base64.StdEncoding.DecodeString(key.Sign(msg))
	if err != nil {
		t.Fatalf("signature is not base64: %v", err)
	}
	if !ed25519.Verify(pub, msg, sig) {
		t.Fatal("a signature from the parsed real key does not verify against minisign's own public key")
	}
}

// TestSigningBytesGolden pins the exact bytes a verifier must reconstruct.
// The service recomputes this string from the body it received and checks the
// signature over it, so field order, key names, compactness and the absence of
// a trailing newline are all wire contract. A literal here fails on any change
// to them — including one this package's own SigningBytes would make
// consistently on both sides and never notice.
func TestSigningBytesGolden(t *testing.T) {
	p := Payload{
		Component: "clawee",
		Channel:   "beta",
		Version:   "0.2.28",
		Stamp:     "v0.2.28.2026.09.04.deadbeef",
		Artifacts: []Artifact{{
			Platform: "darwin/arm64",
			Key:      "clawee/beta/v0.2.28.2026.09.04.deadbeef/clawee-clawee-darwin-arm64.zip",
			SHA256:   "f2ca1bb6c7e907d06dafe4687e579fce76b37e4e93b7605022da52e6ccc26fd2",
			Size:     5,
		}},
		SumsKey:    "clawee/beta/v0.2.28.2026.09.04.deadbeef/SHA256SUMS.txt",
		MinisigKey: "clawee/beta/v0.2.28.2026.09.04.deadbeef/SHA256SUMS.txt.minisig",
		Nonce:      "bm9uY2UtMQ==",
		Signature:  "THIS MUST NOT APPEAR",
	}
	const golden = `{"component":"clawee","channel":"beta","version":"0.2.28",` +
		`"stamp":"v0.2.28.2026.09.04.deadbeef","artifacts":[{"platform":"darwin/arm64",` +
		`"key":"clawee/beta/v0.2.28.2026.09.04.deadbeef/clawee-clawee-darwin-arm64.zip",` +
		`"sha256":"f2ca1bb6c7e907d06dafe4687e579fce76b37e4e93b7605022da52e6ccc26fd2","size":5}],` +
		`"sums_key":"clawee/beta/v0.2.28.2026.09.04.deadbeef/SHA256SUMS.txt",` +
		`"minisig_key":"clawee/beta/v0.2.28.2026.09.04.deadbeef/SHA256SUMS.txt.minisig",` +
		`"nonce":"bm9uY2UtMQ=="}`

	got, err := p.SigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != golden {
		t.Fatalf("the signed bytes changed — this is the WIRE CONTRACT, not an\nimplementation detail; every signature the service verifies depends on it.\n got: %s\nwant: %s", got, golden)
	}
}
