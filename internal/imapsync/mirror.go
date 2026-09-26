package imapsync

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/mpdroog/tidymail/internal/config"
	"github.com/mpdroog/tidymail/internal/store"
)

// searchChunk is how many Message-IDs one SEARCH asks for at once.
const searchChunk = 25

// ServerMove is one box move to repeat on the server.
type ServerMove struct {
	ID        string // tidymail id, reported back when done
	MessageID string
	From      string // box whose folder holds the server copy now
	To        string // box it has to end up in
}

// MirrorMoves repeats box moves on the IMAP server over one connection. It
// returns the ids that need no further work: moved, already in place, or not on
// the server at all. Ids it does not return should be retried later.
// boxNames maps box slugs to display names, used as folder names for boxes
// without a special folder.
func (s *Syncer) MirrorMoves(ctx context.Context, moves []ServerMove, boxNames map[string]string) (done []string, err error) {
	c, err := s.dial()
	if err != nil {
		return nil, err
	}
	stopWatch := context.AfterFunc(ctx, func() { s.closeConn(c) })
	defer func() {
		if !stopWatch() {
			err = fmt.Errorf("mirror interrupted: %w", ctx.Err())
			return
		}
		if lerr := logoutClient(c); lerr != nil && err == nil {
			err = lerr
		}
	}()
	if err = c.Login(s.cfg.Username, s.cfg.Password).Wait(); err != nil {
		return nil, fmt.Errorf("imap login as %s: %w", s.cfg.Username, err)
	}

	fm, err := s.newFolderMap(c, boxNames)
	if err != nil {
		return nil, err
	}

	groups := make(map[string][]ServerMove)
	var order []string
	for _, mv := range moves {
		mv.MessageID = strings.Trim(strings.TrimSpace(mv.MessageID), "<>")
		from, to := fm.folderFor(mv.From), fm.folderFor(mv.To)
		if mv.MessageID == "" || strings.EqualFold(from, to) {
			// Without a Message-ID it cannot be found; in the same folder
			// there is nothing to do.
			done = append(done, mv.ID)
			continue
		}
		if _, ok := groups[to]; !ok {
			order = append(order, to)
		}
		groups[to] = append(groups[to], mv)
	}
	for _, to := range order {
		ids, moveErr := s.mirrorInto(ctx, c, fm, to, groups[to])
		done = append(done, ids...)
		if moveErr != nil {
			return done, moveErr
		}
	}
	return done, nil
}

// mirrorInto finds one destination folder's messages on the server and moves
// them there.
func (s *Syncer) mirrorInto(ctx context.Context, c *imapclient.Client, fm *folderMap, to string, group []ServerMove) (done []string, err error) {
	if err := fm.ensure(c, to); err != nil {
		return nil, err
	}

	remaining := make(map[string][]string, len(group)) // Message-ID -> tidymail ids
	seen := map[string]bool{strings.ToLower(to): true}
	var candidates []string
	addCandidate := func(folder string) {
		if folder != "" && !seen[strings.ToLower(folder)] {
			seen[strings.ToLower(folder)] = true
			candidates = append(candidates, folder)
		}
	}
	for _, mv := range group {
		remaining[mv.MessageID] = append(remaining[mv.MessageID], mv.ID)
		addCandidate(fm.folderFor(mv.From))
	}
	// Local and server state can disagree (mail moved by another client), so
	// also look where sync leaves mail.
	addCandidate(fm.inbox)
	addCandidate(fm.archive)

	for _, folder := range candidates {
		if len(remaining) == 0 {
			break
		}
		if !fm.exists(folder) {
			continue
		}
		if _, err := c.Select(folder, nil).Wait(); err != nil {
			return done, fmt.Errorf("imap select %s: %w", folder, err)
		}
		wanted := make([]string, 0, len(remaining))
		for id := range remaining {
			wanted = append(wanted, id)
		}
		sort.Strings(wanted)

		for start := 0; start < len(wanted); start += searchChunk {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return done, fmt.Errorf("mirror moves: %w", ctxErr)
			}
			chunk := wanted[start:min(start+searchChunk, len(wanted))]
			found, searchErr := c.UIDSearch(messageIDCriteria(chunk), nil).Wait()
			if searchErr != nil {
				return done, fmt.Errorf("imap search %s: %w", folder, searchErr)
			}
			uids := found.AllUIDs()
			if len(uids) == 0 {
				continue
			}
			msgs, fetchErr := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, Envelope: true}).Collect()
			if fetchErr != nil {
				return done, fmt.Errorf("imap fetch envelopes from %s: %w", folder, fetchErr)
			}
			// HEADER search matches substrings, so confirm each hit by its
			// exact Message-ID before moving it.
			var set imap.UIDSet
			var matched []string
			for _, msg := range msgs {
				if msg.Envelope == nil {
					continue
				}
				mid := strings.Trim(msg.Envelope.MessageID, "<>")
				if _, ok := remaining[mid]; ok {
					set.AddNum(msg.UID)
					matched = append(matched, mid)
				}
			}
			if len(matched) == 0 {
				continue
			}
			if moveErr := moveTo(c, set, to); moveErr != nil {
				return done, moveErr
			}
			for _, mid := range matched {
				if ids, ok := remaining[mid]; ok {
					done = append(done, ids...)
					delete(remaining, mid)
				}
			}
		}
	}

	// Not found anywhere: deleted on the server, or never kept there
	// (after_fetch = delete). Nothing left to move.
	if len(remaining) > 0 {
		s.log.Printf("imap: %d message(s) to move to %s are not on the server, skipping", len(remaining), to)
		for _, ids := range remaining {
			done = append(done, ids...)
		}
	}
	return done, nil
}

// messageIDCriteria matches any of the Message-IDs. The angle brackets keep
// <1@host> from matching <11@host> in the substring HEADER search.
func messageIDCriteria(ids []string) *imap.SearchCriteria {
	var crit imap.SearchCriteria
	for i, id := range ids {
		one := imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: "<" + id + ">"}}}
		if i == 0 {
			crit = one
			continue
		}
		crit = imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{crit, one}}}
	}
	return &crit
}

// folderMap maps boxes to IMAP folders for one connection.
type folderMap struct {
	inbox, archive string
	names          map[string]string // lower-cased name -> server name
	special        map[string]string // box slug -> folder
	boxNames       map[string]string // box slug -> display name
}

func (s *Syncer) newFolderMap(c *imapclient.Client, boxNames map[string]string) (*folderMap, error) {
	f, err := s.resolveFolders(c)
	if err != nil {
		return nil, err
	}
	byAttr, names, err := listFolders(c)
	if err != nil {
		return nil, err
	}
	find := func(configured string, attr imap.MailboxAttr, fallback string, candidates ...string) string {
		if configured != "" {
			return configured
		}
		if p := pick(byAttr[attr], names, candidates...); p != "" {
			return p
		}
		return fallback
	}

	archive := f.archive
	if archive == "" {
		archive = pick(byAttr[imap.MailboxAttrArchive], names, "archive", "archives", "inbox.archive")
	}
	if archive == "" {
		archive = byAttr[imap.MailboxAttrAll] // Gmail: All Mail is its archive
	}
	if archive == "" {
		archive = "Archive"
	}
	// With archive or delete, sync moves mail out of INBOX itself; putting it
	// back there would only make the next sync fetch and move it again.
	unprocessed := f.inbox
	if s.cfg.AfterFetch != config.AfterFetchKeep {
		unprocessed = archive
	}

	return &folderMap{
		inbox:   f.inbox,
		archive: archive,
		names:   names,
		special: map[string]string{
			store.BoxUnprocessed: unprocessed,
			store.BoxProcessed:   archive,
			store.BoxSpam: find(s.cfg.Junk, imap.MailboxAttrJunk, "Spam",
				"spam", "junk", "junk e-mail", "junk email", "[gmail]/spam", "[google mail]/spam", "inbox.spam", "inbox.junk"),
			store.BoxTrash: find(s.cfg.Trash, imap.MailboxAttrTrash, "Trash",
				"trash", "deleted items", "deleted messages", "[gmail]/trash", "[google mail]/trash", "inbox.trash"),
		},
		boxNames: boxNames,
	}, nil
}

// folderFor returns the folder a box's mail lives in on the server.
func (fm *folderMap) folderFor(box string) string {
	if folder, ok := fm.special[box]; ok {
		return folder
	}
	if name := fm.boxNames[box]; name != "" {
		return name
	}
	return box
}

func (fm *folderMap) exists(folder string) bool {
	_, ok := fm.names[strings.ToLower(folder)]
	return ok
}

// ensure creates a folder that does not exist yet.
func (fm *folderMap) ensure(c *imapclient.Client, folder string) error {
	if fm.exists(folder) {
		return nil
	}
	if err := c.Create(folder, nil).Wait(); err != nil {
		return fmt.Errorf("imap create %s: %w", folder, err)
	}
	fm.names[strings.ToLower(folder)] = folder
	return nil
}

// listFolders lists the server's folders by special-use attribute and by
// lower-cased name.
func listFolders(c *imapclient.Client) (byAttr map[imap.MailboxAttr]string, names map[string]string, err error) {
	list, err := c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		return nil, nil, fmt.Errorf("imap list mailboxes: %w", err)
	}
	byAttr = make(map[imap.MailboxAttr]string, 4)
	names = make(map[string]string, len(list))
	for _, mb := range list {
		names[strings.ToLower(mb.Mailbox)] = mb.Mailbox
		for _, a := range mb.Attrs {
			if _, seen := byAttr[a]; !seen {
				byAttr[a] = mb.Mailbox
			}
		}
	}
	return byAttr, names, nil
}

// moveTo moves messages to another folder, falling back to copy-then-delete on
// servers without MOVE.
func moveTo(c *imapclient.Client, uids imap.UIDSet, dest string) error {
	if c.Caps().Has(imap.CapMove) {
		if _, err := c.Move(uids, dest).Wait(); err != nil {
			return fmt.Errorf("imap move to %s: %w", dest, err)
		}
		return nil
	}
	if _, err := c.Copy(uids, dest).Wait(); err != nil {
		return fmt.Errorf("imap copy to %s: %w", dest, err)
	}
	return expunge(c, uids)
}
