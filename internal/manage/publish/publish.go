// Package publish is promote and yank: the operator acts that make a staged
// cut public, and that take it back off the channel.
//
// The ORDER is the design (release-management.md §5), and it is the thing the
// tests assert:
//
//	verify → copy every file → GitHub release → manifest LAST → flip → retention
//
// Every step before the flip is reversible by doing nothing: a failure leaves
// the row `staged`, the manifest untouched, and the public surface carrying at
// most some orphaned objects that the next attempt overwrites. Writing the
// manifest last is what makes that true — the manifest is the go-live, so
// until it is written nothing resolves to the new release, and until the row
// flips the catalog still says `staged` and a retry is clean.
package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"

	"github.com/clawee-git/release/internal/manage/backend"
	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/manifest"
	"github.com/clawee-git/release/internal/register"
)

// ErrNoPublisher is promote's fail-closed refusal. A beta row has no public
// path other than the GitHub release, and a release that exists in the bucket
// but not in the listing is one installers can reach and humans cannot find.
var ErrNoPublisher = errors.New("publish: no GitHub publisher is configured")

// ErrNoPublicStore is the same for the public bucket.
var ErrNoPublicStore = errors.New("publish: no public store is configured")

// Deps is everything promote and yank need.
type Deps struct {
	Store   *store.Store
	Staging backend.Staging
	Public  backend.Public
	GitHub  backend.GitHub
	Now     backend.Clock
	Log     *slog.Logger
	// Retain runs retention for (component, channel) at the end of a
	// successful promote. It is a field rather than a direct call so promote
	// does not depend on the pruning half, and so a test can watch it run last.
	Retain func(ctx context.Context, component, channel string) []Event
}

// Event is one line of the NDJSON progress stream.
//
// Progress is streamed because a promote moves several hundred megabytes: a
// blind synchronous one is indistinguishable from a hung one, and an operator
// who cannot tell those apart eventually kills a promote mid-copy.
type Event struct {
	Step   string `json:"step"`
	File   string `json:"file,omitempty"`
	Status string `json:"status"` // start | ok | error | done
	Bytes  int64  `json:"bytes,omitempty"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

// stream writes NDJSON events and flushes each one.
type stream struct {
	enc     *json.Encoder
	flusher http.Flusher
}

// newStream returns nil for a nil writer, and send tolerates a nil receiver:
// a caller with nowhere to report progress (the retain verb, a test) must not
// have to invent a discard writer, and encoding into a nil writer panics.
func newStream(w io.Writer) *stream {
	if w == nil {
		return nil
	}
	s := &stream{enc: json.NewEncoder(w)}
	if f, ok := w.(http.Flusher); ok {
		s.flusher = f
	}
	return s
}

// send writes one event. A write failure means the operator hung up; it does
// NOT abort the promote, because abandoning a half-copied release because
// somebody closed a browser tab is worse than finishing it.
func (s *stream) send(e Event) {
	if s == nil {
		return
	}
	_ = s.enc.Encode(e)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// Promote runs the whole sequence for one row, streaming progress to w.
//
// The returned error is also emitted as a final error event, so a caller
// reading only the stream sees the failure and a caller checking only the
// error sees it too. A promote that failed silently in one of the two would be
// a promote an operator believes succeeded.
func Promote(ctx context.Context, d Deps, rowID int64, w io.Writer) (err error) {
	st := newStream(w)
	defer func() {
		if err != nil {
			st.send(Event{Step: "promote", Status: "error", Error: err.Error()})
		}
	}()

	if d.GitHub == nil {
		return ErrNoPublisher
	}
	if d.Public == nil {
		return ErrNoPublicStore
	}
	if d.Staging == nil {
		return fmt.Errorf("publish: no staging store is configured")
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}

	row, err := d.Store.Get(rowID)
	if err != nil {
		return err
	}
	if row.State != catalog.StateStaged {
		return fmt.Errorf("%w: row %d is %s, only a staged row can be promoted", store.ErrBadState, rowID, row.State)
	}
	var artifacts []register.Artifact
	if err := json.Unmarshal([]byte(row.ArtifactsJSON), &artifacts); err != nil {
		return fmt.Errorf("publish: row %d has an unreadable artifact list: %w", rowID, err)
	}
	if len(artifacts) == 0 {
		return fmt.Errorf("publish: row %d lists no artifacts", rowID)
	}

	st.send(Event{Step: "promote", Status: "start",
		Detail: fmt.Sprintf("%s %s on %s", row.Component, row.Stamp, row.Channel)})

	// ── 1. Verify ─────────────────────────────────────────────────────────
	// Every artifact is checked against the catalog BEFORE anything is copied.
	// The row is what an operator approved; if the bytes in the bucket are not
	// the bytes the row describes, the thing to publish does not exist.
	bodies := map[string][]byte{}
	for _, a := range artifacts {
		st.send(Event{Step: "verify", File: path.Base(a.Key), Status: "start"})
		size, err := d.Staging.Head(ctx, a.Key)
		if err != nil {
			return fmt.Errorf("verify %s: %w", a.Key, err)
		}
		if size != a.Size {
			// Checked before the body is fetched: a wrong-sized object is
			// refused without pulling half a gigabyte first.
			return fmt.Errorf("verify %s: staged object is %d bytes, the catalog says %d", a.Key, size, a.Size)
		}
		body, err := d.Staging.Get(ctx, a.Key)
		if err != nil {
			return fmt.Errorf("verify %s: %w", a.Key, err)
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != a.SHA256 {
			return fmt.Errorf("verify %s: sha256 is %s, the catalog says %s", a.Key, got, a.SHA256)
		}
		bodies[a.Key] = body
		st.send(Event{Step: "verify", File: path.Base(a.Key), Status: "ok", Bytes: size})
	}
	// The sums file and its signature carry no recorded hash — they are what
	// the recorded hashes were taken from — so they are fetched, not checked
	// against the row. They are still needed in hand: the GitHub release
	// carries them as assets.
	for _, key := range []string{row.SumsKey, row.MinisigKey} {
		st.send(Event{Step: "verify", File: path.Base(key), Status: "start"})
		body, err := d.Staging.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("verify %s: %w", key, err)
		}
		bodies[key] = body
		st.send(Event{Step: "verify", File: path.Base(key), Status: "ok", Bytes: int64(len(body))})
	}

	// ── 2. Copy ───────────────────────────────────────────────────────────
	// EVERY file before the manifest is written. A public prefix carrying
	// three of four platforms and a manifest naming all four is a release that
	// is broken for one architecture and looks fine.
	//
	// The copy is SERVER-SIDE, which means the bytes it moves are re-read from
	// staging rather than pushed from the ones just hashed — a window, in
	// principle, between verifying and copying. It is unreachable here, and by
	// construction rather than by luck: the staging bucket has exactly one
	// writer (a cut, uploading a stamp that does not exist yet), the Staging
	// seam deliberately exposes no Delete, and a stamp is unique per
	// (component, channel) in the catalog — so nothing in this system can
	// replace an object under a key that promote is reading. If staging ever
	// gains a second writer, this becomes a real window and the copy has to
	// become an upload of the verified bytes we already hold.
	base := manifest.PublicBase(row.Component, row.Channel, row.Stamp)
	var zipNames []string
	copyOrder := append(keysOf(artifacts), row.SumsKey, row.MinisigKey)
	for _, srcKey := range copyOrder {
		name := path.Base(srcKey)
		dst := base + "/" + name
		st.send(Event{Step: "copy", File: name, Status: "start"})
		if err := d.Public.Copy(ctx, d.Staging.Bucket(), srcKey, dst); err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
		st.send(Event{Step: "copy", File: name, Status: "ok", Bytes: int64(len(bodies[srcKey]))})
	}
	for _, a := range artifacts {
		zipNames = append(zipNames, path.Base(a.Key))
	}

	// ── 3. GitHub ─────────────────────────────────────────────────────────
	tag := ReleaseTag(row.Component, row.Stamp)
	prerelease := row.Channel == catalog.ChannelBeta
	st.send(Event{Step: "github", File: tag, Status: "start"})
	rel, err := d.GitHub.CreateRelease(ctx, tag,
		row.Component+" "+row.Version, ReleaseNotes(row, zipNames), prerelease)
	if err != nil {
		return fmt.Errorf("github release %s: %w", tag, err)
	}
	for _, srcKey := range copyOrder {
		name := path.Base(srcKey)
		st.send(Event{Step: "github-asset", File: name, Status: "start"})
		// UploadAsset is required to be idempotent (backend.GitHub): GitHub
		// refuses a duplicate asset name, so a retry after a partial upload
		// depends on the implementation replacing rather than adding.
		if err := d.GitHub.UploadAsset(ctx, rel, name, backend.ContentType(name), bodies[srcKey]); err != nil {
			return fmt.Errorf("github asset %s: %w", name, err)
		}
		st.send(Event{Step: "github-asset", File: name, Status: "ok"})
	}
	st.send(Event{Step: "github", File: tag, Status: "ok"})

	// ── 4. Manifest, LAST ─────────────────────────────────────────────────
	now := d.Now()
	m := manifest.Build(row.Component, row.Channel, row.Version, row.Stamp,
		zipNames, path.Base(row.SumsKey), path.Base(row.MinisigKey), now)
	body, err := m.Marshal()
	if err != nil {
		return err
	}
	mkey := manifest.Key(row.Component, row.Channel)
	st.send(Event{Step: "manifest", File: mkey, Status: "start"})
	if err := d.Public.Put(ctx, mkey, body, backend.ContentType(mkey)); err != nil {
		return fmt.Errorf("manifest %s: %w", mkey, err)
	}
	st.send(Event{Step: "manifest", File: mkey, Status: "ok"})

	// ── 5. Flip ───────────────────────────────────────────────────────────
	st.send(Event{Step: "flip", Status: "start"})
	if err := d.Store.Promote(rowID, now); err != nil {
		return fmt.Errorf("flip row %d: %w", rowID, err)
	}
	st.send(Event{Step: "flip", Status: "ok"})
	log.Info("promoted", "row", rowID, "component", row.Component, "channel", row.Channel, "stamp", row.Stamp)

	// ── 6. Retention ──────────────────────────────────────────────────────
	if d.Retain != nil {
		for _, e := range d.Retain(ctx, row.Component, row.Channel) {
			st.send(e)
		}
	}

	st.send(Event{Step: "promote", Status: "done",
		Detail: fmt.Sprintf("%s %s is live on %s", row.Component, row.Stamp, row.Channel)})
	return nil
}

// Yank takes a public row off its channel.
//
// The manifest moves FIRST, then the row flips. That ordering is deliberate
// and is the opposite of promote's: a failure between the two must leave the
// yanked build UNSERVED rather than still being handed to every installer. The
// catalog can be corrected afterwards; a channel still serving a withdrawn
// release cannot be, until someone notices.
func Yank(ctx context.Context, d Deps, rowID int64, w io.Writer) (err error) {
	st := newStream(w)
	defer func() {
		if err != nil {
			st.send(Event{Step: "yank", Status: "error", Error: err.Error()})
		}
	}()
	if d.Public == nil {
		return ErrNoPublicStore
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}

	row, err := d.Store.Get(rowID)
	if err != nil {
		return err
	}
	if row.State != catalog.StatePublic {
		return fmt.Errorf("%w: row %d is %s, only a public row can be yanked", store.ErrBadState, rowID, row.State)
	}
	st.send(Event{Step: "yank", Status: "start",
		Detail: fmt.Sprintf("%s %s on %s", row.Component, row.Stamp, row.Channel)})

	// Chosen ONCE. The manifest is written naming this row and the same id is
	// handed to the store, so the catalog cannot end up marking a different
	// build current from the one the channel is serving.
	successor, err := d.Store.NewestPublicExcept(row.Component, row.Channel, rowID)
	if err != nil {
		return err
	}
	mkey := manifest.Key(row.Component, row.Channel)
	if successor != nil {
		var names []string
		names, err = artifactNames(successor)
		if err != nil {
			return err
		}
		m := manifest.Build(successor.Component, successor.Channel, successor.Version, successor.Stamp,
			names, path.Base(successor.SumsKey), path.Base(successor.MinisigKey), d.Now())
		body, merr := m.Marshal()
		if merr != nil {
			return merr
		}
		st.send(Event{Step: "manifest", File: mkey, Status: "start", Detail: "re-pointing to " + successor.Stamp})
		if err := d.Public.Put(ctx, mkey, body, backend.ContentType(mkey)); err != nil {
			return fmt.Errorf("manifest %s: %w", mkey, err)
		}
	} else {
		// Nothing left to serve: the entry is REMOVED rather than left
		// pointing at a withdrawn build.
		//
		// The 404 that leaves behind is the SIGNAL, not a gap. The bootstrap
		// treats a manifest that answers "not found" as this channel saying it
		// serves nothing, and refuses — it does NOT fall through to the GitHub
		// tag list, which still carries this release's tag because yank
		// deliberately leaves the GitHub release in place. Falling through
		// would reinstall exactly what was just withdrawn
		// (tools/bootstrap.template.sh, step 1).
		st.send(Event{Step: "manifest", File: mkey, Status: "start", Detail: "no public row remains; removing"})
		if err := d.Public.Delete(ctx, mkey); err != nil {
			return fmt.Errorf("manifest %s: %w", mkey, err)
		}
	}
	st.send(Event{Step: "manifest", File: mkey, Status: "ok"})

	st.send(Event{Step: "flip", Status: "start"})
	var successorID int64
	if successor != nil {
		successorID = successor.ID
	}
	if err := d.Store.Yank(rowID, successorID, d.Now()); err != nil {
		return fmt.Errorf("flip row %d: %w", rowID, err)
	}
	st.send(Event{Step: "flip", Status: "ok"})
	log.Info("yanked", "row", rowID, "component", row.Component, "channel", row.Channel, "stamp", row.Stamp)
	st.send(Event{Step: "yank", Status: "done"})
	return nil
}

// ReleaseTag is the git tag a promoted row publishes under.
func ReleaseTag(component, stamp string) string { return component + "/" + stamp }

func keysOf(artifacts []register.Artifact) []string {
	out := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, a.Key)
	}
	return out
}

func artifactNames(rv *store.ReleaseVersion) ([]string, error) {
	var artifacts []register.Artifact
	if err := json.Unmarshal([]byte(rv.ArtifactsJSON), &artifacts); err != nil {
		return nil, fmt.Errorf("publish: row %d has an unreadable artifact list: %w", rv.ID, err)
	}
	var names []string
	for _, a := range artifacts {
		names = append(names, path.Base(a.Key))
	}
	return names, nil
}
