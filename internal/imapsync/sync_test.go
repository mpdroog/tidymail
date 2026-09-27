package imapsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/mpdroog/tidymail/internal/config"
	"github.com/mpdroog/tidymail/internal/store"
)

// testServer starts an in-memory IMAP server whose INBOX holds n messages and
// returns its port.
func testServer(t *testing.T, n int) int {
	t.Helper()
	port, _ := startTestServer(t, n)
	return port
}

// startTestServer is testServer that also returns the account, to inspect
// folders afterwards. Message i has Message-ID <i@test>.
func startTestServer(t *testing.T, n int) (int, *imapmemserver.User) {
	t.Helper()
	return startTestServerWith(t, n, nil)
}

// startTestServerWith is startTestServer with wrap, when set, applied to every
// new session, to make the server misbehave.
func startTestServerWith(t *testing.T, n int, wrap func(*imapserver.Conn, imapserver.Session) imapserver.Session) (int, *imapmemserver.User) {
	t.Helper()
	user := imapmemserver.NewUser("me@example.com", "secret")
	for _, name := range []string{"INBOX", "Sent", "Trash", "Archive"} {
		if err := user.Create(name, nil); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	for i := 1; i <= n; i++ {
		raw := fmt.Sprintf("From: ann@example.com\r\nTo: me@example.com\r\nSubject: mail %d\r\n"+
			"Date: Mon, 31 Aug 2026 10:00:00 +0000\r\nMessage-ID: <%d@test>\r\n\r\nbody %d\r\n", i, i, i)
		if _, err := user.Append("INBOX", bytes.NewReader([]byte(raw)), &imap.AppendOptions{}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	mem := imapmemserver.New()
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			if wrap != nil {
				return wrap(conn, mem.NewSession()), nil, nil
			}
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
		InsecureAuth: true,
	})
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !strings.Contains(serveErr.Error(), "closed") {
			t.Errorf("serve: %v", serveErr)
		}
	}()
	t.Cleanup(func() {
		if closeErr := srv.Close(); closeErr != nil {
			t.Errorf("close server: %v", closeErr)
		}
	})
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return port, user
}

// testLog sends log output to the test log.
type testLog struct{ t *testing.T }

func (w testLog) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// cancelOnStore cancels a sync as soon as the first message has been stored.
type cancelOnStore struct{ cancel context.CancelFunc }

func (w cancelOnStore) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("imap: stored")) {
		w.cancel()
	}
	return len(p), nil
}

func TestParallelDownloadsStoreEverything(t *testing.T) {
	cfg := config.IMAP{
		Host: "127.0.0.1", Port: testServer(t, 7), TLS: config.TLSNone,
		Username: "me@example.com", Password: "secret",
		Inbox: "INBOX", Sent: "Sent", AfterFetch: config.AfterFetchKeep,
		MaxFetch: 2, Connections: 3,
	}
	st, err := store.Open(t.Context(), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := st.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})

	res, err := New(cfg, st, log.New(testLog{t}, "", 0)).Sync(t.Context())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Fetched != 7 || res.New != 7 {
		t.Errorf("fetched %d (%d new), want all 7", res.Fetched, res.New)
	}
	cursor, err := st.LoadCursor()
	if err != nil {
		t.Fatalf("load cursor: %v", err)
	}
	if cursor.InboxUID != 7 {
		t.Errorf("cursor = UID %d, want 7", cursor.InboxUID)
	}
}

func TestInterruptedSyncResumesFromLastBatch(t *testing.T) {
	cfg := config.IMAP{
		Host: "127.0.0.1", Port: testServer(t, 3), TLS: config.TLSNone,
		Username: "me@example.com", Password: "secret",
		Inbox: "INBOX", Sent: "Sent", AfterFetch: config.AfterFetchKeep,
		MaxFetch: 1, Connections: 3,
	}
	st, err := store.Open(t.Context(), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := st.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})

	// First run: Ctrl+C right after the first message is stored.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := New(cfg, st, log.New(cancelOnStore{cancel}, "", 0))
	first.Verbose = true // cancelOnStore watches for the per-message line
	if _, syncErr := first.Sync(ctx); syncErr == nil {
		t.Fatal("interrupted sync should report an error")
	}
	cursor, err := st.LoadCursor()
	if err != nil {
		t.Fatalf("load cursor: %v", err)
	}
	if cursor.InboxUID != 1 {
		t.Errorf("cursor after interrupt = UID %d, want 1 (the batch that was stored)", cursor.InboxUID)
	}

	// Second run picks up after that batch instead of starting over. Its log
	// (batch statistics) is visible with go test -v.
	res, err := New(cfg, st, log.New(testLog{t}, "", 0)).Sync(t.Context())
	if err != nil {
		t.Fatalf("resumed sync: %v", err)
	}
	if res.Fetched != 2 {
		t.Errorf("resumed sync fetched %d, want the 2 messages not yet stored", res.Fetched)
	}
}

// testStore opens a store in a temporary directory.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := st.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})
	return st
}

// folderCount returns how many messages a folder on the test server holds.
func folderCount(t *testing.T, user *imapmemserver.User, mailbox string) uint32 {
	t.Helper()
	st, err := user.Status(mailbox, &imap.StatusOptions{NumMessages: true})
	if err != nil {
		t.Fatalf("status %s: %v", mailbox, err)
	}
	return *st.NumMessages
}

func TestArchiveClearsStoredMailWithoutDownloading(t *testing.T) {
	port, user := startTestServer(t, 5)
	cfg := config.IMAP{
		Host: "127.0.0.1", Port: port, TLS: config.TLSNone,
		Username: "me@example.com", Password: "secret",
		Inbox: "INBOX", Sent: "Sent", Archive: "Archive", AfterFetch: config.AfterFetchKeep,
		MaxFetch: 2, Connections: 2,
	}
	st := testStore(t)
	if _, err := New(cfg, st, log.New(testLog{t}, "", 0)).Sync(t.Context()); err != nil {
		t.Fatalf("keep sync: %v", err)
	}

	// Switching keep -> archive must not download the same mail again.
	cfg.AfterFetch = config.AfterFetchArchive
	res, err := New(cfg, st, log.New(testLog{t}, "", 0)).Sync(t.Context())
	if err != nil {
		t.Fatalf("archive sync: %v", err)
	}
	if res.Fetched != 0 || res.Bytes != 0 {
		t.Errorf("fetched %d message(s), %d bytes; want nothing downloaded", res.Fetched, res.Bytes)
	}
	if res.Archived != 5 || res.Remaining != 0 {
		t.Errorf("archived %d, %d left; want 5 archived, 0 left", res.Archived, res.Remaining)
	}
	if in, arch := folderCount(t, user, "INBOX"), folderCount(t, user, "Archive"); in != 0 || arch != 5 {
		t.Errorf("INBOX %d, Archive %d; want 0 and 5", in, arch)
	}
}

func TestArchiveRefetchesMailMissingFromStore(t *testing.T) {
	port, user := startTestServer(t, 3)
	cfg := config.IMAP{
		Host: "127.0.0.1", Port: port, TLS: config.TLSNone,
		Username: "me@example.com", Password: "secret",
		Inbox: "INBOX", Sent: "Sent", Archive: "Archive", AfterFetch: config.AfterFetchArchive,
		MaxFetch: 2, Connections: 1,
	}
	st := testStore(t)
	// The cursor claims all three were seen, but none is stored: a message
	// that failed to store must be fetched again, never archived unread.
	if err := st.SaveCursor(store.Cursor{InboxUID: 3}); err != nil {
		t.Fatalf("save cursor: %v", err)
	}
	res, err := New(cfg, st, log.New(testLog{t}, "", 0)).Sync(t.Context())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.New != 3 || res.Archived != 3 {
		t.Errorf("%d new, %d archived; want 3 and 3", res.New, res.Archived)
	}
	if in := folderCount(t, user, "INBOX"); in != 0 {
		t.Errorf("INBOX holds %d, want 0", in)
	}
}

// dropFirstBodyFetch closes the connection of the first session that asks for
// message bodies, as a server dropping a connection mid-sync would.
type dropFirstBodyFetch struct {
	imapserver.Session
	conn    *imapserver.Conn
	dropped *atomic.Bool
}

func (s dropFirstBodyFetch) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	if len(options.BodySection) > 0 && s.dropped.CompareAndSwap(false, true) {
		if err := s.conn.NetConn().Close(); err != nil {
			return err
		}
		return errors.New("connection dropped")
	}
	return s.Session.Fetch(w, numSet, options)
}

func TestDroppedFetchConnectionIsRetried(t *testing.T) {
	var dropped atomic.Bool
	port, _ := startTestServerWith(t, 6, func(conn *imapserver.Conn, sess imapserver.Session) imapserver.Session {
		return dropFirstBodyFetch{Session: sess, conn: conn, dropped: &dropped}
	})
	cfg := config.IMAP{
		Host: "127.0.0.1", Port: port, TLS: config.TLSNone,
		Username: "me@example.com", Password: "secret",
		Inbox: "INBOX", Sent: "Sent", AfterFetch: config.AfterFetchKeep,
		MaxFetch: 2, Connections: 2,
	}
	s := New(cfg, testStore(t), log.New(testLog{t}, "", 0))
	s.retryWait = 0
	res, err := s.Sync(t.Context())
	if err != nil {
		t.Fatalf("sync should survive one dropped connection: %v", err)
	}
	if !dropped.Load() {
		t.Fatal("the test server never dropped a connection")
	}
	if res.New != 6 {
		t.Errorf("stored %d new, want all 6", res.New)
	}
}
