// Package manifest is the channel manifest — <comp>/latest.json and
// <comp>/beta/latest.json — that every installer resolves.
//
// It lives in its own package because TWO things write it and they must not
// disagree: the cut's mirror (tools/r2-mirror, which writes it only for a
// non-staging run) and promote. A second declaration of these fields on the
// promote side would be a schema that drifts from the one already in the
// bucket, and the symptom would be installers that stop resolving.
//
// Writing the manifest is THE go-live: a release with its bytes in the public
// bucket and no manifest entry is not reachable by anything, which is exactly
// why promote writes it last and why the cut withholds it entirely.
package manifest

import (
	"encoding/json"
	"fmt"
	"time"
)

// Name is the manifest's filename under its channel prefix.
const Name = "latest.json"

// Latest is the manifest schema. Fields are declared in ALPHABETICAL order so
// json.Marshal emits them in the same order as the manifests already in the
// bucket — a stable, diff-friendly shape, and one an operator comparing two
// manifests by eye can read.
type Latest struct {
	Component  string   `json:"component"`
	Minisig    string   `json:"minisig"`
	Path       string   `json:"path"`
	SHA256Sums string   `json:"sha256sums"`
	Stamp      string   `json:"stamp"`
	Updated    string   `json:"updated"`
	Version    string   `json:"version"`
	Zips       []string `json:"zips"`
}

// PublicBase is the public key prefix a promoted cut lands under:
// <comp>/<stamp> on stable, <comp>/beta/<stamp> on beta.
//
// The channel is a PATH SEGMENT, never a pattern matched out of the stamp, so
// a per-channel manifest has a home and retention can be driven by prefix.
func PublicBase(component, channel, stamp string) string {
	if channel == "beta" {
		return component + "/beta/" + stamp
	}
	return component + "/" + stamp
}

// Key is the manifest's object key for a channel: <comp>/latest.json, or
// <comp>/beta/latest.json.
func Key(component, channel string) string {
	if channel == "beta" {
		return component + "/beta/" + Name
	}
	return component + "/" + Name
}

// Build assembles the manifest for one promoted row. zips are the artifact
// basenames; sums and minisig are the two verification files' basenames.
func Build(component, channel, version, stamp string, zips []string, sums, minisig string, at time.Time) Latest {
	base := PublicBase(component, channel, stamp)
	return Latest{
		Component:  component,
		Version:    version,
		Stamp:      stamp,
		Path:       base,
		Zips:       zips,
		SHA256Sums: base + "/" + sums,
		Minisig:    base + "/" + minisig,
		Updated:    at.UTC().Format(time.RFC3339),
	}
}

// Marshal renders the manifest as the bytes to upload.
func (l Latest) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("manifest: marshal: %w", err)
	}
	return append(b, '\n'), nil
}
