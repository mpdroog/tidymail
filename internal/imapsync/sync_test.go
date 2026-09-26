package imapsync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
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
	user := imapmemserver.NewUser("me@example.com", "secret")
	for _, name := range []string{"INBOX", "Sent", "Trash"} {
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
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
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
