package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// ErrBadPattern rejects a body pattern too loose to be safe to act on.
var ErrBadPattern = errors.New("bad pattern")

// minPatternChars is how many characters, wildcards aside, a body pattern
// needs, so a stray "%" cannot match a whole box.
const minPatternChars = 3

// bodyPattern turns what the user typed into a LIKE pattern and the longest
// literal piece of it, which locates the snippet. A pattern always means
// "contains": % matches anything in between, and _ and \ match themselves.
func bodyPattern(typed string) (like, literal string, err error) {
	typed = strings.TrimSpace(typed)
	chars := 0
	for _, r := range typed {
		if r != '%' && !unicode.IsSpace(r) {
			chars++
		}
	}
	if chars < minPatternChars {
		return "", "", fmt.Errorf("%w: use at least %d characters besides %%", ErrBadPattern, minPatternChars)
	}
	typed = "%" + strings.Trim(typed, "%") + "%"
	for _, piece := range strings.Split(typed, "%") {
		if len(piece) > len(literal) {
			literal = piece
		}
	}
	like = strings.NewReplacer(`\`, `\\`, `_`, `\_`).Replace(typed)
	return like, literal, nil
}

// snippetBefore is how much text before the match a snippet shows.
const snippetBefore = 40

// BodyMatches returns up to limit messages in box whose body matches the
// pattern, newest first, each with the text around the match, plus how many
// match in all.
func (s *Store) BodyMatches(ctx context.Context, box, pattern string, limit int) (items []ListItem, total int, err error) {
	like, literal, err := bodyPattern(pattern)
	if err != nil {
		return nil, 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.hasBoxLocked(box) {
		return nil, 0, fmt.Errorf("%w: %s", ErrUnknownBox, box)
	}
	return s.idx.BodyMatches(ctx, box, like, literal, limit)
}

// BodyMatchIDs returns every message in box whose body matches the pattern.
func (s *Store) BodyMatchIDs(ctx context.Context, box, pattern string) ([]string, error) {
	like, _, err := bodyPattern(pattern)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.hasBoxLocked(box) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownBox, box)
	}
	return s.idx.queryIDs(ctx, `SELECT m.id FROM messages_fts f JOIN messages m ON m.id = f.mid
		WHERE m.box = ? AND f.body LIKE ? ESCAPE '\'`, box, like)
}

// BodyMatches scans the stored text of every message in box, once: the total
// comes from the same pass as a window count. It is a full pass over the text
// index, about a second for 180 MiB of mail.
func (i *index) BodyMatches(ctx context.Context, box, like, literal string, limit int) (items []ListItem, total int, err error) {
	rows, err := i.db.QueryContext(ctx, `SELECT m.id, m.box, m.from_name, m.from_addr, m.subject, m.date_unix, m.unread,
		substr(f.body, max(1, instr(lower(f.body), lower(?)) - ?), ?), COUNT(*) OVER ()
		FROM messages_fts f JOIN messages m ON m.id = f.mid
		WHERE m.box = ? AND f.body LIKE ? ESCAPE '\'
		ORDER BY m.date_unix DESC, m.id LIMIT ?`,
		literal, snippetBefore, snippetBefore+len(literal)+80, box, like, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list body matches: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close rows: %w", cerr)
		}
	}()
	for rows.Next() {
		var (
			it     ListItem
			dateU  int64
			unread int
		)
		if err = rows.Scan(&it.ID, &it.Box, &it.FromName, &it.FromAddr, &it.Subject, &dateU, &unread, &it.Snippet, &total); err != nil {
			return nil, 0, fmt.Errorf("scan body match: %w", err)
		}
		it.Date = time.Unix(dateU, 0).UTC()
		it.Unread = unread != 0
		it.Snippet = strings.Join(strings.Fields(it.Snippet), " ")
		items = append(items, it)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate body matches: %w", err)
	}
	return items, total, nil
}
