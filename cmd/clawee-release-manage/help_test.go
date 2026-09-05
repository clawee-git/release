package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
	"unicode/utf8"
)

// exec runs the tool through its REAL entry point and returns stdout, stderr
// and the status. Every help test goes through here: a test that calls a verb
// function directly certifies a path the user cannot reach.
func exec(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(&env{stdout: &out, stderr: &errb, stdin: strings.NewReader("")}, args)
	return out.String(), errb.String(), code
}

// walkNodes visits every node in the tree, root excluded.
func walkNodes(fn func(n *node)) {
	var walk func(n *node)
	walk = func(n *node) {
		for _, c := range n.children {
			fn(c)
			walk(c)
		}
	}
	walk(root)
}

func TestLockstepTreeDispatchAndRegistrars(t *testing.T) {
	paths := map[string]*node{}
	walkNodes(func(n *node) {
		p := pathOf(n)
		paths[p] = n
		if n.leaf() {
			if _, ok := dispatch[p]; !ok {
				t.Errorf("tree node %q has no dispatch handler", p)
			}
		} else if _, ok := dispatch[p]; ok {
			t.Errorf("parent %q has a handler; parents fall back to their page", p)
		}
	})
	for p := range dispatch {
		if n, ok := paths[p]; !ok {
			t.Errorf("dispatch handler %q names no tree node", p)
		} else if !n.leaf() {
			t.Errorf("dispatch handler %q names a parent", p)
		}
	}
	for p := range registrars {
		if _, ok := paths[p]; !ok {
			t.Errorf("registrar %q names no tree node", p)
		}
	}
}

// A child that repeats its parent's summary says nothing twice on a tree page,
// where the two render as adjacent lines.
func TestNoChildRepeatsItsParentsSummary(t *testing.T) {
	var walk func(n *node)
	walk = func(n *node) {
		for _, c := range n.children {
			if c.summary == n.summary {
				t.Errorf("%q repeats its parent's summary", pathOf(c))
			}
			if c.summary == "" {
				t.Errorf("%q has no summary", pathOf(c))
			}
			walk(c)
		}
	}
	walk(root)
}

func TestBareToolAndBareParentPrintTheirOwnPageOnStdout(t *testing.T) {
	out, errb, code := exec(t)
	if code != 0 || errb != "" {
		t.Fatalf("bare tool: code %d, stderr %q", code, errb)
	}
	if !strings.Contains(out, "Usage:\n  "+toolName+" <verb> [flags]") {
		t.Fatalf("bare tool page:\n%s", out)
	}

	out, errb, code = exec(t, "admin")
	if code != 0 || errb != "" {
		t.Fatalf("bare parent: code %d, stderr %q", code, errb)
	}
	if !strings.Contains(out, toolName+" admin <verb> [flags]") {
		t.Fatalf("bare parent page:\n%s", out)
	}
}

// A level's page carries that level's entries and NONE of its siblings'.
func TestPerLevelPagesAreScopedToTheirLevel(t *testing.T) {
	out, _, code := exec(t, "admin", "--help")
	if code != 0 {
		t.Fatalf("admin --help exited %d", code)
	}
	for _, want := range []string{"add", "list", "remove"} {
		if !strings.Contains(out, want) {
			t.Errorf("admin page is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "print the whole command surface") {
		t.Errorf("admin page carries the sibling `docs` verb:\n%s", out)
	}

	out, _, code = exec(t, "admin", "add", "--help")
	if code != 0 {
		t.Fatalf("admin add --help exited %d", code)
	}
	// A leaf's page: its own real flag table, and the derived [flags] marker.
	if !strings.Contains(out, "[--password-stdin]") || !strings.Contains(out, "[--data-dir <dir>]") {
		t.Errorf("leaf page is missing its flag table:\n%s", out)
	}
	if !strings.Contains(out, toolName+" admin add <name> [flags]") {
		t.Errorf("leaf usage line:\n%s", out)
	}
	if strings.Contains(out, "delete an account") {
		t.Errorf("leaf page carries a sibling verb:\n%s", out)
	}
}

// [flags] is DERIVED: a verb with no flags must not claim them.
func TestFlagsMarkerIsDerived(t *testing.T) {
	out, _, _ := exec(t, "docs", "--help")
	if strings.Contains(out, "docs [flags]") {
		t.Fatalf("docs has no flags but its usage line claims them:\n%s", out)
	}
	out, _, _ = exec(t, "version", "--help")
	if !strings.Contains(out, "version [flags]") {
		t.Fatalf("version has flags but its usage line omits them:\n%s", out)
	}
}

func TestEveryHelpSpellingIsAHelpRequestAndTheWordHelpIsNot(t *testing.T) {
	for spelling := range helpSpellings {
		out, errb, code := exec(t, "admin", "add", spelling)
		if code != 0 || errb != "" || !strings.Contains(out, "Usage:") {
			t.Errorf("%q: code %d, stderr %q", spelling, code, errb)
		}
	}
	// Help is reachable AFTER other flags: a parser that only looks at the
	// first argument consumes --help as a value.
	out, _, code := exec(t, "admin", "add", "--data-dir", "/tmp/x", "--help")
	if code != 0 || !strings.Contains(out, "Usage:") {
		t.Fatalf("trailing --help: code %d, out %q", code, out)
	}
	// `help` is a word, not a spelling: there is no help verb.
	_, errb, code := exec(t, "help")
	if code != exitUsage || !strings.Contains(errb, "unknown command") {
		t.Fatalf("`help` as a verb: code %d, stderr %q", code, errb)
	}
}

func TestRefusalsCarryTheLevelAbovePageOnStderr(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantIn    string
		wantPage  string
		wantNotIn string
	}{
		{
			name: "unknown verb → the top page",
			args: []string{"promote"}, wantIn: `unknown command "promote"`,
			wantPage: toolName + " <verb> [flags]",
		},
		{
			name: "unknown subcommand → THAT verb's page, not the tool's",
			args: []string{"admin", "nuke"}, wantIn: `unknown subcommand "nuke"`,
			wantPage: toolName + " admin <verb> [flags]", wantNotIn: "print the whole command surface",
		},
		{
			name:     "stray positional → the leaf's page",
			args:     []string{"admin", "list", "--data-dir", "/tmp/x", "extra"},
			wantIn:   `unexpected argument "extra"`,
			wantPage: toolName + " admin list",
		},
		{
			name: "bad flag → the leaf's page and its flag table",
			args: []string{"admin", "list", "--nope"}, wantIn: "flag provided but not defined",
			wantPage: toolName + " admin list",
		},
		{
			name: "arity → the leaf's page",
			args: []string{"admin", "add", "--data-dir", "/tmp/x"}, wantIn: "takes <name>, got 0",
			wantPage: toolName + " admin add",
		},
		{
			name: "a missing required flag is an invocation-shape error too",
			args: []string{"admin", "add", "ada"}, wantIn: "--data-dir is required",
			wantPage: toolName + " admin add",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errb, code := exec(t, c.args...)
			if code != exitUsage {
				t.Fatalf("code = %d, want %d", code, exitUsage)
			}
			if out != "" {
				t.Fatalf("a refusal wrote to stdout: %q", out)
			}
			// Asserted on CONTENT, not only the status: a status-only
			// assertion passes for an empty message.
			if !strings.Contains(errb, c.wantIn) {
				t.Errorf("stderr missing %q:\n%s", c.wantIn, errb)
			}
			if !strings.Contains(errb, c.wantPage) {
				t.Errorf("stderr missing the level's page %q:\n%s", c.wantPage, errb)
			}
			if c.wantNotIn != "" && strings.Contains(errb, c.wantNotIn) {
				t.Errorf("stderr carries a level it should not (%q):\n%s", c.wantNotIn, errb)
			}
		})
	}
}

// Alignment is recomputed from the rendered page, in runes. A test that
// hard-codes the column is the same hand-picked number wearing a test's name.
// treeDepth is the deepest nesting level under root (1 for a flat tree).
func treeDepth(n *node) int {
	deepest := 0
	for _, c := range n.children {
		if d := treeDepth(c); d > deepest {
			deepest = d
		}
	}
	return deepest + 1
}

func TestAlignmentIsDerivedInRunes(t *testing.T) {
	out, _, _ := exec(t)
	all := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// Only the Commands block is aligned; the Usage line above it is indented
	// prose and would otherwise be read as an very wide verb name.
	var lines []string
	inBlock := false
	for _, l := range all {
		if strings.HasPrefix(l, "Commands:") {
			inBlock = true
			continue
		}
		if inBlock && strings.HasPrefix(l, "Run '") {
			break
		}
		if inBlock {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		t.Fatal("no Commands block on the top page")
	}

	// Verb rows sit at an indent of two spaces per nesting level; the deepest
	// possible one is derived from the tree, not asserted, so the filter below
	// cannot mistake a wrapped summary continuation (indented to the far
	// description column) for a verb name.
	maxVerbIndent := 2 * treeDepth(root)
	verbWidth := 0
	for _, l := range lines {
		trimmed := strings.TrimLeft(l, " ")
		indent := utf8.RuneCountInString(l) - utf8.RuneCountInString(trimmed)
		if trimmed == "" || indent == 0 || indent > maxVerbIndent {
			continue
		}
		if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "<") {
			continue
		}
		name := strings.Fields(l)[0]
		if w := indent + utf8.RuneCountInString(name); w > verbWidth {
			verbWidth = w
		}
	}
	wantTokenCol := verbWidth + gutter

	found := 0
	for _, l := range lines {
		trimmed := strings.TrimLeft(l, " ")
		if !strings.HasPrefix(trimmed, "[--") && !strings.HasPrefix(trimmed, "<") {
			continue
		}
		found++
		if col := utf8.RuneCountInString(l) - utf8.RuneCountInString(trimmed); col != wantTokenCol {
			t.Errorf("token row starts at column %d, recomputed column is %d: %q", col, wantTokenCol, l)
		}
	}
	if found == 0 {
		t.Fatal("no args/flags rows on the top page — the test would pass against an empty page")
	}

	for _, l := range all {
		if utf8.RuneCountInString(l) > pageWidth {
			t.Errorf("line exceeds the fixed page width of %d: %q", pageWidth, l)
		}
	}
}

// A flag whose hint falls back to the parser's TYPE name is how one flag ends
// up documented as two different value shapes across a page set.
func TestNoFlagRendersATypeNameHint(t *testing.T) {
	typeNames := map[string]bool{"string": true, "int": true, "int64": true,
		"uint": true, "float64": true, "duration": true, "value": true}
	for path, reg := range registrars {
		fs := flag.NewFlagSet(path, flag.ContinueOnError)
		reg(fs)
		fs.VisitAll(func(f *flag.Flag) {
			name, _ := flag.UnquoteUsage(f)
			if typeNames[name] {
				t.Errorf("%s --%s renders the type-name hint <%s>; backquote a value word in its usage string", path, f.Name, name)
			}
		})
	}
}

// Help must work before anything that can fail — on a host with no data dir,
// no catalog and no secret key, which is exactly the host whose operator needs
// the help.
func TestHelpIsReachableBeforeAnyResolution(t *testing.T) {
	for _, args := range [][]string{
		{"admin", "add", "--help"},
		{"admin", "list", "--help"},
		{"admin", "remove", "--help"},
		{"version", "--help"},
		{"docs", "--help"},
	} {
		out, errb, code := exec(t, args...)
		if code != 0 || errb != "" || !strings.Contains(out, "Usage:") {
			t.Errorf("%v: code %d, stderr %q", args, code, errb)
		}
	}
}

// Flags are accepted before OR after a verb's positionals, and in the
// --flag=value spelling. Go's flag stops at the first non-flag argument, so
// without the shared helper's permutation `admin add ada --data-dir /x` parses
// nothing and reports two stray positionals — a message that denies the flag
// was given at all.
func TestFlagsAreAcceptedAroundPositionalsAndInEqualsForm(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"admin", "add", "ada", "--data-dir", dir, "--password-stdin"},
		{"admin", "add", "--data-dir", dir, "--password-stdin", "ada"},
		{"admin", "add", "--data-dir=" + dir, "ada", "--password-stdin"},
	} {
		_, errb, code := execStdin(t, "correct-horse-battery\n", args...)
		// Either it worked, or the account already exists from the previous
		// spelling — both prove the flags were parsed. A usage error would not.
		if code == exitUsage {
			t.Errorf("%v refused as a usage error:\n%s", args, errb)
		}
	}
}

// Everything after "--" is positional, including something that looks like a
// help spelling: the terminator means what it says.
func TestDoubleDashEndsFlagParsing(t *testing.T) {
	_, errb, code := exec(t, "admin", "list", "--data-dir", t.TempDir(), "--", "--help")
	if code != exitUsage || !strings.Contains(errb, `unexpected argument "--help"`) {
		t.Fatalf("after --: code %d, stderr %q", code, errb)
	}
}
