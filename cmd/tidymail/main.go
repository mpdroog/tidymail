// Command tidymail is a local IMAP mail client with a browser UI.
//
// It fetches mail into a mail.d directory of plain text files, files each
// message into a box, and serves a keyboard-driven UI on localhost. Box state
// lives on this machine and is repeated in IMAP folders; mail.d, not the IMAP
// account, is the system of record.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mpdroog/tidymail/internal/config"
	"github.com/mpdroog/tidymail/internal/httpd"
	"github.com/mpdroog/tidymail/internal/imapsync"
	"github.com/mpdroog/tidymail/internal/notify"
	"github.com/mpdroog/tidymail/internal/smtpsend"
	"github.com/mpdroog/tidymail/internal/store"
)

// releaseInterval is how often serve checks delayed mail for expiry.
const releaseInterval = time.Minute

func main() {
	if err := run(); err != nil {
		if _, werr := fmt.Fprintf(os.Stderr, "tidymail: %v\n", err); werr != nil {
			// Nothing useful is left to do if stderr itself fails.
			os.Exit(2)
		}
		os.Exit(1)
	}
}

const usage = `tidymail - a local IMAP client with a browser UI

usage: tidymail [-dir mail.d] <command>

commands:
  serve     serve the web UI (default); it never fetches mail
  sync      fetch new mail over IMAP and exit; schedule this
  reindex   rebuild the SQLite search index from the text files
  init      write a starter mail.d/config.toml
  version   print the version
`

func run() error {
	fs := flag.NewFlagSet("tidymail", flag.ContinueOnError)
	dir := fs.String("dir", "mail.d", "directory holding mail, state and config")
	verbose := fs.Bool("v", false, "sync: log every stored message, not only batch statistics")
	fs.Usage = func() {
		if _, err := fmt.Fprint(os.Stderr, usage); err != nil {
			return
		}
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	cmd := "serve"
	if fs.NArg() > 0 {
		cmd = fs.Arg(0)
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)

	switch cmd {
	case "init":
		return initConfig(*dir, logger)
	case "version":
		_, err := fmt.Println("tidymail " + version())
		return err
	case "serve", "sync", "reindex":
		return runWithStore(cmd, *dir, *verbose, logger)
	default:
		return fmt.Errorf("unknown command %q (try: serve, sync, reindex, init, version)", cmd)
	}
}

// runWithStore opens the config and store once for the commands that need them.
func runWithStore(cmd, dir string, verbose bool, logger *log.Logger) (err error) {
	cfg, err := config.Load(dir)
	if err != nil {
		// Only a missing config.toml calls for init; a missing secret file
		// is also "not exist" but needs the file, not a new config.
		if _, statErr := os.Stat(filepath.Join(dir, "config.toml")); errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("%w\nrun 'tidymail -dir %s init' to create one", err, dir)
		}
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first Ctrl+C restore default signal handling, so a second one
	// kills the process if a graceful shutdown hangs.
	// Deferred after stop, so it is unregistered first and a normal exit
	// stays quiet.
	defer context.AfterFunc(ctx, func() {
		stop()
		logger.Print("shutting down (press Ctrl+C again to force quit)")
	})()

	st, err := store.Open(ctx, dir, logger)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	accounts := make([]httpd.Account, 0, len(cfg.Accounts))
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		syncer := imapsync.New(a.Name, a.IMAP, st, accountLogger(logger, a.Name))
		syncer.Verbose = verbose
		accounts = append(accounts, httpd.Account{Name: a.Name, Syncer: syncer, Sender: smtpsend.New(a.SMTP), Mirror: a.IMAP.Mirror})
	}

	switch cmd {
	case "reindex":
		n, err := st.Reindex(ctx)
		if err != nil {
			return err
		}
		logger.Printf("reindexed %d message(s) into %s", n, filepath.Join(st.Dir(), "index.db"))
		return nil

	case "sync":
		return syncAll(ctx, cfg, st, accounts, logger)

	case "serve":
		return serve(ctx, cfg, st, accounts, logger)

	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// accountLogger prefixes an account's log lines with its name, so interleaved
// output from several accounts stays readable.
func accountLogger(logger *log.Logger, name string) *log.Logger {
	if name == "" {
		return logger
	}
	return log.New(logger.Writer(), "["+name+"] ", logger.Flags()|log.Lmsgprefix)
}

// syncAll fetches every account in turn. A failing account is reported and
// the others still sync; the error of every failed account is returned.
func syncAll(ctx context.Context, cfg *config.Config, st *store.Store, accounts []httpd.Account, logger *log.Logger) error {
	var (
		errs     []error
		newByBox = make(map[string]int)
	)
	for _, a := range accounts {
		if ctx.Err() != nil {
			errs = append(errs, fmt.Errorf("sync: %w", ctx.Err()))
			break
		}
		res, err := a.Syncer.Sync(ctx)
		for box, n := range res.NewByBox {
			newByBox[box] += n
		}
		if err != nil {
			if a.Name != "" {
				err = fmt.Errorf("account %s: %w", a.Name, err)
			}
			logger.Printf("sync failed: %v", err)
			errs = append(errs, err)
			continue
		}
		accountLogger(logger, a.Name).Print("sync done: " + res.Summary())
	}
	if cfg.Notify {
		notifyNewMail(st, newByBox, logger)
	}
	return errors.Join(errs...)
}

// serve runs the web server until the context is cancelled. It never fetches
// mail: that is 'tidymail sync', so only one process ever does. Each account's
// syncer appends sent replies to its Sent folder and repeats box moves in its
// IMAP folders.
func serve(ctx context.Context, cfg *config.Config, st *store.Store, accounts []httpd.Account, logger *log.Logger) error {
	srv, err := httpd.New(cfg, st, accounts, logger)
	if err != nil {
		return err
	}

	// Cancelled when the server stops for any reason, not only on a signal,
	// so a failed listen does not leave us waiting on the background loops.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		releaseDelayed(ctx, st, logger)
	}()
	go func() {
		defer wg.Done()
		srv.RunMirror(ctx)
	}()

	err = srv.ListenAndServe(ctx)
	cancel()
	wg.Wait()
	return err
}

// notifyNewMail raises one desktop notification for new mail outside Spam.
func notifyNewMail(st *store.Store, newByBox map[string]int, logger *log.Logger) {
	total := 0
	var parts []string
	for _, b := range st.Boxes() {
		n := newByBox[b.Slug]
		if n == 0 || b.Slug == store.BoxSpam {
			continue
		}
		total += n
		parts = append(parts, fmt.Sprintf("%d in %s", n, b.Name))
	}
	if total == 0 {
		return
	}
	summary := fmt.Sprintf("%d new messages", total)
	if total == 1 {
		summary = "1 new message"
	}
	if err := notify.Send(summary, strings.Join(parts, ", ")); err != nil {
		logger.Printf("sync: desktop notification: %v", err)
	}
}

// releaseDelayed returns delayed mail to its box once its time is up, checking
// at startup and then every minute.
func releaseDelayed(ctx context.Context, st *store.Store, logger *log.Logger) {
	release := func() {
		n, err := st.ReleaseDue(ctx, time.Now())
		if err != nil {
			if ctx.Err() == nil {
				logger.Printf("release delayed mail: %v", err)
			}
			return
		}
		if n > 0 {
			logger.Printf("%d delayed message(s) came back", n)
		}
	}
	release()

	tick := time.NewTicker(releaseInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			release()
		}
	}
}

// configTemplate is written by the init command.
const configTemplate = `# tidymail configuration.
#
# Keep secrets out of this file if you sync mail.d with git: point at a file
# under secrets/, which mail.d/.gitignore excludes:
#   password = "file:secrets/personal-imap.txt"
# or read it from the environment:
#   password = "env:TIDYMAIL_IMAP_PASSWORD"
#
# 'tidymail serve' never fetches mail. Run 'tidymail sync' to fetch, by hand
# or on a timer (cron, systemd).

listen = "127.0.0.1:8080"
notify = true               # desktop notification when sync stores new mail

# One [[account]] per mailbox; all of them share one set of boxes and one UI.
# name tags every message fetched through the account: 1-32 of a-z, 0-9, - and
# _, and do not rename it once mail has been synced. Copy the whole block for
# every further account.

[[account]]
name = "personal"

[account.imap]
host = "imap.example.com"
port = 993
tls = "implicit"            # implicit | starttls | none
username = "you@example.com"
password = "file:secrets/personal-imap.txt"
inbox = "INBOX"
sent = ""                   # empty: detect the \Sent folder
archive = ""                # empty: detect the \Archive folder
trash = ""                  # empty: detect the \Trash folder (Trash box)
junk = ""                   # empty: detect the \Junk folder (Spam box)
after_fetch = "archive"     # archive | delete | keep
scan_sent = true            # learn the whitelist from who you have written to
max_fetch = 200
connections = 3             # parallel downloads during sync (1-10)
mirror = true               # repeat box moves in IMAP folders; false: never
                            # move mail on the server, boxes stay local

[account.smtp]
host = "smtp.example.com"
port = 587
tls = "starttls"            # starttls | implicit | none
username = "you@example.com"
password = "file:secrets/personal-smtp.txt"
from_name = "Your Name"
from_addr = "you@example.com"
`

// initConfig writes a starter config, refusing to clobber an existing one.
func initConfig(dir string, logger *log.Logger) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; edit it instead", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(configTemplate), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	logger.Printf("wrote %s - fill in your account, then run: tidymail -dir %s sync && tidymail -dir %s serve", path, dir, dir)
	return nil
}

// version reports the build version recorded by the Go toolchain.
func version() string {
	if v := buildVersion(); v != "" {
		return v
	}
	return "(devel)"
}
