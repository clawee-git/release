package main

// The `ops render` verb: the deployment artefacts, written to a directory the
// operator then installs BY HAND.
//
// It renders and stops. Nothing here copies a file to a host, reloads nginx,
// or enables a unit — activating a service on a production host is an operator
// act (`~/.agents/guidelines/release.md` §11), and a renderer that also
// installed would be the one command in this repo that could not be run
// safely from a session.
//
// Everything the templates need arrives as a FLAG, and nothing is read from
// the environment. The values are the deployment's — the data root, the
// buckets, the token file, the host — and those live in the sealed
// `release.dp` config, not in this binary and not in an environment a unit
// generator would inherit from whoever ran it (privilege.md: a flag-steered
// root is validated at its own writer, and the root itself is never
// environment-steerable).
//
// The nginx vhost is the exception that is COMMITTED: it names no secret, so
// `ops/nginx/<host>.conf` is this template's output for the release host, and
// TestCommittedVhostIsTheRenderedOne byte-diffs the two. The units are not
// committed and never will be — their ExecStart carries the bucket names, the
// credential paths and the data root.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// Defaults that are DECISIONS, not conveniences.
const (
	// defaultServiceUser is a dedicated, unprivileged account. The service
	// terminates no TLS, binds no privileged port and owns exactly one
	// directory; anything more privileged than this is a wider blast radius
	// for no capability gained.
	defaultServiceUser = "clawee-release"
	// defaultExecPath is where the operator's install step puts the binary.
	defaultExecPath = "/usr/local/bin/clawee-release-manage"
	// defaultRetainAt is the nightly retention pass, in systemd's OnCalendar
	// time-of-day form. Off the hour on purpose: an exact 03:00 shares its
	// wake-up with every other timer on the host.
	defaultRetainAt = "03:20"
	// defaultTLSProtocols is pinned at TLSv1.2 because at least one origin's
	// nginx build rejects TLSv1.3 outright and fails `nginx -t` — see
	// ops/README.md for the host this bit.
	defaultTLSProtocols = "TLSv1.2"
)

// Rendered filenames. They are constants because the unit names are what the
// runbook's `systemctl` lines type, and a generated name the documentation
// guesses at is a name the two can disagree about.
const (
	serviceUnitFile = "clawee-release-manage.service"
	retainUnitFile  = "clawee-release-manage-retain.service"
	retainTimerFile = "clawee-release-manage-retain.timer"
)

// opsRenderOpts is the whole rendering input. The fields are EXPORTED because
// the templates read them directly: a second, template-only struct would be a
// second place for a flag's value to be spelled, and the two would drift the
// first time a flag was added.
type opsRenderOpts struct {
	Out string

	// The service half.
	Exec    string
	User    string
	Group   string
	DataDir string
	Listen  string
	BaseURL string
	storeOpts
	SecretKey string
	RetainAt  string

	// The edge half.
	Host         string
	StaticDir    string
	TLSCert      string
	TLSKey       string
	TLSProtocols string
}

func (o *opsRenderOpts) register(fs *flag.FlagSet) {
	fs.StringVar(&o.Out, "out", "", "the `dir` to write the rendered files into (required); it is created if missing")
	fs.StringVar(&o.Exec, "exec", defaultExecPath, "`path` the service binary is installed at, as the unit will exec it")
	fs.StringVar(&o.User, "user", defaultServiceUser, "the dedicated `user` the unit runs as")
	fs.StringVar(&o.Group, "group", "", "the unit's `group`; empty leaves systemd to use the user's primary group")
	fs.StringVar(&o.DataDir, "data-dir", "", "the `dir` holding the catalog and the service secret key (required); it is the unit's only writable path")
	fs.StringVar(&o.Listen, "listen", defaultListen, "`address` the unit binds and the vhost proxies to; loopback, because the edge terminates TLS")
	fs.StringVar(&o.BaseURL, "base-url", "", "the public `url` the service is reached at (required)")
	fs.StringVar(&o.SecretKey, "secret-key", "", "`path` to the service secret key, when it is not the default inside --data-dir")
	o.storeOpts.register(fs)
	fs.StringVar(&o.RetainAt, "retain-at", defaultRetainAt, "the nightly retention pass's `time` of day, as systemd OnCalendar spells it")
	fs.StringVar(&o.Host, "host", "", "the `hostname` the vhost answers for (required); it names the rendered conf too")
	fs.StringVar(&o.StaticDir, "static-dir", "", "the nginx static root (required) — the same `dir` publish-static writes to")
	fs.StringVar(&o.TLSCert, "tls-cert", "", "`path` to the origin certificate; defaults to the Let's Encrypt layout for --host")
	fs.StringVar(&o.TLSKey, "tls-key", "", "`path` to the origin private key; defaults to the Let's Encrypt layout for --host")
	fs.StringVar(&o.TLSProtocols, "tls-protocols", defaultTLSProtocols, "the `protocols` line for the vhost")
}

func runOpsRender(e *env, n *node, args []string) error {
	var o opsRenderOpts
	fs := flag.NewFlagSet(toolName+" "+pathOf(n), flag.ContinueOnError)
	o.register(fs)
	handled, err := parseVerbFlags(e, n, fs, args)
	if handled || err != nil {
		return err
	}
	if err := rejectResiduals(n, fs); err != nil {
		return err
	}
	for _, req := range []struct{ name, val string }{
		{"out", o.Out}, {"data-dir", o.DataDir}, {"base-url", o.BaseURL},
		{"host", o.Host}, {"static-dir", o.StaticDir},
	} {
		if strings.TrimSpace(req.val) == "" {
			return usagef(n, "--%s is required; every value the templates carry is the deployment's and none of them is guessed or read from the environment", req.name)
		}
	}

	files, err := renderOps(o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", o.Out, err)
	}
	for _, f := range files {
		path := filepath.Join(o.Out, filepath.FromSlash(f.name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(f.body), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Fprintf(e.stdout, "wrote %s\n", path)
	}
	fmt.Fprintf(e.stdout, "\nNothing was installed. Copying these into place, reloading nginx and\n"+
		"enabling the units are OPERATOR steps — see ops/README.md.\n")
	return nil
}

// renderedFile is one artefact, named by its path relative to --out.
type renderedFile struct{ name, body string }

// renderOps produces every artefact. It is a pure function of the options so
// the tests render without writing anything, and so the committed vhost can be
// byte-compared against it.
func renderOps(o opsRenderOpts) ([]renderedFile, error) {
	if o.TLSCert == "" {
		o.TLSCert = "/etc/letsencrypt/live/" + o.Host + "/fullchain.pem"
	}
	if o.TLSKey == "" {
		o.TLSKey = "/etc/letsencrypt/live/" + o.Host + "/privkey.pem"
	}

	data := struct {
		opsRenderOpts
		ServeArgs   []string
		RetainArgs  []string
		Tool        string
		ServiceUnit string
		RetainUnit  string
	}{o, o.serveArgs(), o.retainArgs(), toolName, serviceUnitFile, retainUnitFile}

	out := []renderedFile{}
	for _, tmpl := range []struct {
		name string
		t    *template.Template
	}{
		{"systemd/" + serviceUnitFile, serviceTmpl},
		{"systemd/" + retainUnitFile, retainServiceTmpl},
		{"systemd/" + retainTimerFile, retainTimerTmpl},
		{"nginx/" + o.Host + ".conf", vhostTmpl},
	} {
		var b strings.Builder
		if err := tmpl.t.Execute(&b, data); err != nil {
			return nil, fmt.Errorf("render %s: %w", tmpl.name, err)
		}
		out = append(out, renderedFile{name: tmpl.name, body: b.String()})
	}
	return out, nil
}

// serveArgs is the unit's ExecStart argument list — the SINGLE definition of
// the rendered invocation, so a unit cannot name a flag `serve` does not
// accept. An optional store flag is omitted when it has no value rather than
// rendered empty: `serve` refuses a half-configured store, and a unit that
// renders `--staging-bucket ""` is a service that will not start.
func (o *opsRenderOpts) serveArgs() []string {
	args := []string{"serve", "--data-dir", o.DataDir, "--listen", o.Listen, "--base-url", o.BaseURL}
	if o.SecretKey != "" {
		args = append(args, "--secret-key", o.SecretKey)
	}
	return append(args, o.storeArgs()...)
}

// retainArgs is the timer's invocation: the same store, by construction. The
// nightly pass and the service reading the same flags is what stops a
// retention run from pruning a bucket the service never publishes to.
func (o *opsRenderOpts) retainArgs() []string {
	return append([]string{"retain", "--data-dir", o.DataDir}, o.storeArgs()...)
}

func (o *opsRenderOpts) storeArgs() []string {
	var args []string
	for _, f := range []struct{ name, val string }{
		{"r2-account", o.r2Account},
		{"r2-creds", o.r2Creds},
		{"staging-bucket", o.stagingBucket},
		{"public-bucket", o.publicBucket},
		{"github-repo", o.githubRepo},
		{"github-token-file", o.githubToken},
		{"public-base-url", o.publicBaseURL},
	} {
		if strings.TrimSpace(f.val) != "" {
			args = append(args, "--"+f.name, f.val)
		}
	}
	return args
}

// sysq double-quotes a token for a systemd ExecStart= line, escaping the two
// characters systemd's C-style string parser reads specially. A plain join is
// not safe: a data root with a space in it becomes two arguments, and the
// service starts against a directory nobody named.
func sysq(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

var tmplFuncs = template.FuncMap{"sysq": sysq}

// serviceTmpl is the service unit.
//
// The hardening lines are the whole reason this is a template rather than a
// paragraph in the runbook. ProtectSystem=strict makes the entire filesystem
// read-only and ReadWritePaths re-opens exactly one directory — the data root,
// which holds the catalog and the sealed secret. Everything else the service
// reads (the credentials file, the GitHub token file) it only reads, so the
// single writable path is the complete list, and a future flag that needs a
// second one has to say so here rather than discovering it at runtime.
//
// NoNewPrivileges closes the setuid escalation path from an unprivileged
// account, and the unit names User= explicitly: an unset User= is a system
// unit running as root, which is what this service exists NOT to need
// (privilege.md — the two things elevation is for, neither of which is
// serving HTTP on a loopback port).
var serviceTmpl = template.Must(template.New("service").Funcs(tmplFuncs).Parse(
	`[Unit]
Description=Clawee release manage service
Documentation=https://github.com/clawee-git/release/blob/main/ops/README.md
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User={{.User}}
{{if .Group}}Group={{.Group}}
{{end}}ExecStart={{sysq .Exec}}{{range .ServeArgs}} {{sysq .}}{{end}}
Restart=always
RestartSec=2
# A promote streams progress for the whole publish. SIGTERM lets the service
# drain its own bounded shutdown rather than being killed mid-copy.
KillSignal=SIGTERM
TimeoutStopSec=60

# Hardening. ProtectSystem=strict makes the whole filesystem read-only;
# ReadWritePaths re-opens exactly one directory, and the data root is the
# complete list — the credentials and the token are read, never written.
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths={{.DataDir}}
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
# AF_UNIX is listed because a cgo build resolves names through NSS over a
# unix socket; without it the service would fail every outbound lookup, and
# the symptom reads as a DNS outage. Harmless for the pure-Go build.
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LockPersonality=yes
UMask=0077

[Install]
WantedBy=multi-user.target
`))

// retainServiceTmpl is the nightly pass. Type=oneshot: it runs, prints what it
// pruned, and exits — there is nothing to keep alive, and Restart= on a
// oneshot that failed because a bucket was unreachable would retry it in a
// tight loop instead of at the next OnCalendar.
var retainServiceTmpl = template.Must(template.New("retain-service").Funcs(tmplFuncs).Parse(
	`[Unit]
Description=Clawee release retention pass
Documentation=https://github.com/clawee-git/release/blob/main/ops/README.md
After=network-online.target {{.ServiceUnit}}
Wants=network-online.target

[Service]
Type=oneshot
User={{.User}}
{{if .Group}}Group={{.Group}}
{{end}}ExecStart={{sysq .Exec}}{{range .RetainArgs}} {{sysq .}}{{end}}

NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths={{.DataDir}}
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
# AF_UNIX is listed because a cgo build resolves names through NSS over a
# unix socket; without it the service would fail every outbound lookup, and
# the symptom reads as a DNS outage. Harmless for the pure-Go build.
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LockPersonality=yes
UMask=0077
`))

// retainTimerTmpl schedules it.
//
// Persistent=true is the point of the timer rather than a cron line: a host
// that was down at the scheduled hour runs the missed pass on the next boot.
// Retention is a NET — promote already prunes the pair it published — so a
// skipped night is not a failure, but a night silently skipped forever is how
// a bucket grows without anyone noticing.
var retainTimerTmpl = template.Must(template.New("retain-timer").Funcs(tmplFuncs).Parse(
	`[Unit]
Description=Nightly Clawee release retention pass
Documentation=https://github.com/clawee-git/release/blob/main/ops/README.md

[Timer]
OnCalendar=*-*-* {{.RetainAt}}:00
Persistent=true
RandomizedDelaySec=900
Unit={{.RetainUnit}}

[Install]
WantedBy=timers.target
`))

// vhostTmpl is the edge.
//
// The split it encodes is the feature: everything not matched by one of the
// four static locations is the service's, INCLUDING "/" and "/manage". The
// static half is the trust anchor — the bootstraps a `curl … | sh` fetches,
// the signing pubkey, the checksum and signature files, the badge JSONP — and
// those stay files ON PURPOSE: they embed no version, and a static file cannot
// be affected by whether the service is up.
var vhostTmpl = template.Must(template.New("vhost").Parse(
	`# {{.Host}} — public install channel (service + static)
#
# GENERATED by ` + "`{{.Tool}} ops render`" + `. Do not edit by hand — a
# suite test byte-diffs this file against the renderer, so an edit here fails
# the build. Change the template in cmd/{{.Tool}}/ops.go and regenerate.
#
# TWO surfaces on one vhost, and the split is the point:
#
#   PAGES    proxied to {{.Tool}}. /, /downloads, /verify, /platforms,
#            /docs and the operator's /manage + /api are rendered from the
#            catalog, so the version a visitor reads and the version an
#            installer resolves cannot disagree.
#   STATIC   files under the root below, put there by ` + "`{{.Tool}}" + `
#            publish-static` + "`" + ` when the KIT changes (not per cut):
#            clawee-release.pub, <comp>/install.sh, <comp>/upgrade.sh,
#            <comp>/beta.install.sh, <comp>/beta.upgrade.sh, <comp>/version.js
#            and <comp>/beta.version.js.
#
# The bootstraps stay static ON PURPOSE. They are the trust anchor delivered as
# ` + "`curl … | sh`" + `, they embed no version, and a static file cannot be
# affected by whether the service is up.
#
# Installing this file, issuing the cert and reloading nginx are OPERATOR
# steps; ops/README.md has the runbook, including the two host-specific traps
# (this origin's nginx.conf includes only sites-enabled/*, and its nginx build
# rejects TLSv1.3).

server {
    listen 80;
    listen [::]:80;
    server_name {{.Host}};

    # The edge handles the public redirect; this is a belt-and-suspenders
    # bounce for any direct-to-origin http hit.
    return 301 https://{{.Host}}$request_uri;
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name {{.Host}};

    # === TLS — OPERATOR PLACEHOLDERS ===========================================
    # Issue these and point them at the origin cert, then reload.
    ssl_certificate     {{.TLSCert}};  # OPERATOR: set real path
    ssl_certificate_key {{.TLSKey}};    # OPERATOR: set real path
    ssl_protocols       {{.TLSProtocols}};
    ssl_ciphers         HIGH:!aNULL:!MD5;
    ssl_prefer_server_ciphers on;
    # ===========================================================================

    # Static root — must match the --dest publish-static is run with.
    root  {{.StaticDir}};

    server_tokens off;
    charset utf-8;

    # The pages. Everything not matched by a static location below is the
    # service's, INCLUDING "/" and the operator's /manage — there is no
    # index.html any more, and a try_files fallback here would answer 404 for
    # the site's own front page.
    #
    # OPERATOR: --listen defaults to loopback; keep the two in step.
    location / {
        proxy_pass         http://{{.Listen}};
        proxy_http_version 1.1;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Forwarded-Proto $scheme;
        # Host and the scheme, and deliberately NOT X-Forwarded-For: the
        # service's login rate limit reads RemoteAddr only, so a forwarded
        # client address would be an unauthenticated header the limit could be
        # steered by. Believing a proxy is a decision taken explicitly, not a
        # header added here (ops/README.md, "A note on the trusted proxy").
        # Promote streams NDJSON progress for the whole publish — several
        # hundred megabytes of copying. Buffering it would hand the operator
        # the entire log at the end, which is exactly the "is it hung?"
        # ambiguity the stream exists to remove, and a read timeout would cut
        # a healthy promote off mid-copy.
        proxy_buffering    off;
        proxy_read_timeout 1h;
    }

    # Bootstrap installers: served as a shell script so ` + "`curl … | sh`" + ` is honest
    # about the content type, and never cached stale at the edge for long.
    location ~ \.sh$ {
        default_type  text/x-shellscript;
        add_header    Cache-Control "public, max-age=300";
        try_files     $uri =404;
    }

    # Version JSONP (<comp>/version.js) — consumed cross-origin via a plain
    # <script src> to render the live version badge. Short cache so the badge
    # tracks a fresh release; JSONP means no CORS headers are needed.
    location ~ \.js$ {
        default_type  application/javascript;
        add_header    Cache-Control "public, max-age=300";
        try_files     $uri =404;
    }

    # The signing public key — plain text, the trust anchor for manual verify.
    location = /clawee-release.pub {
        default_type  text/plain;
        add_header    Cache-Control "public, max-age=300";
    }

    # SHA256SUMS / signatures served from the static surface — plain text.
    location ~ \.(txt|minisig)$ {
        default_type  text/plain;
        try_files     $uri =404;
    }
}
`))
