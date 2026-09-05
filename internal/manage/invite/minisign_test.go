package invite

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/intake"
	"github.com/clawee-git/release/internal/register"
)

// A minisign signature file, written in Go so the invite's verification chain
// can be exercised against a REAL signature rather than a stub that always
// says yes. The format is the one internal/register documents from the other
// side (it reads the secret key out of the same file layout):
//
//	untrusted comment: <text>
//	base64( "Ed" || key_id[8] || ed25519_sign(sk, message)[64] )
//	trusted comment: <text>
//	base64( ed25519_sign(sk, signature[64] || trusted_comment_bytes)[64] )
//
// "Ed" is the legacy, non-prehashed algorithm: the signature covers the file's
// bytes directly. The global signature at the end is what stops someone
// swapping the trusted comment of an otherwise valid signature.
func signMinisign(t *testing.T, key register.SigningKey, keyID []byte, message []byte, trustedComment string) []byte {
	t.Helper()
	sig := ed25519.Sign(key.Private, message)

	first := append([]byte("Ed"), keyID...)
	first = append(first, sig...)

	global := ed25519.Sign(key.Private, append(append([]byte{}, sig...), []byte(trustedComment)...))

	var b strings.Builder
	fmt.Fprintf(&b, "untrusted comment: signature from a TEST-ONLY key\n%s\ntrusted comment: %s\n%s\n",
		base64.StdEncoding.EncodeToString(first),
		trustedComment,
		base64.StdEncoding.EncodeToString(global))
	return []byte(b.String())
}

// testOnlyKey loads the signing key and the matching public key line from
// internal/register's testdata — the same pair feature 01's own tests use.
func testOnlyKey(t *testing.T) (register.SigningKey, []byte, string) {
	t.Helper()
	key, err := register.LoadSigningKey("../../register/testdata/test-only.key")
	if err != nil {
		t.Fatalf("load test-only.key: %v", err)
	}
	pubFile, err := os.ReadFile("../../register/testdata/test-only.pub")
	if err != nil {
		t.Fatal(err)
	}
	var pubLine string
	for _, line := range strings.Split(string(pubFile), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "untrusted comment:") {
			continue
		}
		pubLine = line
		break
	}
	if pubLine == "" {
		t.Fatal("test-only.pub has no key line")
	}
	raw, err := base64.StdEncoding.DecodeString(pubLine)
	if err != nil || len(raw) != 42 {
		t.Fatalf("test-only.pub does not decode to 42 bytes: %v", err)
	}
	// Bytes 2..10 are the key id; the signature file must carry the SAME one
	// or minisign refuses before it looks at any cryptography.
	return key, raw[2:10], pubLine
}

// TestSignMinisignProducesSomethingWeCanVerifyOurselves pins the writer above
// before it is used to make a claim about the script. A signer that quietly
// produced garbage would make the execution test below vacuous.
func TestSignMinisignProducesSomethingWeCanVerifyOurselves(t *testing.T) {
	key, keyID, pubLine := testOnlyKey(t)
	raw, _ := base64.StdEncoding.DecodeString(pubLine)
	pub := ed25519.PublicKey(raw[10:])

	message := []byte("the sums file contents\n")
	sig := signMinisign(t, key, keyID, message, "timestamp:1757073600")

	lines := strings.Split(strings.TrimSpace(string(sig)), "\n")
	if len(lines) != 4 {
		t.Fatalf("signature file has %d lines, want 4", len(lines))
	}
	first, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(first) != 74 {
		t.Fatalf("first block: %d bytes, %v", len(first), err)
	}
	if string(first[:2]) != "Ed" {
		t.Fatalf("algorithm = %q, want Ed", first[:2])
	}
	if !ed25519.Verify(pub, message, first[10:]) {
		t.Fatal("the signature does not verify against the test-only public key")
	}
	if !ed25519.Verify(pub, append(append([]byte{}, first[10:]...),
		[]byte(strings.TrimPrefix(lines[2], "trusted comment: "))...), mustB64(t, lines[3])) {
		t.Fatal("the global signature does not verify")
	}
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runWithRealMinisign executes the script with NO stub on PATH, so whatever
// minisign the machine has is the one that verifies.
func runWithRealMinisign(t *testing.T, script, caPEM string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, []byte(caPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", path)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CURL_CA_BUNDLE="+ca)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestRenderedScriptVerifiesARealMinisignSignature closes the last stubbed
// link in the invite chain. Every other execution test replaces minisign with
// a stub that returns a fixed status, which proves the script REACTS to the
// verdict but not that the verdict is real: a script that passed the wrong
// arguments, or rendered a mangled public key, would look identical.
//
// Here the sums file is signed with internal/register's test-only key and
// verified by the machine's actual minisign, through the exact command line
// the template renders.
func TestRenderedScriptVerifiesARealMinisignSignature(t *testing.T) {
	requireTools(t)
	if _, err := exec.LookPath("minisign"); err != nil {
		t.Skip("minisign is not installed; TestMintRendersTheBakedReleaseKey covers the argument")
	}
	f := newFixture(t, catalog.ChannelBeta)
	key, keyID, pubLine := testOnlyKey(t)

	// Sign the REAL sums file the fixture built, so the signature covers the
	// same bytes the script downloads and then hashes against.
	sumsKey := f.row.SumsKey
	f.staging.Objects[f.row.MinisigKey] = signMinisign(t, key, keyID,
		f.staging.Objects[sumsKey], "timestamp:1757073600\tfile:SHA256SUMS.txt")

	f.serveArtifacts(t, "")
	script := f.renderWithPubkey(t, pubLine)

	out, err := runWithRealMinisign(t, script, string(testCert))
	if err != nil {
		t.Fatalf("real minisign rejected a genuine signature: %v\n%s", err, out)
	}
	if !strings.Contains(out, "minisign signature valid") {
		t.Fatalf("the signature step did not report success:\n%s", out)
	}
	if !strings.Contains(out, "INNER INSTALLER RAN") {
		t.Fatalf("the chain did not complete:\n%s", out)
	}
}

// …and a signature over DIFFERENT bytes is refused by that same real minisign.
// Without this, the test above would pass against a script that ran minisign
// and ignored its exit status.
func TestRealMinisignRefusesASignatureOverOtherBytes(t *testing.T) {
	requireTools(t)
	if _, err := exec.LookPath("minisign"); err != nil {
		t.Skip("minisign is not installed")
	}
	f := newFixture(t, catalog.ChannelBeta)
	key, keyID, pubLine := testOnlyKey(t)

	// A perfectly valid signature — over the wrong file.
	f.staging.Objects[f.row.MinisigKey] = signMinisign(t, key, keyID,
		[]byte("some other file entirely"), "timestamp:1757073600")

	f.serveArtifacts(t, "")
	script := f.renderWithPubkey(t, pubLine)

	out, err := runWithRealMinisign(t, script, string(testCert))
	if err == nil {
		t.Fatalf("a signature over other bytes was accepted:\n%s", out)
	}
	if !strings.Contains(out, "signature verification failed") {
		t.Fatalf("the refusal does not name the signature:\n%s", out)
	}
	if strings.Contains(out, "INNER INSTALLER RAN") {
		t.Fatalf("the installer ran anyway:\n%s", out)
	}
}

// TestMintRendersTheBakedReleaseKey is the assertion that still holds on a
// machine with no minisign: whatever the chain does, the key the script
// verifies against must be the one this build bakes in, byte for byte.
func TestMintRendersTheBakedReleaseKey(t *testing.T) {
	f := newFixture(t, catalog.ChannelBeta)
	if _, err := Mint(context.Background(), f.deps, f.row, "ada"); err != nil {
		t.Fatal(err)
	}
	script := string(f.staging.Objects[scriptKeyOf(t, f)])
	want, err := intake.ReleasePubkeyLine()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, `PUBKEY="`+want+`"`) {
		t.Fatalf("the rendered script does not bake the release public key %q:\n%s", want, script)
	}
	// And it is the verifier's argument, not just a string in the file.
	if !strings.Contains(script, `minisign -V -P "$PUBKEY"`) {
		t.Fatalf("the verify line does not use the baked key:\n%s", script)
	}
}
