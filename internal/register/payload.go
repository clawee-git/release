package register

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	SumsName    = "SHA256SUMS.txt"
	MinisigName = "SHA256SUMS.txt.minisig"
)

// Artifact is one uploaded file as the catalog records it. The key is the FULL
// staging object key, not a name relative to something the service would have
// to reconstruct: promote reads these keys back verbatim, and a layout the
// service re-derives is a layout that can disagree with where the bytes are.
type Artifact struct {
	Platform string `json:"platform"`
	Key      string `json:"key"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}

// Payload is the register request body. Field ORDER IS THE WIRE CONTRACT: the
// signature covers the canonical JSON of this struct with Signature empty, and
// Go marshals struct fields in declaration order, so reordering these fields
// silently invalidates every signature the service verifies.
type Payload struct {
	Component  string     `json:"component"`
	Channel    string     `json:"channel"`
	Version    string     `json:"version"`
	Stamp      string     `json:"stamp"`
	Artifacts  []Artifact `json:"artifacts"`
	SumsKey    string     `json:"sums_key"`
	MinisigKey string     `json:"minisig_key"`
	Nonce      string     `json:"nonce"`
	Signature  string     `json:"signature,omitempty"`
}

// SigningBytes is the canonical JSON the signature covers: the whole body with
// signature omitted, compact, no HTML escaping (which would rewrite bytes the
// verifier never sees the original of).
func (p Payload) SigningBytes() ([]byte, error) {
	unsigned := p
	unsigned.Signature = ""
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(unsigned); err != nil {
		return nil, fmt.Errorf("encode canonical payload: %w", err)
	}
	// Encoder appends a newline; the canonical form does not carry one.
	return []byte(strings.TrimSuffix(buf.String(), "\n")), nil
}

// KeyBase is the staging prefix a cut's artifacts live under. It is the one
// place the layout is spelled on this side; tools/r2-mirror's --prefix is fed
// the channel so both agree by construction.
func KeyBase(comp, channel, stamp string) string {
	return comp + "/" + channel + "/" + stamp
}

// zipName matches the release kit's own zip naming: clawee-<comp>-<os>-<arch>.zip.
// The platform is taken from the FILENAME rather than from a build matrix so a
// zip the build produced but the catalog does not know about cannot be
// silently registered under the wrong platform.
var zipName = regexp.MustCompile(`^clawee-[a-z]+-([a-z0-9]+)-([a-z0-9]+)\.zip$`)

// BuildPayload reads stageDir and returns the row for the cut, with every
// artifact's size and SHA-256 measured from the bytes about to be uploaded.
// Nonce and Signature are filled in later, by Register.
func BuildPayload(stageDir, comp, channel, version, stamp string) (Payload, error) {
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		return Payload{}, fmt.Errorf("read stage dir %q: %w", stageDir, err)
	}
	base := KeyBase(comp, channel, stamp)

	var artifacts []Artifact
	var hasSums, hasMinisig bool
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case name == SumsName:
			hasSums = true
		case name == MinisigName:
			hasMinisig = true
		case strings.HasSuffix(name, ".zip"):
			m := zipName.FindStringSubmatch(name)
			if m == nil {
				return Payload{}, fmt.Errorf("zip %q does not match clawee-<comp>-<os>-<arch>.zip — refusing to guess its platform", name)
			}
			if !strings.HasPrefix(name, "clawee-"+comp+"-") {
				continue // another component's zip staged in the same dir
			}
			sum, size, err := digest(filepath.Join(stageDir, name))
			if err != nil {
				return Payload{}, err
			}
			artifacts = append(artifacts, Artifact{
				Platform: m[1] + "/" + m[2],
				Key:      base + "/" + name,
				SHA256:   sum,
				Size:     size,
			})
		}
	}
	if len(artifacts) == 0 {
		return Payload{}, fmt.Errorf("no clawee-%s-*.zip artifacts in %q", comp, stageDir)
	}
	if !hasSums {
		return Payload{}, fmt.Errorf("%s missing from %q", SumsName, stageDir)
	}
	if !hasMinisig {
		return Payload{}, fmt.Errorf("%s missing from %q", MinisigName, stageDir)
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Key < artifacts[j].Key })

	return Payload{
		Component:  comp,
		Channel:    channel,
		Version:    version,
		Stamp:      stamp,
		Artifacts:  artifacts,
		SumsKey:    base + "/" + SumsName,
		MinisigKey: base + "/" + MinisigName,
	}, nil
}

func digest(path string) (string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, fmt.Errorf("read %q: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), int64(len(data)), nil
}
