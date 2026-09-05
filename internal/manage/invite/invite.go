// Package invite mints the one path from a staged cut to a host
// (release-management.md §4).
//
// An invite is a self-contained `curl … | sh` URL: presigned GET URLs for the
// four platform zips plus the sums file and its signature, wrapped in a
// generated install.sh that runs the SAME verification chain as the public
// bootstrap, and itself uploaded to a random single-use key in the private
// staging bucket and presigned.
//
// Three properties are the whole control surface, because there is no
// per-link revocation:
//
//   - The TTL is short and fixed (48 hours).
//   - Every mint is audited — who, which row, when it expires — and a mint
//     whose audit row fails to write FAILS, rather than handing out a link
//     nobody recorded.
//   - The link is a delivery mechanism, not a trust anchor. A leaked URL still
//     installs only bytes the release key signed.
package invite

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/clawee-git/release/internal/manage/backend"
	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/intake"
	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/register"
)

// TTL is how long an invite lives, link and artifacts alike.
//
// Long enough for an operator to hand it to one person and have them run it;
// short enough that a leak is bounded to "installs the build the operator
// already chose to hand out, for at most two days". Both halves matter: a
// script URL that outlived its artifact URLs would be a link that downloads a
// verifier and then 403s.
const TTL = 48 * time.Hour

// ErrNotMintable is returned for a row whose state forbids an invite. The
// handler answers 409: the request was well-formed and the row exists, but a
// yanked or expired release is never handed to anyone.
var ErrNotMintable = errors.New("invite: row is not mintable")

// Deps is what minting needs.
type Deps struct {
	Store   *store.Store
	Staging backend.Staging
	Now     backend.Clock
}

// Result is one minted invite.
type Result struct {
	InviteID  int64     `json:"invite_id"`
	RowID     int64     `json:"row_id"`
	URL       string    `json:"url"`
	Command   string    `json:"command"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Mint builds and records an invite for row, minted by admin.
func Mint(ctx context.Context, d Deps, row *store.ReleaseVersion, admin string) (Result, error) {
	// staged or public only. A yanked row was withdrawn on purpose and an
	// expired one has had its bytes pruned — handing either to a person is
	// either undoing a decision or promising an install that cannot work.
	switch row.State {
	case catalog.StateStaged, catalog.StatePublic:
	default:
		return Result{}, fmt.Errorf("%w: %s %s is %s", ErrNotMintable, row.Component, row.Stamp, row.State)
	}

	var artifacts []register.Artifact
	if err := json.Unmarshal([]byte(row.ArtifactsJSON), &artifacts); err != nil {
		return Result{}, fmt.Errorf("invite: row %d has an unreadable artifact list: %w", row.ID, err)
	}
	if len(artifacts) == 0 {
		return Result{}, fmt.Errorf("invite: row %d lists no artifacts", row.ID)
	}

	now := d.Now()
	expires := now.Add(TTL)

	pubkey, err := intake.ReleasePubkeyLine()
	if err != nil {
		return Result{}, err
	}

	data := scriptData{
		Component: row.Component,
		Version:   row.Version,
		ExpiresAt: expires.UTC().Format("2006-01-02 15:04 UTC"),
		Pubkey:    pubkey,
	}
	for _, a := range artifacts {
		url, err := d.Staging.Presign(a.Key, TTL)
		if err != nil {
			return Result{}, fmt.Errorf("invite: presign %s: %w", a.Key, err)
		}
		data.Platforms = append(data.Platforms, platformScript{
			// The row records the platform as "darwin/arm64"; the script
			// matches on "$OS-$ARCH". One translation, here.
			Platform: strings.ReplaceAll(a.Platform, "/", "-"),
			File:     path.Base(a.Key),
			URL:      url,
		})
	}
	if data.SumsURL, err = d.Staging.Presign(row.SumsKey, TTL); err != nil {
		return Result{}, fmt.Errorf("invite: presign sums: %w", err)
	}
	if data.MinisigURL, err = d.Staging.Presign(row.MinisigKey, TTL); err != nil {
		return Result{}, fmt.Errorf("invite: presign signature: %w", err)
	}

	script, err := renderScript(data)
	if err != nil {
		return Result{}, err
	}

	// A random key per mint, in the PRIVATE bucket. Random because the key is
	// the only thing between an unauthenticated caller and the script; private
	// because even with the key, the object is unreachable without the
	// presigned URL.
	key, err := scriptKey()
	if err != nil {
		return Result{}, err
	}
	if err := d.Staging.Put(ctx, key, []byte(script), backend.ContentType(key)); err != nil {
		return Result{}, fmt.Errorf("invite: upload the install script: %w", err)
	}
	scriptURL, err := d.Staging.Presign(key, TTL)
	if err != nil {
		return Result{}, fmt.Errorf("invite: presign the install script: %w", err)
	}

	// The audit row is part of the mint contract, not best-effort. There is no
	// per-link revocation, so a link nobody recorded is a link nobody can
	// account for afterwards.
	id, err := d.Store.CreateInvite(store.Invite{
		RowID: row.ID, MintedBy: admin, ScriptKey: key, URL: scriptURL,
		CreatedAt: now, ExpiresAt: expires,
	})
	if err != nil {
		return Result{}, fmt.Errorf("invite: record the mint: %w", err)
	}

	return Result{
		InviteID: id, RowID: row.ID, URL: scriptURL,
		Command: Command(scriptURL), ExpiresAt: expires,
	}, nil
}

// Command is the line an operator copies. One spelling, used by the API
// response and the invites page alike.
func Command(url string) string {
	return "curl -fsSL '" + url + "' | sh"
}

// scriptKey is invites/<32 hex>/install.sh.
func scriptKey() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("invite: generate a script key: %w", err)
	}
	return "invites/" + hex.EncodeToString(raw) + "/install.sh", nil
}
