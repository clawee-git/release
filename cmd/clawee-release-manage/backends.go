package main

// Wiring the remote stores from flags.
//
// Every one is OPTIONAL, and a missing one disables exactly the operation that
// needs it, with a 503 that names the gap — never a silent no-op. That is what
// lets the service be brought up in stages: catalog and read surfaces first,
// then invites once the staging bucket is wired, then promote once the public
// bucket and the GitHub token are.
//
// There is deliberately NO default bucket name, and nothing is read from the
// environment. A guessed bucket is either a failed upload or a write to the
// public one, and the second publishes a build nobody approved — the same
// refusal the cut already makes (AGENTS.md, "Sealed config").

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/clawee-git/release/internal/manage/backend"
	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/web"
	"github.com/clawee-git/release/internal/r2"
)

// storeOpts is the flag group shared by `serve` and `retain`, so the two
// cannot be pointed at different buckets by accident.
type storeOpts struct {
	r2Account     string
	stagingBucket string
	publicBucket  string
	r2Creds       string
	githubRepo    string
	githubToken   string
}

func (o *storeOpts) register(fs *flag.FlagSet) {
	fs.StringVar(&o.r2Account, "r2-account", "", "the Cloudflare `account` id the buckets live in")
	fs.StringVar(&o.stagingBucket, "staging-bucket", "", "the PRIVATE staging `bucket` a cut uploads to")
	fs.StringVar(&o.publicBucket, "public-bucket", "", "the public `bucket` promote copies into")
	fs.StringVar(&o.r2Creds, "r2-creds", "", "`path` to the file holding access_key_id and secret_access_key")
	fs.StringVar(&o.githubRepo, "github-repo", "", "`owner/repo` to publish releases to")
	fs.StringVar(&o.githubToken, "github-token-file", "", "`path` to a file holding the GitHub token")
}

// backends builds the seams the flags describe. A partially configured store
// is an ERROR, not a silently disabled one: an operator who spelled one flag
// and forgot its partner meant to have that store.
func (o *storeOpts) backends(n *node) (web.Backends, error) {
	var out web.Backends

	wantR2 := o.r2Account != "" || o.stagingBucket != "" || o.publicBucket != "" || o.r2Creds != ""
	if wantR2 {
		for _, f := range []struct{ name, val string }{
			{"r2-account", o.r2Account}, {"r2-creds", o.r2Creds},
		} {
			if f.val == "" {
				return out, usagef(n, "--%s is required once any other R2 flag is given; a half-configured store is a store that fails at the first promote", f.name)
			}
		}
		key, secret, err := r2.ReadCreds(o.r2Creds)
		if err != nil {
			return out, err
		}
		if o.stagingBucket != "" {
			out.Staging = r2.New(o.r2Account, o.stagingBucket, key, secret, nil)
		}
		if o.publicBucket != "" {
			if o.publicBucket == o.stagingBucket {
				// One bucket for both is not a configuration, it is a public
				// staging store: everything a cut uploads would be readable
				// before anyone promoted it.
				return out, usagef(n, "--public-bucket and --staging-bucket are the same bucket (%q); staging must be private by construction, not by prefix", o.publicBucket)
			}
			out.Public = r2.New(o.r2Account, o.publicBucket, key, secret, nil)
		}
	}

	if o.githubRepo != "" || o.githubToken != "" {
		if o.githubRepo == "" || o.githubToken == "" {
			return out, usagef(n, "--github-repo and --github-token-file are given together or not at all")
		}
		owner, repo, ok := strings.Cut(o.githubRepo, "/")
		if !ok || owner == "" || repo == "" {
			return out, usagef(n, "--github-repo %q is not owner/repo", o.githubRepo)
		}
		raw, err := os.ReadFile(o.githubToken)
		if err != nil {
			return out, fmt.Errorf("read GitHub token: %w", err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return out, fmt.Errorf("GitHub token file %q is empty", o.githubToken)
		}
		out.GitHub = backend.NewGitHubClient(owner, repo, token, &backend.Guard{})
	}
	return out, nil
}

// describe is what the startup log and `retain` print: which seams are live,
// which are absent, and therefore which operations will refuse. Never the
// credentials, and never the token.
func (o *storeOpts) describe(b web.Backends) string {
	var parts []string
	parts = append(parts, seam("staging", b.Staging != nil, "invites"))
	parts = append(parts, seam("public", b.Public != nil, "promote, yank, retention"))
	parts = append(parts, seam("github", b.GitHub != nil, "promote"))
	return strings.Join(parts, "; ")
}

func seam(name string, present bool, gates string) string {
	if present {
		return name + ": configured"
	}
	return fmt.Sprintf("%s: ABSENT (%s will refuse)", name, gates)
}

// componentChannel is one retention scope.
type componentChannel struct{ component, channel string }

func componentsAndChannels() []componentChannel {
	var out []componentChannel
	for _, c := range catalog.Components {
		for _, ch := range catalog.Channels {
			out = append(out, componentChannel{c, ch})
		}
	}
	return out
}

// nowFunc is the service's clock. One place, so a future --at flag for a
// replayed retention pass has somewhere to go.
func nowFunc() backend.Clock { return time.Now }
