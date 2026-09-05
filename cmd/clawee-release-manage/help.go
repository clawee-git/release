package main

// The command tree, the renderer, and nothing else.
//
// One declarative tree is the single source for what exists; one registrar per
// verb is the single source for its flags; one renderer produces every help
// page, every refusal's page, and the generated reference (cli-help.md §0). A
// page that is hand-written is a page that drifts from the parser the day
// after it is written, so nothing here is hand-written except summaries.

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// toolName is the command as the USER types it. Every page, every usage line
// and every error prefix uses this, never the binary's path on disk (§3).
const toolName = "clawee-release-manage"

const synopsis = "the publish-management service for the Clawee release channel"

// pageWidth is fixed, never probed from the terminal: the generated reference
// is byte-compared against a committed file, so a width that varied with the
// tty would make the build gate pass or fail depending on where it ran.
const pageWidth = 100

// gutter is the space between the derived columns.
const gutter = 2

// argForm is one positional shape a verb accepts, with its own summary.
type argForm struct {
	shape   string
	summary string
}

// node is one command. Shape carries POSITIONAL shapes only — never flags: a
// flag written here renders twice (once in the shapes column, once in the
// derived flag table) and drifts the moment the registrar changes.
type node struct {
	name     string
	summary  string
	shape    string
	args     []argForm
	children []*node
}

func (n *node) leaf() bool { return len(n.children) == 0 }

// root is the whole surface. Adding a verb is: a node here, an entry in
// dispatch, a registrar if it has flags, and a regenerated reference — and the
// lockstep tests plus the build gate fail until all four exist.
var root = &node{
	name:    toolName,
	summary: synopsis,
	children: []*node{
		{
			name:    "serve",
			summary: "run the manage service: the catalog, the register endpoints and /manage",
		},
		{
			name:    "admin",
			summary: "operator accounts for the manage surface",
			children: []*node{
				{
					name:    "add",
					summary: "provision an account; the second factor enrols at first login",
					shape:   "<name>",
					args:    []argForm{{"<name>", "the account name, as it is typed at the login page"}},
				},
				{
					name:    "list",
					summary: "print the accounts and whether each has enrolled a second factor",
				},
				{
					name:    "remove",
					summary: "delete an account and end its sessions",
					shape:   "<name>",
					args:    []argForm{{"<name>", "the account to delete"}},
				},
			},
		},
		{
			name:    "retain",
			summary: "run retention over every component and channel — the nightly net",
		},
		{
			name:    "publish-static",
			summary: "copy the kit's generated static files to the release host — run when the KIT changes, not per cut",
		},
		{
			name:    "doctor",
			summary: "check this deployment: the catalog, the roots, the signing key, the buckets and the token",
		},
		{
			name:    "ops",
			summary: "the deployment artefacts — rendered here, installed by the operator",
			children: []*node{
				{
					name:    "render",
					summary: "write the systemd units, the retention timer and the nginx vhost to a directory",
				},
			},
		},
		{
			name:    "version",
			summary: "print the build stamp, and the applied migrations when a data dir is named",
		},
		{
			name:    "docs",
			summary: "print the whole command surface as markdown",
		},
	},
}

// find resolves a command path ("admin add") to its node, and reports the
// deepest VALID node plus the token that failed — which is what a refusal
// needs in order to print the page for the level above the mistake (§2).
func find(path []string) (found *node, deepest *node, badToken string) {
	cur := root
	for i, tok := range path {
		var next *node
		for _, c := range cur.children {
			if c.name == tok {
				next = c
				break
			}
		}
		if next == nil {
			return nil, cur, path[i]
		}
		cur = next
	}
	return cur, cur, ""
}

// pathOf returns the typed path of n ("admin add"), empty for the root.
func pathOf(n *node) string {
	var walk func(cur *node, prefix []string) []string
	walk = func(cur *node, prefix []string) []string {
		if cur == n {
			return prefix
		}
		for _, c := range cur.children {
			if got := walk(c, append(append([]string{}, prefix...), c.name)); got != nil {
				return got
			}
		}
		return nil
	}
	return strings.Join(walk(root, nil), " ")
}

// row is one rendered line before alignment: a verb name at its indent, an
// optional args/flags token, and a summary.
type row struct {
	verb  string // already indented; empty on an args/flags row
	token string // arg shape or rendered flag; empty on a verb row
	desc  string
}

// page renders n's help page. For the root that is the whole tree; for a verb
// it is that verb and its subtree, and nothing of its siblings.
func page(n *node) string {
	var b strings.Builder
	if n == root {
		fmt.Fprintf(&b, "%s — %s\n\nUsage:\n  %s <verb> [flags]\n\nCommands:\n", toolName, synopsis, toolName)
	} else {
		path := pathOf(n)
		fmt.Fprintf(&b, "%s %s — %s\n\nUsage:\n  %s", toolName, path, n.summary, toolName+" "+path)
		if n.leaf() {
			if n.shape != "" {
				b.WriteString(" " + n.shape)
			}
			// [flags] is DERIVED from the registrar, never asserted: a verb
			// with no flags that claims them sends the reader looking for a
			// table that is not there.
			if len(flagRows(n)) > 0 {
				b.WriteString(" [flags]")
			}
		} else {
			b.WriteString(" <verb> [flags]")
		}
		b.WriteString("\n\nCommands:\n")
	}

	var rows []row
	if n == root {
		for _, c := range n.children {
			rows = append(rows, walkRows(c, 0)...)
		}
	} else {
		rows = walkRows(n, 0)
	}
	b.WriteString(align(rows))

	if !n.leaf() || n == root {
		prefix := toolName
		if n != root {
			prefix = toolName + " " + pathOf(n)
		}
		fmt.Fprintf(&b, "\nRun '%s <verb> --help' for that command's help.\n", prefix)
	}
	return b.String()
}

// walkRows renders n and its subtree: the verb on its own line at its nesting
// indent, then — for a leaf — its arg forms one per line and its flags one per
// line, args first.
func walkRows(n *node, depth int) []row {
	indent := strings.Repeat("  ", depth+1)
	rows := []row{{verb: indent + n.name, desc: n.summary}}
	if !n.leaf() {
		for _, c := range n.children {
			rows = append(rows, walkRows(c, depth+1)...)
		}
		return rows
	}
	if len(n.args) == 0 && n.shape != "" {
		rows = append(rows, row{token: n.shape})
	}
	for _, a := range n.args {
		rows = append(rows, row{token: a.shape, desc: a.summary})
	}
	rows = append(rows, flagRows(n)...)
	return rows
}

// align turns rows into text with two derived columns: the args/flags column
// one gutter right of the longest verb name on THIS page, and the summary
// column one gutter right of the longest resulting prefix. Widths are RUNE
// counts — a character that is three bytes and one column would otherwise
// mis-pad every other row on the page.
func align(rows []row) string {
	verbWidth := 0
	for _, r := range rows {
		if w := utf8.RuneCountInString(r.verb); w > verbWidth {
			verbWidth = w
		}
	}
	tokenCol := verbWidth + gutter
	descCol := tokenCol
	for _, r := range rows {
		if r.token == "" {
			continue
		}
		if w := tokenCol + utf8.RuneCountInString(r.token); w > descCol {
			descCol = w
		}
	}
	for _, r := range rows {
		if r.token != "" || r.desc == "" {
			continue
		}
		if w := utf8.RuneCountInString(r.verb); w > descCol-gutter {
			descCol = w + gutter
		}
	}
	descCol += gutter

	var b strings.Builder
	for _, r := range rows {
		var line strings.Builder
		if r.token == "" {
			line.WriteString(r.verb)
		} else {
			line.WriteString(pad("", tokenCol))
			line.WriteString(r.token)
		}
		if r.desc != "" {
			line.WriteString(pad("", descCol-utf8.RuneCountInString(line.String())))
			b.WriteString(wrap(line.String(), r.desc, descCol))
		} else {
			b.WriteString(strings.TrimRight(line.String(), " "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// wrap appends desc to prefix, breaking at pageWidth and indenting every
// continuation to the description column — a summary that overflows continues
// in ITS column, never at column 0.
func wrap(prefix, desc string, col int) string {
	var b strings.Builder
	b.WriteString(prefix)
	width := utf8.RuneCountInString(prefix)
	for i, word := range strings.Fields(desc) {
		wl := utf8.RuneCountInString(word)
		if i > 0 && width+1+wl > pageWidth {
			b.WriteString("\n" + strings.Repeat(" ", col))
			width = col
		} else if i > 0 {
			b.WriteString(" ")
			width++
		}
		b.WriteString(word)
		width += wl
	}
	return b.String()
}

func pad(s string, to int) string {
	n := to - utf8.RuneCountInString(s)
	if n < 0 {
		n = 0
	}
	return s + strings.Repeat(" ", n)
}
