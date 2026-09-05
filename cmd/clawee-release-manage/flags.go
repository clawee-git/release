package main

// One registrar per verb: the single definition of that verb's flags, called
// by the verb's own parse AND by the renderer. A flag therefore exists in
// exactly one place, and no page can disagree with the parser (cli-help.md §7).

import (
	"flag"
	"fmt"
	"sort"
	"strings"
)

// dataDirUsage is shared verbatim by every verb that takes --data-dir, so the
// value hint and the wording cannot drift between pages. The backquoted word
// is what the renderer shows as the value shape; without it the page would
// fall back to the parser's TYPE name and document the same flag as
// `<string>` on one page and `<dir>` on another.
const dataDirUsage = "the `dir` holding the catalog and the service secret key (required)"

// commonOpts is the flag set every persistent verb shares.
type commonOpts struct {
	dataDir string
}

func (o *commonOpts) registerDataDir(fs *flag.FlagSet) {
	fs.StringVar(&o.dataDir, "data-dir", "", dataDirUsage)
}

type serveOpts struct {
	commonOpts
	storeOpts
	listen    string
	baseURL   string
	secretKey string
}

func (o *serveOpts) register(fs *flag.FlagSet) {
	o.registerDataDir(fs)
	o.storeOpts.register(fs)
	fs.StringVar(&o.listen, "listen", defaultListen,
		"`address` to bind; the default is loopback because this service sits behind the host's TLS proxy")
	fs.StringVar(&o.baseURL, "base-url", "",
		"the public `url` this service is reached at, e.g. https://release.example.org (required)")
	fs.StringVar(&o.secretKey, "secret-key", "",
		"`path` to the service secret key; defaults to secret.key inside --data-dir")
}

type adminAddOpts struct {
	commonOpts
	passwordStdin bool
}

func (o *adminAddOpts) register(fs *flag.FlagSet) {
	o.registerDataDir(fs)
	fs.BoolVar(&o.passwordStdin, "password-stdin", false,
		"read the password from standard input instead of prompting")
}

type adminListOpts struct{ commonOpts }

func (o *adminListOpts) register(fs *flag.FlagSet) { o.registerDataDir(fs) }

type adminRemoveOpts struct{ commonOpts }

func (o *adminRemoveOpts) register(fs *flag.FlagSet) { o.registerDataDir(fs) }

type versionOpts struct {
	dataDir string
}

func (o *versionOpts) register(fs *flag.FlagSet) {
	fs.StringVar(&o.dataDir, "data-dir", "", "the `dir` holding the catalog; when given, the applied migrations are printed too")
}

// registrars maps a command path to its flag definitions. The lockstep test
// fails on a key that names no tree node, so a renamed verb cannot leave an
// orphaned registrar behind.
var registrars = map[string]func(*flag.FlagSet){
	"serve":          func(fs *flag.FlagSet) { new(serveOpts).register(fs) },
	"retain":         func(fs *flag.FlagSet) { new(retainOpts).register(fs) },
	"publish-static": func(fs *flag.FlagSet) { new(publishStaticOpts).register(fs) },
	"admin add":      func(fs *flag.FlagSet) { new(adminAddOpts).register(fs) },
	"admin list":     func(fs *flag.FlagSet) { new(adminListOpts).register(fs) },
	"admin remove":   func(fs *flag.FlagSet) { new(adminRemoveOpts).register(fs) },
	"version":        func(fs *flag.FlagSet) { new(versionOpts).register(fs) },
}

// flagRows renders n's flags as page rows, in the same order on every page
// (sorted, which is what flag.VisitAll gives).
func flagRows(n *node) []row {
	reg, ok := registrars[pathOf(n)]
	if !ok {
		return nil
	}
	fs := flag.NewFlagSet(pathOf(n), flag.ContinueOnError)
	reg(fs)
	var rows []row
	fs.VisitAll(func(f *flag.Flag) {
		name, usage := flag.UnquoteUsage(f)
		token := "[--" + f.Name
		if name != "" {
			token += " <" + name + ">"
		}
		token += "]"
		rows = append(rows, row{token: token, desc: usage})
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].token < rows[j].token })
	return rows
}

// flagTable renders a verb's flags for a refusal message, so a bad flag is
// answered with the flags that exist rather than only with the one rejected.
func flagTable(n *node) string {
	rows := flagRows(n)
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nFlags:\n")
	b.WriteString(align(rows))
	return b.String()
}

// helpSpellings is the decided set, applied at every level (cli-help.md §4).
// There is deliberately no `help` verb: a bare parent already falls back to
// its own page, so a second grammar for the same question would only add
// paths, typos and forwarding to get wrong.
var helpSpellings = map[string]bool{"-h": true, "--h": true, "-help": true, "--help": true}

// wantsHelp reports whether args contain a help spelling anywhere before a
// "--" terminator.
//
// ANYWHERE, not just first: a parser that recognises help only in first
// position consumed `--data-dir /x --help` as a value, which in one real tool
// nearly enrolled the literal string "--help" as a public key.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if helpSpellings[a] {
			return true
		}
	}
	return false
}

var _ = fmt.Sprintf
