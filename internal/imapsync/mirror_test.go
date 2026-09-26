package imapsync

import (
	"log"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/mpdroog/tidymail/internal/config"
	"github.com/mpdroog/tidymail/internal/store"
)

func TestMirrorMovesFollowBoxes(t *testing.T) {
	port, user := startTestServer(t, 12)
	cfg := config.IMAP{
		Host: "127.0.0.1", Port: port, TLS: config.TLSNone,
		Username: "me@example.com", Password: "secret",
		Inbox: "INBOX", Sent: "Sent", AfterFetch: config.AfterFetchKeep,
		MaxFetch: 10, Connections: 1,
	}
	s := New(cfg, nil, log.New(testLog{t}, "", 0))
	names := map[string]string{store.BoxPriority: "Priority"}

	count := func(mailbox string) uint32 {
		t.Helper()
		st, err := user.Status(mailbox, &imap.StatusOptions{NumMessages: true})
		if err != nil {
			return 0 // folder does not exist
		}
		return *st.NumMessages
	}

	done, err := s.MirrorMoves(t.Context(), []ServerMove{
		{ID: "a", MessageID: "1@test", From: store.BoxUnprocessed, To: store.BoxTrash},
		{ID: "b", MessageID: "<2@test>", From: store.BoxUnprocessed, To: store.BoxPriority},
		{ID: "c", MessageID: "3@test", From: store.BoxUnprocessed, To: store.BoxProcessed},
		{ID: "d", MessageID: "99@test", From: store.BoxUnprocessed, To: store.BoxTrash},
		{ID: "e", MessageID: "", From: store.BoxUnprocessed, To: store.BoxTrash},
	}, names)
	if err != nil {
		t.Fatalf("MirrorMoves: %v", err)
	}
	if len(done) != 5 {
		t.Errorf("done = %v, want all 5 (moved, missing or unfindable)", done)
	}
	// <1@test> must not also have matched <11@test> or <12@test>.
	for folder, want := range map[string]uint32{"INBOX": 9, "Trash": 1, "Priority": 1, "Archive": 1} {
		if got := count(folder); got != want {
			t.Errorf("%s holds %d, want %d", folder, got, want)
		}
	}

	// Moving it back finds it in the folder of the box it was in.
	if _, err := s.MirrorMoves(t.Context(), []ServerMove{
		{ID: "b", MessageID: "2@test", From: store.BoxPriority, To: store.BoxUnprocessed},
	}, names); err != nil {
		t.Fatalf("MirrorMoves back: %v", err)
	}
	if count("INBOX") != 10 || count("Priority") != 0 {
		t.Errorf("after moving back: INBOX %d, Priority %d; want 10 and 0", count("INBOX"), count("Priority"))
	}
}
