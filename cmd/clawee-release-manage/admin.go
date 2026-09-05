package main

// The `admin` verbs. Accounts are provisioned HERE, on the host, and never
// through an HTTP route: this surface publishes software, so there is no
// signup to get wrong because there is no signup (release-management.md §6).

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/clawee-git/release/internal/manage/auth"
	"github.com/clawee-git/release/internal/manage/store"
	"golang.org/x/term"
)

func runAdminAdd(e *env, n *node, args []string) error {
	var o adminAddOpts
	fs := flag.NewFlagSet(toolName+" "+pathOf(n), flag.ContinueOnError)
	o.register(fs)
	handled, err := parseVerbFlags(e, n, fs, args)
	if handled || err != nil {
		return err
	}
	if err := requireArgs(n, fs, 1); err != nil {
		return err
	}
	if err := requireDataDir(n, o.dataDir); err != nil {
		return err
	}
	name := fs.Arg(0)
	if err := auth.ValidAdminName(name); err != nil {
		// A malformed name is an invocation-shape error, so it carries the
		// page and the usage status like every other refusal in this verb.
		return usagef(n, "%v", err)
	}

	password, err := readPassword(e, o.passwordStdin)
	if err != nil {
		return err
	}

	svc, closeFn, err := openAuth(o.dataDir)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := svc.AddAdmin(name, password); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return fmt.Errorf("admin %q already exists; remove it first if you mean to reset it", name)
		}
		return err
	}
	fmt.Fprintf(e.stdout, "✓ admin %q added\n", name)
	fmt.Fprintf(e.stdout, "  The second factor enrols at first login: the secret is shown once, on the\n")
	fmt.Fprintf(e.stdout, "  code page, and is never readable again.\n")
	return nil
}

func runAdminList(e *env, n *node, args []string) error {
	var o adminListOpts
	fs := flag.NewFlagSet(toolName+" "+pathOf(n), flag.ContinueOnError)
	o.register(fs)
	handled, err := parseVerbFlags(e, n, fs, args)
	if handled || err != nil {
		return err
	}
	// A verb that takes no arguments still REJECTS them: silently discarding
	// is worse than an error, and a stray positional is usually a mistyped flag.
	if err := rejectResiduals(n, fs); err != nil {
		return err
	}
	if err := requireDataDir(n, o.dataDir); err != nil {
		return err
	}
	st, err := store.Open(o.dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	admins, err := st.ListAdmins()
	if err != nil {
		return err
	}
	if len(admins) == 0 {
		fmt.Fprintf(e.stdout, "no admins; add one with: %s admin add <name> --data-dir %s\n", toolName, o.dataDir)
		return nil
	}
	fmt.Fprintf(e.stdout, "%-20s %-10s %s\n", "NAME", "2FA", "ADDED")
	for _, a := range admins {
		second := "pending"
		if a.Enrolled() {
			second = "enrolled"
		}
		fmt.Fprintf(e.stdout, "%-20s %-10s %s\n", a.Name, second, a.CreatedAt.Format("2006-01-02"))
	}
	return nil
}

func runAdminRemove(e *env, n *node, args []string) error {
	var o adminRemoveOpts
	fs := flag.NewFlagSet(toolName+" "+pathOf(n), flag.ContinueOnError)
	o.register(fs)
	handled, err := parseVerbFlags(e, n, fs, args)
	if handled || err != nil {
		return err
	}
	if err := requireArgs(n, fs, 1); err != nil {
		return err
	}
	if err := requireDataDir(n, o.dataDir); err != nil {
		return err
	}
	st, err := store.Open(o.dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	name := fs.Arg(0)
	if err := st.DeleteAdmin(name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no admin named %q; `%s admin list` shows the accounts that exist", name, toolName)
		}
		return err
	}
	fmt.Fprintf(e.stdout, "✓ admin %q removed; its sessions and CSRF tokens went with it\n", name)
	return nil
}

// readPassword prompts on a tty, or reads one line from stdin under
// --password-stdin.
//
// The prompt is only offered when stdin IS a terminal: a prompt written to a
// pipe hangs a provisioning script forever with no output, which is the
// non-interactive failure error-handling.md names — log, propagate, exit
// non-zero, never wait for a user who is not there.
func readPassword(e *env, fromStdin bool) (string, error) {
	if fromStdin {
		r := bufio.NewReader(e.stdin)
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("stdin is not a terminal; pass --password-stdin and pipe the password in")
	}
	fmt.Fprint(e.stdout, "password: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(e.stdout)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	fmt.Fprint(e.stdout, "repeat:   ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(e.stdout)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if string(first) != string(second) {
		return "", fmt.Errorf("the two passwords do not match")
	}
	return string(first), nil
}

// openAuth opens the catalog and the sealer for a CLI verb.
func openAuth(dataDir string) (*auth.Service, func(), error) {
	st, err := store.Open(dataDir)
	if err != nil {
		return nil, nil, err
	}
	abs, err := filepath.Abs(filepath.Join(dataDir, auth.SecretKeyFile))
	if err != nil {
		st.Close()
		return nil, nil, fmt.Errorf("resolve secret key path: %w", err)
	}
	sealer, err := auth.LoadSealer(abs)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	// secure=false: this path never sets a cookie. The service's own value is
	// derived from --base-url's scheme, where it matters.
	return auth.New(st, sealer, false, nil), func() { st.Close() }, nil
}
