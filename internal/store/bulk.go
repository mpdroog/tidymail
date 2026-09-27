package store

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// purgedFile lists the ids of messages removed by EmptyTrash. They stay
// listed so a later sync, which may see the same mail on the server again,
// never stores them a second time.
const purgedFile = "purged.txt"

// MatchingIDs returns the id of every message a list query matches, ignoring
// its limit and offset: what "all results" means in the UI.
func (s *Store) MatchingIDs(ctx context.Context, q Query) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if q.Box != "" && !s.hasBoxLocked(q.Box) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownBox, q.Box)
	}
	return s.idx.MatchingIDs(ctx, q)
}

// IDsFromSender returns the id of every message sent by addr.
func (s *Store) IDsFromSender(ctx context.Context, addr string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idx.IDsFromSender(ctx, addr)
}

// MoveMany moves the given messages to box to and reports how many moved.
// Messages already there are left alone, and ones that no longer exist (emptied
// from Trash meanwhile) are logged and skipped. Like MoveAll the lock is taken
// per message, so the UI and sync keep working during a large move.
func (s *Store) MoveMany(ctx context.Context, ids []string, to string) (int, error) {
	if !s.HasBox(to) {
		return 0, fmt.Errorf("%w: %s", ErrUnknownBox, to)
	}
	moved := 0
	for _, id := range ids {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return moved, fmt.Errorf("move to %s: %w", to, ctxErr)
		}
		ok, err := s.moveUnlessIn(ctx, id, to)
		if errors.Is(err, ErrNotFound) {
			s.log.Printf("store: move %s to %s: %v, skipping", id, to, err)
			continue
		}
		if err != nil {
			return moved, err
		}
		if ok {
			moved++
		}
	}
	return moved, nil
}

// moveUnlessIn moves a message to box to unless it is there already.
func (s *Store) moveUnlessIn(ctx context.Context, id, to string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.readState(id)
	if err != nil {
		return false, err
	}
	if m.Box == to {
		return false, nil
	}
	return true, s.setBoxLocked(ctx, id, to, "", 0)
}

// EmptyTrash removes every message in the Trash box from mail.d for good. See
// PurgeFromTrash.
func (s *Store) EmptyTrash(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, err := s.idx.IDsInBox(ctx, BoxTrash)
	if err != nil {
		return 0, err
	}
	return s.purgeLocked(ctx, ids)
}

// PurgeFromTrash removes the given messages from mail.d for good: their raw and
// text files, state files and index rows. Only messages in Trash are removed;
// any other id is skipped, so nothing is ever purged without first being
// deleted. The ids are recorded in purged.txt first, so an interrupted run
// never lets a sync store the mail again. Mail servers are not touched. It
// returns how many were removed.
func (s *Store) PurgeFromTrash(ctx context.Context, ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgeLocked(ctx, ids)
}

func (s *Store) purgeLocked(ctx context.Context, ids []string) (removed int, err error) {
	msgs := make([]*Message, 0, len(ids))
	for _, id := range ids {
		m, readErr := s.readState(id)
		if errors.Is(readErr, ErrNotFound) {
			s.log.Printf("store: purge %s: %v, skipping", id, readErr)
			continue
		}
		if readErr != nil {
			return 0, readErr
		}
		if m.Box == BoxTrash {
			msgs = append(msgs, m)
		}
	}
	if len(msgs) == 0 {
		return 0, nil
	}

	var added []string
	for _, m := range msgs {
		if _, known := s.purged[m.ID]; !known {
			s.purged[m.ID] = struct{}{}
			added = append(added, m.ID)
		}
	}
	if saveErr := s.savePurgedLocked(); saveErr != nil {
		for _, id := range added {
			delete(s.purged, id)
		}
		return 0, saveErr
	}

	for _, m := range msgs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return removed, fmt.Errorf("empty trash: %w", ctxErr)
		}
		for _, p := range []string{m.Mail.EML, m.Mail.TXT, statePath(m.ID)} {
			if rmErr := s.root.Remove(p); rmErr != nil && !os.IsNotExist(rmErr) {
				return removed, fmt.Errorf("remove %s: %w", s.abs(p), rmErr)
			}
		}
		if delErr := s.idx.Delete(ctx, m.ID); delErr != nil {
			return removed, delErr
		}
		removed++
	}
	return removed, nil
}

// isPurgedLocked reports whether a message was removed by EmptyTrash.
func (s *Store) isPurgedLocked(id string) bool {
	_, ok := s.purged[id]
	return ok
}

// loadPurged reads purged.txt, one message id per line.
func (s *Store) loadPurged() error {
	s.purged = make(map[string]struct{})
	name := s.abs(purgedFile)
	f, err := s.root.Open(purgedFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			s.log.Printf("store: close %s: %v", name, cerr)
		}
	}()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := validID(line); err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		s.purged[line] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	return nil
}

// savePurgedLocked writes purged.txt sorted, so git diffs stay small. Caller
// holds s.mu.
func (s *Store) savePurgedLocked() error {
	list := make([]string, 0, len(s.purged))
	for id := range s.purged {
		list = append(list, id)
	}
	sort.Strings(list)
	var buf bytes.Buffer
	buf.WriteString("# Messages removed with Empty trash. Sync never stores these again.\n")
	for _, id := range list {
		buf.WriteString(id)
		buf.WriteByte('\n')
	}
	return s.writeFileAtomic(purgedFile, buf.Bytes())
}
