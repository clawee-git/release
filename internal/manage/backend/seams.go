// Package backend is every remote store the manage service touches, behind an
// interface, plus the real implementations.
//
// The seams exist so no test ever reaches a real bucket, a real GitHub or a
// real host — the same posture the cut already takes with tools/r2-mirror's
// --bucket and --dry-run flags (AGENTS.md, "Seams"). Promote is the one
// operation in this product that publishes software; a test suite that could
// only exercise it against live infrastructure would be a test suite nobody
// runs.
//
// There are exactly four, and the split is by TRUST, not by protocol:
//
//	Staging  the PRIVATE bucket the cut wrote. Read and presign only, plus the
//	         one write the invite mint needs.
//	Public   the public surface. Write, copy into, delete, list.
//	GitHub   the release listing. Promote FAILS CLOSED without it.
//	Clock    injected time, so a 48-hour expiry is arithmetic in a test.
//
// A single "object store" interface over both buckets would lose the property
// that matters: the staging half has no delete and no unrestricted put, so no
// amount of wrong wiring in promote can damage what a cut staged.
package backend

import (
	"context"
	"time"
)

// Clock is the injected time source. A function, not an interface: it has one
// method, and every caller wants to write `now := deps.Now()`.
type Clock func() time.Time

// Staging is the private staging bucket.
//
// Deliberately NO Delete: the staging store keeps every staged row's bytes
// (release-management.md §7 keeps every `staged` row), and promote has no
// business removing what it is copying from. Retention's staging pass, if it
// is ever built, gets its own narrower seam rather than widening this one.
type Staging interface {
	// Bucket is the bucket's name, needed to build a server-side copy source.
	Bucket() string
	List(ctx context.Context, prefix string) ([]string, error)
	// Head returns the object's size, so promote can refuse a wrong-sized
	// object before pulling its body.
	Head(ctx context.Context, key string) (int64, error)
	Get(ctx context.Context, key string) ([]byte, error)
	// Put is the ONE write: the invite mint uploads its rendered install.sh
	// under invites/<random>/, which lives in the staging bucket because it
	// must not be reachable without a presigned URL.
	Put(ctx context.Context, key string, body []byte, contentType string) error
	// Presign returns a self-contained, time-limited GET URL.
	Presign(key string, ttl time.Duration) (string, error)
}

// Public is the public surface: what installers and the download page read.
type Public interface {
	Put(ctx context.Context, key string, body []byte, contentType string) error
	// Copy is a server-side copy from another bucket. Promote has already
	// fetched and hashed the source against the catalog row, so what is copied
	// is what was verified — and the bytes do not make a second round trip.
	Copy(ctx context.Context, srcBucket, srcKey, dstKey string) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Release identifies a created GitHub release.
type Release struct {
	ID  int64
	Tag string
}

// GitHub is the release listing.
//
// Promote FAILS CLOSED without an implementation of this (503, nothing
// copied): a beta row has no other public path, and a release that exists in
// the bucket but not in the listing is one that installers can reach and
// humans cannot find.
type GitHub interface {
	// CreateRelease creates (or reuses) the release for tag. prerelease is
	// true for the beta channel.
	CreateRelease(ctx context.Context, tag, name, body string, prerelease bool) (Release, error)
	UploadAsset(ctx context.Context, rel Release, name, contentType string, body []byte) error
	// DeleteRelease removes the release AND its tag. Retention calls it
	// best-effort: the catalog row is the source of truth and bytes are
	// reconciled to it, never the other way round.
	DeleteRelease(ctx context.Context, tag string) error
}

// ContentType is the one mapping from a filename to its Content-Type, shared
// by the cut's mirror and by promote so an object does not change type when it
// moves between buckets.
func ContentType(name string) string {
	switch {
	case hasSuffix(name, ".zip"):
		return "application/zip"
	case hasSuffix(name, ".json"):
		return "application/json"
	case hasSuffix(name, ".sh"):
		// text/plain, not application/x-sh: the invite URL is fetched by curl
		// and piped to sh, and a downloadable content type makes some
		// intermediaries offer it as a file instead of serving it.
		return "text/plain; charset=utf-8"
	default:
		return "text/plain"
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
