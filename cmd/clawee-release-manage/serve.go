package main

// The `serve` verb: the HTTP service itself.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/clawee-git/release/internal/manage/auth"
	"github.com/clawee-git/release/internal/manage/intake"
	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/manage/web"
)

// defaultListen binds LOOPBACK, and documented as such: the service is
// designed to sit behind the nginx in ops/ on the same host, which terminates
// TLS. Binding 0.0.0.0 by default would put the login form and the register
// endpoint on the open internet in plaintext the first time anyone ran it.
const defaultListen = "127.0.0.1:8787"

// purgeInterval is how often expired sessions, CSRF tokens and unused nonces
// are swept. It is a background NET, not the enforcement: every expiry is
// enforced at read time by the store, so a missed sweep costs disk, not
// safety.
const purgeInterval = time.Hour

func runServe(e *env, n *node, args []string) error {
	var o serveOpts
	fs := flag.NewFlagSet(toolName+" "+pathOf(n), flag.ContinueOnError)
	o.register(fs)
	handled, err := parseVerbFlags(e, n, fs, args)
	if handled || err != nil {
		return err
	}
	if err := rejectResiduals(n, fs); err != nil {
		return err
	}
	if err := requireDataDir(n, o.dataDir); err != nil {
		return err
	}
	if o.baseURL == "" {
		return usagef(n, "--base-url is required: it is what the register response hands the cut as the row's URL, and there is no way to guess it from a listen address behind a proxy")
	}
	u, err := url.Parse(o.baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return usagef(n, "--base-url %q is not an absolute URL", o.baseURL)
	}

	log := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	st, err := store.Open(o.dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	keyPath := o.secretKey
	if keyPath == "" {
		keyPath = filepath.Join(o.dataDir, auth.SecretKeyFile)
	}
	keyPath, err = filepath.Abs(keyPath)
	if err != nil {
		return fmt.Errorf("resolve secret key path: %w", err)
	}
	sealer, err := auth.LoadSealer(keyPath)
	if err != nil {
		return err
	}

	// Cookies are Secure iff the service is actually reached over https. It is
	// DERIVED from --base-url rather than hard-coded because a Secure cookie is
	// never sent over the http loopback a first local run uses, which would
	// make the login form silently impossible exactly where it is first tried.
	secure := u.Scheme == "https"
	if !secure {
		log.Warn("base URL is not https; session cookies will not be marked Secure",
			"base_url", o.baseURL)
	}

	authSvc := auth.New(st, sealer, secure, nil, log)
	in, err := intake.New(st, o.baseURL, nil, log)
	if err != nil {
		return err
	}
	backends, err := o.backends(n)
	if err != nil {
		return err
	}
	srv, err := web.New(st, authSvc, in, backends, log, nil)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", o.listen, err)
	}
	httpSrv := &http.Server{
		Handler: srv.Handler(),
		// ReadHeaderTimeout and IdleTimeout bound a client that connects and
		// then says nothing — the cheap defence against a stuck or hostile
		// one, and neither has anything to do with how long a handler runs.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// WriteTimeout is 0 ON PURPOSE, and it is the same lesson as the one
		// in internal/r2/r2.go and internal/manage/backend/guard.go, from the
		// server side. It bounds the WHOLE response, and promote's response is
		// a progress stream that stays open for the entire publish: verifying
		// four ~11 MB zips, six copies into the public bucket, six GitHub
		// asset uploads. At 60 seconds it cut the operator's stream off
		// mid-publish — leaving them staring at a truncated log while the
		// promote carried on invisibly, which is exactly the "is it hung?"
		// ambiguity the stream exists to remove.
		//
		// The outbound clients this handler drives deliberately carry no
		// blanket timeout for the same reason; bounding the inbound response
		// while leaving the work unbounded would have been the worst pairing
		// of the two. What can actually wedge is bounded at the phases above
		// and inside those clients.
		WriteTimeout: 0,
		ErrorLog:     slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	if err := checkServerTimeouts(httpSrv); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go purgeLoop(ctx, st, log)

	// The seam summary is logged at startup because "promote answers 503" is
	// otherwise discovered by an operator at the moment they wanted to
	// promote. Never the credentials, never the token.
	log.Info("serving", "listen", ln.Addr().String(), "base_url", o.baseURL,
		"data_dir", o.dataDir, "release_key", in.KeyID, "seams", o.describe(backends))
	fmt.Fprintf(e.stdout, "%s listening on %s (base URL %s)\n", toolName, ln.Addr(), o.baseURL)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		// A bounded drain: an operator's promote in flight finishes, a stuck
		// connection does not hold the restart open.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// purgeLoop sweeps expired rows. It runs once at start too, so a service
// restarted after a long outage does not carry a month of dead sessions until
// the first tick.
func purgeLoop(ctx context.Context, st *store.Store, log *slog.Logger) {
	purge := func() {
		if err := st.PurgeExpired(time.Now()); err != nil {
			log.Warn("purge expired", "err", err)
		}
	}
	purge()
	t := time.NewTicker(purgeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			purge()
		}
	}
}

// checkServerTimeouts is an assertion, not a configuration step: it states the
// invariant the comment above argues for, at the one place a future edit would
// break it. A WriteTimeout reintroduced here would truncate every promote
// stream longer than it, and the symptom — a log that stops mid-publish — reads
// as a crashed promote rather than as a server setting.
func checkServerTimeouts(s *http.Server) error {
	if s.WriteTimeout != 0 {
		return fmt.Errorf("internal: http.Server.WriteTimeout is %s; it must be 0, "+
			"because it bounds the whole promote progress stream", s.WriteTimeout)
	}
	if s.ReadHeaderTimeout <= 0 || s.IdleTimeout <= 0 {
		return fmt.Errorf("internal: a client that connects and says nothing is unbounded")
	}
	return nil
}
