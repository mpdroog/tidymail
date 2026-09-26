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
		if errors.Is(err, os.ErrNotExist) {
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

	syncer := imapsync.New(cfg.IMAP, st, logger)
	syncer.Verbose = verbose
	sender := smtpsend.New(cfg.SMTP)

	switch cmd {
	case "reindex":
		n, err := st.Reindex(ctx)
		if err != nil {
			return err
		}
		logger.Printf("reindexed %d message(s) into %s", n, filepath.Join(st.Dir(), "index.db"))
		return nil

	case "sync":
		res, err := syncer.Sync(ctx)
		if err != nil {
			return err
		}
		logger.Print("sync done: " + res.Summary())
		if cfg.Notify {
			notifyNewMail(st, res, logger)
		}
		return nil

	case "serve":
		return serve(ctx, cfg, st, syncer, sender, logger)

	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// serve runs the web server until the context is cancelled. It never fetches
// mail: that is 'tidymail sync', so only one process ever does. The syncer
// appends sent replies to the Sent folder and repeats box moves in IMAP folders.
func serve(ctx context.Context, cfg *config.Config, st *store.Store, syncer *imapsync.Syncer, sender *smtpsend.Sender, logger *log.Logger) error {
	srv, err := httpd.New(cfg, st, syncer, sender, logger)
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
func notifyNewMail(st *store.Store, res imapsync.Result, logger *log.Logger) {
	total := 0
	var parts []string
	for _, b := range st.Boxes() {
		n := res.NewByBox[b.Slug]
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
# Keep secrets out of this file if you sync mail.d with git: use
#   password = "env:TIDYMAIL_IMAP_PASSWORD"
# or point at a file that git ignores:
#   password = "file:secrets/imap.txt"
#
# 'tidymail serve' never fetches mail. Run 'tidymail sync' to fetch, by hand
# or on a timer (cron, systemd).

listen = "127.0.0.1:8080"
notify = true               # desktop notification when sync stores new mail

[imap]
host = "imap.example.com"
port = 993
tls = "implicit"            # implicit | starttls | none
username = "you@example.com"
password = "env:TIDYMAIL_IMAP_PASSWORD"
inbox = "INBOX"
sent = ""                   # empty: detect the \Sent folder
archive = ""                # empty: detect the \Archive folder
trash = ""                  # empty: detect the \Trash folder (Trash box)
junk = ""                   # empty: detect the \Junk folder (Spam box)
after_fetch = "archive"     # archive | delete | keep
scan_sent = true            # learn the whitelist from who you have written to
max_fetch = 200
connections = 3             # parallel downloads during sync (1-10)

[smtp]
host = "smtp.example.com"
port = 587
tls = "starttls"            # starttls | implicit | none
username = "you@example.com"
password = "env:TIDYMAIL_SMTP_PASSWORD"
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
