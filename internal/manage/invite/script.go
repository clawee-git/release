package invite

// Rendering the invite's install.sh.
//
// The script is generated per mint because the presigned URLs are: they carry
// a signature over an expiry, so they cannot be baked into anything static.
// Everything ELSE in it is fixed — the verification chain, the public key, the
// order of the steps — and is the same chain tools/bootstrap.template.sh runs,
// deliberately, so an invite is not a second, weaker install path.

import (
	_ "embed"
	"fmt"
	"strings"
	"text/template"
)

// scriptTemplate is the POSIX-sh body, rendered per mint.
//
//go:embed install.sh.tmpl
var scriptTemplate string

// platformScript binds one platform's presigned URL and real object basename.
//
// The BASENAME is threaded through as data rather than re-derived inside the
// template, because it has to match the filename in SHA256SUMS.txt exactly. A
// second derivation of the naming rule, keyed on something slightly different,
// is how a checksum lookup starts failing for one platform only.
type platformScript struct {
	Platform string // "darwin-arm64", matching the script's "$OS-$ARCH"
	File     string
	URL      string
}

type scriptData struct {
	Component  string
	Version    string
	ExpiresAt  string
	Pubkey     string
	Platforms  []platformScript
	SumsURL    string
	MinisigURL string
}

// renderScript produces the install.sh body.
//
// It is a pure function: no I/O, no clock, no presigning. The caller presigns
// and passes URLs in, which is what lets the execution test drive the real
// script against a local server.
func renderScript(d scriptData) (string, error) {
	// Every URL and filename is embedded inside single quotes in the script. A
	// single quote in one would break out of that quoting. Presigned SigV4
	// URLs never contain one — refuse fail-closed rather than emit a script
	// that is broken at best and injectable at worst.
	for _, s := range append([]string{d.SumsURL, d.MinisigURL, d.Pubkey}, flatten(d.Platforms)...) {
		if strings.ContainsAny(s, "'\n") {
			return "", fmt.Errorf("invite: refusing to render a script around %q: it contains a quote or newline", s)
		}
	}
	t, err := template.New("install.sh").Parse(scriptTemplate)
	if err != nil {
		return "", fmt.Errorf("invite: parse install.sh template: %w", err)
	}
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "", fmt.Errorf("invite: render install.sh: %w", err)
	}
	return b.String(), nil
}

func flatten(ps []platformScript) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.File, p.URL)
	}
	return out
}
