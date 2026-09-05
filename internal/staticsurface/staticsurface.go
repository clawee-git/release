// Package staticsurface is the ONE list of files the release host serves as
// static bytes, as paths relative to both the kit checkout and the nginx
// static root.
//
// Two things read it and they must not disagree. `publish-static` copies
// exactly these files to the host; the public site's link check asserts that
// every relative link a rendered page emits either resolves to a service route
// or names a file on THIS list. A page that links to a static file nobody
// publishes is a 404 an operator only finds in production, and a published
// file nothing links to is dead weight in the trust anchor's directory.
//
// The site's own pages are deliberately absent: they are server-rendered by
// the manage service now, so there is nothing to copy.
package staticsurface

import "github.com/clawee-git/release/internal/manage/catalog"

// Pubkey is the release signing public key. The bootstraps bake it in, so this
// copy is for a human verifying a download by hand — which is the one place
// the site's /verify page sends them.
const Pubkey = "clawee-release.pub"

// Files returns every static path, in a stable order.
//
// The beta twins are listed unconditionally: they are RENDERED unconditionally
// by tools/gen-bootstraps.sh (the beta manifest they resolve is what decides
// whether a beta exists, not the presence of the file), so a conditional list
// here would be a second, weaker belief about the same question.
func Files() []string {
	out := []string{Pubkey}
	for _, comp := range catalog.Components {
		out = append(out,
			comp+"/install.sh",
			comp+"/upgrade.sh",
			comp+"/beta.install.sh",
			comp+"/beta.upgrade.sh",
			comp+"/version.js",
			comp+"/beta.version.js",
		)
	}
	return out
}

// Has reports whether path (no leading slash) is on the list.
func Has(path string) bool {
	for _, f := range Files() {
		if f == path {
			return true
		}
	}
	return false
}
