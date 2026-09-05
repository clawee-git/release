package intake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/register"
)

var base = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

const testStamp = "v0.2.28.2026.09.04.deadbeef"

// TestEmbeddedKeyMatchesTheRepoRoot is the gate on the one copy in this tree.
// go:embed cannot reach outside its package directory, so the release public
// key is duplicated here; without this test the duplicate would be free to
// drift, and a rotated release key would leave the service verifying against
// the old one with no signal but a 403 nobody can explain.
func TestEmbeddedKeyMatchesTheRepoRoot(t *testing.T) {
	root, err := os.ReadFile("../../../clawee-release.pub")
	if err != nil {
		t.Fatalf("read repo-root key: %v", err)
	}
	local, err := os.ReadFile("clawee-release.pub")
	if err != nil {
		t.Fatalf("read embedded key: %v", err)
	}
	if !bytes.Equal(root, local) {
		t.Fatal("internal/manage/intake/clawee-release.pub differs from the repo root's; copy the root file over it")
	}
	if _, _, err := ReleaseKey(); err != nil {
		t.Fatalf("the baked release key does not parse: %v", err)
	}
}

func TestParseMinisignPublicKeyRefusesJunk(t *testing.T) {
	for _, in := range []string{
		"",
		"untrusted comment: only a comment\n",
		"untrusted comment: x\nnot base64!!\n",
		"untrusted comment: x\n" + "AAAA\n",
	} {
		if _, _, err := parseMinisignPublicKey(in); err == nil {
			t.Errorf("parsed junk: %q", in)
		}
	}
}

// fixture wires a handler over the TEST-ONLY key from internal/register's
// testdata, so the whole handshake runs against real cryptography without the
// release key being anywhere near a test.
type fixture struct {
	h      *Handler
	st     *store.Store
	server *httptest.Server
	now    time.Time
	key    register.SigningKey
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	pubFile, err := os.ReadFile("../../register/testdata/test-only.pub")
	if err != nil {
		t.Fatalf("read test-only.pub: %v", err)
	}
	pub, keyID, err := parseMinisignPublicKey(string(pubFile))
	if err != nil {
		t.Fatalf("parse test-only.pub: %v", err)
	}
	signing, err := register.LoadSigningKey("../../register/testdata/test-only.key")
	if err != nil {
		t.Fatalf("load test-only.key: %v", err)
	}

	f := &fixture{st: st, now: base, key: signing}
	f.h = &Handler{
		Store: st, Key: pub, KeyID: keyID, BaseURL: "https://release.example.invalid",
		Now: func() time.Time { return f.now }, Log: slog.New(slog.DiscardHandler),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/releases/nonce", f.h.HandleNonce)
	mux.HandleFunc("/api/v1/releases/register", f.h.HandleRegister)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// stageDir writes a plausible cut into a temp dir: four zips, the sums file
// and its signature, named exactly as the kit names them.
func stageDir(t *testing.T, comp string) string {
	t.Helper()
	dir := t.TempDir()
	var sums bytes.Buffer
	for _, plat := range []string{"darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64"} {
		name := "clawee-" + comp + "-" + plat + ".zip"
		content := []byte("zip bytes for " + name)
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		sums.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, register.SumsName), sums.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, register.MinisigName), []byte("untrusted comment: sig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f *fixture) payload(t *testing.T, comp, channel, stamp string) register.Payload {
	t.Helper()
	p, err := register.BuildPayload(stageDir(t, comp), comp, channel, "0.2.28", stamp)
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	return p
}

// TestRealClientRegistersACut drives the ACTUAL feature-01 client against this
// service. Nothing here mocks the handshake: the client fetches a nonce, signs
// the canonical bytes with the test key, and posts — so a change to either
// side's idea of what is signed fails here rather than at a real cut.
func TestRealClientRegistersACut(t *testing.T) {
	f := newFixture(t)
	client := register.NewClient(f.server.URL)
	p := f.payload(t, catalog.ComponentCLI, catalog.ChannelStable, testStamp)

	signed, rowURL, err := client.Register(context.Background(), p, f.key)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !strings.HasPrefix(rowURL, "https://release.example.invalid/manage/releases/clawee") {
		t.Fatalf("row URL = %q", rowURL)
	}

	row, err := f.st.Unpromoted(catalog.ComponentCLI, catalog.ChannelStable)
	if err != nil {
		t.Fatalf("the row was not staged: %v", err)
	}
	if row.State != catalog.StateStaged || row.IsCurrent {
		t.Fatalf("row = %+v; a registered cut is staged and never current", row)
	}
	if row.Stamp != testStamp || row.SumsKey != p.SumsKey {
		t.Fatalf("row = %+v, want the payload's stamp and keys", row)
	}
	// The artifacts are stored verbatim, keys included: promote reads them
	// back to find the bytes.
	var artifacts []register.Artifact
	if err := json.Unmarshal([]byte(row.ArtifactsJSON), &artifacts); err != nil {
		t.Fatalf("artifacts_json: %v", err)
	}
	if len(artifacts) != 4 {
		t.Fatalf("stored %d artifacts, want 4", len(artifacts))
	}
	for i, a := range artifacts {
		if a != signed.Artifacts[i] {
			t.Fatalf("artifact %d = %+v, client sent %+v", i, a, signed.Artifacts[i])
		}
	}

	// A second cut of the same stamp is the distribute step re-run, not a new
	// release.
	_, _, err = client.Register(context.Background(), p, f.key)
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("duplicate registration: err = %v, want HTTP 409", err)
	}
}

func TestNonceIsSingleUseAndBoundToItsSignature(t *testing.T) {
	f := newFixture(t)
	client := register.NewClient(f.server.URL)
	p := f.payload(t, catalog.ComponentCLI, catalog.ChannelStable, testStamp)
	signed, _, err := client.Register(context.Background(), p, f.key)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Replay the EXACT request the client sent. The nonce is spent, so the
	// captured signature buys nothing — which is the whole reason the nonce is
	// a field of the signed body rather than a header.
	body, _ := json.Marshal(signed)
	resp := post(t, f.server.URL+"/api/v1/releases/register", body)
	if resp.status != http.StatusForbidden {
		t.Fatalf("replayed request: HTTP %d, want 403", resp.status)
	}
	if !strings.Contains(resp.body, "nonce") {
		t.Fatalf("replay refusal did not mention the nonce: %s", resp.body)
	}
}

func TestExpiredNonceIsRefused(t *testing.T) {
	f := newFixture(t)
	resp := post(t, f.server.URL+"/api/v1/releases/nonce", []byte(`{}`))
	var issued struct {
		Nonce     string `json:"nonce"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal([]byte(resp.body), &issued); err != nil || issued.Nonce == "" {
		t.Fatalf("nonce response: %s", resp.body)
	}
	if issued.ExpiresIn != int(NonceTTL.Seconds()) {
		t.Fatalf("expires_in = %d, want %d", issued.ExpiresIn, int(NonceTTL.Seconds()))
	}

	p := f.payload(t, catalog.ComponentCLI, catalog.ChannelStable, testStamp)
	p.Nonce = issued.Nonce
	msg, _ := p.SigningBytes()
	p.Signature = f.key.Sign(msg)

	f.now = f.now.Add(NonceTTL + time.Second)
	body, _ := json.Marshal(p)
	if got := post(t, f.server.URL+"/api/v1/releases/register", body); got.status != http.StatusForbidden {
		t.Fatalf("expired nonce: HTTP %d, want 403", got.status)
	}
}

func TestBadSignatureIsRefusedAndSpendsNoNonce(t *testing.T) {
	f := newFixture(t)
	resp := post(t, f.server.URL+"/api/v1/releases/nonce", []byte(`{}`))
	var issued struct {
		Nonce string `json:"nonce"`
	}
	json.Unmarshal([]byte(resp.body), &issued)

	p := f.payload(t, catalog.ComponentCLI, catalog.ChannelStable, testStamp)
	p.Nonce = issued.Nonce
	msg, _ := p.SigningBytes()
	p.Signature = f.key.Sign(msg)
	// One flipped field after signing: the canonical bytes no longer match.
	p.Version = "9.9.9"

	body, _ := json.Marshal(p)
	got := post(t, f.server.URL+"/api/v1/releases/register", body)
	if got.status != http.StatusForbidden || !strings.Contains(got.body, "signature") {
		t.Fatalf("tampered payload: HTTP %d %s, want 403 naming the signature", got.status, got.body)
	}

	// The nonce was NOT spent: verification runs first, so a request that
	// cannot be signed cannot burn a live challenge either.
	if err := f.st.ConsumeNonce(issued.Nonce, f.now); err != nil {
		t.Fatalf("a bad signature consumed the nonce: %v", err)
	}
}

func TestMalformedRowsAreRefusedBeforeTheSignature(t *testing.T) {
	f := newFixture(t)
	valid := f.payload(t, catalog.ComponentCLI, catalog.ChannelStable, testStamp)

	cases := []struct {
		name   string
		mutate func(p *register.Payload)
		want   string
	}{
		{"unknown component", func(p *register.Payload) { p.Component = "clawee-cli" }, "unknown component"},
		{"unknown channel", func(p *register.Payload) { p.Channel = "nightly" }, "unknown channel"},
		{"beta stamp claiming stable", func(p *register.Payload) {
			p.Stamp = "v0.3.0.beta.2026.09.04.deadbeef"
		}, "is not a stable stamp"},
		{"empty version", func(p *register.Payload) { p.Version = "" }, "version is empty"},
		{"no artifacts", func(p *register.Payload) { p.Artifacts = nil }, "lists no artifacts"},
		// The keys are what promote reads back to find the bytes, so a row
		// whose keys point outside its own prefix is refused here rather than
		// discovered at go-live.
		{"artifact key outside the cut's prefix", func(p *register.Payload) {
			p.Artifacts[0].Key = "clawee/stable/some-other-stamp/clawee-clawee-darwin-arm64.zip"
		}, "is not under"},
		{"sums key outside the cut's prefix", func(p *register.Payload) {
			p.SumsKey = "elsewhere/SHA256SUMS.txt"
		}, "is not under"},
		{"zero-size artifact", func(p *register.Payload) { p.Artifacts[0].Size = 0 }, "has size 0"},
		{"truncated sha256", func(p *register.Payload) { p.Artifacts[0].SHA256 = "abcd" }, "sha256"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := valid
			p.Artifacts = append([]register.Artifact(nil), valid.Artifacts...)
			p.Nonce = "unused-nonce"
			p.Signature = "unused"
			c.mutate(&p)
			body, _ := json.Marshal(p)
			got := post(t, f.server.URL+"/api/v1/releases/register", body)
			if got.status != http.StatusBadRequest {
				t.Fatalf("HTTP %d %s, want 400", got.status, got.body)
			}
			if !strings.Contains(got.body, c.want) {
				t.Fatalf("body %q does not name the problem (%q)", got.body, c.want)
			}
		})
	}
}

func TestUnknownFieldIsRefused(t *testing.T) {
	f := newFixture(t)
	got := post(t, f.server.URL+"/api/v1/releases/register",
		[]byte(`{"component":"clawee","channel":"stable","surprise":1}`))
	if got.status != http.StatusBadRequest || !strings.Contains(got.body, "surprise") {
		t.Fatalf("unknown field: HTTP %d %s", got.status, got.body)
	}
}

func TestBothEndpointsRefuseGET(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/api/v1/releases/nonce", "/api/v1/releases/register"} {
		resp, err := http.Get(f.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: HTTP %d, want 405", path, resp.StatusCode)
		}
	}
}

type response struct {
	status int
	body   string
}

func post(t *testing.T, url string, body []byte) response {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, body: string(raw)}
}
