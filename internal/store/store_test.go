package store

import (
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func rawMail(from, to, subject, body string) []byte {
	return []byte("From: " + from + "\r\nTo: " + to + "\r\nSubject: " + subject +
		"\r\nDate: Mon, 31 Aug 2026 10:00:00 +0000\r\nMessage-ID: <" + subject + "@test>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n")
}

func TestIngestRoutesToUnprocessed(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	m, isNew, err := s.Ingest(ctx, "", rawMail("Ann <ann@example.com>", "me@example.com", "hello", "first body"), time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !isNew {
		t.Fatal("first ingest should report a new message")
	}
	if m.Box != BoxUnprocessed {
		t.Errorf("box = %q, want %q", m.Box, BoxUnprocessed)
	}
	if m.Mail.From.Address != "ann@example.com" {
		t.Errorf("from = %q", m.Mail.From.Address)
	}
}

func TestIngestIsIdempotent(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	raw := rawMail("ann@example.com", "me@example.com", "dup", "body")
	if _, isNew, err := s.Ingest(ctx, "", raw, time.Now()); err != nil || !isNew {
		t.Fatalf("first ingest: new=%v err=%v", isNew, err)
	}
	if _, isNew, err := s.Ingest(ctx, "", raw, time.Now()); err != nil || isNew {
		t.Fatalf("second ingest: new=%v err=%v, want new=false", isNew, err)
	}
	_, total, err := s.List(ctx, Query{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
}

func TestRewritingStateKeepsOneSearchRow(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	m, _, err := s.Ingest(ctx, "", rawMail("ann@example.com", "me@example.com", "quarterly", "renewal notice"), time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	for _, box := range []string{BoxPriority, BoxProcessed} {
		if _, moveErr := s.SetBox(ctx, m.ID, box); moveErr != nil {
			t.Fatalf("SetBox %s: %v", box, moveErr)
		}
	}
	items, total, err := s.List(ctx, Query{Text: "renewal"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("search hits = %d (%d items), want exactly 1", total, len(items))
	}
	if items[0].Box != BoxProcessed {
		t.Errorf("box = %q, want the latest move to %q", items[0].Box, BoxProcessed)
	}
}

func TestTrashIsABuiltInBox(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	m, _, err := s.Ingest(ctx, "", rawMail("ann@example.com", "me@example.com", "old news", "body"), time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	trashed, err := s.SetBox(ctx, m.ID, BoxTrash)
	if err != nil {
		t.Fatalf("SetBox trash: %v", err)
	}
	if trashed.Box != BoxTrash || trashed.PrevBox != BoxUnprocessed {
		t.Errorf("box = %q (was %q), want trash, was unprocessed", trashed.Box, trashed.PrevBox)
	}
	if err := s.DeleteBox(ctx, BoxTrash); err == nil {
		t.Error("the Trash box should not be deletable")
	}
}

func TestMovesAreQueuedForTheServer(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	m, _, err := s.Ingest(ctx, "", rawMail("ann@example.com", "me@example.com", "queued", "body"), time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	pending, err := s.PendingMoves(ctx, 10, "")
	if err != nil {
		t.Fatalf("PendingMoves: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("fresh mail is where sync found it; pending = %+v", pending)
	}

	if _, moveErr := s.SetBox(ctx, m.ID, BoxPriority); moveErr != nil {
		t.Fatalf("SetBox: %v", moveErr)
	}
	pending, err = s.PendingMoves(ctx, 10, "")
	if err != nil {
		t.Fatalf("PendingMoves: %v", err)
	}
	want := PendingMove{ID: m.ID, Box: BoxPriority, ServerBox: BoxUnprocessed, MessageID: "queued@test"}
	if len(pending) != 1 || pending[0] != want {
		t.Fatalf("pending = %+v, want [%+v]", pending, want)
	}

	if setErr := s.SetServerBox(ctx, m.ID, BoxPriority); setErr != nil {
		t.Fatalf("SetServerBox: %v", setErr)
	}
	if pending, err = s.PendingMoves(ctx, 10, ""); err != nil || len(pending) != 0 {
		t.Fatalf("after mirroring: pending = %+v, err = %v", pending, err)
	}
}

func TestMoveAllEmptiesABox(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	for _, subject := range []string{"one", "two"} {
		if _, _, err := s.Ingest(ctx, "", rawMail("ann@example.com", "me@example.com", subject, "body"), time.Now()); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
	}
	moved, err := s.MoveAll(ctx, BoxUnprocessed, BoxProcessed)
	if err != nil {
		t.Fatalf("MoveAll: %v", err)
	}
	if moved != 2 {
		t.Errorf("moved = %d, want 2", moved)
	}
	if _, total, listErr := s.List(ctx, Query{Box: BoxUnprocessed}); listErr != nil || total != 0 {
		t.Errorf("unprocessed still holds %d (err %v)", total, listErr)
	}
	if _, err := s.MoveAll(ctx, BoxUnprocessed, "nope"); err == nil {
		t.Error("moving to an unknown box should fail")
	}
}

func TestWhitelistRouting(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	if _, err := s.AddContacts("Bob@Example.com"); err != nil {
		t.Fatalf("AddContacts: %v", err)
	}
	m, _, err := s.Ingest(ctx, "", rawMail("bob@example.com", "me@example.com", "hi", "body"), time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if m.Box != BoxWhitelisted {
		t.Errorf("box = %q, want %q", m.Box, BoxWhitelisted)
	}
}

// serve and sync are separate processes on one mail.d; neither may drop the
// contacts the other wrote.
func TestContactsFromAnotherProcessAreKept(t *testing.T) {
	s := testStore(t)
	other, err := Open(t.Context(), s.Dir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("Open second store: %v", err)
	}
	t.Cleanup(func() {
		if err := other.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	for _, step := range []struct {
		st   *Store
		addr string
	}{{s, "a@example.com"}, {other, "b@example.com"}, {s, "c@example.com"}} {
		if _, err := step.st.AddContacts(step.addr); err != nil {
			t.Fatalf("AddContacts(%s): %v", step.addr, err)
		}
	}
	const want = "a@example.com,b@example.com,c@example.com"
	for name, st := range map[string]*Store{"first": s, "second": other} {
		if got := strings.Join(st.Contacts(), ","); got != want {
			t.Errorf("%s store contacts = %s, want %s", name, got, want)
		}
	}
}

func TestRuleBeatsWhitelist(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	if _, err := s.CreateBox("Error mails"); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	if _, err := s.AddContacts("ci@example.com"); err != nil {
		t.Fatalf("AddContacts: %v", err)
	}
	if err := s.AddRule(Rule{Field: MatchFrom, Address: "@example.com", Box: "error-mails"}); err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	m, _, err := s.Ingest(ctx, "", rawMail("ci@example.com", "me@example.com", "build failed", "body"), time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if m.Box != "error-mails" {
		t.Errorf("box = %q, want error-mails", m.Box)
	}
}

func TestDelayReturnsToOriginalBox(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	m, _, err := s.Ingest(ctx, "", rawMail("ann@example.com", "me@example.com", "later", "body"), time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, boxErr := s.SetBox(ctx, m.ID, BoxPriority); boxErr != nil {
		t.Fatalf("SetBox: %v", boxErr)
	}
	d, err := s.Delay(ctx, m.ID, 3)
	if err != nil {
		t.Fatalf("Delay: %v", err)
	}
	if d.Box != BoxDelayed || d.PrevBox != BoxPriority {
		t.Fatalf("after delay box=%q prev=%q", d.Box, d.PrevBox)
	}
	if d.DelayedUntil == nil {
		t.Fatal("DelayedUntil not set")
	}

	if n, relErr := s.ReleaseDue(ctx, time.Now()); relErr != nil || n != 0 {
		t.Fatalf("ReleaseDue early: n=%d err=%v", n, relErr)
	}
	n, err := s.ReleaseDue(ctx, time.Now().AddDate(0, 0, 4))
	if err != nil {
		t.Fatalf("ReleaseDue: %v", err)
	}
	if n != 1 {
		t.Fatalf("released %d, want 1", n)
	}
	back, err := s.Get(m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if back.Box != BoxPriority {
		t.Errorf("box after release = %q, want %q", back.Box, BoxPriority)
	}
	if back.DelayedUntil != nil {
		t.Error("DelayedUntil should be cleared after release")
	}
}

func TestIgnoreBlacklistsSender(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	m, _, err := s.Ingest(ctx, "", rawMail("spam@bad.example", "me@example.com", "buy", "body"), time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, ignErr := s.Ignore(ctx, m.ID, false); ignErr != nil {
		t.Fatalf("Ignore: %v", ignErr)
	}
	got, err := s.Get(m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Box != BoxSpam {
		t.Errorf("box = %q, want %q", got.Box, BoxSpam)
	}
	// A later message from the same sender must be filed as spam on arrival.
	next, _, err := s.Ingest(ctx, "", rawMail("spam@bad.example", "me@example.com", "buy again", "body"), time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if next.Box != BoxSpam {
		t.Errorf("second message box = %q, want %q", next.Box, BoxSpam)
	}
}

func TestFullTextSearch(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	if _, _, err := s.Ingest(ctx, "", rawMail("ann@example.com", "me@example.com", "invoice", "the quarterly invoice is attached"), time.Now()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, _, err := s.Ingest(ctx, "", rawMail("bob@example.com", "me@example.com", "lunch", "shall we get lunch"), time.Now()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"quarterly", 1},
		{"lunch", 1},
		{"from:ann@example.com", 1},
		{"invoice quarterly", 1},
		{"nothingmatches", 0},
		{`weird"punctuation:`, 0},
	} {
		items, total, err := s.List(ctx, Query{Text: tc.query})
		if err != nil {
			t.Fatalf("List(%q): %v", tc.query, err)
		}
		if total != tc.want || len(items) != tc.want {
			t.Errorf("List(%q) = %d items (total %d), want %d", tc.query, len(items), total, tc.want)
		}
	}
}

func TestReindexRebuildsFromStateFiles(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	for i := 0; i < 5; i++ {
		raw := rawMail("ann@example.com", "me@example.com", fmt.Sprintf("subject-%d", i), "body text")
		if _, _, err := s.Ingest(ctx, "", raw, time.Now()); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
	}
	if err := s.idx.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	n, err := s.Reindex(ctx)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if n != 5 {
		t.Fatalf("reindexed %d, want 5", n)
	}
	items, total, err := s.List(ctx, Query{Text: "body"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 5 || len(items) != 5 {
		t.Errorf("after reindex got %d/%d, want 5", len(items), total)
	}
}

func TestDeleteCustomBoxMovesMessages(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	if _, err := s.CreateBox("Temp Box"); err != nil {
		t.Fatalf("CreateBox: %v", err)
	}
	m, _, err := s.Ingest(ctx, "", rawMail("ann@example.com", "me@example.com", "x", "body"), time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, boxErr := s.SetBox(ctx, m.ID, "temp-box"); boxErr != nil {
		t.Fatalf("SetBox: %v", boxErr)
	}
	if delErr := s.DeleteBox(ctx, "temp-box"); delErr != nil {
		t.Fatalf("DeleteBox: %v", delErr)
	}
	got, err := s.Get(m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Box != BoxUnprocessed {
		t.Errorf("box = %q, want %q", got.Box, BoxUnprocessed)
	}
	if err := s.DeleteBox(ctx, BoxSpam); err == nil {
		t.Error("deleting a system box should fail")
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Error mails":  "error-mails",
		"  Spam!!  ":   "spam",
		"A/B testing":  "a-b-testing",
		"2026 Reports": "2026-reports",
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHTMLOnlyBodyBecomesText(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	raw := []byte("From: ann@example.com\r\nTo: me@example.com\r\nSubject: html\r\n" +
		"Date: Mon, 31 Aug 2026 10:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<html><head><style>p{color:red}</style></head><body><p>Hello <b>there</b></p>" +
		"<script>alert(1)</script><a href=\"https://example.com\">link</a></body></html>\r\n")
	m, _, err := s.Ingest(ctx, "", raw, time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	body, err := s.readText(m)
	if err != nil {
		t.Fatalf("readText: %v", err)
	}
	if strings.Contains(body, "alert(1)") || strings.Contains(body, "color:red") {
		t.Errorf("script/style leaked into text body: %q", body)
	}
	if !strings.Contains(body, "Hello there") {
		t.Errorf("body = %q, want it to contain %q", body, "Hello there")
	}
	if !strings.Contains(body, "<https://example.com>") {
		t.Errorf("body = %q, want the link target preserved", body)
	}
}

func TestAccountsKeepTheirOwnCursorAndMoves(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	home, _, err := s.Ingest(ctx, "home", rawMail("ann@example.com", "me@home.example", "from home", "body"), time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	work, _, err := s.Ingest(ctx, "work", rawMail("bob@example.com", "me@work.example", "from work", "body"), time.Now())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got, getErr := s.Get(work.ID); getErr != nil || got.Account != "work" {
		t.Fatalf("stored account = %+v, err = %v; want work", got, getErr)
	}

	for _, id := range []string{home.ID, work.ID} {
		if _, moveErr := s.SetBox(ctx, id, BoxPriority); moveErr != nil {
			t.Fatalf("SetBox: %v", moveErr)
		}
	}
	pending, err := s.PendingMoves(ctx, 10, "work")
	if err != nil {
		t.Fatalf("PendingMoves: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != work.ID || pending[0].Account != "work" {
		t.Fatalf("work pending = %+v, want only the work message", pending)
	}
	if none, noneErr := s.PendingMoves(ctx, 10); noneErr != nil || len(none) != 0 {
		t.Fatalf("no accounts: pending = %+v, err = %v", none, noneErr)
	}

	synced := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if saveErr := s.SaveCursor("work", Cursor{InboxUID: 7, LastSync: synced}); saveErr != nil {
		t.Fatalf("SaveCursor: %v", saveErr)
	}
	if c, loadErr := s.LoadCursor("home"); loadErr != nil || c.InboxUID != 0 {
		t.Fatalf("home cursor = %+v, err = %v; want untouched", c, loadErr)
	}
	stats, err := s.Stats(ctx, []string{"home", "work"}, []string{"work"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.PendingIMAP != 1 {
		t.Errorf("pending = %d, want only the mirrored work account's move", stats.PendingIMAP)
	}
	if !stats.LastSync.Equal(synced) {
		t.Errorf("last sync = %s, want the latest of any account (%s)", stats.LastSync, synced)
	}
}
