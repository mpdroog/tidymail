// Package imapsync pulls new mail from an IMAP account into the local store
// and, optionally, tidies the server side afterwards.
//
// The local store is the system of record. Fetching is one-way — fetch, store,
// then archive or delete what was stored — and box moves are repeated in IMAP
// folders separately, by MirrorMoves.
package imapsync

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/mpdroog/tidymail/internal/config"
	"github.com/mpdroog/tidymail/internal/store"
)

// Syncer fetches mail for one account.
type Syncer struct {
	account string // tags stored mail and names the sync cursor
	cfg     config.IMAP
	st      *store.Store
	log     *log.Logger

	// Verbose also logs every stored message, not only batch statistics.
	Verbose bool
}

// New returns a Syncer for the named account; the single unnamed account is "".
func New(account string, cfg config.IMAP, st *store.Store, logger *log.Logger) *Syncer {
	return &Syncer{account: account, cfg: cfg, st: st, log: logger}
}

// Result summarises one sync run.
type Result struct {
	Fetched   int       `json:"fetched"`
	New       int       `json:"new"`
	Archived  int       `json:"archived"`
	Deleted   int       `json:"deleted"`
	Contacts  int       `json:"contacts_added"`
	Remaining int       `json:"remaining_on_server"`
	At        time.Time `json:"at"`

	Bytes int64         `json:"bytes_downloaded"`
	Took  time.Duration `json:"took"`
	CPU   time.Duration `json:"cpu"` // user+system CPU time used by the sync

	// NewByBox counts new messages by the box they were filed in.
	NewByBox map[string]int `json:"new_by_box"`
}

// Sync runs one full cycle: learn contacts from the Sent folder, then fetch and
// file new INBOX mail. It only touches mail it fetches; releasing delayed mail
// is left to serve, so the two processes never edit the same message.
func (s *Syncer) Sync(ctx context.Context) (res Result, err error) {
	res.At = time.Now().UTC()
	start, cpuStart := time.Now(), cpuTime()

	c, err := s.dial()
	if err != nil {
		return res, err
	}
	// IMAP commands do not take a context, so cancelling (Ctrl+C) closes the
	// connection to unblock whichever command is in flight. Mail stored so far
	// stays stored; anything not yet archived is re-fetched and deduplicated
	// next time.
	stopWatch := context.AfterFunc(ctx, func() {
		if cerr := c.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			s.log.Printf("imap: close on cancel: %v", cerr)
		}
	})
	defer func() {
		if !stopWatch() {
			err = fmt.Errorf("sync interrupted: %w", ctx.Err())
			return
		}
		if lerr := logoutClient(c); lerr != nil && err == nil {
			err = lerr
		}
	}()

	if err = c.Login(s.cfg.Username, s.cfg.Password).Wait(); err != nil {
		return res, fmt.Errorf("imap login as %s: %w", s.cfg.Username, err)
	}

	folders, err := s.resolveFolders(c)
	if err != nil {
		return res, err
	}

	cursor, err := s.st.LoadCursor(s.account)
	if err != nil {
		return res, err
	}

	if s.cfg.ScanSent && folders.sent != "" {
		scanStart := time.Now()
		added, scanErr := s.scanSent(ctx, c, folders.sent, &cursor)
		res.Contacts = added
		if scanErr != nil {
			return res, scanErr
		}
		s.log.Printf("sync: scanned %s in %s, %d new contact(s)", folders.sent, round(time.Since(scanStart)), added)
	}

	if err := s.fetchInbox(ctx, c, folders, &cursor, &res); err != nil {
		return res, err
	}

	cursor.LastSync = res.At
	if err := s.st.SaveCursor(s.account, cursor); err != nil {
		return res, err
	}
	res.Took, res.CPU = time.Since(start), cpuTime()-cpuStart
	return res, nil
}

// folders holds the resolved server folder names.
type folders struct {
	inbox   string
	sent    string
	archive string
}

// dial opens a connection honouring the configured TLS mode.
func (s *Syncer) dial() (*imapclient.Client, error) {
	addr := s.cfg.Addr()
	opts := &imapclient.Options{
		TLSConfig:   &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12},
		WordDecoder: nil,
	}
	var (
		c   *imapclient.Client
		err error
	)
	switch s.cfg.TLS {
	case config.TLSImplicit:
		c, err = imapclient.DialTLS(addr, opts)
	case config.TLSStartTLS:
		c, err = imapclient.DialStartTLS(addr, opts)
	case config.TLSNone:
		c, err = imapclient.DialInsecure(addr, opts)
	default:
		return nil, fmt.Errorf("imap: unsupported tls mode %q", s.cfg.TLS)
	}
	if err != nil {
		return nil, fmt.Errorf("dial imap %s: %w", addr, err)
	}
	return c, nil
}

// logoutClient ends the session, falling back to closing the socket.
func logoutClient(c *imapclient.Client) error {
	if err := c.Logout().Wait(); err != nil {
		if cerr := c.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			return fmt.Errorf("imap logout: %w (and close: %w)", err, cerr)
		}
		return fmt.Errorf("imap logout: %w", err)
	}
	return nil
}

// resolveFolders finds the Sent and Archive folders, preferring the configured
// names and falling back to the RFC 6154 special-use attributes.
func (s *Syncer) resolveFolders(c *imapclient.Client) (folders, error) {
	f := folders{inbox: s.cfg.Inbox, sent: s.cfg.Sent, archive: s.cfg.Archive}
	if f.sent != "" && (f.archive != "" || s.cfg.AfterFetch != config.AfterFetchArchive) {
		return f, nil
	}

	byAttr, names, err := listFolders(c)
	if err != nil {
		return f, err
	}
	if f.sent == "" {
		f.sent = pick(byAttr[imap.MailboxAttrSent], names, "sent", "sent items", "sent mail", "inbox.sent")
	}
	if f.archive == "" {
		f.archive = pick(byAttr[imap.MailboxAttrArchive], names, "archive", "archives", "all mail", "inbox.archive")
	}
	if s.cfg.AfterFetch == config.AfterFetchArchive && f.archive == "" {
		return f, fmt.Errorf("imap: no Archive folder found; set imap.archive or use after_fetch = \"keep\"")
	}
	return f, nil
}

// pick returns the special-use match, or the first candidate name present.
func pick(special string, names map[string]string, candidates ...string) string {
	if special != "" {
		return special
	}
	for _, c := range candidates {
		if n, ok := names[c]; ok {
			return n
		}
	}
	return ""
}

// scanSent records every address we have written to, so their replies land in
// the Whitelisted box. It returns the number of newly whitelisted addresses.
// Contacts and cursor.SentUID are saved after every batch, so an interrupted
// scan resumes where it stopped.
func (s *Syncer) scanSent(ctx context.Context, c *imapclient.Client, mailbox string, cursor *store.Cursor) (added int, err error) {
	sel, err := c.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return 0, fmt.Errorf("imap select %s: %w", mailbox, err)
	}
	if sel.NumMessages == 0 {
		return 0, nil
	}

	criteria := &imap.SearchCriteria{}
	criteria.UID = []imap.UIDSet{uidRangeFrom(imap.UID(cursor.SentUID) + 1)}
	found, err := c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return 0, fmt.Errorf("imap search %s: %w", mailbox, err)
	}
	uids := found.AllUIDs()
	if len(uids) == 0 {
		return 0, nil
	}

	for _, batch := range batchUIDs(uids, s.cfg.MaxFetch) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return added, fmt.Errorf("scan sent: %w", ctxErr)
		}
		msgs, fetchErr := c.Fetch(imap.UIDSetNum(batch...), &imap.FetchOptions{UID: true, Envelope: true}).Collect()
		if fetchErr != nil {
			return added, fmt.Errorf("imap fetch envelopes from %s: %w", mailbox, fetchErr)
		}
		highest := cursor.SentUID
		addrs := make([]string, 0, len(msgs)*2)
		for _, m := range msgs {
			if uint32(m.UID) > highest {
				highest = uint32(m.UID)
			}
			if m.Envelope == nil {
				continue
			}
			addrs = append(addrs, envelopeAddresses(m.Envelope.To)...)
			addrs = append(addrs, envelopeAddresses(m.Envelope.Cc)...)
		}

		n, addErr := s.st.AddContacts(addrs...)
		if addErr != nil {
			return added, addErr
		}
		added += n
		cursor.SentUID = highest
		if saveErr := s.st.SaveCursor(s.account, *cursor); saveErr != nil {
			return added, saveErr
		}
	}
	return added, nil
}

// fetchInbox downloads new INBOX mail, files it locally and then archives or
// deletes it on the server.
func (s *Syncer) fetchInbox(ctx context.Context, c *imapclient.Client, f folders, cursor *store.Cursor, res *Result) error {
	sel, err := c.Select(f.inbox, &imap.SelectOptions{}).Wait()
	if err != nil {
		return fmt.Errorf("imap select %s: %w", f.inbox, err)
	}
	// A UIDVALIDITY change invalidates every stored UID; start over.
	if cursor.UIDValidity != 0 && cursor.UIDValidity != sel.UIDValidity {
		s.log.Printf("imap: %s UIDVALIDITY changed %d -> %d, resetting cursor",
			f.inbox, cursor.UIDValidity, sel.UIDValidity)
		cursor.InboxUID = 0
	}
	cursor.UIDValidity = sel.UIDValidity
	if sel.NumMessages == 0 {
		return nil
	}

	criteria := &imap.SearchCriteria{}
	if s.cfg.AfterFetch == config.AfterFetchKeep {
		// Nothing is removed from the server, so only look past the cursor.
		criteria.UID = []imap.UIDSet{uidRangeFrom(imap.UID(cursor.InboxUID + 1))}
	}
	found, err := c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return fmt.Errorf("imap search %s: %w", f.inbox, err)
	}
	uids := found.AllUIDs()
	res.Remaining = len(uids)
	if len(uids) == 0 {
		return nil
	}

	section := &imap.FetchItemBodySection{Peek: true}
	opts := &imap.FetchOptions{
		UID:          true,
		InternalDate: true,
		RFC822Size:   true,
		BodySection:  []*imap.FetchItemBodySection{section},
	}

	batches := batchUIDs(uids, s.cfg.MaxFetch)
	d, err := s.startDownloads(ctx, f.inbox, batches, opts)
	if err != nil {
		return err
	}
	completed := false
	defer func() { d.close(completed) }()
	s.log.Printf("sync: fetching %d message(s) from %s in %d batch(es) over %d connection(s)",
		len(uids), f.inbox, len(batches), d.conns)

	// Downloads overlap, so CPU is measured between log lines, not per batch.
	fetchStart, lastLog, lastCPU := time.Now(), time.Now(), cpuTime()
	processed := 0
	for i, batch := range batches {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("fetch inbox: %w", ctxErr)
		}
		got := d.next(i)
		if got.err != nil {
			return got.err
		}
		msgs, download := got.msgs, got.download

		storeStart, before, newBefore := time.Now(), s.st.Timings(), res.New
		var batchBytes int64
		var newest time.Time // server receive date, which senders cannot fake
		var done imap.UIDSet
		for _, m := range msgs {
			raw := m.FindBodySection(section)
			batchBytes += int64(len(raw))
			if m.InternalDate.After(newest) {
				newest = m.InternalDate
			}
			if len(raw) == 0 {
				s.log.Printf("imap: UID %d in %s returned an empty body, leaving it on the server", m.UID, f.inbox)
				continue
			}
			stored, isNew, storeErr := s.st.Ingest(ctx, s.account, raw, m.InternalDate)
			if storeErr != nil {
				// One unparseable message must not stall the whole sync;
				// it stays on the server so it can be retried.
				s.log.Printf("imap: UID %d in %s could not be stored: %v", m.UID, f.inbox, storeErr)
				continue
			}
			res.Fetched++
			if isNew {
				res.New++
				if res.NewByBox == nil {
					res.NewByBox = make(map[string]int)
				}
				res.NewByBox[stored.Box]++
				if s.Verbose {
					s.log.Printf("imap: stored %s from %s -> %s (%q)",
						stored.ID, stored.Mail.From.Address, stored.Box, stored.Mail.Subject)
				}
			}
			if uint32(m.UID) > cursor.InboxUID {
				cursor.InboxUID = uint32(m.UID)
			}
			done.AddNum(m.UID)
		}
		// Record progress per batch, so an interrupted sync resumes here
		// instead of fetching everything again.
		if saveErr := s.st.SaveCursor(s.account, *cursor); saveErr != nil {
			return saveErr
		}
		storeTime, spent := time.Since(storeStart), s.st.Timings().Sub(before)

		cleanStart := time.Now()
		if len(done) > 0 {
			archived, deleted, cleanErr := s.cleanup(c, done, f.archive)
			if cleanErr != nil {
				return cleanErr
			}
			res.Archived += archived
			res.Deleted += deleted
			res.Remaining -= archived + deleted
		}
		cleanTime := time.Since(cleanStart)

		res.Bytes += batchBytes
		processed += len(batch)
		now, cpuNow := time.Now(), cpuTime()
		s.log.Printf("sync: batch %d/%d: %d msgs, %d new, %s downloaded in %s | store %s (parse %s, %d file writes %s, index %s) | cleanup %s | cpu %s | newest %s | %d/%d done, ~%s left",
			i+1, len(batches), len(msgs), res.New-newBefore, mib(batchBytes), round(download),
			round(storeTime), round(spent.Parse), spent.FileWrites, round(spent.Files), round(spent.Index),
			round(cleanTime), cpuShare(cpuNow-lastCPU, now.Sub(lastLog)), behind(newest, now),
			processed, len(uids), eta(now.Sub(fetchStart), processed, len(uids)))
		lastLog, lastCPU = now, cpuNow
	}
	completed = true
	return nil
}

// cleanup applies the configured after_fetch action to messages that are now
// safely stored locally.
func (s *Syncer) cleanup(c *imapclient.Client, uids imap.UIDSet, archive string) (archived, deleted int, err error) {
	n, err := countUIDs(uids)
	if err != nil {
		return 0, 0, err
	}

	switch s.cfg.AfterFetch {
	case config.AfterFetchKeep:
		return 0, 0, nil

	case config.AfterFetchArchive:
		if archive == "" {
			return 0, 0, errors.New("imap: after_fetch is archive but no archive folder is known")
		}
		if moveErr := moveTo(c, uids, archive); moveErr != nil {
			return 0, 0, moveErr
		}
		return n, 0, nil

	case config.AfterFetchDelete:
		if expErr := expunge(c, uids); expErr != nil {
			return 0, 0, expErr
		}
		return 0, n, nil

	default:
		return 0, 0, fmt.Errorf("imap: unsupported after_fetch %q", s.cfg.AfterFetch)
	}
}

// expunge flags messages deleted and removes them.
func expunge(c *imapclient.Client, uids imap.UIDSet) error {
	flags := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}
	if err := c.Store(uids, flags, nil).Close(); err != nil {
		return fmt.Errorf("imap flag deleted: %w", err)
	}
	if c.Caps().Has(imap.CapUIDPlus) {
		if err := c.UIDExpunge(uids).Close(); err != nil {
			return fmt.Errorf("imap uid expunge: %w", err)
		}
		return nil
	}
	if err := c.Expunge().Close(); err != nil {
		return fmt.Errorf("imap expunge: %w", err)
	}
	return nil
}

// AppendSent stores a copy of an outgoing message in the Sent folder so other
// clients see it too.
func (s *Syncer) AppendSent(raw []byte, when time.Time) (err error) {
	c, err := s.dial()
	if err != nil {
		return err
	}
	defer func() {
		if lerr := logoutClient(c); lerr != nil && err == nil {
			err = lerr
		}
	}()
	if err = c.Login(s.cfg.Username, s.cfg.Password).Wait(); err != nil {
		return fmt.Errorf("imap login as %s: %w", s.cfg.Username, err)
	}
	f, ferr := s.resolveFolders(c)
	if ferr != nil {
		return ferr
	}
	if f.sent == "" {
		return errors.New("imap: no Sent folder found; set imap.sent to keep copies of replies")
	}

	cmd := c.Append(f.sent, int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}, Time: when})
	if _, err = cmd.Write(raw); err != nil {
		if cerr := cmd.Close(); cerr != nil {
			return fmt.Errorf("imap append to %s: %w (and close: %w)", f.sent, err, cerr)
		}
		return fmt.Errorf("imap append to %s: %w", f.sent, err)
	}
	if err = cmd.Close(); err != nil {
		return fmt.Errorf("imap append to %s: %w", f.sent, err)
	}
	if _, err = cmd.Wait(); err != nil {
		return fmt.Errorf("imap append to %s: %w", f.sent, err)
	}
	return nil
}

// uidRangeFrom builds the set "start:*".
func uidRangeFrom(start imap.UID) imap.UIDSet {
	if start < 1 {
		start = 1
	}
	var set imap.UIDSet
	set.AddRange(start, 0) // 0 means "*"
	return set
}

// batchUIDs splits a UID slice into chunks of at most size.
func batchUIDs(uids []imap.UID, size int) [][]imap.UID {
	if size <= 0 {
		size = 100
	}
	var out [][]imap.UID
	for start := 0; start < len(uids); start += size {
		end := start + size
		if end > len(uids) {
			end = len(uids)
		}
		out = append(out, uids[start:end])
	}
	return out
}

// countUIDs returns how many UIDs a set holds.
func countUIDs(set imap.UIDSet) (int, error) {
	nums, ok := set.Nums()
	if !ok {
		return 0, errors.New("imap: cannot count a dynamic UID set")
	}
	return len(nums), nil
}

// envelopeAddresses flattens IMAP envelope addresses to bare mail addresses.
func envelopeAddresses(list []imap.Address) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		addr := strings.ToLower(strings.TrimSpace(a.Addr()))
		if addr != "" && strings.Contains(addr, "@") {
			out = append(out, addr)
		}
	}
	return out
}
