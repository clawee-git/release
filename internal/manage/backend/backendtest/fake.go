// Package backendtest holds recording fakes for the backend seams.
//
// They live in a normal package rather than in one _test.go file because three
// packages need them — the invite mint, the promote pipeline and the HTTP
// surface — and a fake copied three times is three fakes that drift. They
// RECORD every call in order, because the thing most worth asserting about
// promote is not what it did but what order it did it in.
package backendtest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/clawee-git/release/internal/manage/backend"
)

// Call is one recorded operation. Op is the seam and method ("public.copy"),
// Key the object or tag it acted on.
type Call struct {
	Op  string
	Key string
}

// Recorder is the shared call log. All three fakes write to one, so the ORDER
// ACROSS SEAMS is what a test reads — "verify, then copy, then GitHub, then
// manifest, then flip" is a property of the interleaving, not of any one fake.
type Recorder struct {
	mu    sync.Mutex
	calls []Call
}

func (r *Recorder) record(op, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Op: op, Key: key})
}

// Calls returns the log.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// Ops returns just the operation names, in order.
func (r *Recorder) Ops() []string {
	var out []string
	for _, c := range r.Calls() {
		out = append(out, c.Op)
	}
	return out
}

// FirstIndexOf returns the position of the first call with op, or -1.
func (r *Recorder) FirstIndexOf(op string) int {
	for i, c := range r.Calls() {
		if c.Op == op {
			return i
		}
	}
	return -1
}

// Has reports whether any call used op.
func (r *Recorder) Has(op string) bool { return r.FirstIndexOf(op) >= 0 }

// Staging is a fake private bucket.
type Staging struct {
	*Recorder
	BucketName string
	Objects    map[string][]byte
	// FailGet, FailHead and FailPut fail the call for a key CONTAINING the
	// substring, so a test can break exactly one file of a six-file promote.
	FailGet, FailHead, FailPut string
	PresignPrefix              string
}

// NewStaging builds a fake staging bucket sharing rec.
func NewStaging(rec *Recorder, bucket string) *Staging {
	return &Staging{Recorder: rec, BucketName: bucket, Objects: map[string][]byte{},
		PresignPrefix: "https://staging.example.invalid/"}
}

func (s *Staging) Bucket() string { return s.BucketName }

func (s *Staging) List(ctx context.Context, prefix string) ([]string, error) {
	s.record("staging.list", prefix)
	var out []string
	for k := range s.Objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

func (s *Staging) Head(ctx context.Context, key string) (int64, error) {
	s.record("staging.head", key)
	if s.FailHead != "" && strings.Contains(key, s.FailHead) {
		return 0, fmt.Errorf("fake staging: head %s refused", key)
	}
	b, ok := s.Objects[key]
	if !ok {
		return 0, fmt.Errorf("fake staging: no such key %q", key)
	}
	return int64(len(b)), nil
}

func (s *Staging) Get(ctx context.Context, key string) ([]byte, error) {
	s.record("staging.get", key)
	if s.FailGet != "" && strings.Contains(key, s.FailGet) {
		return nil, fmt.Errorf("fake staging: get %s refused", key)
	}
	b, ok := s.Objects[key]
	if !ok {
		return nil, fmt.Errorf("fake staging: no such key %q", key)
	}
	return append([]byte(nil), b...), nil
}

func (s *Staging) Put(ctx context.Context, key string, body []byte, contentType string) error {
	s.record("staging.put", key)
	if s.FailPut != "" && strings.Contains(key, s.FailPut) {
		return fmt.Errorf("fake staging: put %s refused", key)
	}
	s.Objects[key] = append([]byte(nil), body...)
	return nil
}

func (s *Staging) Presign(key string, ttl time.Duration) (string, error) {
	s.record("staging.presign", key)
	if _, ok := s.Objects[key]; !ok {
		return "", fmt.Errorf("fake staging: cannot presign missing key %q", key)
	}
	return fmt.Sprintf("%s%s?ttl=%d", s.PresignPrefix, key, int(ttl.Seconds())), nil
}

// Public is a fake public bucket.
type Public struct {
	*Recorder
	Objects map[string][]byte
	// FailCopy / FailPut / FailDelete fail for a key containing the substring.
	FailCopy, FailPut, FailDelete string
	// Source is the staging fake, so Copy can move real bytes and a test can
	// assert the public object matches the staged one.
	Source *Staging
}

// NewPublic builds a fake public bucket sharing rec.
func NewPublic(rec *Recorder, src *Staging) *Public {
	return &Public{Recorder: rec, Objects: map[string][]byte{}, Source: src}
}

func (p *Public) Put(ctx context.Context, key string, body []byte, contentType string) error {
	p.record("public.put", key)
	if p.FailPut != "" && strings.Contains(key, p.FailPut) {
		return fmt.Errorf("fake public: put %s refused", key)
	}
	p.Objects[key] = append([]byte(nil), body...)
	return nil
}

func (p *Public) Copy(ctx context.Context, srcBucket, srcKey, dstKey string) error {
	p.record("public.copy", dstKey)
	if p.FailCopy != "" && strings.Contains(dstKey, p.FailCopy) {
		return fmt.Errorf("fake public: copy to %s refused", dstKey)
	}
	if p.Source != nil {
		if srcBucket != p.Source.BucketName {
			return fmt.Errorf("fake public: copy source bucket %q is not the staging bucket %q", srcBucket, p.Source.BucketName)
		}
		b, ok := p.Source.Objects[srcKey]
		if !ok {
			return fmt.Errorf("fake public: copy source %q does not exist", srcKey)
		}
		p.Objects[dstKey] = append([]byte(nil), b...)
		return nil
	}
	p.Objects[dstKey] = []byte("copied")
	return nil
}

func (p *Public) Delete(ctx context.Context, key string) error {
	p.record("public.delete", key)
	if p.FailDelete != "" && strings.Contains(key, p.FailDelete) {
		return fmt.Errorf("fake public: delete %s refused", key)
	}
	delete(p.Objects, key)
	return nil
}

func (p *Public) List(ctx context.Context, prefix string) ([]string, error) {
	p.record("public.list", prefix)
	var out []string
	for k := range p.Objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

// GitHub is a fake release publisher.
type GitHub struct {
	*Recorder
	Releases   map[string]backend.Release
	Assets     map[string][]string
	Bodies     map[string]string
	Prerelease map[string]bool
	// AssetBodies records what was uploaded, keyed "<tag>/<name>", so a test
	// can assert a retry REPLACED wrong bytes rather than merely not failing.
	AssetBodies map[string][]byte
	// RejectDuplicates makes a second upload of the same asset name fail the
	// way GitHub's 422 does, so a test proves the retry path survives the real
	// API's behaviour rather than a more forgiving fake's. The fake itself
	// replaces (it stands in for an idempotent implementation); set
	// replacing=false to see the refusal an appending one would meet.
	RejectDuplicates bool
	replacing        bool
	// FailCreate / FailUpload / FailDelete fail for a tag or asset name
	// containing the substring.
	FailCreate, FailUpload, FailDelete string
	nextID                             int64
}

// NewGitHub builds a fake publisher sharing rec.
func NewGitHub(rec *Recorder) *GitHub {
	return &GitHub{Recorder: rec, replacing: true, Releases: map[string]backend.Release{},
		Assets: map[string][]string{}, Bodies: map[string]string{},
		Prerelease: map[string]bool{}, AssetBodies: map[string][]byte{}}
}

func (g *GitHub) CreateRelease(ctx context.Context, tag, name, body string, prerelease bool) (backend.Release, error) {
	g.record("github.create", tag)
	if g.FailCreate != "" && strings.Contains(tag, g.FailCreate) {
		return backend.Release{}, fmt.Errorf("fake github: create %s refused", tag)
	}
	if rel, ok := g.Releases[tag]; ok {
		return rel, nil
	}
	g.nextID++
	rel := backend.Release{ID: g.nextID, Tag: tag}
	g.Releases[tag] = rel
	g.Bodies[tag] = body
	g.Prerelease[tag] = prerelease
	return rel, nil
}

// UploadAsset models the real API's behaviour on a duplicate NAME: GitHub
// answers 422, it does not overwrite. RejectDuplicates makes the fake do the
// same, so a test can prove promote's retry path survives it rather than
// passing against a fake that is more forgiving than the thing it stands in
// for. Replacement (the client deletes first) is modelled by
// DeleteAssetByName.
func (g *GitHub) UploadAsset(ctx context.Context, rel backend.Release, name, contentType string, body []byte) error {
	g.record("github.upload", name)
	if g.FailUpload != "" && strings.Contains(name, g.FailUpload) {
		return fmt.Errorf("fake github: upload %s refused", name)
	}
	// Replace, the way an idempotent implementation must: delete any asset of
	// this name first. RejectDuplicates then models what the REAL API does to
	// an implementation that skipped this step.
	for i, existing := range g.Assets[rel.Tag] {
		if existing != name {
			continue
		}
		if g.RejectDuplicates && !g.replacing {
			return fmt.Errorf("fake github: 422 asset %q already exists on %s", name, rel.Tag)
		}
		g.Assets[rel.Tag] = append(g.Assets[rel.Tag][:i], g.Assets[rel.Tag][i+1:]...)
		break
	}
	g.Assets[rel.Tag] = append(g.Assets[rel.Tag], name)
	if g.AssetBodies == nil {
		g.AssetBodies = map[string][]byte{}
	}
	g.AssetBodies[rel.Tag+"/"+name] = append([]byte(nil), body...)
	return nil
}

// DeleteAssetByName is what an idempotent uploader calls before re-uploading.
func (g *GitHub) DeleteAssetByName(ctx context.Context, rel backend.Release, name string) error {
	g.record("github.delete-asset", name)
	kept := g.Assets[rel.Tag][:0]
	for _, existing := range g.Assets[rel.Tag] {
		if existing != name {
			kept = append(kept, existing)
		}
	}
	g.Assets[rel.Tag] = kept
	delete(g.AssetBodies, rel.Tag+"/"+name)
	return nil
}

func (g *GitHub) DeleteRelease(ctx context.Context, tag string) error {
	g.record("github.delete", tag)
	if g.FailDelete != "" && strings.Contains(tag, g.FailDelete) {
		return fmt.Errorf("fake github: delete %s refused", tag)
	}
	delete(g.Releases, tag)
	delete(g.Assets, tag)
	return nil
}

// The fakes must satisfy the seams they stand in for. Without these, a change
// to an interface would be caught only where a test happens to assign one.
var (
	_ backend.Staging = (*Staging)(nil)
	_ backend.Public  = (*Public)(nil)
	_ backend.GitHub  = (*GitHub)(nil)
)
