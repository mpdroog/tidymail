package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mpdroog/tidymail/internal/mailparse"

	// Pure-Go SQLite driver: no cgo, so tidymail cross-compiles cleanly.
	_ "modernc.org/sqlite"
)

// schemaVersion is bumped whenever the index layout changes. The index is
// derived data, so a mismatch simply rebuilds it from the state files.
// schemaVersion 2 added server_box, 3 added account; a version change
// rebuilds the index from the state files.
const schemaVersion = 3

// index is the SQLite search index. It owns no truth: every row can be
// reconstructed from mail.d/state.
type index struct {
	db *sql.DB
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS messages (
	id            TEXT PRIMARY KEY,
	box           TEXT NOT NULL,
	prev_box      TEXT NOT NULL DEFAULT '',
	delayed_until INTEGER NOT NULL DEFAULT 0,
	unread        INTEGER NOT NULL DEFAULT 1,
	from_name     TEXT NOT NULL DEFAULT '',
	from_addr     TEXT NOT NULL DEFAULT '',
	to_addrs      TEXT NOT NULL DEFAULT '',
	cc_addrs      TEXT NOT NULL DEFAULT '',
	subject       TEXT NOT NULL DEFAULT '',
	date_unix     INTEGER NOT NULL DEFAULT 0,
	size          INTEGER NOT NULL DEFAULT 0,
	attachments   INTEGER NOT NULL DEFAULT 0,
	message_id    TEXT NOT NULL DEFAULT '',
	in_reply_to   TEXT NOT NULL DEFAULT '',
	eml           TEXT NOT NULL DEFAULT '',
	updated_unix  INTEGER NOT NULL DEFAULT 0,
	server_box    TEXT NOT NULL DEFAULT '',
	account       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS messages_box_date ON messages (box, date_unix DESC);
CREATE INDEX IF NOT EXISTS messages_from ON messages (from_addr);
CREATE INDEX IF NOT EXISTS messages_delayed ON messages (delayed_until) WHERE delayed_until > 0;
CREATE INDEX IF NOT EXISTS messages_thread ON messages (message_id);
CREATE INDEX IF NOT EXISTS messages_pending ON messages (updated_unix) WHERE server_box != '' AND server_box != box;

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5 (
	mid UNINDEXED,
	subject,
	addrs,
	body,
	tokenize = 'unicode61 remove_diacritics 2'
);
`

// openIndex opens (and if necessary rebuilds) the SQLite index at path.
func openIndex(ctx context.Context, path string) (*index, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open index %s: %w", path, err)
	}
	// The index is written under the store mutex; a single connection keeps
	// SQLite writers serialised and avoids spurious lock contention.
	db.SetMaxOpenConns(1)

	i := &index{db: db}
	if err := i.migrate(ctx); err != nil {
		if cerr := db.Close(); cerr != nil {
			return nil, fmt.Errorf("%w (and closing index: %w)", err, cerr)
		}
		return nil, err
	}
	return i, nil
}

func (i *index) migrate(ctx context.Context) error {
	var have int
	if err := i.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&have); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if have != 0 && have != schemaVersion {
		if _, err := i.db.ExecContext(ctx, `DROP TABLE IF EXISTS messages; DROP TABLE IF EXISTS messages_fts;`); err != nil {
			return fmt.Errorf("drop stale schema v%d: %w", have, err)
		}
	}
	if _, err := i.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := i.db.ExecContext(ctx, `PRAGMA user_version = `+strconv.Itoa(schemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return nil
}

// Close releases the database handle.
func (i *index) Close() error {
	if err := i.db.Close(); err != nil {
		return fmt.Errorf("close index: %w", err)
	}
	return nil
}

// Empty reports whether the index holds no messages, which means it has to be
// rebuilt from the state files.
func (i *index) Empty(ctx context.Context) (bool, error) {
	var n int
	if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		return false, fmt.Errorf("count messages: %w", err)
	}
	return n == 0, nil
}

// Reset clears the index so it can be rebuilt.
func (i *index) Reset(ctx context.Context) error {
	if _, err := i.db.ExecContext(ctx, `DELETE FROM messages; DELETE FROM messages_fts;`); err != nil {
		return fmt.Errorf("clear index: %w", err)
	}
	return nil
}

func addrJoin(list []string) string { return strings.Join(list, " ") }

// Upsert writes (or rewrites) the index row for a message.
func (i *index) Upsert(ctx context.Context, m *Message, body string) (err error) {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin index write: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = fmt.Errorf("%w (and rollback: %w)", err, rbErr)
		}
	}()

	var delayed int64
	if m.DelayedUntil != nil {
		delayed = m.DelayedUntil.Unix()
	}
	unread := 0
	if m.Unread {
		unread = 1
	}
	to := addrJoin(mailparse.AddressStrings(m.Mail.To))
	cc := addrJoin(mailparse.AddressStrings(m.Mail.Cc))

	// The full-text row only depends on immutable data, so it is written once,
	// when the message is new. Ask the primary key rather than messages_fts:
	// mid is UNINDEXED there, so looking it up scans the whole text index and
	// makes every ingest slower than the last. Both rows are written in this
	// transaction, so a messages row always has its text row.
	var known int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id = ?`, m.ID).Scan(&known); err != nil {
		return fmt.Errorf("check index row for %s: %w", m.ID, err)
	}

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO messages (id, box, prev_box, delayed_until, unread, from_name, from_addr,
			to_addrs, cc_addrs, subject, date_unix, size, attachments, message_id, in_reply_to,
			eml, updated_unix, server_box, account)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			box=excluded.box, prev_box=excluded.prev_box, delayed_until=excluded.delayed_until,
			unread=excluded.unread, updated_unix=excluded.updated_unix, server_box=excluded.server_box`,
		m.ID, m.Box, m.PrevBox, delayed, unread, m.Mail.From.Name, m.Mail.From.Address,
		to, cc, m.Mail.Subject, m.Mail.Date.Unix(), m.Mail.Size, len(m.Mail.Attachments),
		m.Mail.MessageID, m.Mail.InReplyTo, m.Mail.EML, m.UpdatedAt.Unix(), m.ServerBox, m.Account); err != nil {
		return fmt.Errorf("index message %s: %w", m.ID, err)
	}

	if known == 0 {
		addrs := strings.Join([]string{m.Mail.From.Name, m.Mail.From.Address, to, cc}, " ")
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages_fts (mid, subject, addrs, body) VALUES (?,?,?,?)`,
			m.ID, m.Mail.Subject, addrs, body); err != nil {
			return fmt.Errorf("index text of %s: %w", m.ID, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit index write for %s: %w", m.ID, err)
	}
	return nil
}

// DueDelayed returns the ids of delayed messages whose delay has expired.
func (i *index) DueDelayed(ctx context.Context, now int64) ([]string, error) {
	return i.queryIDs(ctx, `SELECT id FROM messages WHERE box = ? AND delayed_until > 0 AND delayed_until <= ? ORDER BY delayed_until`, BoxDelayed, now)
}

// IDsInBox returns every message id in a box.
func (i *index) IDsInBox(ctx context.Context, box string) ([]string, error) {
	return i.queryIDs(ctx, `SELECT id FROM messages WHERE box = ?`, box)
}

// IDsFromSender returns every message id sent by an address.
func (i *index) IDsFromSender(ctx context.Context, addr string) ([]string, error) {
	return i.queryIDs(ctx, `SELECT id FROM messages WHERE from_addr = ?`, addr)
}

// AllIDs returns every indexed message id.
func (i *index) AllIDs(ctx context.Context) ([]string, error) {
	return i.queryIDs(ctx, `SELECT id FROM messages`)
}

func (i *index) queryIDs(ctx context.Context, query string, args ...any) (ids []string, err error) {
	rows, err := i.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query ids: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close rows: %w", cerr)
		}
	}()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ids: %w", err)
	}
	return ids, nil
}

// BoxCount is the message tally for one box.
type BoxCount struct {
	Box    string `json:"box"`
	Total  int    `json:"total"`
	Unread int    `json:"unread"`
}

// Counts returns per-box totals.
func (i *index) Counts(ctx context.Context) (counts map[string]BoxCount, err error) {
	rows, err := i.db.QueryContext(ctx, `SELECT box, COUNT(*), SUM(unread) FROM messages GROUP BY box`)
	if err != nil {
		return nil, fmt.Errorf("count boxes: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close rows: %w", cerr)
		}
	}()
	counts = make(map[string]BoxCount)
	for rows.Next() {
		var c BoxCount
		var unread sql.NullInt64
		if err = rows.Scan(&c.Box, &c.Total, &unread); err != nil {
			return nil, fmt.Errorf("scan box count: %w", err)
		}
		c.Unread = int(unread.Int64)
		counts[c.Box] = c
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate box counts: %w", err)
	}
	return counts, nil
}

// ListItem is one row in the message list.
type ListItem struct {
	ID           string     `json:"id"`
	Box          string     `json:"box"`
	FromName     string     `json:"from_name"`
	FromAddr     string     `json:"from_addr"`
	ToAddrs      string     `json:"to_addrs"`
	Subject      string     `json:"subject"`
	Date         time.Time  `json:"date"`
	Size         int64      `json:"size"`
	Attachments  int        `json:"attachments"`
	Unread       bool       `json:"unread"`
	DelayedUntil *time.Time `json:"delayed_until,omitempty"`
	Snippet      string     `json:"snippet,omitempty"`
}

// Query selects a page of messages.
type Query struct {
	Box        string
	Text       string
	UnreadOnly bool
	Limit      int
	Offset     int
}

// maxLimit caps a page so a crafted request cannot pull the whole mailbox.
const maxLimit = 500

// filter returns the FROM and WHERE parts of a query, and its arguments.
// search reports whether it joins the full-text table.
func (q Query) filter() (from, cond string, args []any, search bool) {
	var where []string
	if q.Box != "" {
		where = append(where, "m.box = ?")
		args = append(args, q.Box)
	}
	if q.UnreadOnly {
		where = append(where, "m.unread = 1")
	}
	from = "messages m"
	if fts := ftsQuery(q.Text); fts != "" {
		from = "messages m JOIN messages_fts f ON f.mid = m.id"
		where = append(where, "messages_fts MATCH ?")
		args = append(args, fts)
		search = true
	}
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	return from, cond, args, search
}

// MatchingIDs returns every message id a query matches, without paging.
func (i *index) MatchingIDs(ctx context.Context, q Query) ([]string, error) {
	from, cond, args, _ := q.filter()
	return i.queryIDs(ctx, `SELECT m.id FROM `+from+cond, args...)
}

// Delete removes a message's rows from the index.
func (i *index) Delete(ctx context.Context, id string) (err error) {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin index delete: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = fmt.Errorf("%w (and rollback: %w)", err, rbErr)
		}
	}()
	if _, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id); err != nil {
		return fmt.Errorf("unindex message %s: %w", id, err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM messages_fts WHERE mid = ?`, id); err != nil {
		return fmt.Errorf("unindex text of %s: %w", id, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit index delete for %s: %w", id, err)
	}
	return nil
}

// List returns messages matching the query, newest first, plus the total
// number of matches.
func (i *index) List(ctx context.Context, q Query) (items []ListItem, total int, err error) {
	limit := q.Limit
	if limit <= 0 || limit > maxLimit {
		limit = 50
	}

	from, cond, args, search := q.filter()
	sel := `m.id, m.box, m.from_name, m.from_addr, m.to_addrs, m.subject, m.date_unix,
		m.size, m.attachments, m.unread, m.delayed_until, ''`
	if search {
		sel = `m.id, m.box, m.from_name, m.from_addr, m.to_addrs, m.subject, m.date_unix,
			m.size, m.attachments, m.unread, m.delayed_until,
			snippet(messages_fts, 3, '', '', '…', 14)`
	}

	if err = i.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+from+cond, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count messages: %w", err)
	}

	args = append(args, limit, q.Offset)
	// #nosec G202 -- sel, from and cond are fixed SQL from filter; values are bound
	rows, err := i.db.QueryContext(ctx, `SELECT `+sel+` FROM `+from+cond+` ORDER BY m.date_unix DESC, m.id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list messages: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close rows: %w", cerr)
		}
	}()

	items = make([]ListItem, 0, limit)
	for rows.Next() {
		var (
			it      ListItem
			dateU   int64
			delayed int64
			unread  int
		)
		if err = rows.Scan(&it.ID, &it.Box, &it.FromName, &it.FromAddr, &it.ToAddrs, &it.Subject,
			&dateU, &it.Size, &it.Attachments, &unread, &delayed, &it.Snippet); err != nil {
			return nil, 0, fmt.Errorf("scan message row: %w", err)
		}
		it.Date = time.Unix(dateU, 0).UTC()
		it.Unread = unread != 0
		if delayed > 0 {
			t := time.Unix(delayed, 0).UTC()
			it.DelayedUntil = &t
		}
		items = append(items, it)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate message rows: %w", err)
	}
	return items, total, nil
}

// ftsQuery turns free-form user input into a safe FTS5 MATCH expression. Every
// term is quoted, so punctuation in an address or subject cannot be read as
// query syntax. A "field:value" term restricts the match to that column.
func ftsQuery(text string) string {
	fields := map[string]string{"from": "addrs", "to": "addrs", "addr": "addrs", "subject": "subject", "body": "body"}
	var terms []string
	for _, tok := range strings.Fields(text) {
		col := ""
		if k, v, ok := strings.Cut(tok, ":"); ok && v != "" {
			if c, known := fields[strings.ToLower(k)]; known {
				col, tok = c, v
			}
		}
		prefix := ""
		if strings.HasSuffix(tok, "*") {
			tok = strings.TrimSuffix(tok, "*")
			prefix = "*"
		}
		tok = strings.ReplaceAll(tok, `"`, "")
		if tok == "" {
			continue
		}
		term := `"` + tok + `"` + prefix
		if col != "" {
			term = col + " : " + term
		}
		terms = append(terms, term)
	}
	return strings.Join(terms, " AND ")
}
