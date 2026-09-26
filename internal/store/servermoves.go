package store

import (
	"context"
	"fmt"
)

// PendingMove is a message whose IMAP folder no longer matches its box.
type PendingMove struct {
	ID        string
	Box       string // where it should be
	ServerBox string // where the server copy is
	MessageID string
}

// PendingMoves returns up to limit messages that still have to be moved on the
// IMAP server, oldest change first.
func (s *Store) PendingMoves(ctx context.Context, limit int) ([]PendingMove, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idx.PendingMoves(ctx, limit)
}

// SetServerBox records that the server copy of a message is now in the folder
// of serverBox.
func (s *Store) SetServerBox(ctx context.Context, id, serverBox string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.readState(id)
	if err != nil {
		return err
	}
	if m.ServerBox == serverBox {
		return nil
	}
	m.ServerBox = serverBox
	return s.writeState(ctx, m)
}

// MoveAll moves every message in one box to another and reports how many
// moved. A box can hold tens of thousands of messages, so the lock is taken per
// message: the UI and sync keep working meanwhile, and a message moved
// elsewhere in between is left alone.
func (s *Store) MoveAll(ctx context.Context, from, to string) (int, error) {
	s.mu.RLock()
	for _, box := range []string{from, to} {
		if !s.hasBoxLocked(box) {
			s.mu.RUnlock()
			return 0, fmt.Errorf("%w: %s", ErrUnknownBox, box)
		}
	}
	ids, err := s.idx.IDsInBox(ctx, from)
	s.mu.RUnlock()
	if err != nil || from == to {
		return 0, err
	}

	moved := 0
	for _, id := range ids {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return moved, fmt.Errorf("move %s to %s: %w", from, to, ctxErr)
		}
		ok, moveErr := s.moveIfIn(ctx, id, from, to)
		if moveErr != nil {
			return moved, moveErr
		}
		if ok {
			moved++
		}
	}
	return moved, nil
}

// moveIfIn moves a message to box to, but only while it is still in box from.
func (s *Store) moveIfIn(ctx context.Context, id, from, to string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.readState(id)
	if err != nil {
		return false, err
	}
	if m.Box != from {
		return false, nil
	}
	return true, s.setBoxLocked(ctx, id, to, "", 0)
}

// PendingMoves lists messages whose server_box lags behind box.
func (i *index) PendingMoves(ctx context.Context, limit int) (out []PendingMove, err error) {
	rows, err := i.db.QueryContext(ctx, `SELECT id, box, server_box, message_id FROM messages
		WHERE server_box != '' AND server_box != box ORDER BY updated_unix LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query pending moves: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close rows: %w", cerr)
		}
	}()
	for rows.Next() {
		var p PendingMove
		if err = rows.Scan(&p.ID, &p.Box, &p.ServerBox, &p.MessageID); err != nil {
			return nil, fmt.Errorf("scan pending move: %w", err)
		}
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending moves: %w", err)
	}
	return out, nil
}

// CountPendingMoves returns how many messages still have to move on the server.
func (i *index) CountPendingMoves(ctx context.Context) (int, error) {
	var n int
	if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE server_box != '' AND server_box != box`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count pending moves: %w", err)
	}
	return n, nil
}
